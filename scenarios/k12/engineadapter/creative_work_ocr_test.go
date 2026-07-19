package engineadapter

import (
	"context"
	"strings"
	"testing"
)

func TestCreativeWorkOCRAdapterUsesVisionPrimitiveAndParsesVerbatimJSON(t *testing.T) {
	var prompt string
	adapter := NewCreativeWorkOCRAdapter(func(_ context.Context, image []byte, gotPrompt string) (string, error) {
		if string(image) != "real-image" {
			t.Fatalf("image bytes changed: %q", image)
		}
		prompt = gotPrompt
		return "```json\n{\"text\":\"  第一段\\n柳枝象绿色丝带。\\n\"}\n```", nil
	})
	raw, err := adapter.RecognizeWriting(context.Background(), []byte("real-image"))
	if err != nil {
		t.Fatal(err)
	}
	if raw != "  第一段\n柳枝象绿色丝带。\n" {
		t.Fatalf("raw OCR must remain verbatim, got %q", raw)
	}
	if !strings.Contains(prompt, "逐字") || !strings.Contains(prompt, "不要润色") {
		t.Fatalf("writing OCR prompt missing verbatim constraints: %q", prompt)
	}
}

func TestCreativeWorkOCRAdapterRejectsEmptyOrNonJSONModelOutput(t *testing.T) {
	for _, output := range []string{"", "这是一篇作文"} {
		adapter := NewCreativeWorkOCRAdapter(func(context.Context, []byte, string) (string, error) {
			return output, nil
		})
		if _, err := adapter.RecognizeWriting(context.Background(), []byte("image")); err == nil {
			t.Fatalf("output %q must fail closed", output)
		}
	}
}
