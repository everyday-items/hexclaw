package apihttp

import (
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// K12-FACADE-PROGRESSIVE-SOURCE-EXACTSET-001: ImageTask progress does not
// carry source facts. Its public wire must therefore omit the whole optional
// source exact-set rather than serializing one null member from the broader
// source-action DTO.
func TestBUG20260803PublicImageTaskProgressiveOmitsPartialSourceFacts(t *testing.T) {
	projection := publicImageTaskProgressive(usecase.ImageTaskProgressiveSnapshot{
		StructureVersion: 1,
		SnapshotRevision: 7,
		ProblemProgress: []usecase.ImageTaskProblemProgress{{
			ProblemID:          "problem-core-only",
			Status:             "processing",
			InputRevision:      1,
			PublishedRevision:  0,
			CurrentDisposition: "current",
		}},
		Coverage: usecase.ImageTaskProgressiveCoverage{
			Total:              1,
			Published:          0,
			Skipped:            0,
			Awaiting:           1,
			Failed:             0,
			Status:             "in_progress",
			ProjectionRevision: 7,
		},
	})
	raw, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		ProblemProgress []map[string]json.RawMessage `json:"problem_progress"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.ProblemProgress) != 1 {
		t.Fatalf("progress count=%d, want 1", len(decoded.ProblemProgress))
	}
	want := map[string]struct{}{
		"problem_id":          {},
		"status":              {},
		"input_revision":      {},
		"published_revision":  {},
		"current_disposition": {},
	}
	for key := range decoded.ProblemProgress[0] {
		if _, ok := want[key]; !ok {
			t.Fatalf(
				"core-only ImageTask progress leaked partial source fact %q; public wire must emit either all source facts or none",
				key,
			)
		}
	}
	if len(decoded.ProblemProgress[0]) != len(want) {
		t.Fatalf("core-only ImageTask progress keys=%d, want %d", len(decoded.ProblemProgress[0]), len(want))
	}
}
