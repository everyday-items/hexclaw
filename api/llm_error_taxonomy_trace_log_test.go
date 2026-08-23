package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

const chatTraceRawProviderBody = `{"error":{"message":"provider-body-must-not-reach-chat-trace","prompt":"prompt-must-not-reach-chat-trace","completion":"model-content-must-not-reach-chat-trace"}}`

func chatTraceProviderError() error {
	return fmt.Errorf("engine: %w", &llm.ProviderError{
		Provider:   "unit-provider",
		Action:     "chat",
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Body:       chatTraceRawProviderBody,
		RequestID:  "upstream-request-safe-42",
	})
}

func TestChatHTTPTraceLogRedactsProviderError(t *testing.T) {
	var output bytes.Buffer
	previousDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousDefault) })

	srv := &Server{
		cfg:    config.DefaultConfig(),
		engine: &mockEngine{err: chatTraceProviderError()},
	}
	w := httptest.NewRecorder()
	srv.handleChat(w, httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(`{"message":"hello"}`)))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want %d; body=%s", w.Code, http.StatusTooManyRequests, w.Body.String())
	}
	assertChatTraceLogRedacted(t, output.String())
}

func TestChatSSETraceLogRedactsProviderError(t *testing.T) {
	for _, tt := range []struct {
		name string
		eng  *sseStreamEngine
	}{
		{
			name: "startup failure",
			eng:  &sseStreamEngine{startErr: chatTraceProviderError()},
		},
		{
			name: "chunk failure",
			eng: &sseStreamEngine{chunks: []*adapter.ReplyChunk{
				{Error: chatTraceProviderError(), Done: true},
			}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&output, nil))
			ctx := trace.WithLogger(context.Background(), logger)
			w := httptest.NewRecorder()

			(&Server{}).handleChatSSE(ctx, w, &adapter.Message{}, tt.eng, time.Now())

			assertChatTraceLogRedacted(t, output.String())
		})
	}
}

func assertChatTraceLogRedacted(t *testing.T, logOutput string) {
	t.Helper()
	for _, forbidden := range []string{
		chatTraceRawProviderBody,
		"provider-body-must-not-reach-chat-trace",
		"prompt-must-not-reach-chat-trace",
		"model-content-must-not-reach-chat-trace",
	} {
		if strings.Contains(logOutput, forbidden) {
			t.Fatalf("trace log leaked provider payload %q: %s", forbidden, logOutput)
		}
	}

	for key, want := range map[string]string{
		`"error_code"`:           `"UPSTREAM_RATE_LIMITED"`,
		`"provider_status_code"`: strconv.Itoa(http.StatusTooManyRequests),
		`"provider_body_len"`:    strconv.Itoa(len(chatTraceRawProviderBody)),
		`"provider_request_id"`:  `"upstream-request-safe-42"`,
	} {
		if !strings.Contains(logOutput, key+":"+want) {
			t.Fatalf("trace log missing diagnostic %s=%s: %s", key, want, logOutput)
		}
	}
}
