package apihttp

import (
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// BUG_20260726_031: a non-terminal homework dispatch must expose the durable
// per-problem snapshot. Clients must not wait for the page-level completed
// result or invent progressive state from local completion order.
func TestBUG_20260726_031_PublicImageTaskExposesProgressiveProblemSnapshot(t *testing.T) {
	view := usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			DispatchID: "dispatch-progressive-red",
			TaskIntent: k12.ImageTaskIntentCompletedHomework,
			Status:     k12.ImageTaskStatusRouted,
		},
		Homework: &k12.HomeworkSubmission{},
		HomeworkProjection: &usecase.ImageTaskHomeworkProjection{
			Stage:             "assessing",
			ConfirmationState: "confirmed",
			AnchorState:       "located",
		},
	}

	raw, err := json.Marshal(publicImageTask(view))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	projection, ok := payload["target_projection"].(map[string]any)
	if !ok {
		t.Fatalf("BUG_20260726_031 target_projection missing from %s", raw)
	}
	progressive, ok := projection["progressive"].(map[string]any)
	if !ok {
		t.Fatalf(
			"BUG_20260726_031 homework projection has no recoverable progressive snapshot: %s",
			raw,
		)
	}

	for _, key := range []string{
		"structure_version",
		"snapshot_revision",
		"problem_progress",
		"coverage",
	} {
		if _, exists := progressive[key]; !exists {
			t.Errorf("BUG_20260726_031 progressive.%s missing: %s", key, raw)
		}
	}
	coverage, ok := progressive["coverage"].(map[string]any)
	if !ok {
		t.Fatalf("BUG_20260726_031 progressive.coverage must be an object: %s", raw)
	}
	for _, key := range []string{
		"total",
		"published",
		"skipped",
		"awaiting",
		"failed",
		"status",
		"projection_revision",
	} {
		if _, exists := coverage[key]; !exists {
			t.Errorf("BUG_20260726_031 coverage.%s missing: %s", key, raw)
		}
	}
}
