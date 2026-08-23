package api

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon/observe/trace"
)

func TestModelLogRedactionCollectorDropsPayloadFieldsAndKeepsLengths(t *testing.T) {
	const (
		contentSecret   = "model-content-must-not-be-stored"
		reasoningSecret = "model-reasoning-must-not-be-stored"
		sampleSecret    = "memory-sample-must-not-be-stored"
		rawHeadSecret   = "compile-raw-head-must-not-be-stored"
		scriptSecret    = "compile-script-head-must-not-be-stored"
	)

	collector := NewLogCollector(4)
	collector.Add("info", "llm", "model response complete", map[string]any{
		"content":           contentSecret,
		"reasoning":         reasoningSecret,
		"sample":            sampleSecret,
		"raw_head":          rawHeadSecret,
		"script_head":       scriptSecret,
		"response.content":  "nested-" + contentSecret,
		"provider_response": "http_429",
		"status":            "failed",
		"request_id":        "req-safe-42",
	})

	entries, total := collector.Query("", "", "", 10, 0)
	if total != 1 || len(entries) != 1 {
		t.Fatalf("entries = %d/%d, want 1/1", len(entries), total)
	}
	encoded, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatalf("marshal log entry: %v", err)
	}
	for _, secret := range []string{contentSecret, reasoningSecret, sampleSecret, rawHeadSecret, scriptSecret, "nested-" + contentSecret} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("collector retained model payload %q: %s", secret, encoded)
		}
	}

	for _, key := range []string{"content", "reasoning", "sample", "raw_head", "script_head", "response.content"} {
		if _, ok := entries[0].Fields[key]; ok {
			t.Fatalf("collector retained forbidden field %q: %#v", key, entries[0].Fields)
		}
	}
	for key, want := range map[string]int{
		"content_len":          len(contentSecret),
		"reasoning_len":        len(reasoningSecret),
		"sample_len":           len(sampleSecret),
		"raw_head_len":         len(rawHeadSecret),
		"script_head_len":      len(scriptSecret),
		"response.content_len": len("nested-" + contentSecret),
	} {
		if got, ok := entries[0].Fields[key].(int); !ok || got != want {
			t.Fatalf("%s = %#v, want %d", key, entries[0].Fields[key], want)
		}
	}
	if got := entries[0].Fields["status"]; got != "failed" {
		t.Fatalf("status = %#v, want failed", got)
	}
	if got := entries[0].Fields["request_id"]; got != "req-safe-42" {
		t.Fatalf("request_id = %#v, want req-safe-42", got)
	}
	if got := entries[0].Fields["provider_response"]; got != "http_429" {
		t.Fatalf("provider_response = %#v, want http_429", got)
	}
}

func TestModelLogRedactionCollectorSanitizesProviderErrorBody(t *testing.T) {
	const rawBody = `{"error":{"message":"provider-body-must-not-be-stored","type":"insufficient_quota"},"request_id":"upstream-req-42"}`
	providerErr := &llm.ProviderError{
		Provider:   "unit-provider",
		Action:     "chat",
		StatusCode: 429,
		Status:     "429 Too Many Requests",
		Body:       rawBody,
		RequestID:  "upstream-req-42",
		RetryAfter: time.Second,
	}

	collector := NewLogCollector(4)
	collector.Add("warn", "llm", "provider request failed", map[string]any{"err": providerErr})

	entries, total := collector.Query("", "", "", 10, 0)
	if total != 1 || len(entries) != 1 {
		t.Fatalf("entries = %d/%d, want 1/1", len(entries), total)
	}
	encoded, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatalf("marshal log entry: %v", err)
	}
	if strings.Contains(string(encoded), "provider-body-must-not-be-stored") || strings.Contains(string(encoded), rawBody) {
		t.Fatalf("collector retained provider response body: %s", encoded)
	}
	if got := entries[0].Fields["err"]; got != "provider_http_429" {
		t.Fatalf("err = %#v, want provider_http_429", got)
	}
	if got := entries[0].Fields["provider_status_code"]; got != 429 {
		t.Fatalf("provider_status_code = %#v, want 429", got)
	}
	if got := entries[0].Fields["provider_request_id"]; got != "upstream-req-42" {
		t.Fatalf("provider_request_id = %#v, want upstream-req-42", got)
	}
	if got := entries[0].Fields["provider_body_len"]; got != len(rawBody) {
		t.Fatalf("provider_body_len = %#v, want %d", got, len(rawBody))
	}
}

func TestModelLogRedactionCollectorSanitizesProviderBodyAfterSlogSerialization(t *testing.T) {
	const rawBody = `{"error":{"message":"provider-body-must-not-survive-trace-handler"},"request_id":"upstream-req-43"}`
	providerErr := &llm.ProviderError{
		Provider:   "unit-provider",
		Action:     "chat",
		StatusCode: 429,
		Status:     "429 Too Many Requests",
		Body:       rawBody,
	}
	collector := NewLogCollector(4)
	logger := slog.New(trace.NewCollectorHandler(collector, slog.LevelInfo))
	logger.Warn("provider request failed", "err", providerErr)

	entries, total := collector.Query("", "", "", 10, 0)
	if total != 1 || len(entries) != 1 {
		t.Fatalf("entries = %d/%d, want 1/1", len(entries), total)
	}
	encoded, err := json.Marshal(entries[0])
	if err != nil {
		t.Fatalf("marshal log entry: %v", err)
	}
	if strings.Contains(string(encoded), "provider-body-must-not-survive-trace-handler") || strings.Contains(string(encoded), rawBody) {
		t.Fatalf("collector retained trace-handler provider body: %s", encoded)
	}
	if got := entries[0].Fields["provider_request_id"]; got != "upstream-req-43" {
		t.Fatalf("provider_request_id = %#v, want upstream-req-43", got)
	}
	if got := entries[0].Fields["provider_body_len"]; got != len(rawBody) {
		t.Fatalf("provider_body_len = %#v, want %d", got, len(rawBody))
	}
}
