package apihttp_test

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func seedC02FinalArtifact(t *testing.T, db *sql.DB, artifactID, digest, suffix string) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_grading_jobs
			(record_id,agent_name,status,dedupe_key,created_at,updated_at)
		VALUES(?,?,?,?,100,100)`,
		"job-"+suffix, "mingming", "completed", "job-"+suffix,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_grading_final_artifacts
			(artifact_id,agent_name,job_id,structure_version,coverage_status,
			 total_count,published_count,skipped_count,ordered_current_digests_json,
			 canonical_markdown,artifact_digest,summary_invocation_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,100,100)`,
		artifactID, "mingming", "job-"+suffix, 1, "complete",
		16, 16, 0, `["`+digest+`"]`, "14 道正确 / 2 道过程问题", digest,
		"summary-"+suffix,
	); err != nil {
		t.Fatal(err)
	}
}

func TestK12LiveC02SendRejectsClientTargetAndFreezesEveryBoundTarget(t *testing.T) {
	ctx := t.Context()
	const (
		artifactID = "grading-final-c02-all-bound"
		digest     = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-all-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-all-b"},
		},
	}
	var db *sql.DB
	h := newServerWithReceiptTransport(t, delivery, func(conn *sql.DB) {
		db = conn
		seedC02FinalArtifact(t, conn, artifactID, digest, "c02-all-bound")
	})

	withClientTarget := `{
		"agent":"mingming",
		"final_artifact_id":"grading-final-c02-all-bound",
		"final_artifact_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"expected_binding":{
			"binding_id":"agent-rule:101",
			"platform":"dingtalk",
			"instance_id":"bot-a",
			"chat_id":"parent"
		}
	}`
	rec, _ := do(t, h, http.MethodPost, "/tutoring-tips/send", withClientTarget)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("client-authored target must be rejected as unknown input: status=%d", rec.Code)
	}
	var batches, receipts int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM k12_delivery_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM k12_delivery_receipts`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || receipts != 0 || len(delivery.sends) != 0 {
		t.Fatalf("rejected client target changed delivery state: batches=%d receipts=%d sends=%d",
			batches, receipts, len(delivery.sends))
	}

	valid := `{
		"agent":"mingming",
		"final_artifact_id":"grading-final-c02-all-bound",
		"final_artifact_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}`
	rec, batch := do(t, h, http.MethodPost, "/tutoring-tips/send", valid)
	children, _ := batch["receipts"].([]any)
	if rec.Code != http.StatusOK || batch["status"] != string(k12.DeliveryBatchDelivered) ||
		len(children) != len(httpBatchTargets()) || len(delivery.sends) != len(httpBatchTargets()) {
		t.Fatalf("all-bound send: status=%d batch=%v receipts=%d sends=%d want=%d",
			rec.Code, batch, len(children), len(delivery.sends), len(httpBatchTargets()))
	}

	rec, replay := do(t, h, http.MethodPost, "/tutoring-tips/send", valid)
	if rec.Code != http.StatusOK || replay["batch_id"] != batch["batch_id"] ||
		len(delivery.sends) != len(httpBatchTargets()) {
		t.Fatalf("all-bound replay resent or changed identity: status=%d first=%v replay=%v sends=%d",
			rec.Code, batch["batch_id"], replay["batch_id"], len(delivery.sends))
	}
}

func TestK12LiveC02AllBoundOutcomeUnknownReplayQueriesFrozenReceipts(t *testing.T) {
	ctx := t.Context()
	const (
		artifactID = "grading-final-c02-outcome-unknown"
		digest     = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	)
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "dingtalk-c02-unknown-a"},
			{Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "dingtalk-c02-unknown-b"},
		},
		query: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-unknown-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-unknown-b"},
		},
	}
	var db *sql.DB
	h := newServerWithReceiptTransport(t, delivery, func(conn *sql.DB) {
		db = conn
		seedC02FinalArtifact(t, conn, artifactID, digest, "c02-outcome-unknown")
	})
	request := `{
		"agent":"mingming",
		"final_artifact_id":"grading-final-c02-outcome-unknown",
		"final_artifact_digest":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	}`

	rec, first := do(t, h, http.MethodPost, "/tutoring-tips/send", request)
	firstReceipts, _ := first["receipts"].([]any)
	if rec.Code != http.StatusOK || first["status"] != string(k12.DeliveryBatchOutcomeUnknown) ||
		len(firstReceipts) != len(httpBatchTargets()) || len(delivery.sends) != len(httpBatchTargets()) ||
		len(delivery.queries) != 0 {
		t.Fatalf("first all-bound outcome_unknown: status=%d body=%v sends=%d queries=%d",
			rec.Code, first, len(delivery.sends), len(delivery.queries))
	}

	rec, replay := do(t, h, http.MethodPost, "/tutoring-tips/send", request)
	if rec.Code != http.StatusOK || replay["batch_id"] != first["batch_id"] ||
		len(delivery.sends) != len(httpBatchTargets()) || len(delivery.queries) != 0 {
		t.Fatalf("all-bound command replay resent or queried implicitly: status=%d body=%v sends=%d queries=%d",
			rec.Code, replay, len(delivery.sends), len(delivery.queries))
	}
	rec, replay = do(
		t, h, http.MethodPost, "/delivery-batches/"+first["batch_id"].(string)+"/query",
		`{"agent":"mingming"}`,
	)
	replayReceipts, _ := replay["receipts"].([]any)
	if rec.Code != http.StatusOK || replay["batch_id"] != first["batch_id"] ||
		replay["object_id"] != first["object_id"] ||
		replay["status"] != string(k12.DeliveryBatchDelivered) ||
		len(delivery.sends) != len(httpBatchTargets()) ||
		len(delivery.queries) != len(httpBatchTargets()) ||
		len(replayReceipts) != len(firstReceipts) {
		t.Fatalf("all-bound query-only replay: status=%d body=%v sends=%d queries=%d",
			rec.Code, replay, len(delivery.sends), len(delivery.queries))
	}
	for i := range firstReceipts {
		firstReceipt, _ := firstReceipts[i].(map[string]any)
		replayReceipt, _ := replayReceipts[i].(map[string]any)
		if replayReceipt["delivery_id"] != firstReceipt["delivery_id"] ||
			replayReceipt["object_id"] != firstReceipt["object_id"] ||
			replayReceipt["attempt"] != firstReceipt["attempt"] ||
			replayReceipt["status"] != string(k12.DeliveryDelivered) {
			t.Errorf("frozen receipt %d identity/status drift: first=%v replay=%v", i, firstReceipt, replayReceipt)
		}
	}

	var batchCount, receiptCount, attempts, delivered int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM k12_delivery_batches`).Scan(&batchCount); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(attempt),0),
		COALESCE(sum(CASE WHEN status='delivered' THEN 1 ELSE 0 END),0)
		FROM k12_delivery_receipts`).Scan(&receiptCount, &attempts, &delivered); err != nil {
		t.Fatal(err)
	}
	if batchCount != 1 || receiptCount != len(httpBatchTargets()) ||
		attempts != len(httpBatchTargets()) || delivered != len(httpBatchTargets()) {
		t.Errorf("query-only ledger drift: batches=%d receipts=%d attempts=%d delivered=%d",
			batchCount, receiptCount, attempts, delivered)
	}
}
