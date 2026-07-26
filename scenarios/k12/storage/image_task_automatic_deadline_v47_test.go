package k12storage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestImageTaskRestartAutomaticWindowAcceptsExpiredRoutedHomeworkAfterRetryPreflight(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	dispatch, _ := prepareDeadlineDispatch(t, store, "dispatch-grading-retry", 1000)
	routed, target, err := store.CommitImageTaskRouting(
		ctx,
		dispatch.AgentName,
		dispatch.DispatchID,
		dispatch.Version,
		k12storage.ImageTaskRoutingDecision{
			Intent:                 k12.ImageTaskIntentCompletedHomework,
			Evidence:               []string{"worksheet"},
			Confidence:             0.99,
			InvocationResultDigest: "sha256:grading-retry-route",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if target.HomeworkSubmission == nil {
		t.Fatal("homework target missing")
	}
	restarted, err := store.RestartImageTaskAutomaticWindow(
		ctx,
		routed.AgentName,
		routed.DispatchID,
		routed.Version,
		routed.AutomaticDeadlineAt+1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Status != k12.ImageTaskStatusRouted ||
		restarted.AutomaticStartedAt != routed.AutomaticDeadlineAt+1 ||
		restarted.AutomaticDeadlineAt != routed.AutomaticDeadlineAt+301 ||
		restarted.AutomaticRemainingSeconds != k12.ImageTaskAutomaticBudgetSeconds {
		t.Fatalf("wrong restarted nested-grading window: %+v", restarted)
	}
}

func prepareDeadlineDispatch(
	t *testing.T,
	store *k12storage.Store,
	dispatchID string,
	createdAt int64,
) (k12.ImageTaskDispatch, k12.ImageTaskInvocation) {
	t.Helper()
	dispatch := testImageTaskDispatch()
	dispatch.DispatchID = dispatchID
	dispatch.SourceRef = "message-" + dispatchID
	dispatch.IdempotencyKey = "desktop:" + dispatch.SourceRef + ":g1"
	dispatch.ClassificationInvocationID = "invocation-" + dispatchID
	dispatch.CreatedAt = createdAt
	dispatch.UpdatedAt = createdAt
	invocation := k12.ImageTaskInvocation{
		InvocationID: dispatch.ClassificationInvocationID,
		AgentName:    dispatch.AgentName,
		DispatchID:   dispatch.DispatchID,
		Operation:    k12.ImageTaskOperationClassification,
		OperationKey: "dispatch:" + dispatch.DispatchID +
			":classification",
		RequestDigest: dispatch.RequestDigest,
		RouteSnapshot: dispatch.ClassificationRouteSnapshot,
		Status:        k12.ImageTaskInvocationPrepared,
		Attempt:       1,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	}
	stored, created, err := store.PrepareImageTaskDispatch(
		context.Background(), dispatch, invocation,
	)
	if err != nil || !created {
		t.Fatalf("prepare deadline dispatch: created=%v err=%v", created, err)
	}
	storedInvocation, err := store.GetImageTaskInvocation(
		context.Background(), dispatch.AgentName, invocation.InvocationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return stored, storedInvocation
}

func TestImageTaskV47NormalizesAutomaticWindowAndInvocationDeadline(t *testing.T) {
	store, db := setup(t)
	dispatch, invocation := prepareDeadlineDispatch(t, store, "deadline-normalize", 100)

	if dispatch.AutomaticBudgetSeconds != 300 ||
		dispatch.AutomaticStartedAt != 100 ||
		dispatch.AutomaticDeadlineAt != 400 ||
		dispatch.AutomaticRemainingSeconds != 300 {
		t.Fatalf("dispatch automatic window not normalized: %+v", dispatch)
	}
	if invocation.DeadlineAt != 400 {
		t.Fatalf("classification deadline=%d, want 400", invocation.DeadlineAt)
	}
	var budget int
	var startedAt, deadlineAt int64
	if err := db.QueryRow(`SELECT automatic_budget_seconds,automatic_started_at,
		automatic_deadline_at FROM k12_image_task_dispatches WHERE dispatch_id=?`,
		dispatch.DispatchID).Scan(&budget, &startedAt, &deadlineAt); err != nil {
		t.Fatal(err)
	}
	if budget != 300 || startedAt != 100 || deadlineAt != 400 {
		t.Fatalf("persisted automatic window=%d/%d/%d", budget, startedAt, deadlineAt)
	}
}

func TestClaimImageTaskInvocationSendHasOneWinnerAndHonorsDeadline(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	_, invocation := prepareDeadlineDispatch(t, store, "claim-before-deadline", 100)

	claimedInvocation, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", invocation.InvocationID, "provider-request-1", 399,
	)
	if err != nil || !claimed || claimedInvocation.Status != k12.ImageTaskInvocationSent ||
		claimedInvocation.StartedAt != 399 {
		t.Fatalf("first claim: claimed=%v invocation=%+v err=%v",
			claimed, claimedInvocation, err)
	}
	replay, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", invocation.InvocationID, "provider-request-2", 399,
	)
	if err != nil || claimed || replay.ProviderRequestKey != "provider-request-1" {
		t.Fatalf("loser obtained ownership: claimed=%v invocation=%+v err=%v",
			claimed, replay, err)
	}

	_, expired := prepareDeadlineDispatch(t, store, "claim-at-deadline", 100)
	current, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", expired.InvocationID, "provider-request-expired", 400,
	)
	if err != nil || claimed || current.Status != k12.ImageTaskInvocationPrepared ||
		current.ProviderRequestKey != "" {
		t.Fatalf("expired claim escaped: claimed=%v invocation=%+v err=%v",
			claimed, current, err)
	}
}

func TestExpireImageTaskInvocationIsTransactionalIdempotentAndSuccessWins(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()

	preparedDispatch, prepared := prepareDeadlineDispatch(t, store, "expire-prepared", 100)
	gotDispatch, gotInvocation, changed, err := store.ExpireImageTaskInvocation(
		ctx, "mingming", preparedDispatch.DispatchID, prepared.InvocationID, 400,
	)
	if err != nil || !changed ||
		gotDispatch.Status != k12.ImageTaskStatusFailed ||
		!gotDispatch.RetrySafe ||
		gotDispatch.FailureKind != "interactive_deadline_exceeded" ||
		gotInvocation.Status != k12.ImageTaskInvocationFailed ||
		!gotInvocation.RetrySafe ||
		gotInvocation.ErrorKind != "interactive_deadline_exceeded" {
		t.Fatalf("prepared expiry: changed=%v dispatch=%+v invocation=%+v err=%v",
			changed, gotDispatch, gotInvocation, err)
	}
	replayDispatch, replayInvocation, changed, err := store.ExpireImageTaskInvocation(
		ctx, "mingming", preparedDispatch.DispatchID, prepared.InvocationID, 401,
	)
	if err != nil || changed ||
		replayDispatch.Version != gotDispatch.Version ||
		replayInvocation.Status != gotInvocation.Status {
		t.Fatalf("expiry replay was not idempotent: changed=%v dispatch=%+v invocation=%+v err=%v",
			changed, replayDispatch, replayInvocation, err)
	}

	sentDispatch, sent := prepareDeadlineDispatch(t, store, "expire-sent", 100)
	if _, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", sent.InvocationID, "provider-request-sent", 200,
	); err != nil || !claimed {
		t.Fatalf("claim sent invocation: claimed=%v err=%v", claimed, err)
	}
	gotDispatch, gotInvocation, changed, err = store.ExpireImageTaskInvocation(
		ctx, "mingming", sentDispatch.DispatchID, sent.InvocationID, 400,
	)
	if err != nil || !changed || gotDispatch.RetrySafe ||
		gotDispatch.FailureKind != "interactive_deadline_outcome_unknown" ||
		gotInvocation.Status != k12.ImageTaskInvocationOutcomeUnknown ||
		gotInvocation.RetrySafe ||
		gotInvocation.ErrorKind != "interactive_deadline_outcome_unknown" {
		t.Fatalf("sent expiry: changed=%v dispatch=%+v invocation=%+v err=%v",
			changed, gotDispatch, gotInvocation, err)
	}

	successDispatch, success := prepareDeadlineDispatch(t, store, "expire-success", 100)
	if _, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", success.InvocationID, "provider-request-success", 200,
	); err != nil || !claimed {
		t.Fatalf("claim success invocation: claimed=%v err=%v", claimed, err)
	}
	routed, _, err := store.CommitImageTaskRouting(
		ctx, "mingming", successDispatch.DispatchID, successDispatch.Version,
		k12storage.ImageTaskRoutingDecision{
			Intent:                 k12.ImageTaskIntentArtwork,
			Evidence:               []string{"artwork evidence"},
			Confidence:             0.99,
			InvocationResultDigest: "sha256:success",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	gotDispatch, gotInvocation, changed, err = store.ExpireImageTaskInvocation(
		ctx, "mingming", routed.DispatchID, success.InvocationID, 500,
	)
	if err != nil || changed ||
		gotDispatch.Status != k12.ImageTaskStatusRouted ||
		gotInvocation.Status != k12.ImageTaskInvocationSucceeded {
		t.Fatalf("terminal success lost expiry race: changed=%v dispatch=%+v invocation=%+v err=%v",
			changed, gotDispatch, gotInvocation, err)
	}
}

func TestImageTaskHumanConfirmationPausesAndResumesAutomaticWindow(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	now := time.Now().Unix()
	dispatch, _ := prepareDeadlineDispatch(t, store, "pause-routing", now)

	awaiting, _, err := store.CommitImageTaskRouting(
		ctx, "mingming", dispatch.DispatchID, dispatch.Version,
		k12storage.ImageTaskRoutingDecision{
			Intent:     k12.ImageTaskIntentUnknown,
			Evidence:   []string{"mixed page evidence"},
			Confidence: 0.51,
			ConfirmationCandidates: []k12.ImageTaskIntent{
				k12.ImageTaskIntentWriting,
				k12.ImageTaskIntentArtwork,
			},
			InvocationResultDigest: "sha256:pause-routing",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if awaiting.AutomaticDeadlineAt != 0 ||
		awaiting.AutomaticRemainingSeconds < 0 ||
		awaiting.AutomaticRemainingSeconds > 300 {
		t.Fatalf("routing confirmation did not pause window: %+v", awaiting)
	}
	remaining := awaiting.AutomaticRemainingSeconds
	confirmed, _, err := store.ConfirmImageTaskIntent(
		ctx, "mingming", dispatch.DispatchID, awaiting.Version,
		k12.ImageTaskIntentWriting,
	)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.AutomaticDeadlineAt == 0 ||
		confirmed.AutomaticRemainingSeconds != remaining {
		t.Fatalf("routing confirmation did not resume window: %+v", confirmed)
	}

	ocrInvocation := k12.ImageTaskInvocation{
		InvocationID: "invocation-pause-ocr", AgentName: "mingming",
		IntakeID: confirmed.TargetObjectID, Operation: k12.ImageTaskOperationWritingOCR,
		OperationKey:  "intake:" + confirmed.TargetObjectID + ":ocr",
		RequestDigest: "sha256:pause-ocr", RouteSnapshot: testImageRoute(),
		Status: k12.ImageTaskInvocationPrepared, Attempt: 1,
		CreatedAt: time.Now().Unix(), UpdatedAt: time.Now().Unix(),
	}
	ocrInvocation, created, err := store.PrepareImageTaskInvocation(ctx, ocrInvocation)
	if err != nil || !created || ocrInvocation.DeadlineAt != confirmed.AutomaticDeadlineAt {
		t.Fatalf("prepare OCR invocation: created=%v invocation=%+v err=%v",
			created, ocrInvocation, err)
	}
	if _, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", ocrInvocation.InvocationID, "provider-request-ocr",
		time.Now().Unix(),
	); err != nil || !claimed {
		t.Fatalf("claim OCR: claimed=%v err=%v", claimed, err)
	}
	content := "春风吹过小河。"
	sum := sha256.Sum256([]byte(content))
	evidence := k12.CreativeWorkIntakeOCREvidence{
		Raw: content, CanonicalContent: content, CanonicalVersion: 1,
		CanonicalDigest: "sha256:" + hex.EncodeToString(sum[:]),
		Confidence:      0.8,
		RiskSegments: []k12.CreativeWorkIntakeOCRRisk{{
			SegmentID: "line-1", RawText: content, Reasons: []string{"字迹不清"},
		}},
	}
	intake, err := store.GetCreativeWorkIntake(
		ctx, "mingming", confirmed.TargetObjectID,
	)
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.HoldCreativeWorkIntakeOCRConfirmation(
		ctx, "mingming", intake.IntakeID, intake.Version,
		ocrInvocation.InvocationID, evidence,
	)
	if err != nil {
		t.Fatal(err)
	}
	var heldDeadline int64
	var heldRemaining int
	if err := db.QueryRow(`SELECT automatic_deadline_at,automatic_remaining_seconds
		FROM k12_image_task_dispatches WHERE dispatch_id=?`, dispatch.DispatchID).
		Scan(&heldDeadline, &heldRemaining); err != nil {
		t.Fatal(err)
	}
	if heldDeadline != 0 {
		t.Fatalf("OCR confirmation did not pause dispatch deadline: %d", heldDeadline)
	}
	_, err = store.ConfirmCreativeWorkIntakeOCR(
		ctx, "mingming", held.IntakeID, held.Version, 1, content,
		[]k12.CreativeWorkIntakeOCRCorrection{{
			SegmentID: "line-1", CanonicalText: content,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var resumedDeadline int64
	var resumedRemaining int
	if err := db.QueryRow(`SELECT automatic_deadline_at,automatic_remaining_seconds
		FROM k12_image_task_dispatches WHERE dispatch_id=?`, dispatch.DispatchID).
		Scan(&resumedDeadline, &resumedRemaining); err != nil {
		t.Fatal(err)
	}
	if resumedDeadline == 0 || resumedRemaining != heldRemaining {
		t.Fatalf("OCR confirmation did not resume same budget: deadline=%d remaining=%d/%d",
			resumedDeadline, resumedRemaining, heldRemaining)
	}
}

func TestImageTaskRetryAndExplicitRestartCreateFreshAutomaticWindow(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	dispatch, invocation := prepareDeadlineDispatch(t, store, "retry-window", 100)
	failed, _, changed, err := store.ExpireImageTaskInvocation(
		ctx, "mingming", dispatch.DispatchID, invocation.InvocationID, 400,
	)
	if err != nil || !changed {
		t.Fatalf("expire before retry: changed=%v err=%v", changed, err)
	}
	before := time.Now().Unix()
	retried, retryInvocation, err := store.PrepareImageTaskRetry(
		ctx, "mingming", dispatch.DispatchID, failed.Version, "invocation-retry-window-2",
	)
	if err != nil {
		t.Fatal(err)
	}
	if retried.AutomaticBudgetSeconds != 300 ||
		retried.AutomaticRemainingSeconds != 300 ||
		retried.AutomaticStartedAt < before ||
		retried.AutomaticDeadlineAt != retried.AutomaticStartedAt+300 ||
		retryInvocation.DeadlineAt != retried.AutomaticDeadlineAt {
		t.Fatalf("retry did not receive a fresh window: dispatch=%+v invocation=%+v",
			retried, retryInvocation)
	}

	writingBase, writingInvocation := prepareDeadlineDispatch(
		t, store, "writing-retry-window", 100,
	)
	if _, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", writingInvocation.InvocationID,
		"provider-request-writing-classification", 200,
	); err != nil || !claimed {
		t.Fatalf("claim writing classification: claimed=%v err=%v", claimed, err)
	}
	writingRouted, target, err := store.CommitImageTaskRouting(
		ctx, "mingming", writingBase.DispatchID, writingBase.Version,
		k12storage.ImageTaskRoutingDecision{
			Intent:                 k12.ImageTaskIntentWriting,
			Evidence:               []string{"writing evidence"},
			Confidence:             0.99,
			InvocationResultDigest: "sha256:writing-retry-base",
		},
	)
	if err != nil || target.CreativeIntake == nil {
		t.Fatalf("route writing retry base: target=%+v err=%v", target, err)
	}
	ocr := k12.ImageTaskInvocation{
		InvocationID: "invocation-writing-retry-ocr", AgentName: "mingming",
		IntakeID:  target.CreativeIntake.IntakeID,
		Operation: k12.ImageTaskOperationWritingOCR,
		OperationKey: "intake:" + target.CreativeIntake.IntakeID +
			":writing-ocr",
		RequestDigest: "sha256:writing-retry-ocr",
		RouteSnapshot: testImageRoute(), Status: k12.ImageTaskInvocationPrepared,
		Attempt: 1, CreatedAt: 201, UpdatedAt: 201,
	}
	ocr, created, err := store.PrepareImageTaskInvocation(ctx, ocr)
	if err != nil || !created {
		t.Fatalf("prepare writing OCR: created=%v err=%v", created, err)
	}
	writingFailed, _, changed, err := store.ExpireImageTaskInvocation(
		ctx, "mingming", writingRouted.DispatchID, ocr.InvocationID, 400,
	)
	if err != nil || !changed {
		t.Fatalf("expire writing OCR: changed=%v err=%v", changed, err)
	}
	writingRetried, writingRetryInvocation, err := store.PrepareImageTaskRetry(
		ctx, "mingming", writingFailed.DispatchID, writingFailed.Version,
		"invocation-writing-retry-ocr-2",
	)
	if err != nil {
		t.Fatal(err)
	}
	if writingRetried.Status != k12.ImageTaskStatusRouted ||
		writingRetryInvocation.Operation != k12.ImageTaskOperationWritingOCR ||
		writingRetryInvocation.Attempt != 2 ||
		writingRetryInvocation.DeadlineAt != writingRetried.AutomaticDeadlineAt {
		t.Fatalf("writing retry drift: dispatch=%+v invocation=%+v",
			writingRetried, writingRetryInvocation)
	}

	restartBase, restartInvocation := prepareDeadlineDispatch(
		t, store, "explicit-restart-window", 100,
	)
	if _, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", restartInvocation.InvocationID,
		"provider-request-restart-base", 200,
	); err != nil || !claimed {
		t.Fatalf("claim restart base: claimed=%v err=%v", claimed, err)
	}
	routed, _, err := store.CommitImageTaskRouting(
		ctx, "mingming", restartBase.DispatchID, restartBase.Version,
		k12storage.ImageTaskRoutingDecision{
			Intent:                 k12.ImageTaskIntentArtwork,
			Evidence:               []string{"artwork evidence"},
			Confidence:             0.99,
			InvocationResultDigest: "sha256:restart-base",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	expiredGap, _, changed, err := store.ExpireImageTaskInvocation(
		ctx, "mingming", routed.DispatchID, "", 500,
	)
	if err != nil || !changed || !expiredGap.RetrySafe {
		t.Fatalf("expire provider-free gap: changed=%v dispatch=%+v err=%v",
			changed, expiredGap, err)
	}
	restarted, err := store.RestartImageTaskAutomaticWindow(
		ctx, "mingming", expiredGap.DispatchID, expiredGap.Version, 900,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.AutomaticBudgetSeconds != 300 ||
		restarted.AutomaticStartedAt != 900 ||
		restarted.AutomaticDeadlineAt != 1200 ||
		restarted.AutomaticRemainingSeconds != 300 {
		t.Fatalf("explicit restart window=%+v", restarted)
	}
}

func TestWorkFeedbackInvocationInheritsDispatchDeadlineAndExpiresByPromotedOwner(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	dispatch, classification := prepareDeadlineDispatch(
		t, store, "feedback-deadline-owner", 100,
	)
	if _, claimed, err := store.ClaimImageTaskInvocationSend(
		ctx, "mingming", classification.InvocationID,
		"provider-request-feedback-classification", 200,
	); err != nil || !claimed {
		t.Fatalf("claim feedback classification: claimed=%v err=%v", claimed, err)
	}
	routed, target, err := store.CommitImageTaskRouting(
		ctx, "mingming", dispatch.DispatchID, dispatch.Version,
		k12storage.ImageTaskRoutingDecision{
			Intent:                 k12.ImageTaskIntentArtwork,
			Evidence:               []string{"artwork evidence"},
			Confidence:             0.99,
			InvocationResultDigest: "sha256:feedback-owner",
		},
	)
	if err != nil || target.CreativeIntake == nil {
		t.Fatalf("route feedback owner: target=%+v err=%v", target, err)
	}
	work, err := k12.NewCreativeWorkRecord(
		"mingming", "session-feedback-owner",
		k12.CreativeWorkFields{WorkType: k12.WorkTypeWriting},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateCreativeWorkWithInitialGeneration(
		ctx, work, "auto:feedback-owner", "sha256:feedback-owner-work",
		k12.CreativeWorkSourceSnapshot{
			WorkType: k12.WorkTypeWriting, ContentMarkdown: "画面证据",
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE k12_creative_work_intakes
		SET status='promoted',promoted_work_id=?,updated_at=?
		WHERE intake_id=?`,
		work.RecordID, 201, target.CreativeIntake.IntakeID); err != nil {
		t.Fatal(err)
	}
	feedback := k12.ImageTaskInvocation{
		InvocationID: "invocation-feedback-deadline-owner-work",
		AgentName:    "mingming", WorkRecordID: work.RecordID,
		Operation:     k12.ImageTaskOperationWorkFeedback,
		OperationKey:  "work:" + work.RecordID + ":version:v1:feedback",
		RequestDigest: "sha256:feedback-deadline-owner",
		RouteSnapshot: testImageRoute(), Status: k12.ImageTaskInvocationPrepared,
		Attempt: 1, CreatedAt: 202, UpdatedAt: 202,
	}
	feedback, created, err := store.PrepareImageTaskInvocation(ctx, feedback)
	if err != nil || !created || feedback.DeadlineAt != routed.AutomaticDeadlineAt {
		t.Fatalf("prepare feedback deadline: created=%v invocation=%+v err=%v",
			created, feedback, err)
	}
	failedDispatch, failedInvocation, changed, err := store.ExpireImageTaskInvocation(
		ctx, "mingming", routed.DispatchID, feedback.InvocationID, 400,
	)
	if err != nil || !changed || !failedDispatch.RetrySafe ||
		failedInvocation.Status != k12.ImageTaskInvocationFailed ||
		!failedInvocation.RetrySafe {
		t.Fatalf("expire prepared feedback: changed=%v dispatch=%+v invocation=%+v err=%v",
			changed, failedDispatch, failedInvocation, err)
	}
}
