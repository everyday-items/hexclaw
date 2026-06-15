package whatsapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// Covers the WhatsApp Cloud API send path (sendReplyNow) end to end against a
// stub Graph endpoint: request shaping, the <think> strip, and error mapping.

func TestSendReplyNow_PostsStrippedTextAndSucceeds(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"messages":[{"id":"wamid.out"}]}`))
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL, PhoneID: "PHONE1", Token: "tok-secret"})
	err := a.sendReplyNow(context.Background(), "15551230000",
		&adapter.Reply{Content: "<think>secret reasoning</think>visible answer"})
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}

	if gotPath != "/PHONE1/messages" {
		t.Errorf("request path = %q, want /PHONE1/messages", gotPath)
	}
	if gotAuth != "Bearer tok-secret" {
		t.Errorf("auth header = %q, want bearer token", gotAuth)
	}

	var payload struct {
		MessagingProduct string `json:"messaging_product"`
		To               string `json:"to"`
		Text             struct {
			Body string `json:"body"`
		} `json:"text"`
	}
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("request body not JSON: %v (%q)", err, gotBody)
	}
	if payload.MessagingProduct != "whatsapp" || payload.To != "15551230000" {
		t.Errorf("payload = %+v, want messaging_product/to populated", payload)
	}
	if payload.Text.Body != "visible answer" {
		t.Errorf("body = %q, want <think> stripped to %q", payload.Text.Body, "visible answer")
	}
	if strings.Contains(gotBody, "secret reasoning") {
		t.Error("reasoning must not leak to the WhatsApp API")
	}
}

func TestSendReplyNow_NonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad phone"}}`))
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL, PhoneID: "PHONE1", Token: "tok"})
	err := a.sendReplyNow(context.Background(), "15551230000", &adapter.Reply{Content: "hi"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("non-200 API response must error with the status, got: %v", err)
	}
}

func TestSendReplyNow_NilReplyIsNoOp(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL, PhoneID: "PHONE1", Token: "tok"})
	if err := a.sendReplyNow(context.Background(), "15551230000", nil); err != nil {
		t.Fatalf("nil reply must be a no-op, got: %v", err)
	}
	if called {
		t.Error("nil reply must not issue an HTTP request")
	}
}
