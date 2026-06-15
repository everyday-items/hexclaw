package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// Exercises the Slack Events API handler: the url_verification challenge
// response, the signing-secret gate (401), malformed-body handling, and the
// event_callback dispatch path into a unified Message.

func slackSign(secret, ts string, body []byte) string {
	baseString := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(baseString))
	return "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func TestHandleEvents_URLVerificationEchoesChallenge(t *testing.T) {
	a := New(config.SlackConfig{}) // no signing secret -> gate skipped
	body := `{"type":"url_verification","challenge":"chal-xyz"}`
	w := httptest.NewRecorder()

	a.handleEvents(w, httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("url_verification = %d, want 200", w.Code)
	}
	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, w.Body.String())
	}
	if resp["challenge"] != "chal-xyz" {
		t.Fatalf("challenge = %q, want chal-xyz", resp["challenge"])
	}
}

func TestHandleEvents_RejectsBadSignature(t *testing.T) {
	a := New(config.SlackConfig{SigningSecret: "secret"})
	r := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(`{"type":"event_callback"}`))
	r.Header.Set("X-Slack-Request-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	r.Header.Set("X-Slack-Signature", "v0=bogus")
	w := httptest.NewRecorder()

	a.handleEvents(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature = %d, want 401", w.Code)
	}
}

func TestHandleEvents_BadJSONReturns400(t *testing.T) {
	a := New(config.SlackConfig{}) // gate skipped
	w := httptest.NewRecorder()
	a.handleEvents(w, httptest.NewRequest(http.MethodPost, "/events", strings.NewReader("{bad")))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed body = %d, want 400", w.Code)
	}
}

func TestHandleEvents_SignedEventCallbackDispatches(t *testing.T) {
	secret := "secret"
	a := New(config.SlackConfig{SigningSecret: secret})
	ch := make(chan *adapter.Message, 1)
	_ = a.Attach(func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		ch <- m
		return nil, nil
	})

	body := []byte(`{"type":"event_callback","event":{"type":"message","user":"U777",` +
		`"channel":"C42","text":"hi slack","ts":"1700.1","thread_ts":"1699.9"}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	r := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(string(body)))
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", slackSign(secret, ts, body))
	w := httptest.NewRecorder()

	a.handleEvents(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("signed event_callback = %d, want 200", w.Code)
	}
	select {
	case m := <-ch:
		if m.Content != "hi slack" || m.UserID != "U777" || m.ChatID != "C42" {
			t.Errorf("parsed message = %+v, want event fields to round-trip", m)
		}
		if m.Metadata["thread_ts"] != "1699.9" {
			t.Errorf("thread_ts metadata = %q, want 1699.9", m.Metadata["thread_ts"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event_callback message was not dispatched to the handler")
	}
}
