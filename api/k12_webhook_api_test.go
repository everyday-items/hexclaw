package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/webhook"
)

func doK12WebhookAPI(t *testing.T, client *http.Client, method, target string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func TestK12WebhookAPIProductionRouteLifecycleAndReceipt(t *testing.T) {
	srv := newWebhookTestServer(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	srv.webhookMgr.SetK12Clock(func() time.Time { return now })
	var dispatches atomic.Int32
	srv.webhookMgr.SetK12Handler(func(_ context.Context, event webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
		dispatches.Add(1)
		if event.AgentID != "kid-agent" || event.LearnerID != "kid-learner" {
			t.Errorf("binding owner lost at production route: %+v", event)
		}
		return webhook.K12DispatchResult{Reference: "job-api-1", Status: webhook.K12ReceiptSucceeded}, nil
	})
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	createBody := []byte(`{
      "name":"k12-homework",
      "type":"k12",
      "agent_id":"kid-agent",
      "learner_id":"kid-learner",
      "allowed_events":["k12.submission.requested.v1"],
      "user_id":"parent-1"
    }`)
	status, raw := doK12WebhookAPI(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/webhooks", createBody)
	if status != http.StatusOK {
		t.Fatalf("create status=%d body=%s", status, raw)
	}
	var created struct {
		Binding webhook.K12Binding `json:"binding"`
		Secret  string             `json:"secret"`
		URL     string             `json:"url"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if created.Secret == "" || created.Binding.Status != webhook.K12BindingDisabled {
		t.Fatalf("create must return one-time secret and default-disabled binding: %+v", created)
	}
	if created.Binding.Secret != "" || created.URL != "/api/v1/webhooks/k12-homework" {
		t.Fatalf("secret/url contract violated: %+v", created)
	}

	status, raw = doK12WebhookAPI(t, ts.Client(), http.MethodGet, ts.URL+"/api/v1/webhooks?user_id=parent-1&agent_id=kid-agent", nil)
	if status != http.StatusOK || bytes.Contains(raw, []byte(created.Secret)) {
		t.Fatalf("list leaked secret or failed: status=%d body=%s", status, raw)
	}
	var listed struct {
		K12Bindings []webhook.K12Binding `json:"k12_bindings"`
		Total       int                  `json:"total"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil || len(listed.K12Bindings) != 1 || listed.Total != 1 {
		t.Fatalf("list=%+v err=%v body=%s", listed, err, raw)
	}

	status, raw = doK12WebhookAPI(t, ts.Client(), http.MethodPatch, ts.URL+"/api/v1/webhooks/k12-homework?user_id=parent-1&agent_id=kid-agent", []byte(`{"enabled":true}`))
	if status != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", status, raw)
	}

	eventBody := []byte(`{"event_id":"api-event-1","event_type":"k12.submission.requested.v1","payload":{"text":"检查这道题"}}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/webhooks/k12-homework", bytes.NewReader(eventBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	timestamp := now.Format(time.RFC3339)
	req.Header.Set(webhook.K12HeaderTimestamp, timestamp)
	req.Header.Set(webhook.K12HeaderNonce, "nonce-api-1")
	req.Header.Set(webhook.K12HeaderSignature, webhook.K12Signature(created.Secret, timestamp, "nonce-api-1", eventBody))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("receiver status=%d body=%s", resp.StatusCode, raw)
	}
	var accepted struct {
		Receipt webhook.K12Receipt `json:"receipt"`
	}
	if err := json.Unmarshal(raw, &accepted); err != nil || accepted.Receipt.ReceiptID == "" {
		t.Fatalf("accepted response=%s err=%v", raw, err)
	}
	status, raw = doK12WebhookAPI(t, ts.Client(), http.MethodGet,
		ts.URL+"/api/v1/webhooks?user_id=parent-1&agent_id=other-agent&receipt_id="+accepted.Receipt.ReceiptID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-agent Receipt query status=%d body=%s", status, raw)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, raw = doK12WebhookAPI(t, ts.Client(), http.MethodGet,
			ts.URL+"/api/v1/webhooks?user_id=parent-1&agent_id=kid-agent&receipt_id="+accepted.Receipt.ReceiptID, nil)
		if status == http.StatusOK && bytes.Contains(raw, []byte(`"status":"succeeded"`)) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"job_or_execution_ref":"job-api-1"`)) {
		t.Fatalf("receipt query status=%d body=%s", status, raw)
	}
	status, raw = doK12WebhookAPI(t, ts.Client(), http.MethodGet,
		ts.URL+"/api/v1/webhooks?user_id=parent-1&agent_id=kid-agent&binding_name=k12-homework", nil)
	if status != http.StatusOK {
		t.Fatalf("receipt history status=%d body=%s", status, raw)
	}
	var history struct {
		Receipts []webhook.K12Receipt `json:"receipts"`
	}
	if err := json.Unmarshal(raw, &history); err != nil || len(history.Receipts) != 1 ||
		history.Receipts[0].ReceiptID != accepted.Receipt.ReceiptID {
		t.Fatalf("receipt history=%+v err=%v body=%s", history, err, raw)
	}
	status, raw = doK12WebhookAPI(t, ts.Client(), http.MethodGet,
		ts.URL+"/api/v1/webhooks?user_id=parent-1&agent_id=other-agent&binding_name=k12-homework", nil)
	if status != http.StatusNotFound {
		t.Fatalf("cross-agent receipt history status=%d body=%s", status, raw)
	}
	if dispatches.Load() != 1 {
		t.Fatalf("dispatches=%d", dispatches.Load())
	}

	status, raw = doK12WebhookAPI(t, ts.Client(), http.MethodPatch, ts.URL+"/api/v1/webhooks/k12-homework?user_id=parent-1&agent_id=kid-agent", []byte(`{"rotate_secret":true}`))
	if status != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", status, raw)
	}
	var rotated struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(raw, &rotated); err != nil || rotated.Secret == "" || rotated.Secret == created.Secret {
		t.Fatalf("rotate did not return a fresh one-time secret: %s", raw)
	}

	oldSecretReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/webhooks/k12-homework", strings.NewReader(string(eventBody)))
	oldSecretReq.Header.Set("Content-Type", "application/json")
	oldSecretReq.Header.Set(webhook.K12HeaderTimestamp, timestamp)
	oldSecretReq.Header.Set(webhook.K12HeaderNonce, "nonce-old-secret")
	oldSecretReq.Header.Set(webhook.K12HeaderSignature, webhook.K12Signature(created.Secret, timestamp, "nonce-old-secret", eventBody))
	oldSecretResp, err := ts.Client().Do(oldSecretReq)
	if err != nil {
		t.Fatal(err)
	}
	oldSecretResp.Body.Close()
	if oldSecretResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old secret status=%d", oldSecretResp.StatusCode)
	}

	status, raw = doK12WebhookAPI(t, ts.Client(), http.MethodDelete, ts.URL+"/api/v1/webhooks/k12-homework?user_id=parent-1&agent_id=kid-agent", nil)
	if status != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", status, raw)
	}
	status, _ = doK12WebhookAPI(t, ts.Client(), http.MethodPost, ts.URL+"/api/v1/webhooks/k12-homework", eventBody)
	if status != http.StatusNotFound {
		t.Fatalf("deleted receiver status=%d", status)
	}
}

func TestK12WebhookAPIDoesNotAddPublicManagementRoutes(t *testing.T) {
	srv := newWebhookTestServer(t)
	h := srv.routes()
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/webhooks/k12-homework"},
		{http.MethodPost, "/api/v1/webhooks/k12-homework/rotate"},
		{http.MethodGet, "/api/v1/webhooks/k12-homework/receipts"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("unexpected public route %s %s => %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestK12WebhookManagementGetRequiresAuthentication(t *testing.T) {
	srv := newWebhookTestServer(t)
	srv.cfg.Server.APIToken = "k12-management-token"
	h := srv.apiAuthMiddleware(srv.routes())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks?user_id=parent-1", nil)
	req.RemoteAddr = "198.51.100.9:43000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated K12 management GET status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestK12WebhookMutationRequiresBindingOwnerAndAgent(t *testing.T) {
	srv := newWebhookTestServer(t)
	_, _, err := srv.webhookMgr.CreateK12Binding(context.Background(), webhook.K12BindingInput{
		Name: "owner-bound", AgentID: "kid-agent", LearnerID: "kid-learner",
		AllowedEvents: []webhook.K12EventType{webhook.K12EventSubmissionRequested},
		CreatedBy:     "parent-a", Enabled: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPatch,
		"/api/v1/webhooks/owner-bound?user_id=parent-b&agent_id=other-agent", strings.NewReader(`{"enabled":true}`))
	req.SetPathValue("name", "owner-bound")
	rec := httptest.NewRecorder()
	srv.handleUpdateWebhook(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner mutation status=%d body=%s", rec.Code, rec.Body.String())
	}
	binding, getErr := srv.webhookMgr.GetK12Binding(context.Background(), "owner-bound")
	if getErr != nil || binding.Status != webhook.K12BindingDisabled {
		t.Fatalf("cross-owner mutation changed binding=%+v err=%v", binding, getErr)
	}
	deleteReq := httptest.NewRequest(http.MethodDelete,
		"/api/v1/webhooks/owner-bound?user_id=parent-b&agent_id=other-agent", nil)
	deleteReq.SetPathValue("name", "owner-bound")
	deleteRec := httptest.NewRecorder()
	srv.handleDeleteWebhook(deleteRec, deleteReq)
	if deleteRec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner delete status=%d body=%s", deleteRec.Code, deleteRec.Body.String())
	}
	if _, getErr := srv.webhookMgr.GetK12Binding(context.Background(), "owner-bound"); getErr != nil {
		t.Fatalf("cross-owner delete removed binding: %v", getErr)
	}
}

func TestRunK12WorkflowFromWebhookReturnsOnlyAfterDurableTerminal(t *testing.T) {
	srv := newWorkflowTestServer()
	srv.workflowStore.workflows["wf-k12-terminal"] = &WorkflowData{
		ID: "wf-k12-terminal", Name: "K12 terminal",
		Data: map[string]any{
			"scenario": "k12", "agent_id": "kid-agent", "learner_id": "kid-learner", "version": "v1",
		},
		Nodes: []any{}, Edges: []any{},
	}
	runID, err := srv.RunK12WorkflowFromWebhook(context.Background(), "wf-k12-terminal", "v1",
		"weekly review", "kid-agent", "kid-learner")
	if err != nil {
		t.Fatal(err)
	}
	srv.workflowStore.mu.RLock()
	run := srv.workflowStore.runs[runID]
	srv.workflowStore.mu.RUnlock()
	if run == nil || run.Status != "completed" || run.FinishedAt.IsZero() {
		t.Fatalf("method returned before durable workflow terminal: %+v", run)
	}
}

func TestRunK12WorkflowFromWebhookPropagatesFailedTerminal(t *testing.T) {
	srv := newWorkflowTestServer()
	srv.workflowStore.workflows["wf-k12-cycle"] = &WorkflowData{
		ID: "wf-k12-cycle", Name: "K12 invalid cycle",
		Data: map[string]any{
			"scenario": "k12", "agent_id": "kid-agent", "learner_id": "kid-learner", "version": "v1",
		},
		Nodes: []any{
			map[string]any{"id": "a", "type": "noop"},
			map[string]any{"id": "b", "type": "noop"},
		},
		Edges: []any{
			map[string]any{"source": "a", "target": "b"},
			map[string]any{"source": "b", "target": "a"},
		},
	}
	runID, err := srv.RunK12WorkflowFromWebhook(context.Background(), "wf-k12-cycle", "v1",
		"weekly review", "kid-agent", "kid-learner")
	if err == nil || runID == "" {
		t.Fatalf("failed workflow terminal run=%q err=%v", runID, err)
	}
	srv.workflowStore.mu.RLock()
	run := srv.workflowStore.runs[runID]
	srv.workflowStore.mu.RUnlock()
	if run == nil || run.Status != "failed" || run.Error == "" {
		t.Fatalf("failed terminal was not persisted before return: %+v", run)
	}
}
