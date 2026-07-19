package webhook

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

var k12WebhookTestNow = time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

func newK12WebhookTestManager(t *testing.T, db *sql.DB) *Manager {
	t.Helper()
	if db == nil {
		var err error
		db, err = sql.Open("sqlite", ":memory:")
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
	}
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatalf("init numbered K12 webhook migrations: %v", err)
	}
	mgr := NewManager(db)
	if err := mgr.Init(context.Background()); err != nil {
		t.Fatalf("init manager: %v", err)
	}
	mgr.SetK12Clock(func() time.Time { return k12WebhookTestNow })
	return mgr
}

func createEnabledK12Binding(t *testing.T, mgr *Manager, name string) (*K12Binding, string) {
	t.Helper()
	binding, secret, err := mgr.CreateK12Binding(context.Background(), K12BindingInput{
		Name:          name,
		AgentID:       "agent-mingming",
		LearnerID:     "learner-mingming",
		AllowedEvents: []K12EventType{K12EventSubmissionRequested},
		CreatedBy:     "parent-1",
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("create binding: %v", err)
	}
	if secret == "" {
		t.Fatal("create must return the one-time secret")
	}
	return binding, secret
}

func signedK12Request(t *testing.T, name, secret, timestamp, nonce string, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+name, bytes.NewReader(body))
	req.SetPathValue("name", name)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(K12HeaderTimestamp, timestamp)
	req.Header.Set(K12HeaderNonce, nonce)
	req.Header.Set(K12HeaderSignature, K12Signature(secret, timestamp, nonce, body))
	return req
}

func serveK12Request(mgr *Manager, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mgr.Handler().ServeHTTP(rec, req)
	return rec
}

func decodeK12ReceiptResponse(t *testing.T, rec *httptest.ResponseRecorder) K12Receipt {
	t.Helper()
	var out struct {
		Receipt K12Receipt `json:"receipt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	if out.Receipt.ReceiptID == "" {
		t.Fatalf("response does not contain receipt: %s", rec.Body.String())
	}
	return out.Receipt
}

func waitK12ReceiptStatus(t *testing.T, mgr *Manager, receiptID string, want K12ReceiptStatus) K12Receipt {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := mgr.GetK12Receipt(context.Background(), receiptID)
		if err == nil && receipt.Status == want {
			return receipt
		}
		time.Sleep(5 * time.Millisecond)
	}
	receipt, err := mgr.GetK12Receipt(context.Background(), receiptID)
	t.Fatalf("receipt status=%q err=%v, want %q", receipt.Status, err, want)
	return K12Receipt{}
}

func TestK12WebhookBindingSchemaAndOneTimeSecret(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	binding, secret := createEnabledK12Binding(t, mgr, "homework")

	if binding.BindingID == "" || binding.AgentID != "agent-mingming" || binding.LearnerID != "learner-mingming" {
		t.Fatalf("binding owner/schema incomplete: %+v", binding)
	}
	if binding.Scope != K12ScopeDirect || binding.SecretVersion != 1 || binding.Status != K12BindingEnabled {
		t.Fatalf("binding security fields incomplete: %+v", binding)
	}
	if len(binding.AllowedEvents) != 1 || binding.AllowedEvents[0] != K12EventSubmissionRequested {
		t.Fatalf("allowed events = %v", binding.AllowedEvents)
	}
	if binding.Secret != "" || !binding.HasSecret || secret == "" {
		t.Fatalf("secret exposure contract violated: binding=%+v secret=%q", binding, secret)
	}

	listed, err := mgr.ListK12Bindings(context.Background(), "parent-1")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list bindings=%+v err=%v", listed, err)
	}
	if listed[0].Secret != "" || !listed[0].HasSecret {
		t.Fatalf("list must never reveal secret: %+v", listed[0])
	}
}

func TestK12WebhookSignedEventUsesBindingOwnerAndReceiptLifecycle(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	_, secret := createEnabledK12Binding(t, mgr, "homework")

	var calls atomic.Int32
	got := make(chan K12Dispatch, 1)
	mgr.SetK12Handler(func(_ context.Context, event K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		got <- event
		return K12DispatchResult{Reference: "job-001", Status: K12ReceiptSucceeded}, nil
	})

	body := []byte(`{"event_id":"event-001","event_type":"k12.submission.requested.v1","payload":{"text":"请批改第 1 题"}}`)
	timestamp := k12WebhookTestNow.Format(time.RFC3339)
	rec := serveK12Request(mgr, signedK12Request(t, "homework", secret, timestamp, "nonce-001", body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	receipt := decodeK12ReceiptResponse(t, rec)
	terminal := waitK12ReceiptStatus(t, mgr, receipt.ReceiptID, K12ReceiptSucceeded)
	if terminal.Reference != "job-001" {
		t.Fatalf("receipt reference=%q", terminal.Reference)
	}

	select {
	case event := <-got:
		if event.AgentID != "agent-mingming" || event.LearnerID != "learner-mingming" {
			t.Fatalf("dispatch owner did not come from binding: %+v", event)
		}
		if event.EventID != "event-001" || event.EventType != K12EventSubmissionRequested {
			t.Fatalf("dispatch identity mismatch: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("event was not dispatched")
	}
	if calls.Load() != 1 {
		t.Fatalf("dispatch calls=%d", calls.Load())
	}
}

func TestK12WebhookReplayAndStableEventIdempotency(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	_, secret := createEnabledK12Binding(t, mgr, "dedupe")
	var calls atomic.Int32
	mgr.SetK12Handler(func(_ context.Context, _ K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		return K12DispatchResult{Reference: "job-dedupe", Status: K12ReceiptSucceeded}, nil
	})

	timestamp := k12WebhookTestNow.Format(time.RFC3339)
	body := []byte(`{"event_id":"event-dedupe","event_type":"k12.submission.requested.v1","payload":{"text":"same"}}`)
	first := serveK12Request(mgr, signedK12Request(t, "dedupe", secret, timestamp, "nonce-a", body))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	firstReceipt := decodeK12ReceiptResponse(t, first)
	waitK12ReceiptStatus(t, mgr, firstReceipt.ReceiptID, K12ReceiptSucceeded)

	// A transport retry uses a fresh nonce but the same stable event_id/body. It must
	// return the original Receipt and must not dispatch another domain command.
	retry := serveK12Request(mgr, signedK12Request(t, "dedupe", secret, timestamp, "nonce-b", body))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	retryReceipt := decodeK12ReceiptResponse(t, retry)
	if retryReceipt.ReceiptID != firstReceipt.ReceiptID {
		t.Fatalf("retry receipt=%q, want original %q", retryReceipt.ReceiptID, firstReceipt.ReceiptID)
	}
	if calls.Load() != 1 {
		t.Fatalf("stable event dispatched %d times", calls.Load())
	}

	// Reusing the exact nonce is a replay even when the event body is identical.
	replay := serveK12Request(mgr, signedK12Request(t, "dedupe", secret, timestamp, "nonce-b", body))
	if replay.Code != http.StatusConflict {
		t.Fatalf("nonce replay status=%d body=%s", replay.Code, replay.Body.String())
	}

	conflictBody := []byte(`{"event_id":"event-dedupe","event_type":"k12.submission.requested.v1","payload":{"text":"changed"}}`)
	conflict := serveK12Request(mgr, signedK12Request(t, "dedupe", secret, timestamp, "nonce-c", conflictBody))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("same event different body status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("conflicting event dispatched %d times", calls.Load())
	}
}

func TestK12WebhookSignatureWindowOwnerClaimsAndEventExactSetFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		body       []byte
		timestamp  time.Time
		mutate     func(*http.Request)
		wantStatus int
	}{
		{name: "expired", body: []byte(`{"event_id":"e1","event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`), timestamp: k12WebhookTestNow.Add(-5*time.Minute - time.Second), wantStatus: http.StatusUnauthorized},
		{name: "future", body: []byte(`{"event_id":"e2","event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`), timestamp: k12WebhookTestNow.Add(5*time.Minute + time.Second), wantStatus: http.StatusUnauthorized},
		{name: "bad signature", body: []byte(`{"event_id":"e3","event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`), timestamp: k12WebhookTestNow, mutate: func(r *http.Request) { r.Header.Set(K12HeaderSignature, "sha256=00") }, wantStatus: http.StatusUnauthorized},
		{name: "body owner claim", body: []byte(`{"event_id":"e4","event_type":"k12.submission.requested.v1","payload":{"text":"x","agent_id":"attacker"}}`), timestamp: k12WebhookTestNow, wantStatus: http.StatusBadRequest},
		{name: "generic event", body: []byte(`{"event_id":"e5","event_type":"generic","payload":{"text":"x"}}`), timestamp: k12WebhookTestNow, wantStatus: http.StatusBadRequest},
		{name: "remote url", body: []byte(`{"event_id":"e6","event_type":"k12.submission.requested.v1","payload":{"asset_refs":["http://127.0.0.1/private"]}}`), timestamp: k12WebhookTestNow, wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := newK12WebhookTestManager(t, nil)
			_, secret := createEnabledK12Binding(t, mgr, "secure")
			var calls atomic.Int32
			mgr.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
				calls.Add(1)
				return K12DispatchResult{}, nil
			})
			ts := tt.timestamp.Format(time.RFC3339)
			req := signedK12Request(t, "secure", secret, ts, "nonce-"+tt.name, tt.body)
			if tt.mutate != nil {
				tt.mutate(req)
			}
			rec := serveK12Request(mgr, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s want=%d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
			if calls.Load() != 0 {
				t.Fatalf("rejected request caused %d domain dispatches", calls.Load())
			}
		})
	}
}

func TestK12WebhookDedupeSurvivesManagerRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "webhook.db")
	db1, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db1.SetMaxOpenConns(1)
	mgr1 := newK12WebhookTestManager(t, db1)
	_, secret := createEnabledK12Binding(t, mgr1, "restart")
	var calls atomic.Int32
	mgr1.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		return K12DispatchResult{Reference: "job-before-restart", Status: K12ReceiptSucceeded}, nil
	})
	body := []byte(`{"event_id":"event-restart","event_type":"k12.submission.requested.v1","payload":{"text":"same"}}`)
	ts := k12WebhookTestNow.Format(time.RFC3339)
	first := serveK12Request(mgr1, signedK12Request(t, "restart", secret, ts, "nonce-before", body))
	firstReceipt := decodeK12ReceiptResponse(t, first)
	waitK12ReceiptStatus(t, mgr1, firstReceipt.ReceiptID, K12ReceiptSucceeded)
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db2.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db2.Close() })
	mgr2 := newK12WebhookTestManager(t, db2)
	mgr2.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		return K12DispatchResult{}, nil
	})
	retry := serveK12Request(mgr2, signedK12Request(t, "restart", secret, ts, "nonce-after", body))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("restart retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	if got := decodeK12ReceiptResponse(t, retry).ReceiptID; got != firstReceipt.ReceiptID {
		t.Fatalf("restart returned receipt %q, want %q", got, firstReceipt.ReceiptID)
	}
	if calls.Load() != 1 {
		t.Fatalf("event dispatched %d times across restart", calls.Load())
	}
}

func TestK12WebhookConcurrentStableEventCreatesOneReceiptAndDispatch(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	mgr := newK12WebhookTestManager(t, db)
	_, secret := createEnabledK12Binding(t, mgr, "concurrent")
	var calls atomic.Int32
	mgr.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		return K12DispatchResult{Reference: "job-one", Status: K12ReceiptSucceeded}, nil
	})
	body := []byte(`{"event_id":"event-100","event_type":"k12.submission.requested.v1","payload":{"text":"same"}}`)
	ts := k12WebhookTestNow.Format(time.RFC3339)

	const n = 100
	var wg sync.WaitGroup
	wg.Add(n)
	ids := make(chan string, n)
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			req := signedK12Request(t, "concurrent", secret, ts, "nonce-concurrent-"+time.Duration(i).String(), body)
			rec := serveK12Request(mgr, req)
			if rec.Code != http.StatusAccepted {
				errs <- rec.Body.String()
				return
			}
			ids <- decodeK12ReceiptResponse(t, rec).ReceiptID
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errs)
	for msg := range errs {
		t.Fatalf("concurrent delivery failed: %s", msg)
	}
	unique := map[string]struct{}{}
	for id := range ids {
		unique[id] = struct{}{}
	}
	if len(unique) != 1 {
		t.Fatalf("receipt count=%d ids=%v", len(unique), unique)
	}
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("dispatch calls=%d", calls.Load())
	}
}

func TestK12WebhookBodySizeBoundary(t *testing.T) {
	prefix := `{"event_id":"event-size","event_type":"k12.submission.requested.v1","payload":{"text":"`
	suffix := `"}}`
	for _, size := range []int{maxPayloadSize - 1, maxPayloadSize, maxPayloadSize + 1} {
		t.Run(fmt.Sprintf("bytes_%d", size), func(t *testing.T) {
			mgr := newK12WebhookTestManager(t, nil)
			_, secret := createEnabledK12Binding(t, mgr, "size")
			mgr.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
				return K12DispatchResult{}, nil
			})
			filler := size - len(prefix) - len(suffix)
			if filler < 0 {
				t.Fatalf("invalid test fixture size %d", size)
			}
			body := []byte(prefix + strings.Repeat("x", filler) + suffix)
			if len(body) != size {
				t.Fatalf("body size=%d, want %d", len(body), size)
			}
			rec := serveK12Request(mgr, signedK12Request(t, "size", secret, k12WebhookTestNow.Format(time.RFC3339), "nonce-size", body))
			want := http.StatusAccepted
			if size > maxPayloadSize {
				want = http.StatusRequestEntityTooLarge
			}
			if rec.Code != want {
				t.Fatalf("status=%d body=%s want=%d", rec.Code, rec.Body.String(), want)
			}
		})
	}
}

func TestK12WebhookRateLimitUsesBindingIPAndOwnerEventKeys(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	_, secret := createEnabledK12Binding(t, mgr, "limited")
	mgr.SetK12RateLimits(10, 2)
	var calls atomic.Int32
	mgr.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		return K12DispatchResult{}, nil
	})

	for i := 1; i <= 3; i++ {
		body := []byte(fmt.Sprintf(`{"event_id":"event-rate-%d","event_type":"k12.submission.requested.v1","payload":{"text":"same"}}`, i))
		req := signedK12Request(t, "limited", secret, k12WebhookTestNow.Format(time.RFC3339), fmt.Sprintf("nonce-rate-%d", i), body)
		req.RemoteAddr = "203.0.113.7:12345"
		rec := serveK12Request(mgr, req)
		if i <= 2 {
			if rec.Code != http.StatusAccepted {
				t.Fatalf("request %d status=%d body=%s", i, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("rate-limited status=%d retry_after=%q body=%s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
		}
	}
	deadline := time.Now().Add(time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 2 {
		t.Fatalf("accepted dispatch calls=%d, want 2", calls.Load())
	}
}

func TestK12WebhookRejectedRequestPersistsRejectedReceipt(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	_, secret := createEnabledK12Binding(t, mgr, "rejected")
	var calls atomic.Int32
	mgr.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		return K12DispatchResult{}, nil
	})
	body := []byte(`{"event_id":"event-rejected","event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`)
	req := signedK12Request(t, "rejected", secret, k12WebhookTestNow.Format(time.RFC3339), "nonce-rejected", body)
	req.Header.Set(K12HeaderSignature, "sha256=00")
	rec := serveK12Request(mgr, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	receipt := decodeK12ReceiptResponse(t, rec)
	stored, err := mgr.GetK12Receipt(context.Background(), receipt.ReceiptID)
	if err != nil {
		t.Fatalf("get rejected receipt: %v", err)
	}
	if stored.Status != K12ReceiptRejected || stored.FailureKind != "signature_invalid" {
		t.Fatalf("rejected receipt=%+v", stored)
	}
	if calls.Load() != 0 {
		t.Fatalf("rejected request dispatched %d commands", calls.Load())
	}
}

func TestK12WebhookExactEventSetDispatchesOnlyAllowedSchemas(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	_, secret, err := mgr.CreateK12Binding(context.Background(), K12BindingInput{
		Name: "exact-set", AgentID: "agent-mingming", LearnerID: "learner-mingming",
		AllowedEvents: append([]K12EventType(nil), k12EventOrder...), AllowedWorkflows: []string{"weekly@v1"},
		CreatedBy: "parent-1", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create exact-set binding: %v", err)
	}
	got := make(chan K12EventType, len(k12EventOrder))
	mgr.SetK12Handler(func(_ context.Context, dispatch K12Dispatch) (K12DispatchResult, error) {
		got <- dispatch.EventType
		return K12DispatchResult{}, nil
	})
	cases := []struct {
		event   K12EventType
		payload string
	}{
		{K12EventSubmissionRequested, `{"text":"answer"}`},
		{K12EventPracticeReturnRequested, `{"paper_no":"P-1","return_assets":[{"asset_ref":"asset://return-1","item_ids":["Q1"]}]}`},
		{K12EventWorkflowRunRequested, `{"workflow_id":"weekly","workflow_version":"v1"}`},
	}
	for i, tt := range cases {
		body := []byte(fmt.Sprintf(`{"event_id":"event-exact-%d","event_type":%q,"payload":%s}`, i, tt.event, tt.payload))
		rec := serveK12Request(mgr, signedK12Request(t, "exact-set", secret, k12WebhookTestNow.Format(time.RFC3339), fmt.Sprintf("nonce-exact-%d", i), body))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("event %s status=%d body=%s", tt.event, rec.Code, rec.Body.String())
		}
	}
	seen := make(map[K12EventType]bool, len(k12EventOrder))
	for range k12EventOrder {
		select {
		case event := <-got:
			seen[event] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for exact-set dispatch")
		}
	}
	for _, event := range k12EventOrder {
		if !seen[event] {
			t.Fatalf("event %s was not dispatched; seen=%v", event, seen)
		}
	}
}

func TestDetachK12BindingsByAgentRollbackRestoresExactStatuses(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	createEnabledK12Binding(t, mgr, "enabled-before-delete")
	_, _, err := mgr.CreateK12Binding(context.Background(), K12BindingInput{
		Name: "disabled-before-delete", AgentID: "agent-mingming", LearnerID: "learner-mingming",
		AllowedEvents: []K12EventType{K12EventSubmissionRequested}, CreatedBy: "parent-1", Enabled: false,
	})
	if err != nil {
		t.Fatalf("create disabled binding: %v", err)
	}

	rollback, err := mgr.DetachK12BindingsByAgent(context.Background(), "agent-mingming")
	if err != nil || rollback == nil {
		t.Fatalf("detach rollback=%v err=%v", rollback != nil, err)
	}
	for _, name := range []string{"enabled-before-delete", "disabled-before-delete"} {
		binding, getErr := mgr.GetK12Binding(context.Background(), name)
		if getErr != nil || binding.Status != K12BindingDisabled {
			t.Fatalf("detached %s binding=%+v err=%v", name, binding, getErr)
		}
	}
	if err := rollback(context.Background()); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	enabled, _ := mgr.GetK12Binding(context.Background(), "enabled-before-delete")
	disabled, _ := mgr.GetK12Binding(context.Background(), "disabled-before-delete")
	if enabled.Status != K12BindingEnabled || disabled.Status != K12BindingDisabled {
		t.Fatalf("rollback statuses enabled=%s disabled=%s", enabled.Status, disabled.Status)
	}
}
