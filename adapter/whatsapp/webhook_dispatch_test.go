package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

const waTestAppSecret = "test-app-secret"

func signedWARequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	mac := hmac.New(sha256.New, []byte(waTestAppSecret))
	_, _ = mac.Write([]byte(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

// Exercises the WhatsApp webhook handler: the GET verify-token handshake (the
// platform's only inbound auth gate), method/parse guards, and the POST text
// dispatch path that parses the Cloud API payload into a unified Message.

func waMsgSink(t *testing.T) (adapter.MessageHandler, <-chan *adapter.Message) {
	t.Helper()
	ch := make(chan *adapter.Message, 1)
	return func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		ch <- m
		return nil, nil
	}, ch
}

func TestHandleWebhook_VerifyChallengeEcho(t *testing.T) {
	a := New(Config{VerifyToken: "tok-123"})
	r := httptest.NewRequest(http.MethodGet,
		"/wh?hub.mode=subscribe&hub.verify_token=tok-123&hub.challenge=ping42", nil)
	w := httptest.NewRecorder()

	a.handleWebhook(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("valid verify handshake = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "ping42" {
		t.Fatalf("challenge echo = %q, want %q", got, "ping42")
	}
}

func TestHandleWebhook_VerifyRejectsWrongTokenAndMode(t *testing.T) {
	a := New(Config{VerifyToken: "tok-123"})

	cases := map[string]string{
		"wrong token": "/wh?hub.mode=subscribe&hub.verify_token=nope&hub.challenge=x",
		"wrong mode":  "/wh?hub.mode=unsubscribe&hub.verify_token=tok-123&hub.challenge=x",
	}
	for name, url := range cases {
		w := httptest.NewRecorder()
		a.handleWebhook(w, httptest.NewRequest(http.MethodGet, url, nil))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", name, w.Code)
		}
	}
}

func TestHandleWebhook_MethodNotAllowed(t *testing.T) {
	a := New(Config{})
	w := httptest.NewRecorder()
	a.handleWebhook(w, httptest.NewRequest(http.MethodPut, "/wh", strings.NewReader("{}")))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT = %d, want 405", w.Code)
	}
}

func TestHandleWebhook_BadJSONReturns400(t *testing.T) {
	a := New(Config{AppSecret: waTestAppSecret, VerifyToken: "verify"})
	w := httptest.NewRecorder()
	a.handleWebhook(w, signedWARequest(http.MethodPost, "/wh", "{not json"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed POST = %d, want 400", w.Code)
	}
}

func TestHandleWebhook_DispatchesTextMessageWithContactName(t *testing.T) {
	a := New(Config{AppSecret: waTestAppSecret, VerifyToken: "verify"})
	handler, sink := waMsgSink(t)
	_ = a.Attach(handler)

	body := `{"entry":[{"id":"e1","changes":[{"value":{
		"contacts":[{"wa_id":"15551234567","profile":{"name":"Alice"}}],
		"messages":[{"id":"wamid.1","from":"15551234567","type":"text","text":{"body":"hello there"}}]
	}}]}]}`
	w := httptest.NewRecorder()
	a.handleWebhook(w, signedWARequest(http.MethodPost, "/wh", body))

	if w.Code != http.StatusOK {
		t.Fatalf("valid POST = %d, want 200", w.Code)
	}
	select {
	case m := <-sink:
		if m.Content != "hello there" || m.UserID != "15551234567" {
			t.Errorf("parsed message = %+v, want content/from to round-trip", m)
		}
		if m.UserName != "Alice" {
			t.Errorf("contact name = %q, want resolved from contacts list", m.UserName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("text message was not dispatched to the handler")
	}
}

func TestHandleWebhook_IgnoresNonTextMessage(t *testing.T) {
	a := New(Config{AppSecret: waTestAppSecret, VerifyToken: "verify"})
	handler, sink := waMsgSink(t)
	_ = a.Attach(handler)

	body := `{"entry":[{"id":"e1","changes":[{"value":{
		"messages":[{"id":"wamid.2","from":"15550000000","type":"image"}]
	}}]}]}`
	w := httptest.NewRecorder()
	a.handleWebhook(w, signedWARequest(http.MethodPost, "/wh", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	select {
	case m := <-sink:
		t.Fatalf("non-text message must not dispatch, got %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}
