package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/llm/openai"
)

func TestK12NonIdempotentLLMContextUnexpectedEOFPhysicalPOSTOnce(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[`))
	}))
	defer srv.Close()
	provider := openai.New("test", openai.WithBaseURL(srv.URL))

	_, err := provider.Complete(
		k12NonIdempotentLLMContext(context.Background()),
		llm.CompletionRequest{
			Messages: []llm.Message{llm.UserMessage("classify this worksheet")},
		},
	)
	if err == nil {
		t.Fatal("expected truncated-response error")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("physical POST attempts = %d, want 1", calls.Load())
	}
}
