package apihttp

import (
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPublicImageTaskProjectsPersistedAutomaticTimingFacts(t *testing.T) {
	wire := publicImageTask(usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			DispatchID:                "dispatch-1",
			TaskIntent:                k12.ImageTaskIntentWriting,
			Status:                    k12.ImageTaskStatusRouting,
			IntentEvidence:            []string{},
			ConfirmationCandidates:    []k12.ImageTaskIntent{},
			AutomaticBudgetSeconds:    300,
			AutomaticStartedAt:        1000,
			AutomaticDeadlineAt:       1260,
			AutomaticRemainingSeconds: 260,
			Version:                   2,
			CreatedAt:                 900,
			UpdatedAt:                 1000,
		},
		ActiveInvocationDeadlineAt: 1260,
	})
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]float64{
		"automatic_budget_seconds":    300,
		"automatic_started_at":        1000,
		"automatic_deadline_at":       1260,
		"automatic_remaining_seconds": 260,
		"operation_deadline_at":       1260,
	} {
		if got[key] != want {
			t.Fatalf("%s=%v want %v; payload=%s", key, got[key], want, raw)
		}
	}
}
