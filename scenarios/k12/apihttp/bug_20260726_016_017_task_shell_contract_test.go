package apihttp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPublicImageTaskUsesOnlyFrozenFooterAndFinalArtifactFacts(t *testing.T) {
	digest := strings.Repeat("a", 64)
	artifact := &k12.GradingFinalArtifact{
		ArtifactID: "final-1", ArtifactDigest: digest,
	}
	got := publicImageTask(usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			RoutingProvenance: k12.ImageTaskRoutingModelClassified,
			RoutePolicySnapshot: k12.ImageTaskRouteSnapshot{
				ProviderDisplayName: "HexClaw-GPT",
				ModelID:             "gpt-5.6-sol",
			},
		},
		Homework: &k12.HomeworkSubmission{},
		HomeworkProjection: &usecase.ImageTaskHomeworkProjection{
			FinalArtifact: artifact,
		},
	})
	if got.ProviderDisplayName == nil || *got.ProviderDisplayName != "HexClaw-GPT" {
		t.Fatalf("provider display snapshot = %#v", got.ProviderDisplayName)
	}
	if got.ModelID == nil || *got.ModelID != "gpt-5.6-sol" {
		t.Fatalf("model snapshot = %#v", got.ModelID)
	}
	projection, ok := got.TargetProjection.(imageTaskHomeworkProjectionDTO)
	if !ok || projection.FinalArtifact != artifact {
		t.Fatalf("final artifact projection = %#v", got.TargetProjection)
	}

	legacy := publicImageTask(usecase.ImageTaskView{Dispatch: k12.ImageTaskDispatch{}})
	if legacy.ProviderDisplayName != nil || legacy.ModelID != nil {
		t.Fatalf("legacy route must omit unknown display facts: %#v", legacy)
	}
	legacyJSON, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacyJSON), `"provider_display_name":null`) ||
		!strings.Contains(string(legacyJSON), `"model_id":null`) {
		t.Fatalf("legacy JSON must expose explicit nullable facts: %s", legacyJSON)
	}
}
