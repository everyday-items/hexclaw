package web

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"nhooyr.io/websocket/wsjson"
)

func TestWebAdapterLLMStreamErrorFramesCarryMachineCode(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantCode      string
		wantRetryable bool
	}{
		{
			name:          "capability mismatch",
			err:           fmt.Errorf("stream: %w", llmrouter.ErrModelCapabilityMismatch),
			wantCode:      "MODEL_CAPABILITY_MISMATCH",
			wantRetryable: false,
		},
		{
			name: "rate limited",
			err: fmt.Errorf("stream: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusTooManyRequests,
			}),
			wantCode:      "UPSTREAM_RATE_LIMITED",
			wantRetryable: true,
		},
		{
			name: "pool exhausted",
			err: fmt.Errorf("stream: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusServiceUnavailable,
				Body: `{"error":{"code":"pool_exhausted"}}`,
			}),
			wantCode:      "UPSTREAM_POOL_EXHAUSTED",
			wantRetryable: true,
		},
		{
			name: "generic service unavailable",
			err: fmt.Errorf("stream: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusServiceUnavailable,
				Body: `{"error":{"type":"api_error","message":"No available accounts"}}`,
			}),
			wantCode:      "UPSTREAM_UNAVAILABLE",
			wantRetryable: true,
		},
		{
			name: "unavailable",
			err: fmt.Errorf("stream: %w", &llm.ProviderError{
				Provider: "test", StatusCode: http.StatusBadGateway,
			}),
			wantCode:      "UPSTREAM_UNAVAILABLE",
			wantRetryable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := New()
			chunks := make(chan *adapter.ReplyChunk, 1)
			a.SetStreamHandler(func(context.Context, *adapter.Message) (<-chan *adapter.ReplyChunk, error) {
				return chunks, nil
			})
			conn, ctx, _ := dialWebAdapter(t, a)
			if err := wsjson.Write(ctx, conn, wsMessage{
				Type: "message", Content: "hello", SessionID: "session", RequestID: "request",
			}); err != nil {
				t.Fatalf("write request: %v", err)
			}

			chunks <- &adapter.ReplyChunk{Error: tt.err, Done: true}
			close(chunks)

			var frame wsMessage
			if err := wsjson.Read(ctx, conn, &frame); err != nil {
				t.Fatalf("read error frame: %v", err)
			}
			if frame.Type != "error" || frame.Code != tt.wantCode {
				t.Fatalf("frame=%+v, want error code %q", frame, tt.wantCode)
			}
			if frame.Retryable == nil || *frame.Retryable != tt.wantRetryable {
				t.Fatalf("retryable=%v, want %t; frame=%+v", frame.Retryable, tt.wantRetryable, frame)
			}
		})
	}
}
