package engineadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestArchiveRestoreV3WritingOCRUpgradesAndRestoresResolvableEvidence(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	bak, _ := archiveForRestoreAsWithWritingOCR(t, "mingming")
	bak.Version = 3
	bak.ArchiveID = ""
	bak.CreativeWorkOCR = nil
	if err := usecase.SealHexbak(bak); err != nil {
		t.Fatal(err)
	}

	d := usecase.Deps{Records: f.records, ArchiveRestorer: f.restore}
	if _, err := d.Restore(context.Background(), bak); err != nil {
		t.Fatal(err)
	}
	work := findCreativeWorkRecord(t, f, "mingming")
	fields, err := k12.ParseCreativeWorkFields(work.Fields)
	if err != nil {
		t.Fatal(err)
	}
	version := fields.Versions[0]
	job, err := f.records.GetCreativeWorkOCRJob(context.Background(), "mingming", version.OCRJobID)
	if err != nil {
		t.Fatalf("v3 inline confirmation was not materialized: %v", err)
	}
	if job.Status != k12.CreativeWorkOCRConfirmed || job.ConfirmedVersion != version.OCRVersion ||
		job.ConfirmedDigest != version.OCRConfirmedDigest || job.ConfirmedContent != version.ContentMarkdown {
		t.Fatalf("restored job=%+v version=%+v", job, version)
	}
}

func TestArchiveRestoreAsWritingOCRKeepsWorkFeedbackEvidenceResolvableAndRollbackExact(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	registerRestoreTarget(t, f)
	bak, _ := archiveForRestoreAsWithWritingOCR(t, "mingming")

	restoreDeps := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 500 }}
	result, err := restoreDeps.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-writing-ocr",
	})
	if err != nil {
		t.Fatal(err)
	}
	work := findCreativeWorkRecord(t, f, "target-child")
	fields, err := k12.ParseCreativeWorkFields(work.Fields)
	if err != nil {
		t.Fatal(err)
	}
	version := fields.Versions[0]
	if version.OCRJobID == "cwocr-writing-source" {
		t.Fatal("restore-as reused globally owned source OCR job id")
	}
	job, err := f.records.GetCreativeWorkOCRJob(ctx, "target-child", version.OCRJobID)
	if err != nil {
		t.Fatalf("target OCR job is not owner-resolvable: %v", err)
	}
	if job.SourceAssetID != version.SourceAssetID || job.ConfirmedDigest != version.OCRConfirmedDigest {
		t.Fatalf("target job/version mismatch: job=%+v version=%+v", job, version)
	}

	feedbackDeps := usecase.Deps{
		Records: f.records,
		Solver:  archiveOCRFeedbackSolver{},
		WorkFeedbackRoute: func(
			context.Context, string,
		) (k12.ImageTaskRouteSnapshot, error) {
			return k12.ImageTaskRouteSnapshot{
				Provider: "test", Model: "feedback-v1",
				Route: "test/feedback-v1", Capability: "text",
				SelectionSource: "explicit", PolicyVersion: "test-v1",
				PromptVersion: "writing-feedback-v1",
			}, nil
		},
	}
	view, err := feedbackDeps.GenerateWorkFeedback(ctx, "target-child", work.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	feedback := view.Fields.Versions[0].StructuredFeedback
	if feedback == nil {
		t.Fatal("missing structured feedback")
	}
	want := "ocr-confirmed:" + version.OCRJobID + ":v1:sha256:" + version.OCRConfirmedDigest
	if !containsString(feedback.EvidenceRefs, want) {
		t.Fatalf("feedback refs=%v want %q", feedback.EvidenceRefs, want)
	}
	resolved, err := f.records.GetCreativeWorkOCRArchiveEvidence(ctx, "target-child", version.OCRJobID, 1)
	if err != nil || resolved.ContentDigest != version.OCRConfirmedDigest {
		t.Fatalf("feedback ref cannot be dereferenced: evidence=%+v err=%v", resolved, err)
	}

	if _, err := restoreDeps.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: result.MigrationID, TargetAgent: "target-child", GuardianConfirmed: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.records.GetCreativeWorkOCRJob(ctx, "target-child", version.OCRJobID); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("rollback left migrated OCR evidence: %v", err)
	}
}

func TestArchiveRestoreAsOCRInsertFailureRollsBackEveryDurabilitySurface(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	registerRestoreTarget(t, f)
	bak, image := archiveForRestoreAsWithWritingOCR(t, "mingming")
	if _, err := f.db.ExecContext(ctx, `CREATE TRIGGER reject_restore_ocr
		BEFORE INSERT ON k12_creative_work_ocr_jobs
		BEGIN SELECT RAISE(ABORT, 'injected OCR import failure'); END`); err != nil {
		t.Fatal(err)
	}

	d := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 500 }}
	_, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-writing-ocr-fail",
	})
	if err == nil {
		t.Fatal("injected OCR import failure must surface")
	}
	targetRecords, readErr := f.records.ExportAgent(ctx, "target-child")
	if readErr != nil || len(targetRecords) != 0 {
		t.Fatalf("failed restore leaked records=%+v err=%v", targetRecords, readErr)
	}
	var ocrRows int
	if scanErr := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_creative_work_ocr_jobs WHERE agent_name='target-child'`).Scan(&ocrRows); scanErr != nil || ocrRows != 0 {
		t.Fatalf("failed restore leaked OCR rows=%d err=%v", ocrRows, scanErr)
	}
	for _, table := range []string{"k12_restore_archives", "k12_restore_snapshots", "k12_restore_migrations", "k12_restore_journal", "k12_restore_asset_migrations"} {
		var n int
		if scanErr := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); scanErr != nil || n != 0 {
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
	targetCfg, _ := f.dispatcher.GetAgent("target-child")
	if targetCfg.Metadata[k12.MetaKeyChildName] != "小红" {
		t.Fatalf("failed restore leaked profile: %v", targetCfg.Metadata)
	}
}

func TestArchiveRestoreAsRollbackRestoresPreexistingTargetOCREvidenceSnapshot(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	registerRestoreTarget(t, f)

	targetBak, _ := archiveForRestoreAsWithWritingOCR(t, "target-child")
	targetBak.Records[0].RecordID = "target-existing-writing-work"
	targetBak.Records[0].DedupeKey = "target-existing-writing-work"
	targetBak.ArchiveID = ""
	if err := usecase.SealHexbak(targetBak); err != nil {
		t.Fatal(err)
	}
	if err := f.restore.RestoreHexbak(ctx, targetBak); err != nil {
		t.Fatal(err)
	}
	preexistingJobID := targetBak.CreativeWorkOCR[0].JobID

	sourceBak, _ := archiveForRestoreAsWithWritingOCR(t, "mingming")
	d := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 500 }}
	result, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: sourceBak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-writing-ocr-over-existing",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot == nil || len(result.Snapshot.CreativeWorkOCR) != 1 ||
		result.Snapshot.CreativeWorkOCR[0].JobID != preexistingJobID {
		t.Fatalf("pre-restore OCR snapshot=%+v", result.Snapshot)
	}
	migratedRecord := findRecordByID(t, f, "target-child", "restore-writing-work")
	migratedFields, err := k12.ParseCreativeWorkFields(migratedRecord.Fields)
	if err != nil {
		t.Fatal(err)
	}
	migratedJobID := migratedFields.Versions[0].OCRJobID
	if migratedJobID == preexistingJobID {
		t.Fatal("migrated job collided with preexisting target job")
	}

	if _, err := d.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: result.MigrationID, TargetAgent: "target-child", GuardianConfirmed: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.records.GetCreativeWorkOCRJob(ctx, "target-child", preexistingJobID); err != nil {
		t.Fatalf("rollback lost preexisting OCR evidence: %v", err)
	}
	if _, err := f.records.GetCreativeWorkOCRJob(ctx, "target-child", migratedJobID); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("rollback left migrated OCR evidence: %v", err)
	}
	targetRecords, err := f.records.ExportAgent(ctx, "target-child")
	if err != nil || len(targetRecords) != 1 || targetRecords[0].RecordID != "target-existing-writing-work" {
		t.Fatalf("rollback records=%+v err=%v", targetRecords, err)
	}
}

func TestArchiveRestoreAsRollbackPreservesTargetOperationalAndUnreferencedOCRJobs(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	f := newArchiveRestoreFixture(t)
	ctx := context.Background()
	registerRestoreTarget(t, f)
	image := validPNGFixture(t, "target-operational-ocr")
	assetID, err := assetstore.Save("target-child", image)
	if err != nil {
		t.Fatal(err)
	}
	sourceDigest := archiveDigestBytes(image)
	pending, _, err := f.records.CreateCreativeWorkOCRJob(
		ctx, "target-child", "target-pending", assetID, sourceDigest, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	unreferenced, _, err := f.records.CreateCreativeWorkOCRJob(
		ctx, "target-child", "target-unreferenced", assetID, sourceDigest, 11,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.records.MarkCreativeWorkOCRProcessing(ctx, "target-child", unreferenced.JobID, 12); err != nil {
		t.Fatal(err)
	}
	if _, err := f.records.MarkCreativeWorkOCRAwaiting(ctx, "target-child", unreferenced.JobID, "raw", 13); err != nil {
		t.Fatal(err)
	}
	if _, err := f.records.ConfirmCreativeWorkOCR(ctx, "target-child", unreferenced.JobID, "confirmed", archiveDigest("confirmed"), 14); err != nil {
		t.Fatal(err)
	}

	sourceBak, _ := archiveForRestoreAsWithWritingOCR(t, "mingming")
	d := usecase.Deps{ArchiveMigrator: f.restore, Now: func() int64 { return 500 }}
	result, err := d.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: sourceBak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "restore-preserve-operational-ocr",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: result.MigrationID, TargetAgent: "target-child", GuardianConfirmed: true,
	}); err != nil {
		t.Fatal(err)
	}
	pendingAfter, err := f.records.GetCreativeWorkOCRJob(ctx, "target-child", pending.JobID)
	if err != nil || pendingAfter.Status != k12.CreativeWorkOCRPending {
		t.Fatalf("rollback removed/changed target pending job: %+v err=%v", pendingAfter, err)
	}
	unreferencedAfter, err := f.records.GetCreativeWorkOCRJob(ctx, "target-child", unreferenced.JobID)
	if err != nil || unreferencedAfter.Status != k12.CreativeWorkOCRConfirmed {
		t.Fatalf("rollback removed/changed target unreferenced confirmed job: %+v err=%v", unreferencedAfter, err)
	}
}

type archiveOCRFeedbackSolver struct{}

func (archiveOCRFeedbackSolver) Solve(context.Context, string, string, string) (usecase.SolveResult, error) {
	return usecase.SolveResult{}, nil
}

func (archiveOCRFeedbackSolver) GenerateWorkFeedback(context.Context, usecase.WorkFeedbackRequest) (usecase.WorkFeedbackOutput, error) {
	return usecase.WorkFeedbackOutput{
		Feedback:   "这句话的比喻很清楚；建议补充柳枝随风移动的细节。",
		SkillStamp: "writing-feedback@1.0.0/embedded",
	}, nil
}

func archiveForRestoreAsWithWritingOCR(t *testing.T, agent string) (*usecase.Hexbak, []byte) {
	t.Helper()
	image := validPNGFixture(t, "restore-as-writing-ocr")
	assetID, mime, sourceDigest, err := assetstore.Describe(agent, image)
	if err != nil {
		t.Fatal(err)
	}
	content := "春天的校园\n柳枝像绿色丝带。"
	contentDigest := archiveDigest(content)
	work, err := k12.NewCreativeWorkRecord(agent, "source-session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting, Title: "春天", Task: "观察春景",
		Versions: []k12.CreativeWorkVersion{{
			VersionID: "v1", SourceAssetID: assetID, ContentMarkdown: content,
			OCRJobID: "cwocr-writing-source", OCRRaw: "春天的校园\n柳枝象绿色丝带。",
			OCRVersion: 1, OCRConfirmedDigest: contentDigest, ContentConfirmedAt: 101,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	work.RecordID = "restore-writing-work"
	work.DedupeKey = "restore-writing-work"
	work.SchemaVersion = 1
	work.Version = 1
	work.Tags = "[]"
	work.CreatedAt = 100
	work.UpdatedAt = 101
	bak := &usecase.Hexbak{
		Version: usecase.HexbakVersion, AgentName: agent, ExportedAt: 123,
		Records: []*records.AgentRecord{work},
		Profile: &k12.ChildProfile{ChildName: "小明", GradeTerm: "五年级上"},
		Assets: []usecase.HexbakAsset{{
			AssetID: assetID, OwnerAgent: agent, SHA256: sourceDigest, MIME: mime, Data: image,
		}},
		CreativeWorkOCR: []k12.CreativeWorkOCRArchiveEvidence{{
			JobID: "cwocr-writing-source", AgentName: agent, RequestID: "writing-source-request",
			SourceAssetID: assetID, SourceDigest: sourceDigest,
			OCRRaw: "春天的校园\n柳枝象绿色丝带。", Version: 1,
			ContentMarkdown: content, ContentDigest: contentDigest, ConfirmedAt: 101,
			AttemptCount: 1, JobCreatedAt: 90, JobLastUpdatedAt: 101,
		}},
	}
	if err := usecase.SealHexbak(bak); err != nil {
		t.Fatal(err)
	}
	return bak, image
}

func findCreativeWorkRecord(t *testing.T, f *archiveRestoreFixture, agent string) *records.AgentRecord {
	t.Helper()
	recs, err := f.records.ExportAgent(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.Collection == k12.CollectionCreativeWork {
			return rec
		}
	}
	t.Fatalf("creative work not found for %s: %+v", agent, recs)
	return nil
}

func findRecordByID(t *testing.T, f *archiveRestoreFixture, agent, recordID string) *records.AgentRecord {
	t.Helper()
	recs, err := f.records.ExportAgent(context.Background(), agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if rec.RecordID == recordID {
			return rec
		}
	}
	t.Fatalf("record %s not found for %s: %+v", recordID, agent, recs)
	return nil
}

func archiveDigest(value string) string {
	return archiveDigestBytes([]byte(value))
}

func archiveDigestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}
