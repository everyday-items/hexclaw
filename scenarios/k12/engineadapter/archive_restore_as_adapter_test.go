package engineadapter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestArchiveRestoreAsMigratesPackedAssetsAndRollbackRemovesOnlyCreatedTargets(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	registerRestoreTarget(t, f)
	bak, sourceBytes := archiveForRestoreAsWithAsset(t)

	d := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 500 }}
	result, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-assets-once",
	})
	if err != nil {
		t.Fatal(err)
	}
	targetRecords, err := f.records.ExportAgent(ctx, "target-child")
	if err != nil {
		t.Fatal(err)
	}
	targetRefs, err := usecase.ReferencedHexbakAssetIDs(targetRecords)
	if err != nil || len(targetRefs) != 1 {
		t.Fatalf("target refs=%v err=%v records=%+v", targetRefs, err, targetRecords)
	}
	owner, file, err := assetstore.Parse(targetRefs[0])
	if err != nil || owner != "target-child" {
		t.Fatalf("target asset id=%q owner=%q err=%v", targetRefs[0], owner, err)
	}
	got, _, err := assetstore.Read(owner, file)
	if err != nil || !bytes.Equal(got, sourceBytes) {
		t.Fatalf("target asset bytes=%q err=%v", got, err)
	}
	var createdNew int
	if err := f.db.QueryRowContext(ctx, `SELECT created_new FROM k12_restore_asset_migrations WHERE migration_id=?`, result.MigrationID).Scan(&createdNew); err != nil || createdNew != 1 {
		t.Fatalf("asset migration evidence created_new=%d err=%v", createdNew, err)
	}

	if _, err := d.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: result.MigrationID, TargetAgent: "target-child", GuardianConfirmed: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := assetstore.PathFromID(targetRefs[0]); err == nil {
		t.Fatalf("rollback left migration-created target asset %q", targetRefs[0])
	}
	// Immutable source payload remains recoverable even after target cleanup.
	var archiveJSON string
	if err := f.db.QueryRowContext(ctx, `SELECT archive_json FROM k12_restore_archives WHERE archive_digest=?`, result.OriginalArchiveDigest).Scan(&archiveJSON); err != nil || !bytes.Contains([]byte(archiveJSON), []byte(`"assets"`)) {
		t.Fatalf("original packed archive missing after rollback: err=%v json=%s", err, archiveJSON)
	}
}

func TestArchiveRestoreAsRollbackPreservesPreexistingTargetContentAddress(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	registerRestoreTarget(t, f)
	bak, image := archiveForRestoreAsWithAsset(t)
	targetID, created, err := assetstore.Ensure("target-child", image)
	if err != nil || !created {
		t.Fatalf("preinstall target asset id=%q created=%v err=%v", targetID, created, err)
	}

	d := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 500 }}
	result, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-assets-existing",
	})
	if err != nil {
		t.Fatal(err)
	}
	var createdNew int
	if err := f.db.QueryRowContext(ctx, `SELECT created_new FROM k12_restore_asset_migrations WHERE migration_id=?`, result.MigrationID).Scan(&createdNew); err != nil || createdNew != 0 {
		t.Fatalf("preexisting asset evidence created_new=%d err=%v", createdNew, err)
	}
	if _, err := d.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: result.MigrationID, TargetAgent: "target-child", GuardianConfirmed: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := assetstore.PathFromID(targetID); err != nil {
		t.Fatalf("rollback deleted preexisting target asset: %v", err)
	}
}

func TestArchiveRestoreAsPersistsOriginalSnapshotJournalAndRollsBackIdempotently(t *testing.T) {
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	registerRestoreTarget(t, f)
	targetOld := validTargetMistake(t, "target-old", "目标原题")
	if _, err := f.records.Put(ctx, targetOld); err != nil {
		t.Fatal(err)
	}

	d := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 500 }}
	bak := archiveForRestoreAs(t, 2)
	result, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-as-once",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != usecase.RestoreMigrationCompleted || result.MigrationID == "" {
		t.Fatalf("result=%+v", result)
	}
	if result.OriginalArchiveDigest == "" || result.SnapshotDigest == "" || result.MigratedChecksum == "" {
		t.Fatalf("missing durable evidence digest: %+v", result)
	}
	if result.Snapshot == nil || result.Snapshot.AgentName != "target-child" || len(result.Snapshot.Records) != 1 {
		t.Fatalf("pre-restore snapshot=%+v", result.Snapshot)
	}

	targetRecords, err := f.records.ExportAgent(ctx, "target-child")
	if err != nil {
		t.Fatal(err)
	}
	if len(targetRecords) != 2 {
		t.Fatalf("target records=%+v", targetRecords)
	}
	for _, rec := range targetRecords {
		if rec.AgentName != "target-child" {
			t.Fatalf("cross-owner record escaped rewrite: %+v", rec)
		}
	}
	targetCfg, _ := f.dispatcher.GetAgent("target-child")
	if targetCfg.Metadata[k12.MetaKeyChildName] != "小明" || targetCfg.Metadata[k12.MetaKeyGradeTerm] != "五年级上" {
		t.Fatalf("target profile not migrated: %v", targetCfg.Metadata)
	}

	var archiveJSON, snapshotJSON string
	if err := f.db.QueryRowContext(ctx, `SELECT archive_json FROM k12_restore_archives WHERE archive_digest=?`, result.OriginalArchiveDigest).Scan(&archiveJSON); err != nil {
		t.Fatal(err)
	}
	if err := f.db.QueryRowContext(ctx, `SELECT snapshot_json FROM k12_restore_snapshots WHERE snapshot_digest=?`, result.SnapshotDigest).Scan(&snapshotJSON); err != nil {
		t.Fatal(err)
	}
	if archiveJSON == "" || snapshotJSON == "" {
		t.Fatal("archive/snapshot recoverable payload was not retained")
	}
	var journalCount int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_restore_journal WHERE migration_id=?`, result.MigrationID).Scan(&journalCount); err != nil {
		t.Fatal(err)
	}
	if journalCount < 3 || result.JournalEntries != journalCount {
		t.Fatalf("journal result=%d db=%d want record/profile/checksum entries", result.JournalEntries, journalCount)
	}

	second, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-as-once",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Idempotent || second.MigrationID != result.MigrationID {
		t.Fatalf("idempotent retry=%+v first=%+v", second, result)
	}
	var journalAfterRetry int
	_ = f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_restore_journal WHERE migration_id=?`, result.MigrationID).Scan(&journalAfterRetry)
	if journalAfterRetry != journalCount {
		t.Fatalf("idempotent retry appended journal: before=%d after=%d", journalCount, journalAfterRetry)
	}

	rolled, err := d.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: result.MigrationID, TargetAgent: "target-child", GuardianConfirmed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Status != usecase.RestoreMigrationRolledBack {
		t.Fatalf("rollback=%+v", rolled)
	}
	targetRecords, _ = f.records.ExportAgent(ctx, "target-child")
	if len(targetRecords) != 1 || targetRecords[0].RecordID != targetOld.RecordID {
		t.Fatalf("rollback did not exactly restore pre-state: %+v", targetRecords)
	}
	targetCfg, _ = f.dispatcher.GetAgent("target-child")
	if targetCfg.Metadata[k12.MetaKeyChildName] != "小红" || targetCfg.Metadata[k12.MetaKeyGradeTerm] != "四年级下" {
		t.Fatalf("rollback did not restore profile: %v", targetCfg.Metadata)
	}

	rolledAgain, err := d.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: result.MigrationID, TargetAgent: "target-child", GuardianConfirmed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rolledAgain.Idempotent || rolledAgain.Status != usecase.RestoreMigrationRolledBack {
		t.Fatalf("idempotent rollback=%+v", rolledAgain)
	}
	var archives, snapshots int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM k12_restore_archives`).Scan(&archives)
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM k12_restore_snapshots`).Scan(&snapshots)
	if archives != 1 || snapshots != 1 {
		t.Fatalf("rollback removed immutable evidence: archives=%d snapshots=%d", archives, snapshots)
	}
}

func TestArchiveRestoreAsMidTransactionFailureLeavesZeroPartialWrites(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	registerRestoreTarget(t, f)
	targetOld := validTargetMistake(t, "target-old", "目标原题")
	if _, err := f.records.Put(ctx, targetOld); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ExecContext(ctx, `CREATE TRIGGER reject_restore_journal
		BEFORE INSERT ON k12_restore_journal
		BEGIN SELECT RAISE(ABORT, 'injected journal failure'); END`); err != nil {
		t.Fatal(err)
	}

	d := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 500 }}
	bak, image := archiveForRestoreAsWithAsset(t)
	_, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-as-fail",
	})
	if err == nil {
		t.Fatal("injected mid-transaction failure must surface")
	}

	targetRecords, readErr := f.records.ExportAgent(ctx, "target-child")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(targetRecords) != 1 || targetRecords[0].RecordID != targetOld.RecordID {
		t.Fatalf("failed restore leaked records: %+v", targetRecords)
	}
	targetCfg, _ := f.dispatcher.GetAgent("target-child")
	if targetCfg.Metadata[k12.MetaKeyChildName] != "小红" {
		t.Fatalf("failed restore leaked profile: %v", targetCfg.Metadata)
	}
	for _, table := range []string{"k12_restore_archives", "k12_restore_snapshots", "k12_restore_migrations", "k12_restore_journal", "k12_restore_asset_migrations"} {
		var n int
		if scanErr := f.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); scanErr != nil || n != 0 {
			t.Fatalf("failed restore leaked %s rows=%d err=%v", table, n, scanErr)
		}
	}
	targetID, _, _, describeErr := assetstore.Describe("target-child", image)
	if describeErr != nil {
		t.Fatal(describeErr)
	}
	if _, pathErr := assetstore.PathFromID(targetID); pathErr == nil {
		t.Fatalf("failed restore leaked target asset %q", targetID)
	}
	if _, statErr := os.Stat(filepath.Join(assetstore.Root(), "target-child")); !os.IsNotExist(statErr) {
		t.Fatalf("failed restore left target asset directory: %v", statErr)
	}
}

func TestRollbackRestoreAsRequiresExplicitConfirmationAndExactTarget(t *testing.T) {
	f := newArchiveRestoreFixture(t)
	d := usecase.Deps{ArchiveMigrator: f.restore}
	for _, req := range []usecase.RestoreAsRollbackRequest{
		{MigrationID: "migration", TargetAgent: "target-child", GuardianConfirmed: false},
		{MigrationID: "migration", TargetAgent: "", GuardianConfirmed: true},
	} {
		_, err := d.RollbackRestoreAs(context.Background(), req)
		if !errors.Is(err, usecase.ErrInvalidInput) && !errors.Is(err, usecase.ErrGuardianConfirmationRequired) {
			t.Fatalf("req=%+v err=%v", req, err)
		}
	}
}

func registerRestoreTarget(t *testing.T, f *archiveRestoreFixture) {
	t.Helper()
	cfg := router.AgentConfig{Name: "target-child", Metadata: map[string]string{
		"provider": "glm", k12.MetaKeyChildName: "小红", k12.MetaKeyGradeTerm: "四年级下",
	}}
	if err := f.dispatcher.Register(cfg); err != nil {
		t.Fatal(err)
	}
	if err := f.agentStore.SaveAgent(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
}

func archiveForRestoreAs(t *testing.T, version int) *usecase.Hexbak {
	t.Helper()
	rec := validIncomingMistake(t)
	rec.AgentName = "mingming"
	rec.RecordID = "restore-as-record"
	rec.DedupeKey = "restore-as-record"
	bak := &usecase.Hexbak{
		Version: version, AgentName: "mingming", ExportedAt: 123,
		Records: []*records.AgentRecord{rec},
		Profile: &k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上"},
	}
	if version >= 3 {
		bak.ArchiveID = "archive-restore-as"
	}
	if err := usecase.SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	return bak
}

func archiveForRestoreAsWithAsset(t *testing.T) (*usecase.Hexbak, []byte) {
	t.Helper()
	image := validPNGFixture(t, "restore-as-adapter-asset")
	id, mime, digest, err := assetstore.Describe("mingming", image)
	if err != nil {
		t.Fatal(err)
	}
	work, err := k12.NewCreativeWorkRecord("mingming", "source-session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt, Title: "向日葵", Task: "观察色彩",
		Versions: []k12.CreativeWorkVersion{{VersionID: "v1", SourceAssetID: id}},
	})
	if err != nil {
		t.Fatal(err)
	}
	work.RecordID = "restore-work"
	work.DedupeKey = "restore-work"
	work.SchemaVersion = 1
	work.Version = 1
	work.Tags = "[]"
	work.CreatedAt = 100
	work.UpdatedAt = 100
	set, err := k12.NewPracticeSetRecord("mingming", "source-session", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual, Title: "回传卷",
		Items: []k12.PracticeItem{{ItemID: "item-1", QuestionMarkdown: "2+2=?"}},
		ReturnAssets: []k12.PracticeReturnAsset{{
			ReturnID: "return-1", AssetID: id, ItemIDs: []string{"item-1"}, ReturnedAt: 100,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	set.RecordID = "restore-set"
	set.DedupeKey = "restore-set"
	set.SchemaVersion = 1
	set.Version = 1
	set.Tags = "[]"
	set.CreatedAt = 100
	set.UpdatedAt = 100
	bak := &usecase.Hexbak{
		Version: usecase.HexbakVersion, AgentName: "mingming", ExportedAt: 123,
		Records: []*records.AgentRecord{work, set},
		Profile: &k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上"},
		Assets: []usecase.HexbakAsset{{
			AssetID: id, OwnerAgent: "mingming", SHA256: digest, MIME: mime, Data: image,
		}},
	}
	if err := usecase.SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	return bak, image
}

func validTargetMistake(t *testing.T, id, question string) *records.AgentRecord {
	t.Helper()
	rec, err := k12.NewMistakeRecord("target-child", "target-session", k12.MistakeFields{Question: question})
	if err != nil {
		t.Fatal(err)
	}
	rec.RecordID = id
	rec.DedupeKey = id
	return rec
}
