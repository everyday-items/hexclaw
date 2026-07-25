package apihttp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPublicImageTaskProjectsAmbiguousReceiptsAsRecovering(t *testing.T) {
	for _, view := range []usecase.ImageTaskView{
		{
			Dispatch: k12.ImageTaskDispatch{
				DispatchID: "dispatch-homework", TaskIntent: k12.ImageTaskIntentCompletedHomework,
				Status: k12.ImageTaskStatusRouted,
			},
			Homework: &k12.HomeworkSubmission{},
			HomeworkProjection: &usecase.ImageTaskHomeworkProjection{
				Stage: "outcome_unknown",
			},
		},
		{
			Dispatch: k12.ImageTaskDispatch{
				DispatchID: "dispatch-creative", TaskIntent: k12.ImageTaskIntentArtwork,
				Status: k12.ImageTaskStatusRouted,
			},
			Creative: &k12.CreativeWorkIntake{
				Status:   k12.CreativeWorkIntakePromoted,
				WorkType: k12.WorkTypeArt,
			},
			CreativeFeedback: "feedback_outcome_unknown",
		},
	} {
		raw, err := json.Marshal(publicImageTask(view))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "outcome_unknown") ||
			!strings.Contains(string(raw), `"state":"recovering"`) {
			t.Fatalf("internal ambiguity leaked on public wire: %s", raw)
		}
	}
}
