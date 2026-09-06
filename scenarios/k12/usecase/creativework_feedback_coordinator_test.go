package usecase_test

import (
	"context"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func currentFeedbackRoute() k12.ImageTaskRouteSnapshot {
	return k12.ImageTaskRouteSnapshot{
		Provider: "test-provider", Model: "test-model",
		Route: "test-provider/test-model", Capability: "text",
		SelectionSource: "auto", PolicyVersion: "work-feedback-routing-v1",
		PromptVersion: "writing-feedback-v1", TimeoutMS: 5_000,
	}
}

type cancelAwareWorkFeedbackSolver struct {
	entered  chan struct{}
	returned chan struct{}
}

func (s *cancelAwareWorkFeedbackSolver) Solve(
	context.Context, string, string, string,
) (usecase.SolveResult, error) {
	return usecase.SolveResult{}, nil
}

func (s *cancelAwareWorkFeedbackSolver) GenerateWorkFeedback(
	ctx context.Context,
	_ usecase.WorkFeedbackRequest,
) (usecase.WorkFeedbackOutput, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.returned)
	return usecase.WorkFeedbackOutput{}, ctx.Err()
}

func TestCreativeWorkFeedbackCoordinatorQuiesceAgentTerminalizesSentInvocation(t *testing.T) {
	d := newDataDeps(t, "xiaoming", "xiaohong")
	blocked := &cancelAwareWorkFeedbackSolver{
		entered: make(chan struct{}), returned: make(chan struct{}),
	}
	d.Solver = blocked
	workID, generationID, created, err := d.CreateCurrentTextWork(
		context.Background(), "xiaoming", "桂花落在青石板上。", "save-work-delete-race",
	)
	if err != nil || !created {
		t.Fatalf("create current work: created=%v err=%v", created, err)
	}
	coordinator := &usecase.CreativeWorkFeedbackCoordinator{
		Deps: &d, Records: d.Records, BaseContext: context.Background(),
	}
	if !coordinator.StartAsync("xiaoming", generationID) {
		t.Fatal("target worker was not scheduled")
	}
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("target provider did not reach sent boundary")
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resume, err := coordinator.QuiesceAgent(drainCtx, "xiaoming")
	if err != nil {
		t.Fatalf("quiesce target agent: %v", err)
	}
	select {
	case <-blocked.returned:
	default:
		t.Fatal("quiesce returned before provider cancellation completed")
	}
	generation, err := d.Records.GetWorkFeedbackGeneration(
		context.Background(), "xiaoming", generationID,
	)
	if err != nil || generation.Status != k12.WorkFeedbackFailed {
		t.Fatalf("generation did not become terminal failed: %+v err=%v", generation, err)
	}
	invocation, err := d.Records.GetLatestWorkFeedbackInvocation(
		context.Background(),
		"xiaoming",
		workID,
		"work:"+workID+":version:"+generationID+":feedback",
	)
	if err != nil ||
		invocation.Status != k12.ImageTaskInvocationOutcomeUnknown ||
		invocation.RetrySafe {
		t.Fatalf("sent invocation did not park outcome_unknown: %+v err=%v", invocation, err)
	}

	targetWorkID, targetGenerationID, _, err := d.CreateCurrentTextWork(
		context.Background(), "xiaoming", "雨点敲在窗台上。", "save-work-fenced",
	)
	if err != nil {
		t.Fatal(err)
	}
	_, otherGenerationID, _, err := d.CreateCurrentTextWork(
		context.Background(), "xiaohong", "风吹动了银杏叶。", "save-work-other-agent",
	)
	if err != nil {
		t.Fatal(err)
	}
	d.Solver = &fakeWorkFeedbackSolver{
		feedback: "细节清楚；家长可以追问声音；下次补一处触觉细节。",
	}
	resumeAgain, err := coordinator.QuiesceAgent(drainCtx, "xiaoming")
	if err != nil {
		t.Fatalf("repeated quiesce: %v", err)
	}
	resumeAgain()
	if coordinator.StartAsync("xiaoming", targetGenerationID) {
		t.Fatal("repeated quiesce release reopened the original Agent fence")
	}
	if !coordinator.StartAsync("xiaohong", otherGenerationID) {
		t.Fatal("target Agent fence blocked an unrelated Agent")
	}
	if err := coordinator.Wait(drainCtx); err != nil {
		t.Fatal(err)
	}

	resume()
	resume()
	if !coordinator.StartAsync("xiaoming", targetGenerationID) {
		t.Fatal("idempotent resume did not reopen the target Agent")
	}
	if err := coordinator.Wait(drainCtx); err != nil {
		t.Fatal(err)
	}
	target, err := d.GetCreativeWork(context.Background(), "xiaoming", targetWorkID)
	if err != nil ||
		target.GenerationState.Latest == nil ||
		target.GenerationState.Latest.Status != k12.WorkFeedbackSucceeded {
		t.Fatalf("resumed target Agent did not complete new work: %+v err=%v", target, err)
	}
}

func TestCreativeWorkFeedbackCoordinatorAutomaticallyCompletesInitialGeneration(t *testing.T) {
	d := newDataDeps(t)
	solver := &fakeWorkFeedbackSolver{
		feedback: "## 可见证据\n原稿写到桂花落在青石板上。\n## 先这样肯定\n桂花落在青石板上，画面具体。\n## 家长可以这样问或讲\n先问孩子桂花落下时听到了什么，卡住时让孩子回忆周围的声音，再说明新加细节让画面有什么变化。\n## 下一次只试一个点\n只补一个实际听到的声音细节，并解释为什么保留它。",
	}
	d.Solver = solver
	routeCalls := 0
	d.WorkFeedbackRoute = func(
		context.Context,
		string,
	) (k12.ImageTaskRouteSnapshot, error) {
		routeCalls++
		return currentFeedbackRoute(), nil
	}
	workID, generationID, created, err := d.CreateCurrentTextWork(
		context.Background(), "xiaoming", "桂花落在青石板上。", "save-work-1",
	)
	if err != nil || !created {
		t.Fatalf("create current work: created=%v err=%v", created, err)
	}

	callerDeadline := time.Now().Add(3 * time.Second)
	baseCtx, cancelBase := context.WithDeadline(context.Background(), callerDeadline)
	defer cancelBase()
	coordinator := &usecase.CreativeWorkFeedbackCoordinator{
		Deps: &d, Records: d.Records, BaseContext: baseCtx,
	}
	if !coordinator.StartAsync("xiaoming", generationID) {
		t.Fatal("first schedule must be accepted")
	}
	if coordinator.StartAsync("xiaoming", generationID) {
		t.Fatal("active generation must not be scheduled twice")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := coordinator.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}

	view, err := d.GetCreativeWork(context.Background(), "xiaoming", workID)
	if err != nil {
		t.Fatal(err)
	}
	if view.GenerationState.Initial == nil ||
		view.GenerationState.Initial.Status != k12.WorkFeedbackSucceeded ||
		view.GenerationState.Latest == nil ||
		view.GenerationState.Latest.GenerationID != generationID {
		t.Fatalf("automatic feedback did not converge: %+v", view.GenerationState)
	}
	if solver.calls != 1 || routeCalls != 1 {
		t.Fatalf("provider/route calls=%d/%d want 1/1", solver.calls, routeCalls)
	}
	invocation, err := d.Records.GetLatestWorkFeedbackInvocation(
		context.Background(),
		"xiaoming",
		workID,
		"work:"+workID+":version:"+generationID+":feedback",
	)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Status != k12.ImageTaskInvocationSucceeded ||
		invocation.RouteSnapshot != currentFeedbackRoute() {
		t.Fatalf("direct text route/invocation not frozen: %+v", invocation)
	}
	providerDeadline, hasDeadline := solver.lastCtx.Deadline()
	if !hasDeadline || providerDeadline.Unix() != invocation.DeadlineAt ||
		invocation.DeadlineAt != callerDeadline.Unix() {
		t.Fatalf("worker ignored its shorter caller deadline: provider=%v invocation=%+v", providerDeadline, invocation)
	}
}

func TestCreativeWorkFeedbackCoordinatorRecoversQueuedDirectWorkAfterRestart(t *testing.T) {
	d := newDataDeps(t)
	solver := &fakeWorkFeedbackSolver{
		feedback: "原文的桂花细节可见；家长可以追问声音；下一次只补一处听觉细节。",
	}
	d.Solver = solver
	d.WorkFeedbackRoute = func(
		context.Context,
		string,
	) (k12.ImageTaskRouteSnapshot, error) {
		return currentFeedbackRoute(), nil
	}
	workID, generationID, _, err := d.CreateCurrentTextWork(
		context.Background(), "xiaoming", "桂花落在青石板上。", "save-work-recover",
	)
	if err != nil {
		t.Fatal(err)
	}

	restarted := &usecase.CreativeWorkFeedbackCoordinator{
		Deps: &d, Records: d.Records, BaseContext: context.Background(),
	}
	recovered, err := restarted.Recover(context.Background(), []string{"xiaoming"})
	if err != nil || recovered != 1 {
		t.Fatalf("recover queued generation: recovered=%d err=%v", recovered, err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := restarted.Wait(waitCtx); err != nil {
		t.Fatal(err)
	}
	view, err := d.GetCreativeWork(context.Background(), "xiaoming", workID)
	if err != nil {
		t.Fatal(err)
	}
	if view.GenerationState.Latest == nil ||
		view.GenerationState.Latest.GenerationID != generationID ||
		view.GenerationState.Latest.Status != k12.WorkFeedbackSucceeded ||
		solver.calls != 1 {
		t.Fatalf("recovered generation mismatch: state=%+v calls=%d",
			view.GenerationState, solver.calls)
	}
	if recovered, err = restarted.Recover(
		context.Background(), []string{"xiaoming"},
	); err != nil || recovered != 0 {
		t.Fatalf("terminal generation must not recover again: %d %v", recovered, err)
	}
}
