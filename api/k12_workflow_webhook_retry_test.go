package api

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type failingK12WorkflowEngine struct{}

func (failingK12WorkflowEngine) Start(context.Context) error  { return nil }
func (failingK12WorkflowEngine) Stop(context.Context) error   { return nil }
func (failingK12WorkflowEngine) Health(context.Context) error { return nil }
func (failingK12WorkflowEngine) Process(context.Context, *adapter.Message) (*adapter.Reply, error) {
	return nil, errors.New("provider disconnected after request")
}
func (failingK12WorkflowEngine) ProcessStream(context.Context, *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
	return nil, errors.New("unused")
}

func k12OwnedWorkflow(id string, nodes, edges []any) *WorkflowData {
	return &WorkflowData{
		ID: id, Name: id, Nodes: nodes, Edges: edges,
		Data: map[string]any{
			"scenario": "k12", "agent_id": "kid-agent", "learner_id": "kid-learner", "version": "v1",
		},
	}
}

func TestK12WebhookWorkflowCompletedTriggerIsIdempotent(t *testing.T) {
	srv := newWorkflowTestServer()
	srv.workflowStore.workflows["wf-idempotent"] = k12OwnedWorkflow("wf-idempotent", nil, nil)
	firstID, retrySafe, err := srv.RunK12WorkflowFromWebhookDispatch(
		context.Background(), "wf-idempotent", "v1", "weekly", "kid-agent", "kid-learner", "binding-1:event-1",
	)
	if err != nil || retrySafe || firstID == "" {
		t.Fatalf("first run id=%q retrySafe=%v err=%v", firstID, retrySafe, err)
	}
	secondID, retrySafe, err := srv.RunK12WorkflowFromWebhookDispatch(
		context.Background(), "wf-idempotent", "v1", "weekly", "kid-agent", "kid-learner", "binding-1:event-1",
	)
	if err != nil || retrySafe || secondID != firstID {
		t.Fatalf("duplicate run id=%q retrySafe=%v err=%v, want %q", secondID, retrySafe, err, firstID)
	}
	if len(srv.workflowStore.runs) != 1 {
		t.Fatalf("stable trigger created %d workflow ledgers", len(srv.workflowStore.runs))
	}
}

func TestK12WebhookWorkflowLocalFailureCreatesCheckpointContinuation(t *testing.T) {
	srv := newWorkflowTestServer()
	srv.workflowStore.workflows["wf-cycle-retry"] = k12OwnedWorkflow("wf-cycle-retry",
		[]any{
			map[string]any{"id": "a", "type": "noop"},
			map[string]any{"id": "b", "type": "noop"},
		},
		[]any{
			map[string]any{"source": "a", "target": "b"},
			map[string]any{"source": "b", "target": "a"},
		},
	)
	firstID, retrySafe, err := srv.RunK12WorkflowFromWebhookDispatch(
		context.Background(), "wf-cycle-retry", "v1", "weekly", "kid-agent", "kid-learner", "binding-1:event-cycle",
	)
	if err == nil || !retrySafe {
		t.Fatalf("local failure id=%q retrySafe=%v err=%v", firstID, retrySafe, err)
	}
	secondID, retrySafe, err := srv.RunK12WorkflowFromWebhookDispatch(
		context.Background(), "wf-cycle-retry", "v1", "weekly", "kid-agent", "kid-learner", "binding-1:event-cycle",
	)
	if err == nil || !retrySafe || secondID == firstID {
		t.Fatalf("continuation id=%q retrySafe=%v err=%v first=%q", secondID, retrySafe, err, firstID)
	}
	srv.workflowStore.mu.RLock()
	second := srv.workflowStore.runs[secondID]
	srv.workflowStore.mu.RUnlock()
	if second == nil || second.TriggerKey != "binding-1:event-cycle" || second.PriorRunID != firstID {
		t.Fatalf("continuation lost trigger chain: %+v", second)
	}
}

func TestK12WebhookWorkflowExternalFailureIsOutcomeUnknownAndNeverBlindRetried(t *testing.T) {
	srv := newWorkflowTestServer()
	srv.engine = failingK12WorkflowEngine{}
	srv.workflowStore.workflows["wf-agent-unknown"] = k12OwnedWorkflow("wf-agent-unknown",
		[]any{map[string]any{"id": "agent", "type": "agent", "data": map[string]any{"role": "tutor"}}}, nil,
	)
	firstID, retrySafe, err := srv.RunK12WorkflowFromWebhookDispatch(
		context.Background(), "wf-agent-unknown", "v1", "weekly", "kid-agent", "kid-learner", "binding-1:event-agent",
	)
	if !errors.Is(err, ErrK12WorkflowOutcomeUnknown) || retrySafe {
		t.Fatalf("external failure id=%q retrySafe=%v err=%v", firstID, retrySafe, err)
	}
	secondID, retrySafe, err := srv.RunK12WorkflowFromWebhookDispatch(
		context.Background(), "wf-agent-unknown", "v1", "weekly", "kid-agent", "kid-learner", "binding-1:event-agent",
	)
	if !errors.Is(err, ErrK12WorkflowOutcomeUnknown) || retrySafe || secondID != firstID {
		t.Fatalf("blind retry id=%q retrySafe=%v err=%v first=%q", secondID, retrySafe, err, firstID)
	}
	if len(srv.workflowStore.runs) != 1 {
		t.Fatalf("outcome-unknown trigger created %d workflow ledgers", len(srv.workflowStore.runs))
	}
}

func TestK12WorkflowRetryPlanRequiresReplayableCheckpointState(t *testing.T) {
	safe, unknown, resumed := k12WorkflowRetryPlan(&WorkflowRun{Status: "failed", NodeResults: []WorkflowNodeRun{
		{NodeID: "tool", Type: "tool", Status: nodeStatusCompleted, Output: "provider-id-1"},
		{NodeID: "out", Type: "output", Status: nodeStatusFailed},
	}})
	if !safe || unknown || resumed["tool"] != "provider-id-1" {
		t.Fatalf("durable completed external checkpoint plan safe=%v unknown=%v resumed=%v", safe, unknown, resumed)
	}

	safe, unknown, _ = k12WorkflowRetryPlan(&WorkflowRun{Status: "failed", NodeResults: []WorkflowNodeRun{
		{NodeID: "tool", Type: "tool", Status: nodeStatusFailed},
	}})
	if safe || !unknown {
		t.Fatalf("failed external boundary plan safe=%v unknown=%v", safe, unknown)
	}

	safe, unknown, _ = k12WorkflowRetryPlan(&WorkflowRun{Status: "failed", NodeResults: []WorkflowNodeRun{
		{NodeID: "branch", Type: "condition", Status: nodeStatusCompleted, Output: "x"},
		{NodeID: "out", Type: "output", Status: nodeStatusFailed},
	}})
	if safe || unknown {
		t.Fatalf("condition branch without branch-state checkpoint safe=%v unknown=%v", safe, unknown)
	}
}
