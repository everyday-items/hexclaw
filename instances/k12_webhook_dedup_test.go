package instances_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
	"github.com/hexagon-codes/hexclaw/webhook"

	_ "modernc.org/sqlite"
)

func TestK12WebhookDedupOneReceiptForOneHundredConcurrentDeliveries(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "dedup.db"))
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
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	mgr.SetK12Clock(func() time.Time { return now })
	_, secret, err := mgr.CreateK12Binding(context.Background(), webhook.K12BindingInput{
		Name: "instance-dedupe", AgentID: "kid-a", LearnerID: "learner-a",
		AllowedEvents: []webhook.K12EventType{webhook.K12EventSubmissionRequested},
		CreatedBy:     "parent", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	mgr.SetK12Handler(func(context.Context, webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
		calls.Add(1)
		return webhook.K12DispatchResult{Reference: "job-one", Status: webhook.K12ReceiptSucceeded}, nil
	})
	body := []byte(`{"delivery_id":"provider-stable-id","event_type":"k12.submission.requested.v1","payload":{"text":"same"}}`)
	timestamp := now.Format(time.RFC3339)

	const deliveries = 100
	var wg sync.WaitGroup
	wg.Add(deliveries)
	results := make(chan string, deliveries)
	for i := 0; i < deliveries; i++ {
		go func(i int) {
			defer wg.Done()
			nonce := fmt.Sprintf("nonce-%03d", i)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/instance-dedupe", bytes.NewReader(body))
			req.SetPathValue("name", "instance-dedupe")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(webhook.K12HeaderTimestamp, timestamp)
			req.Header.Set(webhook.K12HeaderNonce, nonce)
			req.Header.Set(webhook.K12HeaderSignature, webhook.K12Signature(secret, timestamp, nonce, body))
			rec := httptest.NewRecorder()
			mgr.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusAccepted {
				results <- fmt.Sprintf("error:%d:%s", rec.Code, rec.Body.String())
				return
			}
			var response struct {
				Receipt webhook.K12Receipt `json:"receipt"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				results <- "error:decode:" + err.Error()
				return
			}
			results <- response.Receipt.ReceiptID
		}(i)
	}
	wg.Wait()
	close(results)
	unique := map[string]struct{}{}
	for result := range results {
		if len(result) >= 6 && result[:6] == "error:" {
			t.Fatal(result)
		}
		unique[result] = struct{}{}
	}
	if len(unique) != 1 {
		t.Fatalf("100 deliveries created %d receipts: %v", len(unique), unique)
	}
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("100 deliveries dispatched %d domain commands", calls.Load())
	}
}
