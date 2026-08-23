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

func TestWorkFeedbackInvocationSentClaimHasSingleWinner(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	rec, err := k12.NewCreativeWorkRecord(
		"mingming",
		"session-claim",
		k12.CreativeWorkFields{WorkType: k12.WorkTypeWriting},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.CreateCreativeWorkWithInitialGeneration(
		ctx,
		rec,
		"auto:claim-test",
		"sha256:claim-test",
		k12.CreativeWorkSourceSnapshot{
			WorkType:        k12.WorkTypeWriting,
			ContentMarkdown: "桂花落在青石板上。",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	invocation := k12.ImageTaskInvocation{
		InvocationID: "invocation-feedback-claim", AgentName: "mingming",
		WorkRecordID: rec.RecordID, Operation: k12.ImageTaskOperationWorkFeedback,
		OperationKey:  "work:" + rec.RecordID + ":version:v1:feedback",
		RequestDigest: "sha256:feedback-request", RouteSnapshot: testImageRoute(),
		Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
		CreatedAt: 100, UpdatedAt: 100,
	}
	prepared, created, err := store.PrepareImageTaskInvocation(ctx, invocation)
	if err != nil || !created {
		t.Fatalf("prepare feedback invocation: created=%v err=%v", created, err)
	}
	if _, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", prepared.InvocationID, "provider-request-1", 100,
	); err != nil || !claimed {
		t.Fatalf("first claim must win: claimed=%v err=%v", claimed, err)
	}
	if _, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", prepared.InvocationID, "provider-request-1", 100,
	); err != nil || claimed {
		t.Fatalf(
			"second work-feedback sender claim=%v err=%v; want clean CAS loss",
			claimed,
			err,
		)
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

func TestImageTaskPrepareIdempotencyLoserBindsStoredDispatchOwnerScope(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	dispatch.OwnerScope = "owner-a"
	invocation := k12.ImageTaskInvocation{
		InvocationID: "invocation-classify-1", AgentName: "mingming",
		DispatchID: "dispatch-1", Operation: k12.ImageTaskOperationClassification,
		OperationKey:  "dispatch:dispatch-1:classification",
		RequestDigest: "sha256:classify-request", RouteSnapshot: testImageRoute(),
		Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
		CreatedAt: 100, UpdatedAt: 100,
	}
	winner, created, err := store.PrepareImageTaskDispatch(ctx, dispatch, invocation)
	if err != nil || !created {
		t.Fatalf("prepare winner=%+v created=%v err=%v", winner, created, err)
	}

	loser := dispatch
	loser.DispatchID = "dispatch-concurrent-loser"
	loser.ClassificationInvocationID = "invocation-concurrent-loser"
	loserInvocation := invocation
	loserInvocation.DispatchID = loser.DispatchID
	loserInvocation.InvocationID = loser.ClassificationInvocationID
	loserInvocation.OperationKey = "dispatch:" + loser.DispatchID + ":classification"
	replay, created, err := store.PrepareImageTaskDispatch(ctx, loser, loserInvocation)
	if err != nil || created || replay.DispatchID != winner.DispatchID {
		t.Fatalf("idempotency loser replay=%+v created=%v err=%v", replay, created, err)
	}
	owner, err := store.GetImageTaskOwnerScope(ctx, "mingming", winner.DispatchID)
	if err != nil || owner != "owner-a" {
		t.Fatalf("winner owner scope=%q err=%v", owner, err)
	}
	if _, err := store.GetImageTaskOwnerScope(ctx, "mingming", loser.DispatchID); !errors.Is(err, k12storage.ErrImageTaskNotFound) {
		t.Fatalf("loser dispatch unexpectedly received an owner binding: %v", err)
	}
	otherOwner := loser
	otherOwner.DispatchID = "dispatch-other-owner-loser"
	otherOwner.OwnerScope = "owner-b"
	otherOwner.ClassificationInvocationID = "invocation-other-owner-loser"
	otherInvocation := loserInvocation
	otherInvocation.DispatchID = otherOwner.DispatchID
	otherInvocation.InvocationID = otherOwner.ClassificationInvocationID
	otherInvocation.OperationKey = "dispatch:" + otherOwner.DispatchID + ":classification"
	if _, _, err := store.PrepareImageTaskDispatch(
		ctx, otherOwner, otherInvocation,
	); !errors.Is(err, k12storage.ErrImageTaskConflict) {
		t.Fatalf("cross-owner idempotency replay err=%v, want conflict", err)
	}
}

func TestParentSelectedPrepareIdempotencyLoserBindsStoredDispatchOwnerScope(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	dispatch := testImageTaskDispatch()
	dispatch.OwnerScope = "owner-a"
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
	winner, winnerIntake, created, err := store.PrepareParentSelectedCreativeDispatch(ctx, dispatch)
	if err != nil || !created || winnerIntake == nil {
		t.Fatalf("prepare manual winner=%+v intake=%+v created=%v err=%v", winner, winnerIntake, created, err)
	}
	loser := dispatch
	loser.DispatchID = "dispatch-manual-concurrent-loser"
	replay, replayIntake, created, err := store.PrepareParentSelectedCreativeDispatch(ctx, loser)
	if err != nil || created || replay.DispatchID != winner.DispatchID ||
		replayIntake == nil || replayIntake.IntakeID != winnerIntake.IntakeID {
		t.Fatalf("manual idempotency loser replay=%+v intake=%+v created=%v err=%v", replay, replayIntake, created, err)
	}
	owner, err := store.GetImageTaskOwnerScope(ctx, "mingming", winner.DispatchID)
	if err != nil || owner != "owner-a" {
		t.Fatalf("manual winner owner scope=%q err=%v", owner, err)
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
	if works != 1 || versions != 0 {
		t.Fatalf("atomic promotion works=%d versions=%d", works, versions)
	}
	intake, err := store.GetCreativeWorkIntake(ctx, "mingming", target.CreativeIntake.IntakeID)
	if err != nil || intake.Status != k12.CreativeWorkIntakePromoted ||
		intake.PromotedWorkID != workID || intake.PromotedGenerationID == "" ||
		intake.PromotedVersionID != "" {
		t.Fatalf("promoted intake=%+v err=%v", intake, err)
	}
	t.Run("BUG-20260724-012 automatic promotion commits current work and initial generation together", func(t *testing.T) {
		record, err := store.Get(ctx, workID)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := k12.ParseCreativeWorkFields(record.Fields)
		if err != nil {
			t.Fatal(err)
		}
		if len(fields.Versions) != 0 {
			t.Fatalf("BUG-20260724-012 promoted work versions=%+v, want no legacy versions", fields.Versions)
		}
		var generationCount int
		if err := db.QueryRow(`SELECT COUNT(*) FROM k12_work_feedback_generations
			WHERE work_id=?`, workID).Scan(&generationCount); err != nil {
			t.Fatal(err)
		}
		if generationCount != 1 {
			t.Fatalf("BUG-20260724-012 promotion returned with generations=%d, want 1 queued atomically", generationCount)
		}
		var generationID, status, initialID, feedbackState string
		var generationNo int
		if err := db.QueryRow(`SELECT generation_id, generation_no, status
			FROM k12_work_feedback_generations WHERE work_id=?`, workID).
			Scan(&generationID, &generationNo, &status); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT initial_feedback_generation_id, feedback_state
			FROM k12_creative_works WHERE record_id=?`, workID).
			Scan(&initialID, &feedbackState); err != nil {
			t.Fatal(err)
		}
		if generationID == "" || generationNo != 1 || status != "queued" ||
			initialID != generationID || intake.PromotedGenerationID != generationID ||
			feedbackState != "queued" {
			t.Fatalf("BUG-20260724-012 generation=%q no=%d status=%q initial=%q intake=%q work_state=%q",
				generationID, generationNo, status, initialID,
				intake.PromotedGenerationID, feedbackState)
		}
	})
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
	if committed.PromotedWorkID == "" || committed.PromotedGenerationID == "" ||
		committed.PromotedVersionID != "" ||
		replayed.PromotedWorkID != committed.PromotedWorkID ||
		replayed.PromotedGenerationID != committed.PromotedGenerationID ||
		replayed.CommitReceipt == nil ||
		replayed.CommitReceipt.CommandDigest != command.CommandDigest ||
		replayed.CommitReceipt.WorkID != committed.PromotedWorkID ||
		replayed.CommitReceipt.GenerationID != committed.PromotedGenerationID ||
		replayed.CommitReceipt.VersionID != "" {
		t.Fatalf("explicit commit receipt/replay drift: first=%+v replay=%+v", committed, replayed)
	}
	t.Run("BUG-20260724-012 manual commit returns current work and initial generation atomically", func(t *testing.T) {
		record, err := store.Get(ctx, committed.PromotedWorkID)
		if err != nil {
			t.Fatal(err)
		}
		fields, err := k12.ParseCreativeWorkFields(record.Fields)
		if err != nil {
			t.Fatal(err)
		}
		if len(fields.Versions) != 0 {
			t.Fatalf("BUG-20260724-012 committed work versions=%+v, want no legacy versions", fields.Versions)
		}
		var works, generations int
		if err := db.QueryRow(`SELECT COUNT(*) FROM k12_creative_works
			WHERE source_intake_id=?`, intake.IntakeID).Scan(&works); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM k12_work_feedback_generations
			WHERE work_id=?`, committed.PromotedWorkID).Scan(&generations); err != nil {
			t.Fatal(err)
		}
		if works != 1 || generations != 1 {
			t.Fatalf("BUG-20260724-012 replay left works=%d generations=%d, want 1/1", works, generations)
		}
		var generationID, status, initialID, feedbackState string
		var generationNo int
		if err := db.QueryRow(`SELECT generation_id, generation_no, status
			FROM k12_work_feedback_generations WHERE work_id=?`, committed.PromotedWorkID).
			Scan(&generationID, &generationNo, &status); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT initial_feedback_generation_id, feedback_state
			FROM k12_creative_works WHERE record_id=?`, committed.PromotedWorkID).
			Scan(&initialID, &feedbackState); err != nil {
			t.Fatal(err)
		}
		if generationID == "" || generationNo != 1 || status != "queued" ||
			initialID != generationID || committed.PromotedGenerationID != generationID ||
			feedbackState != "queued" {
			t.Fatalf("BUG-20260724-012 generation=%q no=%d status=%q initial=%q intake=%q work_state=%q",
				generationID, generationNo, status, initialID,
				committed.PromotedGenerationID, feedbackState)
		}
	})
}

func TestParentSelectedRevisionIsRejectedBeforeCurrentWrites(t *testing.T) {
	store, db := setup(t)
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
	legacy, err := store.Get(ctx, work.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	legacyFields, err := k12.ParseCreativeWorkFields(legacy.Fields)
	if err != nil || len(legacyFields.Versions) != 1 ||
		legacyFields.Versions[0].VersionID != "v1" {
		t.Fatalf("historical version is not readable: fields=%+v err=%v", legacyFields, err)
	}

	dispatch := testImageTaskDispatch()
	dispatch.DispatchID = "dispatch-revision"
	dispatch.OwnerScope = "owner-mingming"
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
	stored, intake, created, err := store.PrepareParentSelectedCreativeDispatch(ctx, dispatch)
	if err == nil || created || stored.DispatchID != "" || intake != nil {
		t.Fatalf("revision write was not rejected: stored=%+v intake=%+v created=%v err=%v",
			stored, intake, created, err)
	}
	for table, want := range map[string]int{
		"k12_image_task_dispatches":   0,
		"k12_creative_work_intakes":   0,
		"k12_image_task_owner_scopes": 0,
	} {
		var got int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE dispatch_id=?`,
			dispatch.DispatchID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("revision rejection left %s rows=%d, want %d", table, got, want)
		}
	}
	after, err := store.Get(ctx, work.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	afterFields, err := k12.ParseCreativeWorkFields(after.Fields)
	if err != nil || len(afterFields.Versions) != 1 ||
		afterFields.Versions[0].VersionID != "v1" {
		t.Fatalf("revision rejection rewrote historical work: fields=%+v err=%v", afterFields, err)
	}
}
