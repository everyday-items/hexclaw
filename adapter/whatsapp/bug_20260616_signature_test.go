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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestBug20260616_WhatsAppWebhookRequiresSignature(t *testing.T) {
	a := New(Config{Name: "wa", AppSecret: "topsecret", VerifyToken: "verify"})
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
	a := New(Config{Name: "wa", AppSecret: secret, VerifyToken: "verify"})
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

func TestWhatsAppWebhookRejectsEmptyAppSecretEvenWithEmptyKeySignature(t *testing.T) {
	a := New(Config{Name: "wa"})
	called := make(chan *adapter.Message, 1)
	// Bypass Attach intentionally: this test locks the HTTP handler's own
	// fail-closed guard even if it is mounted incorrectly by another caller.
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		called <- m
		return nil, nil
	}

	body := `{"entry":[{"changes":[{"value":{"messages":[{"id":"empty-key","from":"123","type":"text","text":{"body":"hi"}}]}}]}]}`
	mac := hmac.New(sha256.New, nil)
	_, _ = mac.Write([]byte(body))
	req := httptest.NewRequest("POST", "/webhook/whatsapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	a.handleWebhook(w, req)

	if w.Code != 401 {
		t.Fatalf("empty app_secret POST status = %d, want 401", w.Code)
	}
	select {
	case m := <-called:
		t.Fatalf("empty app_secret dispatched message %q", m.ID)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestWhatsAppValidateConfigRequiresAppSecretBeforeNetwork(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()

	a := New(Config{Token: "token", PhoneID: "phone", BaseURL: srv.URL})
	err := a.ValidateConfig(context.Background())
	if err == nil || !strings.Contains(err.Error(), "app_secret") {
		t.Fatalf("ValidateConfig error = %v, want missing app_secret", err)
	}
	if called {
		t.Fatal("ValidateConfig contacted Meta before checking app_secret")
	}
}

func TestWhatsAppStartRequiresAppSecret(t *testing.T) {
	a := New(Config{WebhookPort: -1})
	err := a.Start(context.Background(), func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "app_secret") {
		t.Fatalf("Start error = %v, want missing app_secret", err)
	}
}

func TestWhatsAppStopWaitsForActiveMessageHandler(t *testing.T) {
	a := New(Config{AppSecret: "secret", VerifyToken: "verify"})
	entered := make(chan struct{})
	release := make(chan struct{})
	if err := a.Attach(func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		close(entered)
		<-release
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"entry":[{"changes":[{"value":{"messages":[{"id":"active","from":"123","type":"text","text":{"body":"hi"}}]}}]}]}`
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	a.handleWebhook(httptest.NewRecorder(), req)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := a.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with active handler = %v, want deadline exceeded", err)
	}
	close(release)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after handler release: %v", err)
	}
}

func TestWhatsAppStopCancelsActiveHandlerSend(t *testing.T) {
	sendStarted := make(chan struct{}, 1)
	a := New(Config{AppSecret: "secret", VerifyToken: "verify", PhoneID: "phone", Token: "token"})
	a.client = &http.Client{Transport: whatsappRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case sendStarted <- struct{}{}:
		default:
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(500 * time.Millisecond):
			return nil, errors.New("safety timeout")
		}
	})}
	if err := a.Attach(func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "reply"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	body := `{"entry":[{"changes":[{"value":{"messages":[{"id":"send","from":"123","type":"text","text":{"body":"hi"}}]}}]}]}`
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	a.handleWebhook(httptest.NewRecorder(), req)
	select {
	case <-sendStarted:
	case <-time.After(time.Second):
		t.Fatal("handler send did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop should cancel active send, got %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("Stop took %v, active send was not cancelled", elapsed)
	}
}

type whatsappRoundTripFunc func(*http.Request) (*http.Response, error)

func (f whatsappRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestWhatsAppAttachRejectsMissingWebhookCredentials(t *testing.T) {
	handler := func(context.Context, *adapter.Message) (*adapter.Reply, error) { return nil, nil }
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "missing app secret", cfg: Config{VerifyToken: "verify"}, want: "app_secret"},
		{name: "missing verify token", cfg: Config{AppSecret: "secret"}, want: "verify_token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := New(tt.cfg).Attach(handler)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Attach error = %v, want missing %s", err, tt.want)
			}
		})
	}
}

func TestWhatsAppEmptyVerifyTokenCannotAuthenticateChallenge(t *testing.T) {
	a := New(Config{AppSecret: "secret"})
	req := httptest.NewRequest(http.MethodGet, "/webhook/whatsapp?hub.mode=subscribe&hub.challenge=x", nil)
	w := httptest.NewRecorder()
	a.handleWebhook(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("empty verify_token challenge status = %d, want 403", w.Code)
	}
}

func TestWhatsAppWebhookDuringStopIsRejectedForRetry(t *testing.T) {
	const secret = "secret"
	a := New(Config{AppSecret: secret, VerifyToken: "verify"})
	called := make(chan struct{}, 1)
	if err := a.Attach(func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		called <- struct{}{}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	body := `{"entry":[{"changes":[{"value":{"messages":[{"id":"stopping","from":"123","type":"text","text":{"body":"hi"}}]}}]}]}`
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/webhook/whatsapp", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w := httptest.NewRecorder()
	a.handleWebhook(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopping webhook status = %d, want 503 so Meta retries", w.Code)
	}
	select {
	case <-called:
		t.Fatal("stopping adapter dispatched a webhook message")
	case <-time.After(25 * time.Millisecond):
	}
}
