package dingtalk_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	"github.com/hexagon-codes/hexclaw/webhook"

	_ "modernc.org/sqlite"
)

func newDingTalkK12WebhookFixture(t *testing.T) (*webhook.Manager, string, time.Time, *atomic.Int32) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(migrate.K12WebhooksV18DDL); err != nil {
		t.Fatal(err)
	}
	mgr := webhook.NewManager(db)
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	mgr.SetK12Clock(func() time.Time { return now })
	_, secret, err := mgr.CreateK12Binding(context.Background(), webhook.K12BindingInput{
		Name: "dingtalk-boundary", AgentID: "kid-a", LearnerID: "learner-a",
		AllowedEvents: []webhook.K12EventType{webhook.K12EventSubmissionRequested},
		CreatedBy:     "parent", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	mgr.SetK12Handler(func(context.Context, webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
		calls.Add(1)
		return webhook.K12DispatchResult{Status: webhook.K12ReceiptSucceeded}, nil
	})
	return mgr, secret, now, calls
}

func TestWebhookK12DingTalkSignatureCannotAuthenticateK12Binding(t *testing.T) {
	mgr, _, now, calls := newDingTalkK12WebhookFixture(t)
	body := []byte(`{"event_id":"dt-1","event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/dingtalk-boundary", bytes.NewReader(body))
	req.SetPathValue("name", "dingtalk-boundary")
	req.Header.Set("Content-Type", "application/json")
	// A provider callback signature is not interchangeable with the dedicated
	// K12 timestamp+nonce+raw-body signature protocol.
	req.Header.Set("X-DingTalk-Signature", "provider-signature")
	req.Header.Set("timestamp", now.Format(time.RFC3339))
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("DingTalk signature authenticated K12 binding: %d %s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("foreign signature caused a K12 domain dispatch")
	}
}

func TestWebhookGroupMetadataRejectedBeforeK12Dispatch(t *testing.T) {
	mgr, secret, now, calls := newDingTalkK12WebhookFixture(t)
	body := []byte(`{"event_id":"dt-group","event_type":"k12.submission.requested.v1","payload":{"text":"x","conversation_scope":"group"}}`)
	timestamp := now.Format(time.RFC3339)
	nonce := "nonce-group"
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/dingtalk-boundary", bytes.NewReader(body))
	req.SetPathValue("name", "dingtalk-boundary")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.K12HeaderTimestamp, timestamp)
	req.Header.Set(webhook.K12HeaderNonce, nonce)
	req.Header.Set(webhook.K12HeaderSignature, webhook.K12Signature(secret, timestamp, nonce, body))
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("group metadata status=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("group event reached K12 domain dispatch")
	}
}
