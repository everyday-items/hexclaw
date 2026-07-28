package usecase

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

type imageTaskClassifierStub struct {
	result  ImageTaskClassification
	err     error
	calls   int
	image   []byte
	entered chan struct{}
	block   <-chan struct{}
}

type definitiveImageTaskTestError struct{ message string }

func (e definitiveImageTaskTestError) Error() string { return e.message }
func (definitiveImageTaskTestError) ProviderResponseStatusCode() int {
	return 503
}

func (s *imageTaskClassifierStub) ClassifyImageTask(ctx context.Context, in ImageTaskClassificationInput) (ImageTaskClassification, error) {
	s.calls++
	if s.entered != nil {
		select {
		case s.entered <- struct{}{}:
		default:
		}
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ImageTaskClassification{}, ctx.Err()
		}
	}
	if len(in.Images) > 0 {
		s.image = append([]byte(nil), in.Images[0]...)
	}
	return s.result, s.err
}

type imageTaskGradingStub struct {
	starts                int
	async                 int
	cancels               int
	jobID                 string
	startErr              error
	input                 StartPhotoGradingInput
	parentRetryAllowed    bool
	parentRetryAttemptID  string
	parentRetryDeadlineAt int64
}

func (s *imageTaskGradingStub) resolvedJobID() string {
	if s.jobID != "" {
		return s.jobID
	}
	return "internal-grading-job"
}

func (s *imageTaskGradingStub) StartPhotoGradingJob(_ context.Context, in StartPhotoGradingInput) (GradingJobView, bool, error) {
	s.starts++
	s.input = in
	if s.startErr != nil {
		return GradingJobView{}, false, s.startErr
	}
	return GradingJobView{Record: &records.AgentRecord{RecordID: s.resolvedJobID()}}, true, nil
}

func (s *imageTaskGradingStub) StartAsync(jobID string) bool {
	if jobID == s.resolvedJobID() {
		s.async++
	}
	return true
}

func (s *imageTaskGradingStub) ConfirmPhotoGradingJob(
	context.Context, string, ConfirmPhotoGradingInput,
) (GradingJobView, bool, error) {
	return GradingJobView{Record: &records.AgentRecord{RecordID: "internal-grading-job"}}, true, nil
}

func (s *imageTaskGradingStub) CancelImageTaskHomework(
	_ context.Context, agentName, jobID string,
) error {
	if agentName != "mingming" || jobID != s.resolvedJobID() {
		return errors.New("unexpected cancellation scope")
	}
	s.cancels++
	return nil
}

func (s *imageTaskGradingStub) CanRetryPhotoGradingWithParentAutomaticWindow(
	context.Context,
	string,
) (bool, error) {
	return s.parentRetryAllowed, nil
}

func (s *imageTaskGradingStub) RetryPhotoGradingJobWithParentAutomaticWindow(
	_ context.Context,
	jobID, parentAutomaticAttemptID string,
	parentAutomaticDeadlineAt int64,
) (GradingJobView, bool, error) {
	if jobID != s.resolvedJobID() {
		return GradingJobView{}, false, errors.New("unexpected grading retry job")
	}
	s.parentRetryAttemptID = parentAutomaticAttemptID
	s.parentRetryDeadlineAt = parentAutomaticDeadlineAt
	return GradingJobView{
		Record: &records.AgentRecord{RecordID: jobID},
	}, true, nil
}

type imageTaskOCRStub struct {
	result         ImageTaskWritingOCRResult
	err            error
	calls          int
	panicAfterSend bool
	block          <-chan struct{}
}

type imageTaskFeedbackSolver struct {
	calls          int
	routeCalls     int
	err            error
	routeErr       error
	panicAfterSend bool
	snapshots      []k12.GradingModelSnapshot
	block          <-chan struct{}
}

func (s *imageTaskFeedbackSolver) Solve(
	context.Context, string, string, string,
) (SolveResult, error) {
	return SolveResult{}, nil
}

func (s *imageTaskFeedbackSolver) GenerateWorkFeedback(
	ctx context.Context,
	_ WorkFeedbackRequest,
) (WorkFeedbackOutput, error) {
	s.calls++
	if snapshot, ok := k12.GradingModelSnapshotFromContext(ctx); ok {
		s.snapshots = append(s.snapshots, snapshot)
	}
	if s.panicAfterSend {
		panic("simulated process crash after work feedback send")
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return WorkFeedbackOutput{}, ctx.Err()
		}
	}
	return WorkFeedbackOutput{
		Feedback:   "画面中的人物和小猫位置清楚；建议补充地面上的可见阴影细节。",
		SkillStamp: "art-feedback@1.0.0/test",
	}, s.err
}

func (s *imageTaskOCRStub) RecognizeImageTaskWriting(ctx context.Context, _ []byte) (ImageTaskWritingOCRResult, error) {
	s.calls++
	if s.panicAfterSend {
		panic("simulated process crash after provider send")
	}
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return ImageTaskWritingOCRResult{}, ctx.Err()
		}
	}
	return s.result, s.err
}

func imageTaskRouteForTest(requested k12.ImageTaskRouteSnapshot) (k12.ImageTaskRouteSnapshot, error) {
	requested.Provider = strings.TrimSpace(requested.Provider)
	requested.Model = strings.TrimSpace(requested.Model)
	if requested.Provider == "" {
		requested.Provider = "hexclaw-gpt"
	}
	if requested.Model == "" {
		requested.Model = "gpt-5.6-sol"
	}
	requested.Route = requested.Provider + "/" + requested.Model
	requested.Capability = "vision"
	if requested.SelectionSource == "" {
		requested.SelectionSource = "auto"
	}
	requested.PolicyVersion = "image-task-routing-v1"
	requested.PromptVersion = "image-task-classifier-v1"
	return requested, nil
}

const imageTaskAssetForTest = "asset://mingming/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png"

func newImageTaskCoordinatorForTest(t *testing.T, classifier *imageTaskClassifierStub) (*ImageTaskCoordinator, *imageTaskGradingStub) {
	t.Helper()
	deps, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		nil,
	)
	gr := &imageTaskGradingStub{}
	feedbackSolver := &imageTaskFeedbackSolver{}
	feedbackDeps := deps
	feedbackDeps.Solver = feedbackSolver
	feedbackDeps.WorkFeedbackRoute = func(
		_ context.Context, _ string,
	) (k12.ImageTaskRouteSnapshot, error) {
		feedbackSolver.routeCalls++
		if feedbackSolver.routeErr != nil {
			return k12.ImageTaskRouteSnapshot{}, feedbackSolver.routeErr
		}
		return k12.ImageTaskRouteSnapshot{
			Provider: "new-default", Model: "model-b",
			Route: "new-default/model-b", Capability: "vision",
			SelectionSource: "explicit", PolicyVersion: "work-feedback-routing-v1",
			PromptVersion: "art-feedback-v1",
		}, nil
	}
	return &ImageTaskCoordinator{
		Records:    deps.Records,
		Classifier: classifier,
		WritingOCR: &imageTaskOCRStub{result: ImageTaskWritingOCRResult{
			Raw: "我的好爸爸", CanonicalContent: "我的好爸爸", Confidence: 0.99,
		}},
		Grading:      gr,
		WorkFeedback: &feedbackDeps,
		ResolveRoute: imageTaskRouteForTest,
		ReadAsset: func(agent, ref string) ([]byte, error) {
			if agent != "mingming" || ref != imageTaskAssetForTest {
				t.Fatalf("unexpected owner/ref %q %q", agent, ref)
			}
			return []byte("real-image-bytes"), nil
		},
		Now: func() int64 { return 1000 },
		NewID: func(kind string) string {
			return map[string]string{
				"dispatch":             "dispatch-1",
				"classification":       "classification-1",
				"writing_ocr":          "writing-ocr-1",
				"solve_preflight":      "solve-preflight-1",
				"classification_retry": "classification-2",
				"writing_ocr_retry":    "writing-ocr-2",
			}[kind]
		},
	}, gr
}

func testCreateImageTaskInput() CreateImageTaskInput {
	return CreateImageTaskInput{
		AgentName:         "mingming",
		LearnerID:         "learner-1",
		SourceKind:        k12.ImageTaskSourceDesktop,
		SourceRef:         "message-1",
		SourceSessionID:   "session-1",
		SourceAssetRefs:   []string{imageTaskAssetForTest},
		MessageIntent:     "请处理",
		AttemptGeneration: 1,
		RouteRequest: k12.ImageTaskRouteSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			SelectionSource: "explicit",
		},
	}
}

func TestManualArtworkSkipsClassificationAndWaitsForCommit(t *testing.T) {
	coordinator, _ := newImageTaskCoordinatorForTest(t, nil)
	coordinator.ResolveRoute = nil
	input := testCreateImageTaskInput()
	input.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryNewWork, TaskIntent: k12.ImageTaskIntentArtwork,
	}
	prepared, created, err := coordinator.Create(context.Background(), input)
	if err != nil || !created {
		t.Fatalf("manual create: view=%+v created=%v err=%v", prepared, created, err)
	}
	if prepared.Dispatch.RoutingProvenance != k12.ImageTaskRoutingParentSelected ||
		prepared.Dispatch.ClassificationInvocationID != "" ||
		prepared.Dispatch.RoutePolicySnapshot != (k12.ImageTaskRouteSnapshot{}) ||
		prepared.Creative == nil ||
		prepared.Creative.RoutePolicySnapshot != (k12.ImageTaskRouteSnapshot{}) ||
		prepared.Creative.Status != k12.CreativeWorkIntakeReady {
		t.Fatalf("manual create leaked unexecuted model route or auto promotion: %+v", prepared)
	}
	resumed, err := coordinator.Run(
		context.Background(), input.AgentName, prepared.Dispatch.DispatchID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Creative == nil ||
		resumed.Creative.Status != k12.CreativeWorkIntakeReady ||
		resumed.Creative.PromotedWorkID != "" {
		t.Fatalf("manual ready intake auto-promoted during resume: %+v", resumed.Creative)
	}
	committed, err := coordinator.Confirm(context.Background(), ConfirmImageTaskInput{
		AgentName: input.AgentName, DispatchID: prepared.Dispatch.DispatchID,
		ExpectedVersion: prepared.Dispatch.Version,
		Creative: &ConfirmCreativeImageTaskInput{
			Action:    CreativeImageTaskActionCommit,
			WorkTitle: "彩虹和小猫", TaskRequirement: "观察色彩与构图",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if committed.Creative == nil ||
		committed.Creative.Status != k12.CreativeWorkIntakePromoted ||
		committed.Creative.PromotedVersionID != "v1" {
		t.Fatalf("manual commit did not create exactly one v1: %+v", committed.Creative)
	}
	completed, err := coordinator.Run(
		context.Background(), input.AgentName, prepared.Dispatch.DispatchID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.CreativeFeedback != "feedback_ready" {
		t.Fatalf("manual commit did not complete automatic feedback: %+v", completed)
	}
	invocation, err := coordinator.Records.GetLatestWorkFeedbackInvocation(
		context.Background(), input.AgentName, committed.Creative.PromotedWorkID,
		"work:"+committed.Creative.PromotedWorkID+":version:v1:feedback",
	)
	if err != nil {
		t.Fatalf("manual commit did not register automatic feedback: %v", err)
	}
	if invocation.Status != k12.ImageTaskInvocationSucceeded {
		t.Fatalf("automatic feedback invocation did not succeed: %+v", invocation)
	}
	if err := invocation.RouteSnapshot.Validate(); err != nil {
		t.Fatalf("automatic feedback route snapshot not frozen: %+v err=%v", invocation, err)
	}
	if invocation.RouteSnapshot.Provider != "new-default" ||
		invocation.RouteSnapshot.Model != "model-b" ||
		invocation.RouteSnapshot.Route != "new-default/model-b" {
		t.Fatalf("automatic feedback did not use the independent feedback route: %+v", invocation)
	}
}

func TestManualWritingClearOCRStillWaitsForParentFreeze(t *testing.T) {
	classifier := &imageTaskClassifierStub{}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	resolveCalls := 0
	coordinator.ResolveRoute = func(
		request k12.ImageTaskRouteSnapshot,
	) (k12.ImageTaskRouteSnapshot, error) {
		resolveCalls++
		return imageTaskRouteForTest(request)
	}
	input := testCreateImageTaskInput()
	input.CreativeEntry = &k12.ImageTaskCreativeEntry{
		Kind: k12.CreativeWorkEntryNewWork, TaskIntent: k12.ImageTaskIntentWriting,
	}
	prepared, _, err := coordinator.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 0 ||
		prepared.Dispatch.RoutePolicySnapshot != (k12.ImageTaskRouteSnapshot{}) ||
		prepared.Creative == nil ||
		prepared.Creative.RoutePolicySnapshot != (k12.ImageTaskRouteSnapshot{}) {
		t.Fatalf("manual writing resolved an unexecuted route during create: calls=%d view=%+v",
			resolveCalls, prepared)
	}
	view, err := coordinator.Run(
		context.Background(), input.AgentName, prepared.Dispatch.DispatchID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 1 {
		t.Fatalf("writing OCR route resolution calls=%d want=1", resolveCalls)
	}
	invocation, err := coordinator.Records.GetLatestWritingOCRInvocation(
		context.Background(), input.AgentName, view.Creative.IntakeID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.RouteSnapshot.Provider != input.RouteRequest.Provider ||
		invocation.RouteSnapshot.Model != input.RouteRequest.Model ||
		invocation.RouteSnapshot.PromptVersion != "creative-work-writing-ocr-v1" ||
		invocation.RouteSnapshot.Capability != "vision" {
		t.Fatalf("actual OCR invocation did not freeze resolved route: %+v", invocation.RouteSnapshot)
	}
	if view.Creative == nil ||
		view.Creative.Status != k12.CreativeWorkIntakeAwaitingConfirmation ||
		view.Creative.ConfirmationProvenance == k12.CreativeWorkEvidenceAutoFreeze {
		t.Fatalf("clear manual OCR was auto-frozen: %+v", view.Creative)
	}
	if len(view.Creative.OCREvidence.RiskSegments) != 0 {
		t.Fatalf("manual confirmation gate invented OCR risk evidence: %+v",
			view.Creative.OCREvidence.RiskSegments)
	}
	frozen, err := coordinator.Confirm(context.Background(), ConfirmImageTaskInput{
		AgentName: input.AgentName, DispatchID: prepared.Dispatch.DispatchID,
		ExpectedVersion: prepared.Dispatch.Version,
		Creative: &ConfirmCreativeImageTaskInput{
			Action:           CreativeImageTaskActionFreezeOCR,
			CanonicalVersion: view.Creative.OCREvidence.CanonicalVersion,
			CanonicalContent: view.Creative.OCREvidence.CanonicalContent,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Creative == nil ||
		frozen.Creative.Status != k12.CreativeWorkIntakeReady ||
		frozen.Creative.PromotedWorkID != "" {
		t.Fatalf("freeze_ocr created a formal work: %+v", frozen.Creative)
	}
	committed, err := coordinator.Confirm(context.Background(), ConfirmImageTaskInput{
		AgentName: input.AgentName, DispatchID: prepared.Dispatch.DispatchID,
		ExpectedVersion: prepared.Dispatch.Version,
		Creative: &ConfirmCreativeImageTaskInput{
			Action: CreativeImageTaskActionCommit, WorkTitle: "我的好爸爸",
			ContentMarkdown: frozen.Creative.OCREvidence.CanonicalContent,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if committed.Creative == nil ||
		committed.Creative.Status != k12.CreativeWorkIntakePromoted ||
		committed.Creative.PromotedVersionID != "v1" {
		t.Fatalf("commit did not promote frozen writing: %+v", committed.Creative)
	}
}

func createAndRunImageTask(
	t *testing.T,
	coordinator *ImageTaskCoordinator,
	input CreateImageTaskInput,
) (ImageTaskView, bool, error) {
	t.Helper()
	prepared, created, err := coordinator.Create(context.Background(), input)
	if err != nil {
		return prepared, created, err
	}
	view, runErr := coordinator.Run(
		context.Background(), input.AgentName, prepared.Dispatch.DispatchID,
	)
	return view, created, runErr
}

func restartImageTaskCoordinator(
	original *ImageTaskCoordinator,
	classifier ImageTaskClassifier,
) *ImageTaskCoordinator {
	return &ImageTaskCoordinator{
		Records: original.Records, Classifier: classifier,
		WritingOCR: original.WritingOCR, Grading: original.Grading,
		WorkFeedback: original.WorkFeedback, ResolveRoute: original.ResolveRoute,
		ResolveGrade: original.ResolveGrade, ReadAsset: original.ReadAsset,
		Now: original.Now, NewID: original.NewID,
	}
}

func TestImageTaskCoordinatorRejectsNonOwnerAssetBeforeClassifierCall(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent: k12.ImageTaskIntentArtwork, IntentEvidence: []string{"drawing"}, Confidence: 1,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	input := testCreateImageTaskInput()
	input.SourceAssetRefs = []string{
		"asset://other/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.png",
	}
	if _, _, err := coordinator.Create(context.Background(), input); err == nil {
		t.Fatal("cross-owner image task asset must fail closed")
	}
	if classifier.calls != 0 {
		t.Fatalf("classifier was called before owner validation: %d", classifier.calls)
	}
}

func TestImageTaskCoordinatorCreateReturnsDurableIdentityBeforeProviderAndDuplicateSchedulesOnce(t *testing.T) {
	release := make(chan struct{})
	classifier := &imageTaskClassifierStub{
		result: ImageTaskClassification{
			Intent: k12.ImageTaskIntentArtwork, IntentEvidence: []string{"drawing"},
			Confidence: 1,
		},
		entered: make(chan struct{}, 2), block: release,
	}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	prepared, created, err := coordinator.Create(
		context.Background(), testCreateImageTaskInput(),
	)
	if err != nil || !created ||
		prepared.Dispatch.Status != k12.ImageTaskStatusRouting ||
		classifier.calls != 0 {
		t.Fatalf("Create must only persist prepared identity: view=%+v created=%v calls=%d err=%v",
			prepared, created, classifier.calls, err)
	}
	if !coordinator.StartAsync("mingming", prepared.Dispatch.DispatchID) {
		t.Fatal("first worker was not scheduled")
	}
	select {
	case <-classifier.entered:
	case <-time.After(time.Second):
		t.Fatal("classifier worker did not start")
	}
	replayed, duplicate, err := coordinator.Create(
		context.Background(), testCreateImageTaskInput(),
	)
	if err != nil || duplicate ||
		replayed.Dispatch.DispatchID != prepared.Dispatch.DispatchID {
		t.Fatalf("duplicate POST did not return same durable identity: %+v %v %v",
			replayed, duplicate, err)
	}
	if coordinator.StartAsync("mingming", replayed.Dispatch.DispatchID) {
		t.Fatal("duplicate POST scheduled a second active worker")
	}
	close(release)
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := coordinator.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	if classifier.calls != 1 {
		t.Fatalf("duplicate POST caused %d provider calls, want 1", classifier.calls)
	}
}

func TestImageTaskCoordinatorRecoveryRunsOnlyPreparedCheckpoint(t *testing.T) {
	originalClassifier := &imageTaskClassifierStub{}
	original, _ := newImageTaskCoordinatorForTest(t, originalClassifier)
	prepared, created, err := original.Create(
		context.Background(),
		testCreateImageTaskInput(),
	)
	if err != nil || !created {
		t.Fatalf("prepare: created=%v err=%v", created, err)
	}
	recoveryClassifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent: k12.ImageTaskIntentArtwork, IntentEvidence: []string{"drawing"},
		Confidence: 1,
	}}
	restarted := restartImageTaskCoordinator(original, recoveryClassifier)
	recovered, err := restarted.Recover(context.Background(), []string{"mingming"})
	if err != nil || recovered != 1 {
		t.Fatalf("recover prepared: count=%d err=%v", recovered, err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := restarted.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	view, err := restarted.Get(
		context.Background(),
		"mingming",
		prepared.Dispatch.DispatchID,
	)
	if err != nil || recoveryClassifier.calls != 1 ||
		view.Creative == nil ||
		view.Creative.Status != k12.CreativeWorkIntakePromoted {
		t.Fatalf("prepared checkpoint did not recover exactly once: calls=%d view=%+v err=%v",
			recoveryClassifier.calls, view, err)
	}
}

func TestImageTaskCoordinatorRecoveryNeverResendsSentCheckpoint(t *testing.T) {
	originalClassifier := &imageTaskClassifierStub{}
	original, _ := newImageTaskCoordinatorForTest(t, originalClassifier)
	prepared, _, err := original.Create(context.Background(), testCreateImageTaskInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := original.Records.ClaimImageTaskInvocationSend(
		context.Background(),
		"mingming",
		prepared.Dispatch.ClassificationInvocationID,
		"image-task:"+prepared.Dispatch.DispatchID+":classification",
		1000,
	); err != nil || !claimed {
		t.Fatalf("claim sent checkpoint: claimed=%v err=%v", claimed, err)
	}
	recoveryClassifier := &imageTaskClassifierStub{}
	restarted := restartImageTaskCoordinator(original, recoveryClassifier)
	recovered, err := restarted.Recover(context.Background(), []string{"mingming"})
	if err != nil || recovered != 0 {
		t.Fatalf("sent checkpoint should remain parked: count=%d err=%v", recovered, err)
	}
	if recoveryClassifier.calls != 0 {
		t.Fatalf("recovery blindly resent sent invocation: %d", recoveryClassifier.calls)
	}
}

func TestImageTaskCoordinatorShutdownCancelsAndSealsWorkers(t *testing.T) {
	classifier := &imageTaskClassifierStub{
		block:   make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	input := testCreateImageTaskInput()
	input.RouteRequest.TimeoutMS = 5_000
	prepared, _, err := coordinator.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !coordinator.StartAsync("mingming", prepared.Dispatch.DispatchID) {
		t.Fatal("worker was not scheduled")
	}
	select {
	case <-classifier.entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not start")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := coordinator.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown did not cancel/drain worker: %v", err)
	}
	if coordinator.StartAsync("mingming", prepared.Dispatch.DispatchID) {
		t.Fatal("sealed coordinator accepted new worker")
	}
	invocation, err := coordinator.Records.GetImageTaskInvocation(
		context.Background(),
		"mingming",
		prepared.Dispatch.ClassificationInvocationID,
	)
	if err != nil || invocation.Status != k12.ImageTaskInvocationOutcomeUnknown ||
		invocation.RetrySafe {
		t.Fatalf("shutdown cancellation was not parked safely: invocation=%+v err=%v",
			invocation, err)
	}
}

func TestImageTaskCoordinatorQuiesceAgentCancelsSentWorkerWithoutGlobalSeal(t *testing.T) {
	classifier := &imageTaskClassifierStub{
		block:   make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	input := testCreateImageTaskInput()
	input.RouteRequest.TimeoutMS = 5_000
	prepared, _, err := coordinator.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !coordinator.StartAsync("mingming", prepared.Dispatch.DispatchID) {
		t.Fatal("worker was not scheduled")
	}
	select {
	case <-classifier.entered:
	case <-time.After(time.Second):
		t.Fatal("provider did not reach sent boundary")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resume, err := coordinator.QuiesceAgent(drainCtx, "mingming")
	if err != nil {
		t.Fatalf("quiesce target agent: %v", err)
	}
	invocation, err := coordinator.Records.GetImageTaskInvocation(
		context.Background(),
		"mingming",
		prepared.Dispatch.ClassificationInvocationID,
	)
	if err != nil ||
		invocation.Status != k12.ImageTaskInvocationOutcomeUnknown ||
		invocation.RetrySafe {
		t.Fatalf("agent cancellation was not parked safely: invocation=%+v err=%v",
			invocation, err)
	}
	if coordinator.StartAsync("mingming", prepared.Dispatch.DispatchID) {
		t.Fatal("quiesced Agent accepted a new worker")
	}
	resume()
	resume()
	if !coordinator.StartAsync("mingming", prepared.Dispatch.DispatchID) {
		t.Fatal("idempotent resume left the coordinator globally sealed")
	}
	if err := coordinator.Wait(drainCtx); err != nil {
		t.Fatal(err)
	}
}

func TestImageTaskCoordinatorProviderTimeoutAndAmbiguousTransportNeverBlindRetry(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		classifier := &imageTaskClassifierStub{block: make(chan struct{})}
		coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
		input := testCreateImageTaskInput()
		input.RouteRequest.TimeoutMS = 20
		prepared, _, err := coordinator.Create(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.Run(
			context.Background(),
			"mingming",
			prepared.Dispatch.DispatchID,
		); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline error=%v", err)
		}
		invocation, err := coordinator.Records.GetImageTaskInvocation(
			context.Background(),
			"mingming",
			prepared.Dispatch.ClassificationInvocationID,
		)
		if err != nil || invocation.Status != k12.ImageTaskInvocationOutcomeUnknown ||
			invocation.RetrySafe {
			t.Fatalf("deadline not parked outcome_unknown: %+v err=%v", invocation, err)
		}
		if _, err := coordinator.Retry(
			context.Background(),
			"mingming",
			prepared.Dispatch.DispatchID,
			1,
		); !errors.Is(err, k12storage.ErrImageTaskInvalidState) {
			t.Fatalf("ordinary retry accepted timeout ambiguity: %v", err)
		}
	})

	t.Run("unexpected EOF", func(t *testing.T) {
		classifier := &imageTaskClassifierStub{err: io.ErrUnexpectedEOF}
		coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
		prepared, _, err := coordinator.Create(
			context.Background(),
			testCreateImageTaskInput(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.Run(
			context.Background(),
			"mingming",
			prepared.Dispatch.DispatchID,
		); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("transport error=%v", err)
		}
		if _, err := coordinator.Run(
			context.Background(),
			"mingming",
			prepared.Dispatch.DispatchID,
		); err != nil {
			t.Fatalf("parked replay should be a pure projection: %v", err)
		}
		if classifier.calls != 1 {
			t.Fatalf("ambiguous transport was resent: %d", classifier.calls)
		}
	})
}

func TestImageTaskCoordinatorDownstreamProvidersUseFrozenTimeout(t *testing.T) {
	t.Run("writing OCR", func(t *testing.T) {
		classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
			Intent: k12.ImageTaskIntentWriting, IntentEvidence: []string{"essay"},
			Confidence: 1,
		}}
		coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
		ocr := coordinator.WritingOCR.(*imageTaskOCRStub)
		ocr.block = make(chan struct{})
		input := testCreateImageTaskInput()
		input.RouteRequest.TimeoutMS = 20
		prepared, _, err := coordinator.Create(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.Run(
			context.Background(),
			"mingming",
			prepared.Dispatch.DispatchID,
		); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("OCR deadline error=%v", err)
		}
		view, err := coordinator.Get(
			context.Background(),
			"mingming",
			prepared.Dispatch.DispatchID,
		)
		if err != nil || view.Creative == nil {
			t.Fatalf("OCR checkpoint missing: view=%+v err=%v", view, err)
		}
		invocation, err := coordinator.Records.GetLatestWritingOCRInvocation(
			context.Background(),
			"mingming",
			view.Creative.IntakeID,
		)
		if err != nil || invocation.Status != k12.ImageTaskInvocationOutcomeUnknown ||
			invocation.RouteSnapshot.TimeoutMS != 20 {
			t.Fatalf("OCR timeout not frozen/parked: invocation=%+v err=%v", invocation, err)
		}
	})

	t.Run("work feedback", func(t *testing.T) {
		classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
			Intent: k12.ImageTaskIntentArtwork, IntentEvidence: []string{"drawing"},
			Confidence: 1,
		}}
		coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
		solver := coordinator.WorkFeedback.(*Deps).Solver.(*imageTaskFeedbackSolver)
		solver.block = make(chan struct{})
		input := testCreateImageTaskInput()
		input.RouteRequest.TimeoutMS = 20
		prepared, _, err := coordinator.Create(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coordinator.Run(
			context.Background(),
			"mingming",
			prepared.Dispatch.DispatchID,
		); err == nil {
			t.Fatalf("feedback deadline error=%v", err)
		}
		view, err := coordinator.Get(
			context.Background(),
			"mingming",
			prepared.Dispatch.DispatchID,
		)
		if err != nil ||
			view.CreativeFeedback != "feedback_outcome_unknown" ||
			view.feedbackInvocation == nil ||
			view.feedbackInvocation.RouteSnapshot.TimeoutMS != 20 {
			t.Fatalf("feedback timeout not frozen/parked: view=%+v err=%v", view, err)
		}
	})
}

func TestImageTaskCoordinatorRoutesHomeworkToInternalGradingWithoutLeakingModelChoice(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	feedbackDeps := coordinator.WorkFeedback.(*Deps)
	persistedJob, created, err := feedbackDeps.CreateGradingJob(
		context.Background(), "mingming", "session-receipt",
		CreateGradingJobInput{
			SubmissionID: "submission-receipt", SourceKind: "test",
			SourceKey: "image-task-receipt", ConfirmedVersion: 0,
			ModelSnapshot: k12.GradingModelSnapshot{
				Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
				Route: "hexclaw-gpt/gpt-5.6-sol", Capability: "vision",
			},
		},
	)
	if err != nil || !created {
		t.Fatalf("persist grading job fixture: created=%v err=%v", created, err)
	}
	grading.jobID = persistedJob.Record.RecordID

	view, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !created {
		t.Fatalf("Create: view=%+v created=%v err=%v", view, created, err)
	}
	if view.Dispatch.TaskIntent != k12.ImageTaskIntentCompletedHomework ||
		view.Dispatch.TargetObjectType != k12.ImageTaskTargetHomeworkSubmission {
		t.Fatalf("wrong public dispatch: %+v", view.Dispatch)
	}
	if grading.starts != 1 || grading.async != 1 {
		t.Fatalf("grading start=%d async=%d", grading.starts, grading.async)
	}
	if string(grading.input.Photo.Image) != "real-image-bytes" ||
		grading.input.ModelSnapshot.Route != "hexclaw-gpt/gpt-5.6-sol" {
		t.Fatalf("wrong frozen grading input: %+v", grading.input)
	}
	submission, err := coordinator.Records.GetHomeworkSubmission(
		context.Background(), "mingming", view.Dispatch.TargetObjectID,
	)
	if err != nil || submission.GradingJobID != grading.jobID {
		t.Fatalf("internal binding missing: submission=%+v err=%v", submission, err)
	}
	jobID, err := coordinator.ResolveTutoringTipsGradingJob(
		context.Background(), "mingming", view.Dispatch.DispatchID,
	)
	if err != nil || jobID != grading.jobID {
		t.Fatalf("dispatch did not resolve internal tutoring facts: job=%q err=%v", jobID, err)
	}
	if _, err := coordinator.ResolveTutoringTipsGradingJob(
		context.Background(), "other", view.Dispatch.DispatchID,
	); err == nil {
		t.Fatal("cross-owner tutoring dispatch lookup must fail closed")
	}
	invocation, _, err := coordinator.Records.PrepareModelInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID: "solve-invocation-1", AgentName: "mingming",
			JobID: grading.jobID, Stage: "solve",
			RequestDigest: "sha256:solve-request", Attempt: 1,
			RouteSnapshot: k12.GradingModelSnapshot{
				Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
				Route: "hexclaw-gpt/gpt-5.6-sol", Capability: "vision",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err = coordinator.Records.MarkModelInvocationSent(
		context.Background(), "mingming", invocation.InvocationID, "provider-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Records.MarkModelInvocationSucceeded(
		context.Background(), "mingming", invocation.InvocationID,
		"sha256:terminal-answer", "external-request-1",
	); err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Result(context.Background(), "mingming", view.Dispatch.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.OperationReceipts) != 1 ||
		result.OperationReceipts[0].Operation != "solve" ||
		result.OperationReceipts[0].Provider != "hexclaw-gpt" ||
		result.OperationReceipts[0].Model != "gpt-5.6-sol" ||
		result.OperationReceipts[0].Status != "succeeded" ||
		result.OperationReceipts[0].ResultDigest != "sha256:terminal-answer" {
		t.Fatalf("solve receipt not projected from durable grading ledger: %+v", result.OperationReceipts)
	}
}

func TestImageTaskCoordinatorCancelPropagatesToHomeworkAndIsReplaySafe(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible handwritten answers"},
		Confidence:     0.99,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	view, _, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil {
		t.Fatal(err)
	}

	cancelled, err := coordinator.Cancel(
		context.Background(), "mingming", view.Dispatch.DispatchID, view.Dispatch.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	if grading.cancels != 1 ||
		cancelled.Dispatch.Status != k12.ImageTaskStatusCancelled ||
		cancelled.Homework == nil ||
		cancelled.Homework.Status != k12.HomeworkSubmissionCancelled {
		t.Fatalf("cancellation did not propagate: cancels=%d view=%+v", grading.cancels, cancelled)
	}

	replayed, err := coordinator.Cancel(
		context.Background(), "mingming", view.Dispatch.DispatchID, view.Dispatch.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	if grading.cancels != 1 || replayed.Dispatch.Status != k12.ImageTaskStatusCancelled {
		t.Fatalf("cancel replay was not idempotent: cancels=%d view=%+v", grading.cancels, replayed)
	}
}

func TestImageTaskCoordinatorArtworkImmediatelyPromotesWithoutGradingJob(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentArtwork,
		IntentEvidence: []string{"free-form crayon illustration"},
		Confidence:     0.99,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)

	view, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !created {
		t.Fatalf("Create: view=%+v created=%v err=%v", view, created, err)
	}
	if grading.starts != 0 {
		t.Fatalf("artwork must never create GradingJob, starts=%d", grading.starts)
	}
	if view.Creative == nil || view.Creative.Status != k12.CreativeWorkIntakePromoted ||
		view.Creative.PromotedWorkID == "" {
		t.Fatalf("artwork was not promoted: %+v", view)
	}
	if view.CreativeFeedback != "feedback_ready" ||
		view.CreativeWork == nil ||
		view.CreativeWork.Record.Status != k12.WorkStatusFeedbackReady {
		t.Fatalf("artwork was not automatically advanced to durable feedback: %+v", view)
	}
}

func TestImageTaskCoordinatorImageCandidatesRemainEvidenceWithoutBecomingFormalFacts(t *testing.T) {
	for _, confidence := range []float64{0, 0.99} {
		t.Run(fmt.Sprintf("confidence_%g", confidence), func(t *testing.T) {
			classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
				Intent:         k12.ImageTaskIntentArtwork,
				IntentEvidence: []string{"visible drawing"},
				Confidence:     0.99,
				WorkTitleCandidate: &k12.FactCandidate{
					Value: "模型猜出的标题", Source: k12.FactCandidateSourceImageVision,
					Confidence: confidence, EvidenceRef: "asset_index:0#top",
				},
				TaskRequirementCandidate: &k12.FactCandidate{
					Value: "模型猜出的任务", Source: k12.FactCandidateSourceImageVision,
					Confidence: confidence, EvidenceRef: "asset_index:0#bottom",
				},
			}}
			coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
			view, _, err := createAndRunImageTask(
				t,
				coordinator,
				testCreateImageTaskInput(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if view.Creative == nil ||
				view.Creative.WorkTitleCandidate == nil ||
				view.Creative.WorkTitleCandidate.Value != "模型猜出的标题" {
				t.Fatalf("candidate evidence was not preserved: %+v", view.Creative)
			}
			if view.CreativeWork == nil ||
				view.CreativeWork.Fields.WorkTitle != "" ||
				view.CreativeWork.Fields.TaskRequirement != "" ||
				view.CreativeWork.Fields.DisplayName != "美术作品" {
				t.Fatalf("unconfirmed image candidate became formal fact: %+v",
					view.CreativeWork)
			}
		})
	}
}

func TestImageTaskCoordinatorReplayUsesFrozenDispatchBeforeAssetOrRouteResolution(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentArtwork,
		IntentEvidence: []string{"free-form crayon illustration"},
		Confidence:     0.99,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	first, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	coordinator.ReadAsset = func(string, string) ([]byte, error) {
		return nil, errors.New("idempotent replay must not reread immutable asset")
	}
	coordinator.ResolveRoute = func(k12.ImageTaskRouteSnapshot) (k12.ImageTaskRouteSnapshot, error) {
		return k12.ImageTaskRouteSnapshot{}, errors.New("idempotent replay must not resolve mutable default")
	}

	replayed, created, err := coordinator.Create(context.Background(), testCreateImageTaskInput())
	if err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	if replayed.Dispatch.DispatchID != first.Dispatch.DispatchID ||
		replayed.Dispatch.ClassificationRouteSnapshot != first.Dispatch.ClassificationRouteSnapshot ||
		classifier.calls != 1 {
		t.Fatalf("replay drifted frozen dispatch: first=%+v replay=%+v calls=%d",
			first.Dispatch, replayed.Dispatch, classifier.calls)
	}
}

func TestImageTaskCoordinatorReplayRejectsRouteRequestSemanticDrift(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CreateImageTaskInput)
	}{
		{
			name: "explicit model A to B",
			mutate: func(in *CreateImageTaskInput) {
				in.RouteRequest.Model = "model-b"
			},
		},
		{
			name: "explicit to auto",
			mutate: func(in *CreateImageTaskInput) {
				in.RouteRequest = k12.ImageTaskRouteSnapshot{SelectionSource: "auto"}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classifier := &imageTaskClassifierStub{}
			coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
			input := testCreateImageTaskInput()
			if _, created, err := coordinator.Create(context.Background(), input); err != nil || !created {
				t.Fatalf("prepare: created=%v err=%v", created, err)
			}
			tc.mutate(&input)
			if _, _, err := coordinator.Create(context.Background(), input); !errors.Is(
				err, k12storage.ErrImageTaskConflict,
			) {
				t.Fatalf("same idempotency generation accepted route drift: %v", err)
			}
			if classifier.calls != 0 {
				t.Fatalf("route conflict called provider: %d", classifier.calls)
			}
		})
	}
}

func TestImageTaskCoordinatorAutoReplayDoesNotResolveChangedDefault(t *testing.T) {
	classifier := &imageTaskClassifierStub{}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	input := testCreateImageTaskInput()
	input.RouteRequest = k12.ImageTaskRouteSnapshot{SelectionSource: "auto"}
	first, created, err := coordinator.Create(context.Background(), input)
	if err != nil || !created {
		t.Fatalf("prepare auto: created=%v err=%v", created, err)
	}
	coordinator.ResolveRoute = func(k12.ImageTaskRouteSnapshot) (k12.ImageTaskRouteSnapshot, error) {
		return k12.ImageTaskRouteSnapshot{}, errors.New("changed default must not be read")
	}
	replayed, created, err := coordinator.Create(context.Background(), input)
	if err != nil || created ||
		replayed.Dispatch.DispatchID != first.Dispatch.DispatchID ||
		replayed.Dispatch.RoutePolicySnapshot != first.Dispatch.RoutePolicySnapshot {
		t.Fatalf("auto replay drifted frozen route: first=%+v replay=%+v created=%v err=%v",
			first.Dispatch, replayed.Dispatch, created, err)
	}
}

func TestImageTaskCoordinatorResumeRejectsDeletedOrReplacedContentAddressedAssetBeforeProvider(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2n1cAAAAASUVORK5CYII=",
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, assetID string, raw []byte)
	}{
		{
			name: "deleted",
			mutate: func(t *testing.T, assetID string, _ []byte) {
				t.Helper()
				if removed, removeErr := assetstore.Remove("mingming", assetID); removeErr != nil || !removed {
					t.Fatalf("remove asset: removed=%v err=%v", removed, removeErr)
				}
			},
		},
		{
			name: "replaced valid png",
			mutate: func(t *testing.T, assetID string, original []byte) {
				t.Helper()
				_, file, parseErr := assetstore.Parse(assetID)
				if parseErr != nil {
					t.Fatal(parseErr)
				}
				replacement := append([]byte(nil), original...)
				replacement[len(replacement)-1] ^= 0x01
				if writeErr := os.WriteFile(
					filepath.Join(assetstore.Root(), "mingming", file),
					replacement,
					0o600,
				); writeErr != nil {
					t.Fatal(writeErr)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
			assetID, saveErr := assetstore.Save("mingming", raw)
			if saveErr != nil {
				t.Fatal(saveErr)
			}
			classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
				Intent: k12.ImageTaskIntentArtwork, IntentEvidence: []string{"drawing"},
				Confidence: 1,
			}}
			coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
			coordinator.ReadAsset = nil
			input := testCreateImageTaskInput()
			input.SourceAssetRefs = []string{assetID}
			prepared, created, createErr := coordinator.Create(context.Background(), input)
			if createErr != nil || !created {
				t.Fatalf("prepare: created=%v err=%v", created, createErr)
			}
			tc.mutate(t, assetID, raw)
			if _, runErr := coordinator.Run(
				context.Background(), "mingming", prepared.Dispatch.DispatchID,
			); runErr == nil {
				t.Fatal("mutated immutable asset was accepted")
			}
			if classifier.calls != 0 {
				t.Fatalf("provider called after asset mutation: %d", classifier.calls)
			}
			stored, getErr := coordinator.Get(
				context.Background(), "mingming", prepared.Dispatch.DispatchID,
			)
			if getErr != nil || stored.Dispatch.Status != k12.ImageTaskStatusRouting ||
				stored.Creative != nil {
				t.Fatalf("mutation advanced durable target: view=%+v err=%v", stored, getErr)
			}
		})
	}
}

func TestImageTaskCoordinatorRechecksSourceDigestImmediatelyBeforeCreativePromotion(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent: k12.ImageTaskIntentArtwork, IntentEvidence: []string{"drawing"},
		Confidence: 1,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	reads := 0
	coordinator.ReadAsset = func(string, string) ([]byte, error) {
		reads++
		if reads >= 3 {
			return []byte("different-valid-image-bytes"), nil
		}
		return []byte("real-image-bytes"), nil
	}
	prepared, created, err := coordinator.Create(
		context.Background(), testCreateImageTaskInput(),
	)
	if err != nil || !created {
		t.Fatalf("prepare: created=%v err=%v", created, err)
	}
	if _, err := coordinator.Run(
		context.Background(), "mingming", prepared.Dispatch.DispatchID,
	); err == nil {
		t.Fatal("pre-promotion digest drift was accepted")
	}
	projected, err := coordinator.Get(
		context.Background(), "mingming", prepared.Dispatch.DispatchID,
	)
	if err != nil || projected.Creative == nil ||
		projected.Creative.Status != k12.CreativeWorkIntakeReady ||
		projected.Creative.PromotedWorkID != "" {
		t.Fatalf("digest drift promoted a work: view=%+v err=%v", projected, err)
	}
	solver := coordinator.WorkFeedback.(*Deps).Solver.(*imageTaskFeedbackSolver)
	if solver.calls != 0 {
		t.Fatalf("feedback provider called before verified promotion: %d", solver.calls)
	}
}

func TestImageTaskCoordinatorWorkFeedbackRetryReusesFrozenRoute(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentArtwork,
		IntentEvidence: []string{"free-form crayon illustration"},
		Confidence:     0.99,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	feedbackDeps := coordinator.WorkFeedback.(*Deps)
	solver := feedbackDeps.Solver.(*imageTaskFeedbackSolver)
	solver.err = definitiveImageTaskTestError{message: "provider unavailable"}

	_, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err == nil || !created {
		t.Fatalf("first feedback failure must be visible after durable promotion: created=%v err=%v", created, err)
	}
	failed, err := coordinator.Get(context.Background(), "mingming", "dispatch-1")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Creative == nil ||
		failed.Creative.Status != k12.CreativeWorkIntakePromoted ||
		failed.CreativeFeedback != "feedback_failed" {
		t.Fatalf("feedback failure did not preserve promoted work/recovery state: %+v", failed)
	}
	if solver.routeCalls != 0 || len(solver.snapshots) != 1 ||
		solver.snapshots[0].Route != "hexclaw-gpt/gpt-5.6-sol" {
		t.Fatalf("ImageTask feedback drifted to mutable default: routes=%d snapshots=%+v",
			solver.routeCalls, solver.snapshots)
	}
	workID := failed.Creative.PromotedWorkID
	work, getErr := coordinator.Records.Get(context.Background(), workID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	fields, parseErr := k12.ParseCreativeWorkFields(work.Fields)
	if parseErr != nil || len(fields.Versions) == 0 {
		t.Fatalf("work projection missing: fields=%+v err=%v", fields, parseErr)
	}
	operationKey := "work:" + workID + ":version:" +
		fields.Versions[len(fields.Versions)-1].VersionID + ":feedback"
	invocation, getErr := coordinator.Records.GetLatestWorkFeedbackInvocation(
		context.Background(), "mingming", workID, operationKey,
	)
	if getErr != nil ||
		invocation.RouteSnapshot.Provider != "hexclaw-gpt" ||
		invocation.RouteSnapshot.Model != "gpt-5.6-sol" {
		t.Fatalf("durable feedback route did not inherit dispatch snapshot: invocation=%+v err=%v",
			invocation, getErr)
	}

	solver.err = nil
	solver.routeErr = errors.New("mutable default must not be resolved during retry")
	retried, err := coordinator.Retry(
		context.Background(), "mingming", "dispatch-1", failed.Dispatch.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	if solver.routeCalls != 0 || solver.calls != 2 ||
		len(solver.snapshots) != 2 ||
		solver.snapshots[1] != solver.snapshots[0] {
		t.Fatalf("feedback retry drifted frozen route: routes=%d calls=%d snapshots=%+v",
			solver.routeCalls, solver.calls, solver.snapshots)
	}
	if retried.CreativeFeedback != "feedback_ready" ||
		retried.CreativeWork == nil ||
		retried.CreativeWork.Record.Status != k12.WorkStatusFeedbackReady {
		t.Fatalf("retry did not commit durable feedback: %+v", retried)
	}
	result, err := coordinator.Result(context.Background(), "mingming", "dispatch-1")
	if err != nil || result.Kind != "creative" ||
		result.CreativeWork == nil ||
		result.CreativeWork.Fields.Versions[0].StructuredFeedback == nil {
		t.Fatalf("creative result missing canonical feedback: result=%+v err=%v", result, err)
	}
}

func TestImageTaskCoordinatorNeverBlindResendsSentWorkFeedback(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentArtwork,
		IntentEvidence: []string{"free-form crayon illustration"},
		Confidence:     0.99,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	feedbackDeps := coordinator.WorkFeedback.(*Deps)
	solver := feedbackDeps.Solver.(*imageTaskFeedbackSolver)
	solver.panicAfterSend = true
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected simulated feedback crash")
			}
		}()
		prepared, _, err := coordinator.Create(context.Background(), testCreateImageTaskInput())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = coordinator.Run(
			context.Background(), "mingming", prepared.Dispatch.DispatchID,
		)
	}()

	solver.panicAfterSend = false
	before := solver.calls
	replayed, created, err := coordinator.Create(
		context.Background(), testCreateImageTaskInput(),
	)
	if err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	if solver.calls != before ||
		replayed.CreativeFeedback != "feedback_pending" ||
		replayed.feedbackInvocation == nil ||
		replayed.feedbackInvocation.Status != k12.ImageTaskInvocationSent {
		t.Fatalf("sent feedback was blindly resent or leaked wrong state: calls=%d->%d view=%+v",
			before, solver.calls, replayed)
	}
}

func TestImageTaskCoordinatorWritingFreezesOCRAndPromotesWithoutPlaceholderFacts(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentWriting,
		IntentEvidence: []string{"continuous handwritten essay paragraphs"},
		Confidence:     0.98,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)

	view, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !created {
		t.Fatalf("Create: view=%+v created=%v err=%v", view, created, err)
	}
	if grading.starts != 0 {
		t.Fatalf("writing must never create GradingJob, starts=%d", grading.starts)
	}
	if view.Creative == nil || view.Creative.Status != k12.CreativeWorkIntakePromoted ||
		view.Creative.PromotedWorkID == "" {
		t.Fatalf("writing was not promoted: %+v", view)
	}
	work, err := coordinator.Records.Get(context.Background(), view.Creative.PromotedWorkID)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParseCreativeWorkFields(work.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if fields.WorkTitle != "" || fields.TaskRequirement != "" ||
		fields.DisplayName != "语文写作" || fields.Versions[0].ContentMarkdown != "我的好爸爸" {
		t.Fatalf("placeholder or OCR drift: %+v", fields)
	}
}

func TestImageTaskCoordinatorRiskyWritingAwaitsMinimumConfirmationAndGetIsPure(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentWriting,
		IntentEvidence: []string{"continuous handwritten essay paragraphs"},
		Confidence:     0.98,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	ocr := coordinator.WritingOCR.(*imageTaskOCRStub)
	ocr.result = ImageTaskWritingOCRResult{
		Raw: "我的〔字迹不清〕爸爸", CanonicalContent: "我的〔字迹不清〕爸爸",
		Confidence: 0.72,
		RiskSegments: []k12.CreativeWorkIntakeOCRRisk{{
			SegmentID: "line-1-word-3", RawText: "〔字迹不清〕",
			Reasons: []string{"illegible"},
		}},
	}
	view, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || !created {
		t.Fatalf("Create: view=%+v created=%v err=%v", view, created, err)
	}
	if grading.starts != 0 || view.Creative == nil ||
		view.Creative.Status != k12.CreativeWorkIntakeAwaitingConfirmation ||
		view.Creative.PromotedWorkID != "" {
		t.Fatalf("risky writing must await confirmation without grading/promotion: %+v", view)
	}
	beforeCalls := ocr.calls
	projected, err := coordinator.Get(context.Background(), "mingming", view.Dispatch.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	if ocr.calls != beforeCalls || projected.Creative == nil ||
		projected.Creative.Status != k12.CreativeWorkIntakeAwaitingConfirmation {
		t.Fatalf("GET caused OCR/provider side effect: calls=%d->%d view=%+v",
			beforeCalls, ocr.calls, projected)
	}
}

func TestImageTaskCoordinatorReplayNeverBlindResendsSentWritingOCR(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentWriting,
		IntentEvidence: []string{"continuous handwritten essay paragraphs"},
		Confidence:     0.98,
	}}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	ocr := coordinator.WritingOCR.(*imageTaskOCRStub)
	ocr.panicAfterSend = true
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected simulated crash")
			}
		}()
		prepared, _, err := coordinator.Create(context.Background(), testCreateImageTaskInput())
		if err != nil {
			t.Fatal(err)
		}
		_, _ = coordinator.Run(
			context.Background(), "mingming", prepared.Dispatch.DispatchID,
		)
	}()
	invocation, err := coordinator.Records.GetImageTaskInvocation(
		context.Background(), "mingming", "writing-ocr-1",
	)
	if err != nil || invocation.Status != k12.ImageTaskInvocationSent {
		t.Fatalf("crash point did not leave sent receipt: invocation=%+v err=%v", invocation, err)
	}

	ocr.panicAfterSend = false
	before := ocr.calls
	view, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err != nil || created {
		t.Fatalf("replay: created=%v err=%v", created, err)
	}
	if ocr.calls != before ||
		view.Creative == nil ||
		view.Creative.Status != k12.CreativeWorkIntakePreparing {
		t.Fatalf("sent OCR was blindly resent: calls=%d->%d view=%+v", before, ocr.calls, view)
	}
}

func TestImageTaskCoordinatorPersistsClassificationFailureAndFrozenRoute(t *testing.T) {
	classifier := &imageTaskClassifierStub{
		err: definitiveImageTaskTestError{message: "provider unavailable"},
	}
	coordinator, _ := newImageTaskCoordinatorForTest(t, classifier)
	_, created, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput())
	if err == nil || !created {
		t.Fatalf("Create should expose provider failure after durable prepare: created=%v err=%v", created, err)
	}
	stored, getErr := coordinator.Get(context.Background(), "mingming", "dispatch-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.Dispatch.Status != k12.ImageTaskStatusFailed || !stored.Dispatch.RetrySafe ||
		stored.Dispatch.ClassificationRouteSnapshot.Route != "hexclaw-gpt/gpt-5.6-sol" {
		t.Fatalf("classification failure was not durably parked: %+v", stored.Dispatch)
	}
	invocation, getErr := coordinator.Records.GetImageTaskInvocation(
		context.Background(), "mingming", "classification-1",
	)
	if getErr != nil || invocation.Status != k12.ImageTaskInvocationFailed ||
		invocation.RouteSnapshot != stored.Dispatch.ClassificationRouteSnapshot {
		t.Fatalf("classification receipt drift: invocation=%+v err=%v", invocation, getErr)
	}
}

func TestImageTaskCoordinatorRetryReusesFrozenRouteWithoutResolvingMutableDefault(t *testing.T) {
	classifier := &imageTaskClassifierStub{
		err: definitiveImageTaskTestError{message: "provider unavailable"},
	}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	if _, _, err := createAndRunImageTask(t, coordinator, testCreateImageTaskInput()); err == nil {
		t.Fatal("first provider failure expected")
	}
	failed, err := coordinator.Get(context.Background(), "mingming", "dispatch-1")
	if err != nil {
		t.Fatal(err)
	}
	classifier.err = nil
	classifier.result = ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"visible answers"},
		Confidence:     0.99,
	}
	coordinator.ResolveRoute = func(k12.ImageTaskRouteSnapshot) (k12.ImageTaskRouteSnapshot, error) {
		return k12.ImageTaskRouteSnapshot{}, errors.New("mutable resolver must not run during retry")
	}
	retried, err := coordinator.Retry(
		context.Background(), "mingming", "dispatch-1", failed.Dispatch.Version,
	)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Dispatch.TaskIntent != k12.ImageTaskIntentCompletedHomework ||
		grading.starts != 1 {
		t.Fatalf("retry did not finish on frozen route: %+v starts=%d", retried, grading.starts)
	}
	invocation, err := coordinator.Records.GetImageTaskInvocation(
		context.Background(), "mingming", "classification-2",
	)
	if err != nil || invocation.Attempt != 2 ||
		invocation.RouteSnapshot.Route != "hexclaw-gpt/gpt-5.6-sol" {
		t.Fatalf("retry invocation route/attempt drift: %+v err=%v", invocation, err)
	}
}
