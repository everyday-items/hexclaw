package apihttp

import (
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestBUG20260726022CreativeWorkDTOProjectsPersistedCardTimes(t *testing.T) {
	view := usecase.CreativeWorkView{
		Record: &records.AgentRecord{
			RecordID:  "work-time-1",
			CreatedAt: 1_785_536_418,
		},
		Fields: k12.CreativeWorkFields{
			WorkType:    k12.WorkTypeWriting,
			DisplayName: "语文写作",
		},
		GenerationState: k12.CreativeWorkGenerationState{
			Latest: &k12.WorkFeedbackGeneration{
				GenerationID: "generation-time-1",
				Status:       k12.WorkFeedbackSucceeded,
				UpdatedAt:    1_785_575_743,
			},
		},
	}

	raw, err := json.Marshal(toCreativeWorkDTO(view))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["created_at"] != float64(view.Record.CreatedAt) {
		t.Fatalf("created_at = %#v, want %d", got["created_at"], view.Record.CreatedAt)
	}
	if got["latest_generation_at"] != float64(view.GenerationState.Latest.UpdatedAt) {
		t.Fatalf("latest_generation_at = %#v, want %d", got["latest_generation_at"], view.GenerationState.Latest.UpdatedAt)
	}
}
