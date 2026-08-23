package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon/observe/trace"
)

func TestModelLogRedactionReplyLogKeepsLengthsWithoutPayload(t *testing.T) {
	const (
		contentSecret   = "assistant-content-must-not-be-logged"
		reasoningSecret = "assistant-reasoning-must-not-be-logged"
	)

	var output bytes.Buffer
	ctx := trace.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&output, nil)))
	logModelReply(ctx, "stream", "session-safe-42", "provider-safe", "model-safe", contentSecret, reasoningSecret, 2, false)

	logged := output.String()
	if strings.Contains(logged, contentSecret) || strings.Contains(logged, reasoningSecret) {
		t.Fatalf("reply log retained model payload: %s", logged)
	}
	if !strings.Contains(logged, `"content_len":`) || !strings.Contains(logged, `"reasoning_len":`) {
		t.Fatalf("reply log omitted diagnostic lengths: %s", logged)
	}
	if !strings.Contains(logged, `"session":"session-safe-42"`) || !strings.Contains(logged, `"model":"model-safe"`) {
		t.Fatalf("reply log omitted routing diagnostics: %s", logged)
	}
}

func TestModelLogRedactionProviderErrorLogFieldsKeepDiagnosticsWithoutBody(t *testing.T) {
	const rawBody = `{"error":{"message":"provider-body-must-not-reach-slog"}}`
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	providerErr := &llm.ProviderError{
		Provider:   "unit-provider",
		Action:     "chat",
		StatusCode: 429,
		Status:     "429 Too Many Requests",
		Body:       rawBody,
		RequestID:  "upstream-req-42",
		RetryAfter: time.Second,
	}
	logger.Warn("provider failed", appendModelErrorLogFields([]any{"session", "session-safe-42"}, providerErr)...)

	logged := output.String()
	if strings.Contains(logged, "provider-body-must-not-reach-slog") || strings.Contains(logged, rawBody) {
		t.Fatalf("provider log retained response body: %s", logged)
	}
	for _, want := range []string{
		`"err":"provider_http_429"`,
		`"provider_status_code":429`,
		`"provider_request_id":"upstream-req-42"`,
		`"provider_body_len":`,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("provider log omitted %s: %s", want, logged)
		}
	}
}
