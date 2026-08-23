package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
)

type llmErrorWire struct {
	Error     string `json:"error"`
	Code      string `json:"code"`
	Retryable *bool  `json:"retryable"`
	Done      bool   `json:"done"`
}

func taxonomyErrorCases() []struct {
	name          string
	err           error
	wantStatus    int
	wantCode      string
	wantRetryable bool
} {
	return []struct {
		name          string
		err           error
		wantStatus    int
		wantCode      string
		wantRetryable bool
	}{
		{
			name:          "capability mismatch",
			err:           fmt.Errorf("engine: %w", llmrouter.ErrModelCapabilityMismatch),
			wantStatus:    http.StatusUnprocessableEntity,
			wantCode:      "MODEL_CAPABILITY_MISMATCH",
			wantRetryable: false,
		},
		{
			name: "provider rate limited",
			err: fmt.Errorf("engine: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusTooManyRequests,
			}),
			wantStatus:    http.StatusTooManyRequests,
			wantCode:      "UPSTREAM_RATE_LIMITED",
			wantRetryable: true,
		},
		{
			name: "provider pool exhausted",
			err: fmt.Errorf("engine: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusServiceUnavailable,
				Body: `{"error":{"type":"pool_exhausted"}}`,
			}),
			wantStatus:    http.StatusServiceUnavailable,
			wantCode:      "UPSTREAM_POOL_EXHAUSTED",
			wantRetryable: true,
		},
		{
			name: "provider generic service unavailable",
			err: fmt.Errorf("engine: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusServiceUnavailable,
				Body: `{"error":{"type":"api_error","message":"No available accounts"}}`,
			}),
			wantStatus:    http.StatusServiceUnavailable,
			wantCode:      "UPSTREAM_UNAVAILABLE",
			wantRetryable: true,
		},
		{
			name: "provider unavailable",
			err: fmt.Errorf("engine: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusBadGateway,
			}),
			wantStatus:    http.StatusServiceUnavailable,
			wantCode:      "UPSTREAM_UNAVAILABLE",
			wantRetryable: true,
		},
	}
}

func TestHandleChatLLMErrorsUseMachineCodesAndHTTPStatuses(t *testing.T) {
	for _, tt := range taxonomyErrorCases() {
		t.Run(tt.name, func(t *testing.T) {
			eng := &mockEngine{err: tt.err}
			srv := NewServer(config.DefaultConfig(), eng, nil, nil)
			w := httptest.NewRecorder()

			srv.handleChat(w, httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(`{"message":"hello"}`)))

			if w.Code != tt.wantStatus {
				t.Fatalf("status=%d, want %d; body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			var body llmErrorWire
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Code != tt.wantCode {
				t.Fatalf("code=%q, want %q; body=%s", body.Code, tt.wantCode, w.Body.String())
			}
			if body.Retryable == nil || *body.Retryable != tt.wantRetryable {
				t.Fatalf("retryable=%v, want %t; body=%s", body.Retryable, tt.wantRetryable, w.Body.String())
			}
		})
	}
}

func TestHandleChatSSELLMErrorFramesRetainMachineCodeAfterStreamOpens(t *testing.T) {
	for _, tt := range taxonomyErrorCases() {
		t.Run(tt.name, func(t *testing.T) {
			eng := &sseStreamEngine{chunks: []*adapter.ReplyChunk{
				{Content: "partial"},
				{Error: tt.err, Done: true},
			}}
			w := httptest.NewRecorder()

			(&Server{}).handleChatSSE(context.Background(), w, &adapter.Message{}, eng, time.Now())

			if w.Code != http.StatusOK {
				t.Fatalf("opened SSE status=%d, want 200", w.Code)
			}
			frames := sseFrames(w.Body.String())
			if len(frames) != 2 {
				t.Fatalf("frames=%d, want content plus error; body=%q", len(frames), w.Body.String())
			}
			var frame llmErrorWire
			if err := json.Unmarshal([]byte(frames[1]), &frame); err != nil {
				t.Fatalf("decode error frame: %v; frame=%s", err, frames[1])
			}
			if !frame.Done || frame.Code != tt.wantCode {
				t.Fatalf("frame=%+v, want done=true code=%q", frame, tt.wantCode)
			}
			if frame.Retryable == nil || *frame.Retryable != tt.wantRetryable {
				t.Fatalf("retryable=%v, want %t; frame=%s", frame.Retryable, tt.wantRetryable, frames[1])
			}
		})
	}
}

func TestLLMErrorPayloadDoesNotExposeRawProviderBody(t *testing.T) {
	err := fmt.Errorf("engine: %w", &llm.ProviderError{
		Provider:   "test",
		StatusCode: http.StatusServiceUnavailable,
		Status:     "503 Service Unavailable",
		Body:       `{"error":{"type":"pool_exhausted","message":"Temporarily unavailable","internal_marker":"must-not-leak"}}`,
	})

	payload, marshalErr := json.Marshal(llmErrorPayload(err))
	if marshalErr != nil {
		t.Fatalf("marshal payload: %v", marshalErr)
	}
	if strings.Contains(string(payload), "internal_marker") || strings.Contains(string(payload), "must-not-leak") {
		t.Fatalf("raw provider body leaked: %s", payload)
	}
}
