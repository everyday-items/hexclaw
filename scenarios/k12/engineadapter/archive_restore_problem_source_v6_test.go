package engineadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image/png"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestArchiveRestoreV6RestartsProblemSourceClosureAndReplaysFrozenReceipt(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	ctx := context.Background()
	source := newArchiveRestoreFixture(t)
	seeded, err := source.records.ExportAgent(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range seeded {
		if err := source.records.Delete(ctx, "mingming", record.RecordID); err != nil {
			t.Fatal(err)
		}
	}
	command, image := seedRestorableProblemSourceActionV6(t, source)

	deps := usecase.Deps{Records: source.records, Now: func() int64 { return 500 }}
	bak, err := deps.Backup(ctx, "mingming")
	if err != nil {
		t.Fatalf("backup v6 source closure: %v", err)
	}
	if bak.ProblemSource == nil || len(bak.ProblemSource.ActionReceipts) != 1 ||
		len(bak.ProblemSource.InputRevisions) == 0 || len(bak.Assets) == 0 ||
		len(bak.ProblemSource.FinalizationGenerations) != 1 ||
		bak.ProblemSource.FinalizationGenerations[0].Generation != 1 {
		t.Fatalf("v6 backup omitted source closure/assets: %+v", bak)
	}
	archivedSummary, found := archiveV6ProjectingInvocation(
		bak.ProblemSource.ModelInvocations,
	)
	if !found || archivedSummary.Status != k12.ModelInvocationSucceeded ||
		archivedSummary.ResultJSON == "" || archivedSummary.ProviderIdempotencyKey != "" ||
		archivedSummary.ExternalRequestID != "" {
		t.Fatalf("v6 backup omitted safe summary crash-recovery payload: %+v", archivedSummary)
	}
	wantFrozen := append(
		json.RawMessage(nil), bak.ProblemSource.ActionReceipts[0].ResponseJSON...,
	)
	assetID := bak.ProblemSource.PageAssets[0].PageAssetID
	if removed, err := assetstore.Remove("mingming", assetID); err != nil || !removed {
		t.Fatalf("simulate fresh filesystem: removed=%v err=%v", removed, err)
	}

	target := newArchiveRestoreFixture(t)
	if err := target.restore.RestoreHexbak(ctx, bak); err != nil {
		t.Fatalf("restore v6 archive: %v", err)
	}
	restored, err := target.records.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.ActionReceipts) != 1 || len(restored.InputRevisions) == 0 ||
		len(restored.PageAssets) != 1 {
		t.Fatalf("restart projection is incomplete: %+v", restored)
	}
	restoredSummary, err := target.records.GetModelInvocation(
		ctx,
		"mingming",
		archivedSummary.InvocationID,
	)
	if err != nil || restoredSummary.Status != k12.ModelInvocationSucceeded ||
		restoredSummary.ResultJSON != archivedSummary.ResultJSON ||
		restoredSummary.ResultDigest != archivedSummary.ResultDigest {
		t.Fatalf("restore lost summary crash-recovery payload: got=%+v want=%+v err=%v",
			restoredSummary, archivedSummary, err)
	}
	var restoredGeneration int64
	if err := target.db.QueryRowContext(ctx, `SELECT finalization_generation
		FROM k12_grading_jobs WHERE agent_name=? AND record_id=?`,
		"mingming", "job-v6-source").Scan(&restoredGeneration); err != nil ||
		restoredGeneration != 1 {
		t.Fatalf("restored source-action finalization generation=%d err=%v", restoredGeneration, err)
	}
	replay, err := target.records.CommitProblemSourceAction(ctx, command)
	if err != nil {
		t.Fatalf("replay restored source action: %v", err)
	}
	if !bytes.Equal(replay.JSON, wantFrozen) {
		t.Fatalf("frozen source action replay bytes drifted:\n got=%s\nwant=%s", replay.JSON, wantFrozen)
	}
	owner, file, err := assetstore.Parse(assetID)
	if err != nil {
		t.Fatal(err)
	}
	restoredImage, _, err := assetstore.Read(owner, file)
	if err != nil || !reflect.DeepEqual(restoredImage, image) {
		t.Fatalf("restored PageAsset bytes drifted: err=%v", err)
	}
}

func TestArchiveRestoreAsV6MigratesProblemSourceAndRollbackRestoresEmptyClosure(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	ctx := context.Background()
	source := newArchiveRestoreFixture(t)
	seeded, err := source.records.ExportAgent(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range seeded {
		if err := source.records.Delete(ctx, "mingming", record.RecordID); err != nil {
			t.Fatal(err)
		}
	}
	_, image := seedRestorableProblemSourceActionV6(t, source)
	sourceDeps := usecase.Deps{Records: source.records, Now: func() int64 { return 600 }}
	bak, err := sourceDeps.Backup(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}

	target := newArchiveRestoreFixture(t)
	registerRestoreTarget(t, target)
	deps := usecase.Deps{ArchiveMigrator: target.restore, Now: func() int64 { return 700 }}
	result, err := deps.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "source-v6-terminal",
	})
	if err != nil {
		t.Fatalf("restore-as v6 source closure: %v", err)
	}
	if result.Snapshot == nil || result.Snapshot.ProblemSource != nil {
		t.Fatalf("empty pre-restore source closure was not snapshotted exactly: %+v", result.Snapshot)
	}
	migrated, err := target.records.ExportProblemSourceArchiveV6(ctx, "target-child")
	if err != nil {
		t.Fatal(err)
	}
	if migrated.IsEmpty() || len(migrated.ActionReceipts) != 1 ||
		migrated.ActionReceipts[0].AgentName != "target-child" ||
		migrated.Dispatches[0].DispatchID == bak.ProblemSource.Dispatches[0].DispatchID ||
		migrated.PageAssets[0].PageAssetID == bak.ProblemSource.PageAssets[0].PageAssetID {
		t.Fatalf("restore-as source rewrite incomplete: %+v", migrated)
	}
	migratedSummary, found := archiveV6ProjectingInvocation(migrated.ModelInvocations)
	if !found || migratedSummary.Status != k12.ModelInvocationSucceeded ||
		migratedSummary.ResultJSON == "" {
		t.Fatalf("restore-as lost safe summary recovery payload: %+v", migratedSummary)
	}
	var journal int
	if err := target.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_restore_journal
		WHERE migration_id=? AND entity_kind='problem_source_ledger'`,
		result.MigrationID,
	).Scan(&journal); err != nil || journal != 1 {
		t.Fatalf("problem-source migration journal=%d err=%v", journal, err)
	}
	targetAssetID, _, _, err := assetstore.Describe("target-child", image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := assetstore.PathFromID(targetAssetID); err != nil {
		t.Fatalf("migrated source PageAsset bytes missing: %v", err)
	}
	preexistingArtifact, replay, err := target.records.CommitGradingFinalArtifact(
		ctx,
		k12.GradingFinalArtifact{
			AgentName: "target-child", JobID: "job-v6-source",
			StructureVersion: 1,
			CoverageStatus:   k12.GradingFinalArtifactCoverageWithSkips,
			TotalCount:       1, PublishedCount: 0, SkippedCount: 1,
			OrderedCurrentDigestsJSON: `["target-skip-v6"]`,
			CanonicalMarkdown:         "# target final before second restore",
			ArtifactDigest:            "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		},
		1,
	)
	if err != nil || replay {
		t.Fatalf("seed target final artifact: replay=%v err=%v", replay, err)
	}
	second, err := deps.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "source-v6-terminal-second",
	})
	if err != nil {
		t.Fatalf("second restore-as over terminal target: %v", err)
	}
	if second.Snapshot == nil || second.Snapshot.ProblemSource == nil ||
		len(second.Snapshot.ProblemSource.FinalizationGenerations) != 1 ||
		second.Snapshot.ProblemSource.FinalizationGenerations[0].Artifact == nil {
		t.Fatalf("preexisting final artifact missing from rollback snapshot: %+v", second.Snapshot)
	}
	if _, err := deps.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: second.MigrationID, TargetAgent: "target-child",
		GuardianConfirmed: true,
	}); err != nil {
		t.Fatalf("rollback second v6 restore-as: %v", err)
	}
	restoredArtifact, err := target.records.GetGradingFinalArtifactByJob(
		ctx, "target-child", "job-v6-source",
	)
	if err != nil || !reflect.DeepEqual(restoredArtifact, preexistingArtifact) {
		t.Fatalf("rollback did not exactly restore target final artifact: got=%+v want=%+v err=%v", restoredArtifact, preexistingArtifact, err)
	}

	if _, err := deps.RollbackRestoreAs(ctx, usecase.RestoreAsRollbackRequest{
		MigrationID: result.MigrationID, TargetAgent: "target-child",
		GuardianConfirmed: true,
	}); err != nil {
		t.Fatalf("rollback v6 source closure: %v", err)
	}
	after, err := target.records.ExportProblemSourceArchiveV6(ctx, "target-child")
	if err != nil {
		t.Fatal(err)
	}
	if !after.IsEmpty() {
		t.Fatalf("rollback left migrated source facts: %+v", after)
	}
	if _, err := assetstore.PathFromID(targetAssetID); err == nil {
		t.Fatalf("rollback left migration-created source PageAsset %q", targetAssetID)
	}
	var finalArtifacts int
	if err := target.db.QueryRowContext(ctx, `SELECT COUNT(*)
		FROM k12_grading_final_artifacts WHERE agent_name=? AND job_id=?`,
		"target-child", "job-v6-source").Scan(&finalArtifacts); err != nil || finalArtifacts != 0 {
		t.Fatalf("rollback to empty closure left final artifacts=%d err=%v", finalArtifacts, err)
	}
	var metadata int
	if err := target.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM k12_page_assets WHERE agent_name=? AND page_asset_id=?`,
		"target-child", targetAssetID,
	).Scan(&metadata); err != nil || metadata != 0 {
		t.Fatalf("rollback left PageAsset metadata rows=%d err=%v", metadata, err)
	}
}

func TestArchiveRestoreAsV6JournalFailureRollsBackSourceRowsMetadataAndBytes(t *testing.T) {
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	ctx := context.Background()
	source := newArchiveRestoreFixture(t)
	seeded, err := source.records.ExportAgent(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range seeded {
		if err := source.records.Delete(ctx, "mingming", record.RecordID); err != nil {
			t.Fatal(err)
		}
	}
	_, image := seedRestorableProblemSourceActionV6(t, source)
	bak, err := (usecase.Deps{
		Records: source.records, Now: func() int64 { return 800 },
	}).Backup(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}

	target := newArchiveRestoreFixture(t)
	registerRestoreTarget(t, target)
	if _, err := target.db.ExecContext(ctx, `CREATE TRIGGER reject_v6_source_journal
		BEFORE INSERT ON k12_restore_journal
		BEGIN SELECT RAISE(ABORT, 'injected v6 source journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	targetAssetID, _, _, err := assetstore.Describe("target-child", image)
	if err != nil {
		t.Fatal(err)
	}
	deps := usecase.Deps{ArchiveMigrator: target.restore, Now: func() int64 { return 900 }}
	if _, err := deps.RestoreAs(ctx, usecase.RestoreAsRequest{
		Archive: bak, SourceAgent: "mingming", TargetAgent: "target-child",
		GuardianConfirmed: true, IdempotencyKey: "source-v6-journal-failure",
	}); err == nil {
		t.Fatal("injected v6 journal failure must surface")
	}
	after, err := target.records.ExportProblemSourceArchiveV6(ctx, "target-child")
	if err != nil {
		t.Fatal(err)
	}
	if !after.IsEmpty() {
		t.Fatalf("failed v6 transaction leaked source closure: %+v", after)
	}
	if _, err := assetstore.PathFromID(targetAssetID); err == nil {
		t.Fatalf("failed v6 transaction leaked source PageAsset %q", targetAssetID)
	}
	for _, table := range []string{
		"k12_restore_archives", "k12_restore_snapshots", "k12_restore_migrations",
		"k12_restore_journal", "k12_restore_asset_migrations",
	} {
		var count int
		if err := target.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed v6 transaction leaked %s rows=%d err=%v", table, count, err)
		}
	}
}

func seedRestorableProblemSourceActionV6(
	t *testing.T,
	f *archiveRestoreFixture,
) (k12storage.ProblemSourceActionCommand, []byte) {
	t.Helper()
	ctx := context.Background()
	const (
		agent      = "mingming"
		ownerScope = "family-v6"
		dispatchID = "dispatch-v6-source"
		problemID  = "problem-v6-source"
		jobID      = "job-v6-source"
	)
	image := validPNGFixture(t, "v6-source-page")
	assetID, mime, digest, err := assetstore.Describe(agent, image)
	if err != nil {
		t.Fatal(err)
	}
	config, err := png.DecodeConfig(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.records.PreparePageAsset(ctx, k12storage.PageAssetMetadata{
		OwnerScope: ownerScope, AgentName: agent, PageAssetID: assetID,
		ContentDigest: digest, MediaType: mime, SizeBytes: int64(len(image)),
		PixelWidth: config.Width, PixelHeight: config.Height,
		OrientationPolicy:        k12storage.PageAssetOrientationUnverified,
		OrientationPolicyVersion: "unverified-v1", TransformChainJSON: "[]",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := assetstore.Ensure(agent, image); err != nil {
		t.Fatal(err)
	}
	if _, err := f.records.MarkPageAssetReady(ctx, ownerScope, agent, assetID); err != nil {
		t.Fatal(err)
	}

	route := k12.ImageTaskRouteSnapshot{
		Provider: "test", Model: "vision", Route: "test/vision",
		Capability: "vision", SelectionSource: "auto",
		PolicyVersion: "v1", PromptVersion: "v1",
	}
	dispatch := k12.ImageTaskDispatch{
		DispatchID: dispatchID, OwnerScope: ownerScope, AgentName: agent,
		LearnerID: "learner-v6", SourceKind: k12.ImageTaskSourceDesktop,
		SourceRef: "message-v6", SourceAssetRefs: []string{assetID},
		SourceDigest: "sha256:source-v6", TaskIntent: k12.ImageTaskIntentUnknown,
		IntentEvidence: []string{}, Status: k12.ImageTaskStatusRouting,
		RoutingProvenance:           k12.ImageTaskRoutingModelClassified,
		ClassificationRouteSnapshot: route, ClassificationInvocationID: "classification-v6",
		RoutePolicySnapshot: route, IdempotencyKey: "dispatch-key-v6",
		RequestDigest: "sha256:dispatch-v6", AttemptGeneration: 1,
		CreatedAt: 100, UpdatedAt: 100,
	}
	if _, created, err := f.records.PrepareImageTaskDispatch(ctx, dispatch, k12.ImageTaskInvocation{
		InvocationID: "classification-v6", AgentName: agent,
		DispatchID: dispatchID, Operation: k12.ImageTaskOperationClassification,
		OperationKey:  "dispatch:" + dispatchID + ":classification",
		RequestDigest: dispatch.RequestDigest, RouteSnapshot: route,
		Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
		CreatedAt: 100, UpdatedAt: 100,
	}); err != nil || !created {
		t.Fatalf("prepare v6 dispatch: created=%v err=%v", created, err)
	}
	_, target, err := f.records.CommitImageTaskRouting(
		ctx, agent, dispatchID, 0,
		k12storage.ImageTaskRoutingDecision{
			Intent:   k12.ImageTaskIntentCompletedHomework,
			Evidence: []string{"test"}, Confidence: 1,
		},
	)
	if err != nil || target.HomeworkSubmission == nil {
		t.Fatalf("route v6 homework target: target=%+v err=%v", target, err)
	}
	submissionID := target.HomeworkSubmission.SubmissionID
	job, err := k12.NewGradingJobRecord(agent, "session-v6", k12.GradingJobFields{
		SubmissionID: submissionID, SourceKind: "desktop",
		IdempotencyKey:    k12.BuildGradingIdempotencyKey("desktop", "source-v6", 0),
		ConfirmationState: k12.GradingConfirmationPending,
		AnchorState:       k12.GradingAnchorPending,
		ModelSnapshot: k12.GradingModelSnapshot{
			Provider: "test", Model: "grader", Route: "test/grader",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	job.RecordID = jobID
	job.DedupeKey = "job-v6-source"
	job.SchemaVersion = 1
	job.Version = 1
	job.Tags = "[]"
	job.CreatedAt = 100
	job.UpdatedAt = 100
	if _, err := f.records.Put(ctx, job); err != nil {
		t.Fatal(err)
	}
	if _, err := f.records.BindHomeworkSubmissionGradingJob(
		ctx, agent, submissionID, jobID, target.HomeworkSubmission.Version,
	); err != nil {
		t.Fatal(err)
	}
	if err := f.records.PutProblemAttemptSnapshot(ctx, k12.ProblemAttemptSnapshot{
		Problems: []k12.Problem{{
			ProblemID: problemID, AgentName: agent, SubmissionID: submissionID,
			PageAssetID: assetID, Ordinal: 0, ProblemKind: k12.ProblemKindStandalone,
			Subject: "数学", StemRaw: "2+2=?", StemMarkdown: "2+2=?",
			ConceptIDs: []string{"加法"}, CanonicalVersion: 1,
			CreatedAt: 100, UpdatedAt: 100,
		}},
		Attempts: []k12.Attempt{{
			AttemptID: "attempt-v6-source", AgentName: agent,
			SubmissionID: submissionID, ProblemID: problemID,
			AnswerState: "present", AnswerRaw: "4", AnswerMarkdown: "4",
			ConfirmedVersion: 1, InputDigest: "sha256:engine-v6-confirmed-input",
			CreatedAt: 100, UpdatedAt: 100,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	command := k12storage.ProblemSourceActionCommand{
		OwnerScope: ownerScope, TrustedAgentName: agent,
		DispatchID: dispatchID, ProblemID: problemID,
		IdempotencyKey: "skip-v6-source", Action: "skip",
		StructureVersion: 1, ExpectedInputRevision: 1,
		Payload: json.RawMessage(`{}`),
	}
	if _, err := f.records.CommitProblemSourceAction(ctx, command); err != nil {
		t.Fatal(err)
	}
	summaryJSON := archiveV6SummaryResultJSON(jobID, submissionID)
	prepared, created, err := f.records.PrepareModelInvocation(
		ctx,
		k12.ModelInvocation{
			InvocationID: "summary-v6-before-artifact", AgentName: agent,
			JobID: jobID, Stage: k12.GradingStageProjecting,
			RequestDigest: "sha256:summary-v6-request",
			RouteSnapshot: k12.GradingModelSnapshot{
				Provider: "test", Model: "summary", Route: "test/summary",
			},
			Attempt: 1, CreatedAt: 101, UpdatedAt: 101,
		},
	)
	if err != nil || !created {
		t.Fatalf("prepare v6 summary invocation: created=%v err=%v", created, err)
	}
	if prepared, err = f.records.MarkModelInvocationSent(
		ctx, agent, prepared.InvocationID, "provider-key-v6-summary",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := f.records.MarkModelInvocationSucceededWithResult(
		ctx,
		agent,
		prepared.InvocationID,
		archiveV6ModelResultDigest(summaryJSON),
		summaryJSON,
		"provider-request-v6-summary",
	); err != nil {
		t.Fatalf("persist v6 summary payload before artifact: %v", err)
	}
	return command, image
}

func archiveV6ProjectingInvocation(
	invocations []k12.ModelInvocation,
) (k12.ModelInvocation, bool) {
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageProjecting {
			return invocation, true
		}
	}
	return k12.ModelInvocation{}, false
}

func archiveV6SummaryResultJSON(jobID, submissionID string) string {
	return `{"GradingJobID":"` + jobID + `","SubmissionID":"` + submissionID +
		`","Grade":"五年级下","Subject":"数学","knowledge_points":["加法"],` +
		`"sections":[` +
		`{"title":"这页在练什么","content":"练习加法。","source_label":"🤖 AI 归纳·供参考"},` +
		`{"title":"小明要留意","content":"留意计算步骤。","source_label":"🧠 学情信号"},` +
		`{"title":"每道题怎么带（不直接给答案）","content":"先说清题意。","source_label":"🤖 AI 归纳·供参考"}` +
		`]}`
}

func archiveV6ModelResultDigest(raw string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(raw))
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}
