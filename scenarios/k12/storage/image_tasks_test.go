package k12storage_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func testImageRoute() k12.ImageTaskRouteSnapshot {
	return k12.ImageTaskRouteSnapshot{
		Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
		Route: "hexclaw-gpt/gpt-5.6-sol", Capability: "vision",
		SelectionSource: "explicit", PolicyVersion: "image-task-routing-v1",
		PromptVersion: "image-task-classifier-v1",
	}
}

func testImageTaskDispatch() k12.ImageTaskDispatch {
	asset := "asset://mingming/" + strings.Repeat("a", 64) + ".png"
	return k12.ImageTaskDispatch{
		DispatchID:                  "dispatch-1",
		AgentName:                   "mingming",
		LearnerID:                   "learner-1",
		SourceKind:                  k12.ImageTaskSourceDesktop,
		SourceRef:                   "message-1",
		SourceSessionID:             "session-1",
		SourceAssetRefs:             []string{asset},
		SourceDigest:                "sha256:source",
		MessageIntent:               "请看这张图",
		TaskIntent:                  k12.ImageTaskIntentUnknown,
		IntentEvidence:              []string{},
		IntentConfidence:            0,
		Status:                      k12.ImageTaskStatusRouting,
		ClassificationRouteSnapshot: testImageRoute(),
		RoutePolicySnapshot:         testImageRoute(),
		ClassificationInvocationID:  "invocation-classify-1",
		IdempotencyKey:              "desktop:message-1:g1",
		RequestDigest:               "sha256:request",
		AttemptGeneration:           1,
		CreatedAt:                   100,
		UpdatedAt:                   100,
	}
}

func TestImageTaskPrepareIsIdempotentAndRouteImmutable(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	invocation := k12.ImageTaskInvocation{
		InvocationID: "invocation-classify-1", AgentName: "mingming",
		DispatchID: "dispatch-1", Operation: k12.ImageTaskOperationClassification,
		OperationKey:  "dispatch:dispatch-1:classification",
		RequestDigest: "sha256:classify-request", RouteSnapshot: testImageRoute(),
		Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
		CreatedAt: 100, UpdatedAt: 100,
	}
	got, created, err := store.PrepareImageTaskDispatch(ctx, dispatch, invocation)
	if err != nil || !created || got.DispatchID != dispatch.DispatchID {
		t.Fatalf("prepare got=%+v created=%v err=%v", got, created, err)
	}
	replay, created, err := store.PrepareImageTaskDispatch(ctx, dispatch, invocation)
	if err != nil || created || replay.DispatchID != dispatch.DispatchID {
		t.Fatalf("replay got=%+v created=%v err=%v", replay, created, err)
	}

	changed := dispatch
	changed.DispatchID = "attacker-dispatch"
	changed.ClassificationInvocationID = "attacker-invocation"
	changed.ClassificationRouteSnapshot.Model = "other-model"
	changed.ClassificationRouteSnapshot.Route = "hexclaw-gpt/other-model"
	if _, _, err := store.PrepareImageTaskDispatch(ctx, changed, invocation); !errors.Is(err, k12storage.ErrImageTaskConflict) {
		t.Fatalf("same key with another route must fail closed, got %v", err)
	}
}

func TestConfirmImageTaskIntentCreatesCandidateTargetWithoutRewritingClassificationReceipt(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	invocation := k12.ImageTaskInvocation{
		InvocationID: "invocation-classify-1", AgentName: "mingming",
		DispatchID: "dispatch-1", Operation: k12.ImageTaskOperationClassification,
		OperationKey:  "dispatch:dispatch-1:classification",
		RequestDigest: "sha256:classify-request", RouteSnapshot: testImageRoute(),
		Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
		CreatedAt: 100, UpdatedAt: 100,
	}
	if _, _, err := store.PrepareImageTaskDispatch(ctx, dispatch, invocation); err != nil {
		t.Fatal(err)
	}
	awaiting, _, err := store.CommitImageTaskRouting(ctx, "mingming", dispatch.DispatchID, 0,
		k12storage.ImageTaskRoutingDecision{
			Intent: k12.ImageTaskIntentUnknown, Evidence: []string{"mixed page evidence"},
			Confidence: 0.51, ConfirmationCandidates: []k12.ImageTaskIntent{
				k12.ImageTaskIntentWriting, k12.ImageTaskIntentArtwork,
			},
			InvocationResultDigest: "sha256:immutable-classifier-result",
		})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.GetImageTaskInvocation(ctx, "mingming", dispatch.ClassificationInvocationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ConfirmImageTaskIntent(
		ctx, "mingming", dispatch.DispatchID, awaiting.Version,
		k12.ImageTaskIntentCompletedHomework,
	); !errors.Is(err, k12storage.ErrImageTaskConflict) {
		t.Fatalf("non-candidate confirmation must fail closed, got %v", err)
	}
	confirmed, target, err := store.ConfirmImageTaskIntent(
		ctx, "mingming", dispatch.DispatchID, awaiting.Version,
		k12.ImageTaskIntentArtwork,
	)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Status != k12.ImageTaskStatusRouted ||
		confirmed.TaskIntent != k12.ImageTaskIntentArtwork ||
		target.CreativeIntake == nil {
		t.Fatalf("wrong confirmation target: dispatch=%+v target=%+v", confirmed, target)
	}
	after, err := store.GetImageTaskInvocation(ctx, "mingming", dispatch.ClassificationInvocationID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ResultDigest != before.ResultDigest || after.ResultJSON != before.ResultJSON ||
		after.RouteSnapshot != before.RouteSnapshot {
		t.Fatalf("parent confirmation rewrote classifier receipt: before=%+v after=%+v", before, after)
	}
}

func TestImageTaskArtworkRoutesToIntakeThenPromotesExactlyOnce(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	invocation := k12.ImageTaskInvocation{
		InvocationID: "invocation-classify-1", AgentName: "mingming",
		DispatchID: "dispatch-1", Operation: k12.ImageTaskOperationClassification,
		OperationKey:  "dispatch:dispatch-1:classification",
		RequestDigest: "sha256:classify-request", RouteSnapshot: testImageRoute(),
		Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
		CreatedAt: 100, UpdatedAt: 100,
	}
	if _, _, err := store.PrepareImageTaskDispatch(ctx, dispatch, invocation); err != nil {
		t.Fatal(err)
	}
	routed, target, err := store.CommitImageTaskRouting(ctx, "mingming", dispatch.DispatchID, 0,
		k12storage.ImageTaskRoutingDecision{
			Intent: k12.ImageTaskIntentArtwork, Evidence: []string{"freeform_drawing"},
			Confidence: 0.99, WorkTitleCandidate: nil, TaskRequirementCandidate: nil,
			InvocationResultDigest: "sha256:classification-result",
		})
	if err != nil {
		t.Fatal(err)
	}
	if routed.Status != k12.ImageTaskStatusRouted ||
		routed.TargetObjectType != k12.ImageTaskTargetCreativeWorkIntake {
		t.Fatalf("wrong routed dispatch: %+v", routed)
	}
	if target.CreativeIntake == nil || target.CreativeIntake.Status != k12.CreativeWorkIntakeReady {
		t.Fatalf("art target must be ready intake: %+v", target)
	}
	var formalBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_creative_works`).Scan(&formalBefore); err != nil {
		t.Fatal(err)
	}
	if formalBefore != 0 {
		t.Fatalf("routing created a premature formal work: %d", formalBefore)
	}

	workID, created, err := store.PromoteCreativeWorkIntake(ctx, "mingming",
		target.CreativeIntake.IntakeID, target.CreativeIntake.Version)
	if err != nil || !created || workID == "" {
		t.Fatalf("promote work=%q created=%v err=%v", workID, created, err)
	}
	replayedID, created, err := store.PromoteCreativeWorkIntake(ctx, "mingming",
		target.CreativeIntake.IntakeID, target.CreativeIntake.Version+1)
	if err != nil || created || replayedID != workID {
		t.Fatalf("replay work=%q created=%v err=%v", replayedID, created, err)
	}
	var works, versions int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_creative_works WHERE source_intake_id=?`,
		target.CreativeIntake.IntakeID).Scan(&works); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_creative_work_versions WHERE work_record_id=?`,
		workID).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if works != 1 || versions != 1 {
		t.Fatalf("atomic promotion works=%d versions=%d", works, versions)
	}
	intake, err := store.GetCreativeWorkIntake(ctx, "mingming", target.CreativeIntake.IntakeID)
	if err != nil || intake.Status != k12.CreativeWorkIntakePromoted || intake.PromotedWorkID != workID {
		t.Fatalf("promoted intake=%+v err=%v", intake, err)
	}
}

func TestPromoteCreativeWorkIntakeRejectsNonAssetSourceEvenWhenStorageCalledDirectly(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	dispatch.SourceAssetRefs = []string{"/tmp/untrusted-drawing.png"}
	invocation := k12.ImageTaskInvocation{
		InvocationID: "invocation-classify-1", AgentName: "mingming",
		DispatchID: "dispatch-1", Operation: k12.ImageTaskOperationClassification,
		OperationKey: "dispatch:dispatch-1:classification", RequestDigest: "sha256:request",
		RouteSnapshot: testImageRoute(), Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
		CreatedAt: 100, UpdatedAt: 100,
	}
	if _, _, err := store.PrepareImageTaskDispatch(ctx, dispatch, invocation); err != nil {
		t.Fatal(err)
	}
	_, target, err := store.CommitImageTaskRouting(ctx, "mingming", "dispatch-1", 0,
		k12storage.ImageTaskRoutingDecision{
			Intent: k12.ImageTaskIntentArtwork, Evidence: []string{"drawing"}, Confidence: 1,
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PromoteCreativeWorkIntake(
		ctx, "mingming", target.CreativeIntake.IntakeID, target.CreativeIntake.Version,
	); !errors.Is(err, k12storage.ErrImageTaskConflict) {
		t.Fatalf("non asset:// source reached formal work: %v", err)
	}
}

func TestImageTaskWritingCannotPromoteBeforeCanonicalFreeze(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	invocation := k12.ImageTaskInvocation{
		InvocationID: "invocation-classify-1", AgentName: "mingming",
		DispatchID: "dispatch-1", Operation: k12.ImageTaskOperationClassification,
		OperationKey: "dispatch:dispatch-1:classification", RequestDigest: "sha256:req",
		RouteSnapshot: testImageRoute(), Status: k12.ImageTaskInvocationPrepared,
		Attempt: 1, CreatedAt: 100, UpdatedAt: 100,
	}
	if _, _, err := store.PrepareImageTaskDispatch(ctx, dispatch, invocation); err != nil {
		t.Fatal(err)
	}
	_, target, err := store.CommitImageTaskRouting(ctx, "mingming", dispatch.DispatchID, 0,
		k12storage.ImageTaskRoutingDecision{
			Intent: k12.ImageTaskIntentWriting, Evidence: []string{"handwritten_composition"},
			Confidence: 0.98, InvocationResultDigest: "sha256:result",
		})
	if err != nil {
		t.Fatal(err)
	}
	if target.CreativeIntake == nil || target.CreativeIntake.Status != k12.CreativeWorkIntakePreparing {
		t.Fatalf("writing target=%+v", target)
	}
	if _, _, err := store.PromoteCreativeWorkIntake(ctx, "mingming",
		target.CreativeIntake.IntakeID, target.CreativeIntake.Version); err == nil {
		t.Fatal("writing intake promoted without canonical freeze")
	}
}

func TestParentSelectedArtworkWaitsForExplicitCommitAndReplaysReceipt(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	dispatch.TaskIntent = k12.ImageTaskIntentArtwork
	dispatch.IntentEvidence = []string{"parent_selected:artwork"}
	dispatch.IntentConfidence = 1
	dispatch.Status = k12.ImageTaskStatusRouted
	dispatch.RoutingProvenance = k12.ImageTaskRoutingParentSelected
	dispatch.ClassificationRouteSnapshot = k12.ImageTaskRouteSnapshot{}
	dispatch.ClassificationInvocationID = ""
	dispatch.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryNewWork, TaskIntent: k12.ImageTaskIntentArtwork,
	}
	stored, intake, created, err := store.PrepareParentSelectedCreativeDispatch(ctx, dispatch)
	if err != nil || !created || intake == nil {
		t.Fatalf("prepare manual dispatch: stored=%+v intake=%+v created=%v err=%v",
			stored, intake, created, err)
	}
	if intake.Status != k12.CreativeWorkIntakeReady ||
		intake.PromotionPolicy != k12.CreativeWorkPromotionExplicitCommit {
		t.Fatalf("manual art must be ready but unpromoted: %+v", intake)
	}
	var classificationRoute, routePolicy, operationRouteRequest, classificationInvocation string
	if err := db.QueryRow(`SELECT classification_route_snapshot_json,
        route_policy_snapshot_json, operation_route_request_json,
        classification_invocation_id FROM k12_image_task_dispatches
        WHERE dispatch_id=?`, stored.DispatchID).
		Scan(&classificationRoute, &routePolicy, &operationRouteRequest, &classificationInvocation); err != nil {
		t.Fatal(err)
	}
	var classificationCalls int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_invocations
        WHERE dispatch_id=? AND operation='classification'`, stored.DispatchID).
		Scan(&classificationCalls); err != nil {
		t.Fatal(err)
	}
	if classificationRoute != "" || routePolicy != "" ||
		operationRouteRequest == "" || classificationInvocation != "" ||
		classificationCalls != 0 {
		t.Fatalf("manual dispatch persisted fake model evidence: classification=%q policy=%q request=%q invocation=%q calls=%d",
			classificationRoute, routePolicy, operationRouteRequest,
			classificationInvocation, classificationCalls)
	}
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_creative_works`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before != 0 {
		t.Fatalf("manual intake auto-promoted %d formal works", before)
	}
	command := k12.CreativeWorkCommitCommand{
		CommandDigest: "sha256:commit-1", WorkTitle: "彩虹和小猫",
		TaskRequirement: "观察色彩和构图", Intent: "家长保存作品",
	}
	committed, err := store.CommitManualCreativeWorkIntake(
		ctx, "mingming", intake.IntakeID, intake.Version, command,
	)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.CommitManualCreativeWorkIntake(
		ctx, "mingming", intake.IntakeID, committed.Version, command,
	)
	if err != nil {
		t.Fatal(err)
	}
	if committed.PromotedWorkID == "" || committed.PromotedVersionID != "v1" ||
		replayed.PromotedWorkID != committed.PromotedWorkID ||
		replayed.CommitReceipt == nil ||
		replayed.CommitReceipt.CommandDigest != command.CommandDigest {
		t.Fatalf("explicit commit receipt/replay drift: first=%+v replay=%+v", committed, replayed)
	}
}

func TestParentSelectedRevisionValidatesLatestBaseAndAppendsOneVersion(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	asset := "asset://mingming/" + strings.Repeat("a", 64) + ".png"
	work, err := k12.NewCreativeWorkRecord("mingming", "session-1", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt,
		Versions: []k12.CreativeWorkVersion{{
			VersionID: "v1", SourceAssetID: asset,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, work); err != nil {
		t.Fatal(err)
	}

	dispatch := testImageTaskDispatch()
	dispatch.DispatchID = "dispatch-revision"
	dispatch.SourceRef = "revision-upload-1"
	dispatch.IdempotencyKey = "desktop:revision-upload-1:g1"
	dispatch.TaskIntent = k12.ImageTaskIntentArtwork
	dispatch.IntentEvidence = []string{"parent_selected:artwork"}
	dispatch.IntentConfidence = 1
	dispatch.Status = k12.ImageTaskStatusRouted
	dispatch.RoutingProvenance = k12.ImageTaskRoutingParentSelected
	dispatch.ClassificationRouteSnapshot = k12.ImageTaskRouteSnapshot{}
	dispatch.ClassificationInvocationID = ""
	dispatch.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryRevision, TaskIntent: k12.ImageTaskIntentArtwork,
		WorkID: work.RecordID, BaseVersionID: "v1",
	}
	_, intake, _, err := store.PrepareParentSelectedCreativeDispatch(ctx, dispatch)
	if err != nil {
		t.Fatal(err)
	}
	concurrent := dispatch
	concurrent.DispatchID = "dispatch-concurrent-revision"
	concurrent.SourceRef = "revision-upload-concurrent"
	concurrent.IdempotencyKey = "desktop:revision-upload-concurrent:g1"
	_, concurrentIntake, _, err := store.PrepareParentSelectedCreativeDispatch(
		ctx, concurrent,
	)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := store.CommitManualCreativeWorkIntake(
		ctx, "mingming", intake.IntakeID, intake.Version,
		k12.CreativeWorkCommitCommand{
			CommandDigest:   "sha256:revision-commit",
			ContentMarkdown: "家长对这版画作的修改说明",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if committed.PromotedWorkID != work.RecordID ||
		committed.PromotedVersionID != "v2" {
		t.Fatalf("revision result drift: %+v", committed)
	}
	if _, err := store.CommitManualCreativeWorkIntake(
		ctx, "mingming", concurrentIntake.IntakeID, concurrentIntake.Version,
		k12.CreativeWorkCommitCommand{CommandDigest: "sha256:concurrent-revision"},
	); !errors.Is(err, k12storage.ErrImageTaskVersionConflict) {
		t.Fatalf("stale base accepted at commit: %v", err)
	}
	record, err := store.Get(ctx, work.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParseCreativeWorkFields(record.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields.Versions) != 2 || fields.Versions[1].VersionID != "v2" ||
		fields.Versions[1].ContentMarkdown != "家长对这版画作的修改说明" {
		t.Fatalf("revision did not append exactly one version: %+v", fields.Versions)
	}

	stale := dispatch
	stale.DispatchID = "dispatch-stale-revision"
	stale.SourceRef = "revision-upload-stale"
	stale.IdempotencyKey = "desktop:revision-upload-stale:g1"
	stale.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryRevision, TaskIntent: k12.ImageTaskIntentArtwork,
		WorkID: work.RecordID, BaseVersionID: "v1",
	}
	if _, _, _, err := store.PrepareParentSelectedCreativeDispatch(
		ctx, stale,
	); !errors.Is(err, k12storage.ErrImageTaskVersionConflict) {
		t.Fatalf("stale revision base accepted: %v", err)
	}

	typeMismatch := dispatch
	typeMismatch.DispatchID = "dispatch-type-mismatch"
	typeMismatch.SourceRef = "revision-type-mismatch"
	typeMismatch.IdempotencyKey = "desktop:revision-type-mismatch:g1"
	typeMismatch.TaskIntent = k12.ImageTaskIntentWriting
	typeMismatch.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryRevision, TaskIntent: k12.ImageTaskIntentWriting,
		WorkID: work.RecordID, BaseVersionID: "v2",
	}
	if _, _, _, err := store.PrepareParentSelectedCreativeDispatch(
		ctx, typeMismatch,
	); !errors.Is(err, k12storage.ErrImageTaskVersionConflict) {
		t.Fatalf("cross-type revision accepted: %v", err)
	}

	foreign, err := k12.NewCreativeWorkRecord("lele", "session-2", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeArt,
		Versions: []k12.CreativeWorkVersion{{
			VersionID:     "v1",
			SourceAssetID: "asset://lele/" + strings.Repeat("b", 64) + ".png",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	crossOwner := dispatch
	crossOwner.DispatchID = "dispatch-cross-owner"
	crossOwner.SourceRef = "revision-cross-owner"
	crossOwner.IdempotencyKey = "desktop:revision-cross-owner:g1"
	crossOwner.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryRevision, TaskIntent: k12.ImageTaskIntentArtwork,
		WorkID: foreign.RecordID, BaseVersionID: "v1",
	}
	if _, _, _, err := store.PrepareParentSelectedCreativeDispatch(
		ctx, crossOwner,
	); !errors.Is(err, k12storage.ErrImageTaskConflict) {
		t.Fatalf("cross-owner revision accepted: %v", err)
	}

	current, err := store.Get(ctx, work.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	pendingArchive := dispatch
	pendingArchive.DispatchID = "dispatch-pending-archive"
	pendingArchive.SourceRef = "revision-pending-archive"
	pendingArchive.IdempotencyKey = "desktop:revision-pending-archive:g1"
	pendingArchive.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryRevision, TaskIntent: k12.ImageTaskIntentArtwork,
		WorkID: work.RecordID, BaseVersionID: "v2",
	}
	_, pendingArchiveIntake, _, err := store.PrepareParentSelectedCreativeDispatch(
		ctx, pendingArchive,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(
		ctx, work.RecordID, k12.WorkStatusArchived, nil, current.Version,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitManualCreativeWorkIntake(
		ctx, "mingming", pendingArchiveIntake.IntakeID, pendingArchiveIntake.Version,
		k12.CreativeWorkCommitCommand{CommandDigest: "sha256:archived-after-intake"},
	); !errors.Is(err, k12storage.ErrImageTaskInvalidState) {
		t.Fatalf("revision commit ignored archived target: %v", err)
	}
	archived := dispatch
	archived.DispatchID = "dispatch-archived"
	archived.SourceRef = "revision-archived"
	archived.IdempotencyKey = "desktop:revision-archived:g1"
	archived.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryRevision, TaskIntent: k12.ImageTaskIntentArtwork,
		WorkID: work.RecordID, BaseVersionID: "v2",
	}
	if _, _, _, err := store.PrepareParentSelectedCreativeDispatch(
		ctx, archived,
	); !errors.Is(err, k12storage.ErrImageTaskInvalidState) {
		t.Fatalf("archived revision accepted: %v", err)
	}
}
