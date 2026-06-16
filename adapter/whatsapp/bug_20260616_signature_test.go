package whatsapp

// BUG-20260616: the WhatsApp webhook POST path decoded and dispatched the body
// without verifying Meta's X-Hub-Signature-256 HMAC. Anyone who knew the URL
// could inject forged messages (impersonating any user). Every sibling adapter
// (Slack/LINE/wecom/...) verifies its signature; WhatsApp did not.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestBug20260616_WhatsAppWebhookRequiresSignature(t *testing.T) {
	a := New(Config{Name: "wa", AppSecret: "topsecret"})
	called := make(chan *adapter.Message, 1)
	if err := a.Attach(func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		called <- m
		return nil, nil
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	body := `{"entry":[{"changes":[{"value":{"messages":[{"id":"x1","from":"123","type":"text","text":{"body":"hi"}}]}}]}]}`
	req := httptest.NewRequest("POST", "/webhook/whatsapp", strings.NewReader(body))
	// No X-Hub-Signature-256 header => forged/unsigned request.
	a.handleWebhook(httptest.NewRecorder(), req)

	select {
	case m := <-called:
		t.Fatalf("unsigned webhook was processed (accepted message %q) — forgery possible", m.ID)
	case <-time.After(500 * time.Millisecond):
		// good: rejected before dispatch
	}
}

func TestBug20260616_WhatsAppValidSignatureAccepted(t *testing.T) {
	const secret = "topsecret"
	a := New(Config{Name: "wa", AppSecret: secret})
	called := make(chan *adapter.Message, 1)
	if err := a.Attach(func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		called <- m
		return nil, nil
	}); err != nil {
		t.Fatalf("attach: %v", err)
	}

	body := `{"entry":[{"changes":[{"value":{"messages":[{"id":"x2","from":"123","type":"text","text":{"body":"hi"}}]}}]}]}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	req := httptest.NewRequest("POST", "/webhook/whatsapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	a.handleWebhook(httptest.NewRecorder(), req)

	select {
	case m := <-called:
		if m.ID != "x2" {
			t.Fatalf("processed wrong message %q", m.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("a validly-signed webhook must be processed")
	}
}
