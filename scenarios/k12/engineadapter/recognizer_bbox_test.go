package engineadapter

import (
	"context"
	"strings"
	"testing"
)

func TestCoreRecognizeNeverTrustsOrReturnsModelBBox(t *testing.T) {
	vision := func(context.Context, []byte, string) (string, error) {
		return `[{"question":"3.8×3=?","subject":"数学","answer_state":"present","student_answer":"10.4",` +
			`"bbox":{"x":0.12,"y":0.34,"w":0.18,"h":0.05}}]`, nil
	}
	questions, err := NewRecognizerAdapter(vision).Recognize(context.Background(), []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	if len(questions) != 1 || questions[0].BBox != nil {
		t.Fatalf("core recognition must not expose unverified geometry: %#v", questions)
	}
}

func TestCoreRecognizePromptSeparatesAnswerStateFromOptionalAnchoring(t *testing.T) {
	for _, required := range []string{"answer_state", "blank", "present", "unclear", "本阶段不输出 bbox", "后续独立批量证据阶段"} {
		if !strings.Contains(recognizePrompt, required) {
			t.Errorf("recognize prompt missing %q", required)
		}
	}
}
