package apihttp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPublicImageTaskDoesNotExposeFinalAnnotatedAssetIdentity(t *testing.T) {
	artifact := &k12.GradingFinalArtifact{
		ArtifactID:               "grading-final-public-annotation",
		CanonicalMarkdown:        "# durable grading result",
		AnnotatedAssetOwnerScope: "guardian-private",
		AnnotatedAssetID:         "asset://mingming/" + strings.Repeat("a", 64) + ".png",
		AnnotatedMIME:            "image/png",
		AnnotatedDigest:          strings.Repeat("a", 64),
		OriginalSourceDigest:     strings.Repeat("b", 64),
	}
	wire := publicImageTask(usecase.ImageTaskView{
		Dispatch: k12.ImageTaskDispatch{
			DispatchID: "dispatch-public-annotation",
			TaskIntent: k12.ImageTaskIntentCompletedHomework,
			Status:     k12.ImageTaskStatusRouted,
		},
		Homework: &k12.HomeworkSubmission{},
		HomeworkProjection: &usecase.ImageTaskHomeworkProjection{
			Stage: k12.GradingStageCompleted, FinalArtifact: artifact,
		},
	})
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, private := range []string{
		"guardian-private", "asset://mingming/", "annotated_asset_id",
		"annotated_mime", "annotated_digest", "original_source_digest",
	} {
		if strings.Contains(text, private) {
			t.Fatalf("public ImageTask leaked final annotated asset fact %q: %s", private, text)
		}
	}
	if !strings.Contains(text, artifact.ArtifactID) ||
		!strings.Contains(text, artifact.CanonicalMarkdown) {
		t.Fatalf("public ImageTask removed the final artifact projection: %s", text)
	}
}
