package webhook

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestK12WebhookStableRetryBypassesNewEventRateLimit(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	_, secret := createEnabledK12Binding(t, mgr, "retry-rate")
	mgr.SetK12RateLimits(1, 1)
	var calls atomic.Int32
	mgr.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		return K12DispatchResult{Reference: "job-rate", Status: K12ReceiptSucceeded}, nil
	})

	body := []byte(`{"event_id":"event-rate-stable","event_type":"k12.submission.requested.v1","payload":{"text":"same"}}`)
	ts := k12WebhookTestNow.Format(time.RFC3339)
	first := serveK12Request(mgr, signedK12Request(t, "retry-rate", secret, ts, "nonce-rate-first", body))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	firstReceipt := decodeK12ReceiptResponse(t, first)
	waitK12ReceiptStatus(t, mgr, firstReceipt.ReceiptID, K12ReceiptSucceeded)

	retry := serveK12Request(mgr, signedK12Request(t, "retry-rate", secret, ts, "nonce-rate-retry", body))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("stable retry must bypass new-event rate budget: status=%d body=%s", retry.Code, retry.Body.String())
	}
	if got := decodeK12ReceiptResponse(t, retry).ReceiptID; got != firstReceipt.ReceiptID {
		t.Fatalf("retry receipt=%q want=%q", got, firstReceipt.ReceiptID)
	}
	if calls.Load() != 1 {
		t.Fatalf("stable retry dispatched %d times", calls.Load())
	}
}

func TestK12WebhookInvalidSignatureCannotReserveLegitimateNonce(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	_, secret := createEnabledK12Binding(t, mgr, "bad-signature-replay")
	body := []byte(`{"event_id":"event-bad-signature","event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`)
	ts := k12WebhookTestNow.Format(time.RFC3339)
	forged := signedK12Request(t, "bad-signature-replay", secret, ts, "nonce-bad-signature", body)
	forged.Header.Set(K12HeaderSignature, "sha256=00")
	first := serveK12Request(mgr, forged)
	if first.Code != http.StatusUnauthorized {
		t.Fatalf("first bad signature status=%d body=%s", first.Code, first.Body.String())
	}

	// A forged request must not be able to reserve a nonce and deny the
	// legitimately signed delivery that follows.
	legitimate := serveK12Request(mgr, signedK12Request(t, "bad-signature-replay", secret, ts,
		"nonce-bad-signature", body))
	if legitimate.Code != http.StatusAccepted {
		t.Fatalf("legitimate request after forged nonce reservation status=%d body=%s", legitimate.Code, legitimate.Body.String())
	}
}

func TestK12WebhookValidSignatureConsumesNonceBeforeSchemaValidation(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	_, secret := createEnabledK12Binding(t, mgr, "schema-replay")
	// event_id is deliberately absent. The HMAC is valid, so the nonce must be
	// consumed even though the envelope fails schema validation afterwards.
	body := []byte(`{"event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`)
	ts := k12WebhookTestNow.Format(time.RFC3339)
	first := serveK12Request(mgr, signedK12Request(t, "schema-replay", secret, ts, "nonce-schema", body))
	if first.Code != http.StatusBadRequest {
		t.Fatalf("schema-invalid status=%d body=%s", first.Code, first.Body.String())
	}
	replay := serveK12Request(mgr, signedK12Request(t, "schema-replay", secret, ts, "nonce-schema", body))
	if replay.Code != http.StatusConflict {
		t.Fatalf("validly signed schema-invalid replay status=%d body=%s", replay.Code, replay.Body.String())
	}
}

func TestK12WebhookHandlerCannotImplicitlyMarkReceiptSucceeded(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	_, secret := createEnabledK12Binding(t, mgr, "explicit-terminal")
	mgr.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		return K12DispatchResult{Reference: "domain-object-created-only"}, nil
	})
	body := []byte(`{"event_id":"event-explicit-terminal","event_type":"k12.submission.requested.v1","payload":{"text":"x"}}`)
	rec := serveK12Request(mgr, signedK12Request(t, "explicit-terminal", secret,
		k12WebhookTestNow.Format(time.RFC3339), "nonce-explicit-terminal", body))
	receipt := decodeK12ReceiptResponse(t, rec)
	terminal := waitK12ReceiptStatus(t, mgr, receipt.ReceiptID, K12ReceiptFailed)
	if terminal.FailureKind != "terminal_status_missing" {
		t.Fatalf("receipt=%+v", terminal)
	}
}

type k12DispatchRecoverer interface {
	RecoverK12Dispatches(context.Context) (int, error)
}

func ensureK12DispatchColumnForRed(t *testing.T, db *sql.DB) {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(k12_webhook_receipts)`)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		found = found || name == "dispatch_json"
	}
	rows.Close()
	if !found {
		if _, err := db.Exec(`ALTER TABLE k12_webhook_receipts ADD COLUMN dispatch_json TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestK12WebhookAcceptedDispatchRecoversAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "dispatch-recovery.db")
	db1, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db1.SetMaxOpenConns(1)
	mgr1 := newK12WebhookTestManager(t, db1)
	binding, _ := createEnabledK12Binding(t, mgr1, "recover-accepted")
	ensureK12DispatchColumnForRed(t, db1)
	dispatch := K12Dispatch{
		ReceiptID: "receipt-recover-accepted", BindingID: binding.BindingID,
		EventID: "event-recover-accepted", EventType: K12EventSubmissionRequested,
		AgentID: binding.AgentID, LearnerID: binding.LearnerID,
		Payload: json.RawMessage(`{"text":"persist me"}`),
	}
	raw, _ := json.Marshal(dispatch)
	if _, err := db1.Exec(`INSERT INTO k12_webhook_receipts
      (receipt_id,binding_id,event_id,event_type,payload_digest,status,reference,failure_kind,created_at,updated_at,dispatch_json)
      VALUES(?,?,?,?,?,?,?,?,?,?,?)`, dispatch.ReceiptID, dispatch.BindingID, dispatch.EventID,
		dispatch.EventType, sha256Hex(dispatch.Payload), K12ReceiptAccepted, "", "",
		k12WebhookTestNow.UnixNano(), k12WebhookTestNow.UnixNano(), string(raw)); err != nil {
		t.Fatal(err)
	}
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
	var calls atomic.Int32
	mgr2.SetK12Handler(func(_ context.Context, got K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		if got.EventID != dispatch.EventID || string(got.Payload) != string(dispatch.Payload) {
			t.Errorf("recovered dispatch=%+v", got)
		}
		return K12DispatchResult{Reference: "job-recovered", Status: K12ReceiptSucceeded}, nil
	})
	recoverer, ok := any(mgr2).(k12DispatchRecoverer)
	if !ok {
		t.Fatal("Manager must expose durable K12 dispatch recovery")
	}
	if n, err := recoverer.RecoverK12Dispatches(context.Background()); err != nil || n != 1 {
		t.Fatalf("recovered=%d err=%v", n, err)
	}
	terminal := waitK12ReceiptStatus(t, mgr2, dispatch.ReceiptID, K12ReceiptSucceeded)
	if terminal.Reference != "job-recovered" || calls.Load() != 1 {
		t.Fatalf("terminal=%+v calls=%d", terminal, calls.Load())
	}
}

func TestK12WebhookProcessingCrashBecomesOutcomeUnknownWithoutReplay(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	binding, _ := createEnabledK12Binding(t, mgr, "recover-processing")
	ensureK12DispatchColumnForRed(t, mgr.db)
	dispatch := K12Dispatch{
		ReceiptID: "receipt-recover-processing", BindingID: binding.BindingID,
		EventID: "event-recover-processing", EventType: K12EventSubmissionRequested,
		AgentID: binding.AgentID, LearnerID: binding.LearnerID, Payload: json.RawMessage(`{"text":"x"}`),
	}
	raw, _ := json.Marshal(dispatch)
	if _, err := mgr.db.Exec(`INSERT INTO k12_webhook_receipts
      (receipt_id,binding_id,event_id,event_type,payload_digest,status,reference,failure_kind,created_at,updated_at,dispatch_json)
      VALUES(?,?,?,?,?,?,?,?,?,?,?)`, dispatch.ReceiptID, dispatch.BindingID, dispatch.EventID,
		dispatch.EventType, sha256Hex(dispatch.Payload), K12ReceiptProcessing, "", "",
		k12WebhookTestNow.UnixNano(), k12WebhookTestNow.UnixNano(), string(raw)); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	mgr.SetK12Handler(func(context.Context, K12Dispatch) (K12DispatchResult, error) {
		calls.Add(1)
		return K12DispatchResult{}, errors.New("must not replay an unknown side effect")
	})
	recoverer, ok := any(mgr).(k12DispatchRecoverer)
	if !ok {
		t.Fatal("Manager must expose durable K12 dispatch recovery")
	}
	if _, err := recoverer.RecoverK12Dispatches(context.Background()); err != nil {
		t.Fatal(err)
	}
	receipt := waitK12ReceiptStatus(t, mgr, dispatch.ReceiptID, K12ReceiptOutcomeUnknown)
	if receipt.FailureKind != "process_restarted" || calls.Load() != 0 {
		t.Fatalf("receipt=%+v calls=%d", receipt, calls.Load())
	}
}

func TestDeleteK12BindingCancelsAcceptedAndRefusesProcessing(t *testing.T) {
	mgr := newK12WebhookTestManager(t, nil)
	binding, _ := createEnabledK12Binding(t, mgr, "delete-linearized")
	ensureK12DispatchColumnForRed(t, mgr.db)
	insert := func(id string, status K12ReceiptStatus) {
		t.Helper()
		if _, err := mgr.db.Exec(`INSERT INTO k12_webhook_receipts
        (receipt_id,binding_id,event_id,event_type,payload_digest,status,reference,failure_kind,created_at,updated_at,dispatch_json)
        VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, binding.BindingID, "event-"+id, K12EventSubmissionRequested,
			"digest", status, "", "", k12WebhookTestNow.UnixNano(), k12WebhookTestNow.UnixNano(), `{}`); err != nil {
			t.Fatal(err)
		}
	}
	insert("receipt-processing", K12ReceiptProcessing)
	if err := mgr.DeleteK12Binding(context.Background(), binding.Name); err == nil {
		t.Fatal("delete must refuse while a domain execution is already processing")
	}
	if _, err := mgr.GetK12Binding(context.Background(), binding.Name); err != nil {
		t.Fatalf("busy delete removed binding: %v", err)
	}
	if _, err := mgr.db.Exec(`UPDATE k12_webhook_receipts SET status=? WHERE receipt_id=?`, K12ReceiptSucceeded, "receipt-processing"); err != nil {
		t.Fatal(err)
	}
	insert("receipt-accepted", K12ReceiptAccepted)
	if err := mgr.DeleteK12Binding(context.Background(), binding.Name); err != nil {
		t.Fatal(err)
	}
	var status, failure, dispatchJSON string
	if err := mgr.db.QueryRow(`SELECT status,failure_kind,dispatch_json FROM k12_webhook_receipts WHERE receipt_id=?`, "receipt-accepted").Scan(&status, &failure, &dispatchJSON); err != nil {
		t.Fatal(err)
	}
	if status != string(K12ReceiptFailed) || failure != "binding_deleted" || dispatchJSON != "" {
		t.Fatalf("accepted receipt after delete status=%q failure=%q dispatch=%q", status, failure, dispatchJSON)
	}
}
