package engineadapter

import (
	"context"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestArchiveRestoreV5MergesProblemAttemptAndPageAssetIdempotently(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	bak, image := problemAttemptArchiveForAdapter(t, "mingming", "source")

	for i := 0; i < 2; i++ {
		if err := f.restore.RestoreHexbak(ctx, bak); err != nil {
			t.Fatalf("restore attempt %d: %v", i+1, err)
		}
	}
	got, err := f.records.ExportProblemAttemptSnapshots(ctx, "mingming")
	if err != nil || !reflect.DeepEqual(got, bak.ProblemAttempts) {
		t.Fatalf("restored ledger differs: err=%v got=%+v want=%+v", err, got, bak.ProblemAttempts)
	}
	owner, file, err := assetstore.Parse(got[0].Problems[0].PageAssetID)
	if err != nil {
		t.Fatal(err)
	}
	gotImage, _, err := assetstore.Read(owner, file)
	if err != nil || !reflect.DeepEqual(gotImage, image) {
		t.Fatalf("restored page image err=%v got=%q", err, gotImage)
	}

	conflict := cloneAdapterHexbak(t, bak)
	conflict.ProblemAttempts[0].Problems[0].StemRaw = "另一份不可覆盖的 OCR 原文"
	if err := usecase.SealHexbak(conflict); err != nil {
		t.Fatal(err)
	}
	if err := f.restore.RestoreHexbak(ctx, conflict); err == nil {
		t.Fatal("same stable ID with different immutable fact must fail closed")
	}
	after, err := f.records.ExportProblemAttemptSnapshots(ctx, "mingming")
	if err != nil || !reflect.DeepEqual(after, bak.ProblemAttempts) {
		t.Fatalf("conflict leaked a partial update: err=%v got=%+v", err, after)
	}
}

func TestArchiveRestoreAsV5MigratesStableProblemAttemptIDsAndRollbackIsExact(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	registerRestoreTarget(t, f)
	ctx := context.Background()

	preImage := []byte("\x89PNG\r\n\x1a\ntarget-pre-page")
	preAssetID, err := assetstore.Save("target-child", preImage)
	if err != nil {
		t.Fatal(err)
	}
	pre := problemAttemptSnapshotForAdapter("target-child", "target", "submission-target", preAssetID)
	if err := f.records.PutProblemAttemptSnapshot(ctx, pre); err != nil {
		t.Fatal(err)
	}
	archive, _ := problemAttemptArchiveForAdapter(t, "source-child", "source")

	d := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 500 }}
	result, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: archive, SourceAgent: "source-child", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "problem-attempt-v5",
	})
	if err != nil {
		t.Fatal(err)
	}
	afterRestore, err := f.records.ExportProblemAttemptSnapshots(ctx, "target-child")
	if err != nil || len(afterRestore) != 2 {
		t.Fatalf("target must contain pre-state plus migrated ledger: err=%v got=%+v", err, afterRestore)
	}
	var migrated k12.ProblemAttemptSnapshot
	for _, snapshot := range afterRestore {
		if snapshot.Problems[0].SubmissionID == archive.ProblemAttempts[0].Problems[0].SubmissionID {
			migrated = snapshot
		}
	}
	if len(migrated.Problems) != 1 ||
		migrated.Problems[0].ProblemID != archive.ProblemAttempts[0].Problems[0].ProblemID ||
		migrated.Attempts[0].AttemptID != archive.ProblemAttempts[0].Attempts[0].AttemptID ||
		migrated.Problems[0].AgentName != "target-child" ||
		migrated.Problems[0].PageAssetID == archive.ProblemAttempts[0].Problems[0].PageAssetID {
		t.Fatalf("restore-as did not preserve stable IDs/rewrite owner asset: %+v", migrated)
	}
	if result.Snapshot == nil || len(result.Snapshot.ProblemAttempts) != 1 ||
		!reflect.DeepEqual(result.Snapshot.ProblemAttempts[0], pre) {
		t.Fatalf("pre-restore snapshot omitted V19 ledger: %+v", result.Snapshot)
	}
	var journal int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_restore_journal
		WHERE migration_id=? AND entity_kind='problem_attempt_ledger'`, result.MigrationID).Scan(&journal); err != nil || journal != 1 {
		t.Fatalf("problem/attempt migration journal=%d err=%v", journal, err)
	}

	if _, err := d.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: result.MigrationID, TargetAgent: "target-child", GuardianConfirmed: true,
	}); err != nil {
		t.Fatal(err)
	}
	afterRollback, err := f.records.ExportProblemAttemptSnapshots(ctx, "target-child")
	if err != nil || len(afterRollback) != 1 || !reflect.DeepEqual(afterRollback[0], pre) {
		t.Fatalf("rollback did not exact-restore V19 ledger: err=%v got=%+v", err, afterRollback)
	}
	if _, err := assetstore.PathFromID(preAssetID); err != nil {
		t.Fatalf("rollback deleted pre-existing target page asset: %v", err)
	}
	if _, err := assetstore.PathFromID(migrated.Problems[0].PageAssetID); err == nil {
		t.Fatalf("rollback left migration-created page asset %q", migrated.Problems[0].PageAssetID)
	}
}

func TestArchiveRestoreAsSourceStillInDatabaseFailsClosedWithoutPartialV19Writes(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	registerRestoreTarget(t, f)
	ctx := context.Background()

	page := []byte("\x89PNG\r\n\x1a\nsource-live-page")
	pageID, err := assetstore.Save("mingming", page)
	if err != nil {
		t.Fatal(err)
	}
	sourceProblem := problemAttemptSnapshotForAdapter("mingming", "source-live", "submission-live", pageID)
	if err := f.records.PutProblemAttemptSnapshot(ctx, sourceProblem); err != nil {
		t.Fatal(err)
	}
	// f already contains a source-owned AgentRecord. Its globally unique record_id
	// cannot coexist with a target-owner copy in the same database.
	dSource := usecase.Deps{Records: f.records, Now: func() int64 { return 600 }}
	archive, err := dSource.Backup(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}

	targetPage := []byte("\x89PNG\r\n\x1a\ntarget-before-conflict")
	targetPageID, err := assetstore.Save("target-child", targetPage)
	if err != nil {
		t.Fatal(err)
	}
	targetBefore := problemAttemptSnapshotForAdapter("target-child", "target-before", "submission-target-before", targetPageID)
	if err := f.records.PutProblemAttemptSnapshot(ctx, targetBefore); err != nil {
		t.Fatal(err)
	}

	d := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 700 }}
	if _, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: archive, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "source-live-global-record-conflict",
	}); err == nil {
		t.Fatal("source-owned global record IDs must fail closed while source is still in this database")
	}
	after, err := f.records.ExportProblemAttemptSnapshots(ctx, "target-child")
	if err != nil || len(after) != 1 || !reflect.DeepEqual(after[0], targetBefore) {
		t.Fatalf("record-id conflict leaked target V19 writes: err=%v got=%+v", err, after)
	}
	sourceAfter, err := f.records.ExportProblemAttemptSnapshots(ctx, "mingming")
	if err != nil || len(sourceAfter) != 1 || !reflect.DeepEqual(sourceAfter[0], sourceProblem) {
		t.Fatalf("record-id conflict mutated source V19 facts: err=%v got=%+v", err, sourceAfter)
	}
	for _, table := range []string{"k12_restore_archives", "k12_restore_snapshots", "k12_restore_migrations", "k12_restore_journal"} {
		var count int
		if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed restore leaked %s rows=%d err=%v", table, count, err)
		}
	}
}

func problemAttemptArchiveForAdapter(t *testing.T, agent, prefix string) (*usecase.Hexbak, []byte) {
	t.Helper()
	image := []byte("\x89PNG\r\n\x1a\narchive-page-" + prefix)
	assetID, mime, digest, err := assetstore.Describe(agent, image)
	if err != nil {
		t.Fatal(err)
	}
	bak := &usecase.Hexbak{
		Version: usecase.HexbakVersion, AgentName: agent, ExportedAt: 200,
		Assets: []usecase.HexbakAsset{{
			AssetID: assetID, OwnerAgent: agent, SHA256: digest, MIME: mime, Data: image,
		}},
		ProblemAttempts: []k12.ProblemAttemptSnapshot{
			problemAttemptSnapshotForAdapter(agent, prefix, "photo-submission-"+prefix, assetID),
		},
	}
	if err := usecase.SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	return bak, image
}

func problemAttemptSnapshotForAdapter(agent, prefix, submissionID, pageAssetID string) k12.ProblemAttemptSnapshot {
	return k12.ProblemAttemptSnapshot{
		Problems: []k12.Problem{{
			ProblemID: "problem-" + prefix, AgentName: agent, SubmissionID: submissionID,
			PageAssetID: pageAssetID, Ordinal: 0, ProblemKind: k12.ProblemKindStandalone,
			Subject: "数学", StemRaw: "18÷3=?", StemMarkdown: "18\\div3=?",
			ConceptIDs: []string{"整数除法"}, CanonicalVersion: 1, CreatedAt: 100, UpdatedAt: 100,
		}},
		Attempts: []k12.Attempt{{
			AttemptID: "attempt-" + prefix, AgentName: agent, SubmissionID: submissionID,
			ProblemID: "problem-" + prefix, AnswerState: "present", AnswerRaw: "6",
			AnswerMarkdown: "6", CreatedAt: 100, UpdatedAt: 100,
		}},
	}
}

func cloneAdapterHexbak(t *testing.T, source *usecase.Hexbak) *usecase.Hexbak {
	t.Helper()
	digest, err := usecase.HexbakDigest(source)
	if err != nil || digest == "" {
		t.Fatalf("archive clone precondition: digest=%q err=%v", digest, err)
	}
	out := *source
	out.Records = append([]*records.AgentRecord(nil), source.Records...)
	out.Assets = append([]usecase.HexbakAsset(nil), source.Assets...)
	for i := range out.Assets {
		out.Assets[i].Data = append([]byte(nil), source.Assets[i].Data...)
	}
	out.ProblemAttempts = make([]k12.ProblemAttemptSnapshot, len(source.ProblemAttempts))
	for i, snapshot := range source.ProblemAttempts {
		out.ProblemAttempts[i].Problems = append([]k12.Problem(nil), snapshot.Problems...)
		out.ProblemAttempts[i].Attempts = append([]k12.Attempt(nil), snapshot.Attempts...)
	}
	return &out
}
