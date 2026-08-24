package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type imageTaskIMRoutingGateStub struct {
	allow bool
	calls int
}

func (s *imageTaskIMRoutingGateStub) AllowIMCompletedHomeworkGrading(
	_ context.Context,
	dispatch k12.ImageTaskDispatch,
) (bool, error) {
	s.calls++
	if dispatch.SourceKind != k12.ImageTaskSourceIM ||
		dispatch.TaskIntent != k12.ImageTaskIntentCompletedHomework {
		panic("routing gate received an unrelated image task")
	}
	return s.allow, nil
}

func TestImageTaskIMCompletedHomeworkRouteGateDefersGradingAcrossRestart(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"有学生作答痕迹，卷面号 P-2629-01"},
		Confidence:     0.99,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	gate := &imageTaskIMRoutingGateStub{allow: false}
	coordinator.IMCompletedHomeworkRoutingGate = gate
	input := testCreateImageTaskInput()
	input.SourceKind = k12.ImageTaskSourceIM
	input.SourceRef = "dingtalk-inbound:receipt-1"

	created, _, err := coordinator.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		view, err := coordinator.Run(
			context.Background(), input.AgentName, created.Dispatch.DispatchID,
		)
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		if view.Homework == nil || view.Homework.GradingJobID != "" {
			t.Fatalf("attempt %d crossed grading boundary: %+v", attempt, view.Homework)
		}
	}
	if gate.calls != 2 || grading.starts != 0 || grading.async != 0 {
		t.Fatalf("gate/grading calls=%d/%d/%d", gate.calls, grading.starts, grading.async)
	}
}

func TestImageTaskIMCompletedHomeworkRouteGateAllowsOnlyFrozenNewSubmission(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"有学生作答痕迹"},
		Confidence:     0.99,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	gate := &imageTaskIMRoutingGateStub{allow: true}
	coordinator.IMCompletedHomeworkRoutingGate = gate
	input := testCreateImageTaskInput()
	input.SourceKind = k12.ImageTaskSourceIM
	input.SourceRef = "dingtalk-inbound:receipt-1"

	created, _, err := coordinator.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := coordinator.Run(
			context.Background(), input.AgentName, created.Dispatch.DispatchID,
		); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
	}
	if gate.calls != 1 || grading.starts != 1 || grading.async != 1 {
		t.Fatalf("gate/grading calls=%d/%d/%d", gate.calls, grading.starts, grading.async)
	}
}

func TestImageTaskNonIMSourcesDoNotConsultIMRouteGate(t *testing.T) {
	classifier := &imageTaskClassifierStub{result: ImageTaskClassification{
		Intent:         k12.ImageTaskIntentCompletedHomework,
		IntentEvidence: []string{"有学生作答痕迹"},
		Confidence:     0.99,
	}}
	coordinator, grading := newImageTaskCoordinatorForTest(t, classifier)
	gate := &imageTaskIMRoutingGateStub{allow: false}
	coordinator.IMCompletedHomeworkRoutingGate = gate
	input := testCreateImageTaskInput()

	created, _, err := coordinator.Create(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Run(
		context.Background(), input.AgentName, created.Dispatch.DispatchID,
	); err != nil {
		t.Fatal(err)
	}
	if gate.calls != 0 || grading.starts != 1 || grading.async != 1 {
		t.Fatalf("desktop gate/grading calls=%d/%d/%d", gate.calls, grading.starts, grading.async)
	}
}
