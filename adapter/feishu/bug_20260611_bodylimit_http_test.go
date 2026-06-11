package feishu

// BUG-20260611 (review Low L4 follow-up): the adapter-level source scan only
// proves a MaxBytesReader call exists; this test exercises the real HTTP
// behavior — an oversized webhook body must yield 413 while a legitimate
// small payload (the Feishu URL-verification challenge) still parses.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestBug20260611_WebhookBodyLimitHTTPBehavior(t *testing.T) {
	a := New(config.FeishuConfig{})

	t.Run("oversized body returns 413", func(t *testing.T) {
		big := bytes.Repeat([]byte("a"), 1<<20+1) // 1 MiB + 1 byte, just over the cap
		req := httptest.NewRequest(http.MethodPost, "/webhook/feishu", bytes.NewReader(big))
		rec := httptest.NewRecorder()

		a.handleWebhook(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("[BUG-20260611] oversized webhook body: got status %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
		}
	})

	t.Run("small payload still parses", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/webhook/feishu",
			bytes.NewReader([]byte(`{"challenge":"chal-20260611"}`)))
		rec := httptest.NewRecorder()

		a.handleWebhook(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("small webhook payload: got status %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
		}
		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("challenge response is not JSON: %v", err)
		}
		if resp["challenge"] != "chal-20260611" {
			t.Errorf("challenge echo: got %q, want %q", resp["challenge"], "chal-20260611")
		}
	})
}
