package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestK12WebhookFailedReceiptRedispatchReusesFrozenIdentity(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	binding, secret := createEnabledK12Binding(t, mgr, "retry-safe")
	var calls atomic.Int32
	var mu sync.Mutex
	var dispatches []K12Dispatch
	mgr.SetK12Handler(func(_ context.Context, dispatch K12Dispatch) (K12DispatchResult, error) {
		mu.Lock()
		dispatches = append(dispatches, dispatch)
		mu.Unlock()
		if calls.Add(1) == 1 {
			return K12DispatchResult{Reference: "grading-job-stable", RetrySafe: true}, errors.New("local validation failed before side effect")
		}
		return K12DispatchResult{Reference: "grading-job-stable", Status: K12ReceiptSucceeded}, nil
	})

	body := []byte(`{"event_id":"event-retry","event_type":"k12.submission.requested.v1","payload":{"text":"3/4+1/4"}}`)
	response := serveK12Request(mgr, signedK12Request(t, binding.Name, secret, k12WebhookTestNow.Format("2006-01-02T15:04:05Z07:00"), "nonce-retry", body))
	first := decodeK12ReceiptResponse(t, response)
	failed := waitK12ReceiptStatus(t, mgr, first.ReceiptID, K12ReceiptFailed)
	if !failed.RetrySafe || failed.AttemptCount != 1 || failed.Reference != "grading-job-stable" {
		t.Fatalf("failed Receipt lacks retry evidence: %+v", failed)
	}

	requeued, err := mgr.RetryK12ReceiptForOwner(context.Background(), binding.Name, first.ReceiptID, "parent-1", "agent-mingming")
	if err != nil {
		t.Fatalf("retry failed Receipt: %v", err)
	}
	if requeued.ReceiptID != first.ReceiptID || requeued.EventID != first.EventID || requeued.Status != K12ReceiptAccepted {
		t.Fatalf("retry must reuse same Receipt/event: first=%+v retry=%+v", first, requeued)
	}
	completed := waitK12ReceiptStatus(t, mgr, first.ReceiptID, K12ReceiptSucceeded)
	if completed.RetrySafe || completed.AttemptCount != 2 || completed.Reference != "grading-job-stable" {
		t.Fatalf("completed retry Receipt=%+v", completed)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(dispatches) != 2 {
		t.Fatalf("dispatch count=%d", len(dispatches))
	}
	if dispatches[0].ReceiptID != dispatches[1].ReceiptID || dispatches[0].BindingID != dispatches[1].BindingID ||
		dispatches[0].EventID != dispatches[1].EventID || string(dispatches[0].Payload) != string(dispatches[1].Payload) {
		t.Fatalf("retry changed frozen dispatch: before=%+v after=%+v", dispatches[0], dispatches[1])
	}
}

func TestK12WebhookRetryFailsClosedForUnprovenOrNonFailedStates(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	binding, _ := createEnabledK12Binding(t, mgr, "retry-closed")
	now := k12WebhookTestNow.UnixNano()
	dispatch := K12Dispatch{
		ReceiptID: "receipt-closed", BindingID: binding.BindingID, EventID: "event-closed",
		EventType: K12EventSubmissionRequested, AgentID: binding.AgentID, LearnerID: binding.LearnerID,
		Payload: []byte(`{"text":"x"}`),
	}
	raw, err := jsonMarshal(dispatch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.db.Exec(`INSERT INTO k12_webhook_receipts
		(receipt_id,binding_id,event_id,event_type,payload_digest,status,reference,failure_kind,dispatch_json,retry_safe,attempt_count,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, dispatch.ReceiptID, dispatch.BindingID, dispatch.EventID,
		dispatch.EventType, "digest", K12ReceiptFailed, "", "handler_failed", string(raw), 0, 1, now, now); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.RetryK12ReceiptForOwner(context.Background(), binding.Name, dispatch.ReceiptID, "parent-1", binding.AgentID); !errors.Is(err, ErrK12ReceiptNotRetryable) {
		t.Fatalf("failed without evidence err=%v", err)
	}
	for _, status := range []K12ReceiptStatus{
		K12ReceiptAccepted, K12ReceiptProcessing, K12ReceiptSucceeded, K12ReceiptOutcomeUnknown, K12ReceiptRejected,
	} {
		if _, err := mgr.db.Exec(`UPDATE k12_webhook_receipts SET status=?,retry_safe=1 WHERE receipt_id=?`, status, dispatch.ReceiptID); err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.RetryK12ReceiptForOwner(context.Background(), binding.Name, dispatch.ReceiptID, "parent-1", binding.AgentID); !errors.Is(err, ErrK12ReceiptNotRetryable) {
			t.Fatalf("status %s retry err=%v", status, err)
		}
	}
	if _, err := mgr.RetryK12ReceiptForOwner(context.Background(), binding.Name, dispatch.ReceiptID, "other-parent", binding.AgentID); !errors.Is(err, ErrK12BindingNotFound) {
		t.Fatalf("cross-owner retry err=%v", err)
	}
}

func TestK12WebhookConcurrentRetryClaimsOnce(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	binding, secret := createEnabledK12Binding(t, mgr, "retry-once")
	var calls atomic.Int32
	release := make(chan struct{})
	mgr.SetK12Handler(func(_ context.Context, _ K12Dispatch) (K12DispatchResult, error) {
		if calls.Add(1) == 1 {
			return K12DispatchResult{RetrySafe: true}, errors.New("safe local failure")
		}
		<-release
		return K12DispatchResult{Status: K12ReceiptSucceeded}, nil
	})
	body := []byte(`{"event_id":"event-once","event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`)
	first := serveK12Request(mgr, signedK12Request(t, binding.Name, secret, k12WebhookTestNow.Format("2006-01-02T15:04:05Z07:00"), "nonce-once", body))
	receipt := waitK12ReceiptStatus(t, mgr, decodeK12ReceiptResponse(t, first).ReceiptID, K12ReceiptFailed)

	const contenders = 20
	var won atomic.Int32
	var wg sync.WaitGroup
	wg.Add(contenders)
	for range contenders {
		go func() {
			defer wg.Done()
			if _, err := mgr.RetryK12ReceiptForOwner(context.Background(), binding.Name, receipt.ReceiptID, "parent-1", binding.AgentID); err == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("successful retry claims=%d, want 1", won.Load())
	}
	close(release)
	terminal := waitK12ReceiptStatus(t, mgr, receipt.ReceiptID, K12ReceiptSucceeded)
	if calls.Load() != 2 || terminal.AttemptCount != 2 {
		t.Fatalf("calls=%d receipt=%+v", calls.Load(), terminal)
	}
}

func TestK12WebhookRetryEvidenceSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retry.db")
	db1, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db1.SetMaxOpenConns(1)
	mgr1 := newK12WebhookTestManager(t, db1)
	binding, secret := createEnabledK12Binding(t, mgr1, "retry-restart")
	mgr1.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		return K12DispatchResult{Reference: "same-domain-key", RetrySafe: true}, errors.New("safe before external call")
	})
	body := []byte(`{"event_id":"event-restart-retry","event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`)
	response := serveK12Request(mgr1, signedK12Request(t, binding.Name, secret, k12WebhookTestNow.Format("2006-01-02T15:04:05Z07:00"), "nonce-restart-retry", body))
	failed := waitK12ReceiptStatus(t, mgr1, decodeK12ReceiptResponse(t, response).ReceiptID, K12ReceiptFailed)
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db2.SetMaxOpenConns(1)
	defer db2.Close()
	mgr2 := newK12WebhookTestManager(t, db2)
	mgr2.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		return K12DispatchResult{Reference: "same-domain-key", Status: K12ReceiptSucceeded}, nil
	})
	if _, err := mgr2.RetryK12ReceiptForOwner(context.Background(), binding.Name, failed.ReceiptID, "parent-1", binding.AgentID); err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	terminal := waitK12ReceiptStatus(t, mgr2, failed.ReceiptID, K12ReceiptSucceeded)
	if terminal.AttemptCount != 2 || terminal.Reference != "same-domain-key" {
		t.Fatalf("restart retry receipt=%+v", terminal)
	}
}

// jsonMarshal keeps the test fixture aligned with the durable dispatch JSON
// without exposing an implementation helper from production code.
func jsonMarshal(value any) ([]byte, error) {
	return json.Marshal(value)
}
