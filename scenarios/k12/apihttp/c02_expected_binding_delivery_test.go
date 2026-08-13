package apihttp_test

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestK12LiveC02ProcessIssueDingTalkExpectedBindingIsCheckedBeforeSend(t *testing.T) {
	ctx := t.Context()
	const (
		artifactID = "grading-final-c02-process"
		content    = "14 道正确 / 2 道过程问题\n第 15、16 题答案正确，过程需要关注；不记为错题。"
		digest     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	delivery := &httpBatchTransport{
		targets: httpBatchTargets()[:1],
		send: []usecase.DeliveryTransportAck{{
			Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-process",
		}},
	}
	var db *sql.DB
	h := newServerWithReceiptTransport(t, delivery, func(conn *sql.DB) {
		db = conn
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO k12_grading_jobs
				(record_id,agent_name,status,dedupe_key,created_at,updated_at)
			VALUES('job-c02-process','mingming','completed','job-c02-process',100,100);
			INSERT INTO k12_grading_final_artifacts
				(artifact_id,agent_name,job_id,structure_version,coverage_status,
				 total_count,published_count,skipped_count,ordered_current_digests_json,
				 canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			artifactID, "mingming", "job-c02-process", 1, "complete",
			16, 16, 0, `["`+digest+`"]`, content, digest, "summary-c02-process", 100, 100,
		); err != nil {
			t.Fatal(err)
		}
	})

	countRows := func() (int, int) {
		t.Helper()
		var batches, receipts int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM k12_delivery_batches`).Scan(&batches); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM k12_delivery_receipts`).Scan(&receipts); err != nil {
			t.Fatal(err)
		}
		return batches, receipts
	}

	const exact = `{
		"agent":"mingming",
		"final_artifact_id":"grading-final-c02-process",
		"final_artifact_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"expected_binding":{
			"binding_id":"agent-rule:101",
			"platform":"dingtalk",
			"instance_id":"bot-a",
			"chat_id":"parent"
		}
	}`
	invalidExpectations := []string{
		strings.Replace(exact, `"instance_id":"bot-a"`, `"instance_id":""`, 1),
		strings.Replace(exact, `"chat_id":"parent"`, `"chat_id":"parent","label":"client-picked"`, 1),
	}
	for i, invalid := range invalidExpectations {
		rec, out := do(t, h, http.MethodPost, "/tutoring-tips/send", invalid)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid expected_binding exact-set %d: status=%d body=%v", i+1, rec.Code, out)
		}
	}
	if batches, receipts := countRows(); batches != 0 || receipts != 0 || len(delivery.sends) != 0 {
		t.Fatalf("invalid expected_binding changed delivery state: batches=%d receipts=%d sends=%d",
			batches, receipts, len(delivery.sends))
	}

	// 预期绑定是 CAS 前置条件，不用于选择接收方。即使它的字段与某个目标匹配，
	// 只要权威集合含两个目标，也必须在写入任何投递领域数据或调用提供方之前失败。
	delivery.targets = httpBatchTargets()
	rec, out := do(t, h, http.MethodPost, "/tutoring-tips/send", exact)
	if rec.Code != http.StatusConflict || out["error"] != "binding_snapshot_conflict" {
		t.Fatalf("multi-binding snapshot must fail as a typed 409: status=%d body=%v", rec.Code, out)
	}
	if batches, receipts := countRows(); batches != 0 || receipts != 0 || len(delivery.sends) != 0 {
		t.Fatalf("multi-binding snapshot changed delivery state: batches=%d receipts=%d sends=%d",
			batches, receipts, len(delivery.sends))
	}

	delivery.targets = httpBatchTargets()[:1]
	mismatches := []string{
		strings.Replace(exact, `"binding_id":"agent-rule:101"`, `"binding_id":"agent-rule:other"`, 1),
		strings.Replace(exact, `"platform":"dingtalk"`, `"platform":"other"`, 1),
		strings.Replace(exact, `"instance_id":"bot-a"`, `"instance_id":"other"`, 1),
		strings.Replace(exact, `"chat_id":"parent"`, `"chat_id":"wrong-parent"`, 1),
	}
	for i, mismatch := range mismatches {
		rec, out = do(t, h, http.MethodPost, "/tutoring-tips/send", mismatch)
		if rec.Code != http.StatusConflict || out["error"] != "binding_snapshot_conflict" {
			t.Fatalf("binding field drift %d must fail as a typed 409: status=%d body=%v", i+1, rec.Code, out)
		}
	}
	if len(delivery.sends) != 0 {
		t.Fatalf("binding drift reached provider: sends=%d", len(delivery.sends))
	}
	if batches, receipts := countRows(); batches != 0 || receipts != 0 {
		t.Fatalf("binding drift changed delivery ledger: batches=%d receipts=%d", batches, receipts)
	}

	rec, first := do(t, h, http.MethodPost, "/tutoring-tips/send", exact)
	if rec.Code != http.StatusOK || first["status"] != string(k12.DeliveryBatchDelivered) {
		t.Fatalf("exact application binding send: status=%d body=%v", rec.Code, first)
	}
	firstReceipt := onlyBatchReceipt(t, first)
	firstTarget, _ := firstReceipt["target"].(map[string]any)
	if firstReceipt["binding_id"] != "agent-rule:101" ||
		firstTarget["platform"] != "dingtalk" ||
		firstTarget["instance_id"] != "bot-a" ||
		firstTarget["chat_id"] != "parent" || len(delivery.sends) != 1 {
		t.Fatalf("frozen receipt or provider count drifted: receipt=%v sends=%d", firstReceipt, len(delivery.sends))
	}
	if batches, receipts := countRows(); batches != 1 || receipts != 1 {
		t.Fatalf("exact send ledger: batches=%d receipts=%d", batches, receipts)
	}

	// 回放以不可变回执为准，因此忽略之后可变权威集合的扩展。
	delivery.targets = httpBatchTargets()
	rec, replay := do(t, h, http.MethodPost, "/tutoring-tips/send", exact)
	if rec.Code != http.StatusOK || replay["batch_id"] != first["batch_id"] || len(delivery.sends) != 1 {
		t.Fatalf("same artifact/binding replay resent or changed identity: status=%d body=%v sends=%d",
			rec.Code, replay, len(delivery.sends))
	}

	rec, out = do(t, h, http.MethodPost, "/tutoring-tips/send", mismatches[3])
	if rec.Code != http.StatusConflict || out["error"] != "binding_snapshot_conflict" || len(delivery.sends) != 1 {
		t.Fatalf("changed expected binding replay must fail without resend: status=%d body=%v sends=%d",
			rec.Code, out, len(delivery.sends))
	}
	if batches, receipts := countRows(); batches != 1 || receipts != 1 {
		t.Fatalf("changed replay mutated ledger: batches=%d receipts=%d", batches, receipts)
	}
}

func TestK12LiveC02ExpectedBindingOutcomeUnknownReplayQueriesFrozenReceipt(
	t *testing.T,
) {
	ctx := t.Context()
	const (
		artifactID = "grading-final-c02-outcome-unknown"
		digest     = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	)
	delivery := &httpBatchTransport{
		targets: httpBatchTargets()[:1],
		send: []usecase.DeliveryTransportAck{{
			Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "dingtalk-c02-unknown",
		}},
		query: []usecase.DeliveryTransportAck{{
			Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-unknown",
		}},
	}
	var db *sql.DB
	h := newServerWithReceiptTransport(t, delivery, func(conn *sql.DB) {
		db = conn
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO k12_grading_jobs
				(record_id,agent_name,status,dedupe_key,created_at,updated_at)
			VALUES('job-c02-unknown','mingming','completed','job-c02-unknown',100,100);
			INSERT INTO k12_grading_final_artifacts
				(artifact_id,agent_name,job_id,structure_version,coverage_status,
				 total_count,published_count,skipped_count,ordered_current_digests_json,
				 canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			artifactID, "mingming", "job-c02-unknown", 1, "complete",
			16, 16, 0, `["`+digest+`"]`, "14 道正确 / 2 道过程问题", digest,
			"summary-c02-unknown", 100, 100,
		); err != nil {
			t.Fatal(err)
		}
	})

	const exact = `{
		"agent":"mingming",
		"final_artifact_id":"grading-final-c02-outcome-unknown",
		"final_artifact_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"expected_binding":{
			"binding_id":"agent-rule:101",
			"platform":"dingtalk",
			"instance_id":"bot-a",
			"chat_id":"parent"
		}
	}`
	rec, first := do(t, h, http.MethodPost, "/tutoring-tips/send", exact)
	if rec.Code != http.StatusOK || first["status"] != string(k12.DeliveryBatchOutcomeUnknown) ||
		len(delivery.sends) != 1 || len(delivery.queries) != 0 {
		t.Fatalf("first outcome_unknown send: status=%d body=%v sends=%d queries=%d",
			rec.Code, first, len(delivery.sends), len(delivery.queries))
	}
	firstReceipt := onlyBatchReceipt(t, first)
	deliveryID, _ := firstReceipt["delivery_id"].(string)
	if deliveryID == "" {
		t.Fatalf("first receipt has no delivery_id: %v", firstReceipt)
	}

	drift := strings.Replace(exact, `"chat_id":"parent"`, `"chat_id":"other-parent"`, 1)
	rec, out := do(t, h, http.MethodPost, "/tutoring-tips/send", drift)
	if rec.Code != http.StatusConflict || out["error"] != "binding_snapshot_conflict" ||
		len(delivery.sends) != 1 || len(delivery.queries) != 0 {
		t.Fatalf("drift must fail before query/send: status=%d body=%v sends=%d queries=%d",
			rec.Code, out, len(delivery.sends), len(delivery.queries))
	}

	rec, replay := do(t, h, http.MethodPost, "/tutoring-tips/send", exact)
	if rec.Code != http.StatusOK || replay["batch_id"] != first["batch_id"] ||
		replay["object_id"] != first["object_id"] ||
		replay["status"] != string(k12.DeliveryBatchDelivered) {
		t.Errorf("same expected_binding replay did not query-converge frozen batch: status=%d body=%v",
			rec.Code, replay)
	}
	if len(delivery.sends) != 1 || len(delivery.queries) != 1 {
		t.Errorf("same replay transport calls: sends=%d queries=%d want 1/1",
			len(delivery.sends), len(delivery.queries))
	} else if delivery.queries[0].DeliveryID != deliveryID {
		t.Errorf("replay queried delivery_id=%q want frozen %q",
			delivery.queries[0].DeliveryID, deliveryID)
	}

	var batches, receipts, attempts int
	var receiptStatus string
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM k12_delivery_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(attempt),0), COALESCE(max(status),'')
		FROM k12_delivery_receipts`).Scan(&receipts, &attempts, &receiptStatus); err != nil {
		t.Fatal(err)
	}
	if batches != 1 || receipts != 1 || attempts != 1 || receiptStatus != string(k12.DeliveryDelivered) {
		t.Errorf("replay ledger drift: batches=%d receipts=%d attempts=%d status=%s",
			batches, receipts, attempts, receiptStatus)
	}
}

func TestK12LiveC02ExpectedBindingOutcomeUnknownReplayQueryOnlyOutcomes(
	t *testing.T,
) {
	tests := []struct {
		name       string
		query      usecase.DeliveryTransportAck
		wantStatus k12.DeliveryReceiptStatus
	}{
		{
			name: "provider confirms failed",
			query: usecase.DeliveryTransportAck{
				Status: k12.DeliveryFailed, ExternalMessageID: "dingtalk-c02-query-only",
				Detail: "provider confirmed failure",
			},
			wantStatus: k12.DeliveryFailed,
		},
		{
			name: "provider remains unknown",
			query: usecase.DeliveryTransportAck{
				Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "dingtalk-c02-query-only",
				Detail: "provider still cannot prove outcome",
			},
			wantStatus: k12.DeliveryOutcomeUnknown,
		},
		{
			name: "query transport error",
			query: usecase.DeliveryTransportAck{
				ExternalMessageID: "dingtalk-c02-query-only",
				Err:               errors.New("query transport unavailable"),
			},
			wantStatus: k12.DeliveryOutcomeUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			const digest = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
			delivery := &httpBatchTransport{
				targets: httpBatchTargets()[:1],
				send: []usecase.DeliveryTransportAck{{
					Status:            k12.DeliveryOutcomeUnknown,
					ExternalMessageID: "dingtalk-c02-query-only",
				}},
				query: []usecase.DeliveryTransportAck{tt.query},
			}
			var db *sql.DB
			h := newServerWithReceiptTransport(t, delivery, func(conn *sql.DB) {
				db = conn
				if _, err := conn.ExecContext(ctx, `
					INSERT INTO k12_grading_jobs
						(record_id,agent_name,status,dedupe_key,created_at,updated_at)
					VALUES('job-c02-query-only','mingming','completed','job-c02-query-only',100,100);
					INSERT INTO k12_grading_final_artifacts
						(artifact_id,agent_name,job_id,structure_version,coverage_status,
						 total_count,published_count,skipped_count,ordered_current_digests_json,
						 canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at)
					VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					"grading-final-c02-query-only", "mingming", "job-c02-query-only", 1, "complete",
					16, 16, 0, `["`+digest+`"]`, "14 道正确 / 2 道过程问题", digest,
					"summary-c02-query-only", 100, 100,
				); err != nil {
					t.Fatal(err)
				}
			})
			const exact = `{
				"agent":"mingming",
				"final_artifact_id":"grading-final-c02-query-only",
				"final_artifact_digest":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
				"expected_binding":{
					"binding_id":"agent-rule:101",
					"platform":"dingtalk",
					"instance_id":"bot-a",
					"chat_id":"parent"
				}
			}`
			rec, first := do(t, h, http.MethodPost, "/tutoring-tips/send", exact)
			if rec.Code != http.StatusOK || first["status"] != string(k12.DeliveryBatchOutcomeUnknown) {
				t.Fatalf("first outcome_unknown: status=%d body=%v", rec.Code, first)
			}
			firstReceipt := onlyBatchReceipt(t, first)

			rec, replay := do(t, h, http.MethodPost, "/tutoring-tips/send", exact)
			if rec.Code != http.StatusOK || replay["batch_id"] != first["batch_id"] ||
				replay["object_id"] != first["object_id"] ||
				len(delivery.sends) != 1 || len(delivery.queries) != 1 {
				t.Fatalf("query-only replay: status=%d body=%v sends=%d queries=%d",
					rec.Code, replay, len(delivery.sends), len(delivery.queries))
			}
			replayReceipt := onlyBatchReceipt(t, replay)
			if replayReceipt["delivery_id"] != firstReceipt["delivery_id"] ||
				replayReceipt["object_id"] != firstReceipt["object_id"] ||
				replayReceipt["attempt"] != firstReceipt["attempt"] ||
				replayReceipt["status"] != string(tt.wantStatus) {
				t.Errorf("frozen receipt identity/status drift: first=%v replay=%v want_status=%s",
					firstReceipt, replayReceipt, tt.wantStatus)
			}
			if tt.wantStatus == k12.DeliveryOutcomeUnknown &&
				replayReceipt["last_error"] != firstReceipt["last_error"] {
				t.Errorf("unknown query rewrote durable evidence: first=%v replay=%v",
					firstReceipt["last_error"], replayReceipt["last_error"])
			}
			var attempts int
			var status string
			if err := db.QueryRowContext(ctx, `SELECT attempt,status FROM k12_delivery_receipts`).Scan(
				&attempts, &status,
			); err != nil {
				t.Fatal(err)
			}
			if attempts != 1 || status != string(tt.wantStatus) {
				t.Errorf("query-only ledger attempt/status=%d/%s want 1/%s",
					attempts, status, tt.wantStatus)
			}
		})
	}
}
