package engineadapter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPracticeVariantAdapterNormalizesOutputAndPreservesOutcomeSemantics(t *testing.T) {
	t.Run("normalizes one bounded generation response", func(t *testing.T) {
		adapter := NewPracticeVariantAdapter(func(
			_ context.Context, subject, prompt, grade string,
		) (string, error) {
			if subject != "数学" || prompt == "" || grade != "五年级下" {
				t.Fatalf("unexpected request: subject=%q prompt=%q grade=%q", subject, prompt, grade)
			}
			return "问题：2+3=?\n解答：相加\n答案：5\n\n```hexclaw-subagents\n[]\n```", nil
		})

		got, err := adapter.GeneratePracticeVariant(
			context.Background(), "数学", "请出一道题", "五年级下",
		)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got.Solution, "## 问题") ||
			!strings.Contains(got.Solution, "## 解答") ||
			!strings.Contains(got.Solution, "## 答案") ||
			strings.Contains(got.Solution, "hexclaw-subagents") {
			t.Fatalf("unexpected normalized output: %q", got.Solution)
		}
		if got.Evidence.Verdict != usecase.VerdictUnverifiable ||
			got.Evidence.EvidenceType != usecase.EvidenceNone {
			t.Fatalf("unexpected evidence: %+v", got.Evidence)
		}
	})

	for _, tc := range []struct {
		name       string
		provider   error
		definitive bool
	}{
		{
			name: "definitive provider response",
			provider: &llm.ProviderError{
				Provider: "test", StatusCode: 503, Status: "503 Service Unavailable",
			},
			definitive: true,
		},
		{
			name: "ambiguous transport failure",
			provider: &llm.ProviderError{
				Provider: "test", Cause: errors.New("connection reset"),
			},
			definitive: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := NewPracticeVariantAdapter(func(
				context.Context, string, string, string,
			) (string, error) {
				return "", tc.provider
			})
			_, err := adapter.GeneratePracticeVariant(
				context.Background(), "数学", "请出一道题", "五年级下",
			)
			var response usecase.DefinitiveProviderResponse
			if got := errors.As(err, &response); got != tc.definitive {
				t.Fatalf("definitive=%v, want %v, err=%v", got, tc.definitive, err)
			}
			if !errors.Is(err, tc.provider) {
				t.Fatalf("adapter lost original error identity: %v", err)
			}
		})
	}
}
