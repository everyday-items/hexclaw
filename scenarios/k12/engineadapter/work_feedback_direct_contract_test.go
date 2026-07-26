package engineadapter

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestArtFeedbackPromptWithoutTaskOrIntentRequiresDirectCompleteCritique(t *testing.T) {
	subject, prompt, _, err := buildWorkFeedbackPrompt(usecase.WorkFeedbackRequest{
		WorkType:      k12.WorkTypeArt,
		SourceAssetID: "asset://mingming/art.png",
		Grade:         "五年级下",
	}, nil)
	if err != nil {
		t.Fatalf("build art feedback prompt: %v", err)
	}
	if subject != "美术" {
		t.Fatalf("subject=%q, want 美术", subject)
	}
	if strings.Contains(prompt, "intent 缺失时必须先问") {
		t.Fatalf("art prompt still gates critique on missing intent:\n%s", prompt)
	}
	for _, required := range []string{
		"不得先追问",
		"直接完成",
		"只依据画面可见证据",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("art prompt missing direct-critique contract %q:\n%s", required, prompt)
		}
	}
}
