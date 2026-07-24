package engineadapter

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestRecognizerPreservesDefinitiveProvider400(t *testing.T) {
	upstream := &llm.ProviderError{
		Provider:   "hexclaw-gpt",
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Body:       "model does not support image inputs",
	}
	adapter := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) {
		return "", upstream
	})

	_, err := adapter.callVision(context.Background(), []byte("image"), "recognize")
	var response usecase.DefinitiveProviderResponse
	if !errors.As(err, &response) {
		t.Fatalf("recognizer lost definitive provider response: %v", err)
	}
	if response.ProviderResponseStatusCode() != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", response.ProviderResponseStatusCode())
	}
	if !errors.Is(err, upstream) {
		t.Fatalf("recognizer lost upstream error identity: %v", err)
	}
}
