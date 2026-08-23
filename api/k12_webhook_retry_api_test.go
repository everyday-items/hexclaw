package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/webhook"
)

func postSignedK12EventForRetry(t *testing.T, client *http.Client, baseURL, name, secret, eventID string) webhook.K12Receipt {
	t.Helper()
	body := []byte(`{"event_id":"` + eventID + `","event_type":"k12.submission.requested.v1","payload":{"text":"retry me"}}`)
	timestamp := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/webhooks/"+name, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.K12HeaderTimestamp, timestamp)
	req.Header.Set(webhook.K12HeaderNonce, "nonce-"+eventID)
	req.Header.Set(webhook.K12HeaderSignature, webhook.K12Signature(secret, timestamp, "nonce-"+eventID, body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("receive status=%d body=%s", resp.StatusCode, raw)
	}
	var decoded struct {
		Receipt webhook.K12Receipt `json:"receipt"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded.Receipt
}

func waitK12ReceiptAPIStatus(t *testing.T, client *http.Client, baseURL, receiptID string, want webhook.K12ReceiptStatus) webhook.K12Receipt {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, raw := doK12WebhookAPI(t, client, http.MethodGet,
			baseURL+"/api/v1/webhooks?user_id=parent-1&agent_id=kid-agent&receipt_id="+receiptID, nil)
		if status == http.StatusOK {
			var decoded struct {
				Receipt webhook.K12Receipt `json:"receipt"`
			}
			if json.Unmarshal(raw, &decoded) == nil && decoded.Receipt.Status == want {
				return decoded.Receipt
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for Receipt " + string(want))
	return webhook.K12Receipt{}
}

func TestK12WebhookAPIRetriesOnlyPersistedSafeFailure(t *testing.T) {
	srv := newWebhookTestServer(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	srv.webhookMgr.SetK12Clock(func() time.Time { return now })
	binding, secret, err := srv.webhookMgr.CreateK12Binding(context.Background(), webhook.K12BindingInput{
		Name: "retry-api", AgentID: "kid-agent", LearnerID: "kid-learner",
		AllowedEvents: []webhook.K12EventType{webhook.K12EventSubmissionRequested}, CreatedBy: defaultDesktopUserID, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv.webhookMgr.SetK12Handler(func(context.Context, webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
		if calls.Add(1) == 1 {
			return webhook.K12DispatchResult{Reference: "job-same", RetrySafe: true}, errors.New("local preflight failed")
		}
		return webhook.K12DispatchResult{Reference: "job-same", Status: webhook.K12ReceiptSucceeded}, nil
	})
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	receipt := postSignedK12EventForRetry(t, ts.Client(), ts.URL, binding.Name, secret, "event-retry-api")
	failed := waitK12ReceiptAPIStatus(t, ts.Client(), ts.URL, receipt.ReceiptID, webhook.K12ReceiptFailed)
	if !failed.RetrySafe || failed.AttemptCount != 1 {
		t.Fatalf("failed Receipt=%+v", failed)
	}

	status, raw := doK12WebhookAPI(t, ts.Client(), http.MethodPatch,
		ts.URL+"/api/v1/webhooks/retry-api?user_id=parent-1&agent_id=kid-agent",
		[]byte(`{"retry_receipt_id":"`+receipt.ReceiptID+`"}`))
	if status != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", status, raw)
	}
	if !bytes.Contains(raw, []byte(`"status":"accepted"`)) || !bytes.Contains(raw, []byte(receipt.ReceiptID)) {
		t.Fatalf("retry response=%s", raw)
	}
	completed := waitK12ReceiptAPIStatus(t, ts.Client(), ts.URL, receipt.ReceiptID, webhook.K12ReceiptSucceeded)
	if completed.AttemptCount != 2 || calls.Load() != 2 {
		t.Fatalf("completed=%+v calls=%d", completed, calls.Load())
	}
}

func TestK12WebhookAPIRetryOwnerAndOutcomeUnknownFailClosed(t *testing.T) {
	srv := newWebhookTestServer(t)
	const crossOwnerAPIToken = "k12-retry-cross-owner-token"
	srv.cfg.Server.APIToken = crossOwnerAPIToken
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	srv.webhookMgr.SetK12Clock(func() time.Time { return now })
	binding, secret, err := srv.webhookMgr.CreateK12Binding(context.Background(), webhook.K12BindingInput{
		Name: "retry-unknown", AgentID: "kid-agent", LearnerID: "kid-learner",
		AllowedEvents: []webhook.K12EventType{webhook.K12EventSubmissionRequested}, CreatedBy: defaultDesktopUserID, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.webhookMgr.SetK12Handler(func(context.Context, webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
		return webhook.K12DispatchResult{Reference: "maybe-created", RetrySafe: true}, webhook.ErrK12OutcomeUnknown
	})
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	receipt := postSignedK12EventForRetry(t, ts.Client(), ts.URL, binding.Name, secret, "event-unknown-api")
	unknown := waitK12ReceiptAPIStatus(t, ts.Client(), ts.URL, receipt.ReceiptID, webhook.K12ReceiptOutcomeUnknown)
	if unknown.RetrySafe {
		t.Fatalf("outcome_unknown must discard retry hint: %+v", unknown)
	}
	body := []byte(`{"retry_receipt_id":"` + receipt.ReceiptID + `"}`)
	status, raw := doK12WebhookAPI(t, ts.Client(), http.MethodPatch,
		ts.URL+"/api/v1/webhooks/retry-unknown?user_id=parent-1&agent_id=kid-agent", body)
	if status != http.StatusConflict {
		t.Fatalf("outcome_unknown retry status=%d body=%s", status, raw)
	}
	// API token 将该请求认证为 api-user，确保跨 owner 断言经过真实认证边界。
	crossOwnerReq, err := http.NewRequest(http.MethodPatch,
		ts.URL+"/api/v1/webhooks/retry-unknown?user_id=other-parent&agent_id=kid-agent",
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create cross-owner request: %v", err)
	}
	crossOwnerReq.Header.Set("Content-Type", "application/json")
	crossOwnerReq.Header.Set("Authorization", "Bearer "+crossOwnerAPIToken)
	crossOwnerResp, err := ts.Client().Do(crossOwnerReq)
	if err != nil {
		t.Fatalf("send cross-owner request: %v", err)
	}
	raw, err = io.ReadAll(crossOwnerResp.Body)
	crossOwnerResp.Body.Close()
	if err != nil {
		t.Fatalf("read cross-owner response: %v", err)
	}
	status = crossOwnerResp.StatusCode
	if status != http.StatusNotFound {
		t.Fatalf("cross-owner retry status=%d body=%s", status, raw)
	}
	status, raw = doK12WebhookAPI(t, ts.Client(), http.MethodPatch,
		ts.URL+"/api/v1/webhooks/retry-unknown?user_id=parent-1&agent_id=kid-agent",
		[]byte(`{"retry_receipt_id":"`+receipt.ReceiptID+`","enabled":false}`))
	if status != http.StatusBadRequest {
		t.Fatalf("mixed retry mutation status=%d body=%s", status, raw)
	}
}
