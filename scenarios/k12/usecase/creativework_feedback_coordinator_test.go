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

func TestCreativeWorkFeedbackCoordinatorAutomaticallyCompletesInitialGeneration(t *testing.T) {
	d := newDataDeps(t)
	solver := &fakeWorkFeedbackSolver{
		feedback: "「桂花落在青石板上」画面清楚；家长可以问孩子闻到了什么；下次只补一个声音细节。",
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

	coordinator := &usecase.CreativeWorkFeedbackCoordinator{
		Deps: &d, Records: d.Records, BaseContext: context.Background(),
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
