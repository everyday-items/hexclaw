package line

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// Exercises the LINE webhook handler: the method guard, the HMAC-SHA256
// signature gate, malformed-body handling, and the text-event dispatch path
// that carries the reply token through to the unified Message.

func lineSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func lineMsgSink() (adapter.MessageHandler, <-chan *adapter.Message) {
	ch := make(chan *adapter.Message, 1)
	return func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		ch <- m
		return nil, nil
	}, ch
}

func TestHandleWebhook_MethodNotAllowed(t *testing.T) {
	a := New(Config{})
	w := httptest.NewRecorder()
	a.handleWebhook(w, httptest.NewRequest(http.MethodGet, "/wh", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", w.Code)
	}
}

func TestHandleWebhook_RejectsInvalidSignature(t *testing.T) {
	a := New(Config{ChannelSecret: "shhh"})
	body := `{"events":[]}`
	r := httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader(body))
	r.Header.Set("X-Line-Signature", "deadbeef")
	w := httptest.NewRecorder()

	a.handleWebhook(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("bad signature = %d, want 403", w.Code)
	}
}

func TestHandleWebhook_BadJSONReturns400(t *testing.T) {
	// No ChannelSecret configured, so the signature gate is skipped.
	a := New(Config{})
	w := httptest.NewRecorder()
	a.handleWebhook(w, httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader("{bad")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", w.Code)
	}
}

func TestHandleWebhook_ValidSignatureDispatchesText(t *testing.T) {
	secret := "shhh"
	a := New(Config{ChannelSecret: secret})
	handler, sink := lineMsgSink()
	_ = a.Attach(handler)

	body := []byte(`{"events":[{"type":"message","replyToken":"rt-9","timestamp":1700000000000,` +
		`"source":{"type":"user","userId":"U123"},"message":{"type":"text","id":"m1","text":"hi line"}}]}`)
	r := httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader(string(body)))
	r.Header.Set("X-Line-Signature", lineSign(secret, body))
	w := httptest.NewRecorder()

	a.handleWebhook(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("valid signed POST = %d, want 200", w.Code)
	}
	select {
	case m := <-sink:
		if m.Content != "hi line" || m.UserID != "U123" {
			t.Errorf("parsed message = %+v, want text/userId to round-trip", m)
		}
		if m.Metadata["reply_token"] != "rt-9" {
			t.Errorf("reply_token metadata = %q, want rt-9", m.Metadata["reply_token"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("text event was not dispatched to the handler")
	}
}

func TestHandleWebhook_IgnoresNonTextEvent(t *testing.T) {
	a := New(Config{})
	handler, sink := lineMsgSink()
	_ = a.Attach(handler)

	body := `{"events":[{"type":"message","source":{"type":"user","userId":"U1"},` +
		`"message":{"type":"sticker","id":"s1"}}]}`
	w := httptest.NewRecorder()
	a.handleWebhook(w, httptest.NewRequest(http.MethodPost, "/wh", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	select {
	case m := <-sink:
		t.Fatalf("non-text event must not dispatch, got %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}
