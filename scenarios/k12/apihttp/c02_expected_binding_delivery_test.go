package apihttp_test

import (
	"database/sql"
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func seedAnnotatedGradingFinalArtifact(
	t *testing.T,
	db *sql.DB,
	artifactID, suffix, markdown string,
) k12.GradingFinalArtifact {
	t.Helper()
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	if _, err := db.ExecContext(t.Context(), `
		INSERT INTO k12_grading_jobs
			(record_id,agent_name,status,dedupe_key,created_at,updated_at)
		VALUES(?,?,?,?,100,100)`,
		"job-"+suffix, "mingming", "completed", "job-"+suffix,
	); err != nil {
		t.Fatal(err)
	}
	store := k12storage.NewStore(db, nil)
	ready, err := (&usecase.PageAssetRepository{Records: store}).Persist(
		t.Context(), "guardian-"+suffix, "mingming", tinyPNGBytes(t),
	)
	if err != nil {
		t.Fatalf("persist annotated grading asset: %v", err)
	}
	artifact := k12.GradingFinalArtifact{
		ArtifactID:                artifactID,
		AgentName:                 "mingming",
		JobID:                     "job-" + suffix,
		StructureVersion:          k12.GradingFinalArtifactStructureVersion,
		CoverageStatus:            k12.GradingFinalArtifactCoverageComplete,
		TotalCount:                1,
		PublishedCount:            1,
		OrderedCurrentDigestsJSON: `["grading-item-` + suffix + `"]`,
		CanonicalMarkdown:         markdown,
		SummaryInvocationID:       "summary-" + suffix,
		AnnotatedAssetOwnerScope:  "guardian-" + suffix,
		AnnotatedAssetID:          ready.Metadata.PageAssetID,
		AnnotatedMIME:             ready.Metadata.MediaType,
		AnnotatedDigest:           ready.Metadata.ContentDigest,
		OriginalSourceDigest:      ready.Metadata.ContentDigest,
		CreatedAt:                 100,
		UpdatedAt:                 100,
	}
	artifact.ArtifactDigest = k12.ComputeGradingFinalArtifactDigest(artifact)
	stored, replay, err := store.CommitGradingFinalArtifact(t.Context(), artifact, 0)
	if err != nil || replay {
		t.Fatalf("commit annotated grading final artifact: replay=%v err=%v", replay, err)
	}
	return stored
}

func TestK12LiveC02SendRejectsClientTargetAndFreezesEveryBoundTarget(t *testing.T) {
	ctx := t.Context()
	const artifactID = "grading-final-c02-all-bound"
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-all-a-markdown"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-all-a-image"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-all-b-markdown"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-all-b-image"},
		},
	}
	var db *sql.DB
	var artifact k12.GradingFinalArtifact
	h := newServerWithReceiptTransport(t, delivery, func(conn *sql.DB) {
		db = conn
		artifact = seedAnnotatedGradingFinalArtifact(
			t, conn, artifactID, "c02-all-bound", "14 道正确 / 2 道过程问题",
		)
	})

	withClientTarget := fmt.Sprintf(`{
		"agent":"mingming",
		"final_artifact_id":"grading-final-c02-all-bound",
		"final_artifact_digest":"%s",
		"expected_binding":{
			"binding_id":"agent-rule:101",
			"platform":"dingtalk",
			"instance_id":"bot-a",
			"chat_id":"parent"
		}
	}`, artifact.ArtifactDigest)
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

	valid := fmt.Sprintf(`{
		"agent":"mingming",
		"final_artifact_id":"grading-final-c02-all-bound",
		"final_artifact_digest":"%s"
	}`, artifact.ArtifactDigest)
	rec, batch := do(t, h, http.MethodPost, "/tutoring-tips/send", valid)
	children, _ := batch["receipts"].([]any)
	wantReceipts := 2 * len(httpBatchTargets())
	if rec.Code != http.StatusOK || batch["status"] != string(k12.DeliveryBatchDelivered) ||
		len(children) != wantReceipts || len(delivery.sends) != wantReceipts {
		t.Fatalf("all-bound send: status=%d batch=%v receipts=%d sends=%d want=%d",
			rec.Code, batch, len(children), len(delivery.sends), wantReceipts)
	}

	rec, replay := do(t, h, http.MethodPost, "/tutoring-tips/send", valid)
	if rec.Code != http.StatusOK || replay["batch_id"] != batch["batch_id"] ||
		len(delivery.sends) != wantReceipts {
		t.Fatalf("all-bound replay resent or changed identity: status=%d first=%v replay=%v sends=%d",
			rec.Code, batch["batch_id"], replay["batch_id"], len(delivery.sends))
	}
}

func TestK12LiveC02AllBoundOutcomeUnknownReplayQueriesFrozenReceipts(t *testing.T) {
	ctx := t.Context()
	const artifactID = "grading-final-c02-outcome-unknown"
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "dingtalk-c02-unknown-a-markdown"},
			{Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "dingtalk-c02-unknown-a-image"},
			{Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "dingtalk-c02-unknown-b-markdown"},
			{Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "dingtalk-c02-unknown-b-image"},
		},
		query: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-unknown-a-markdown"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-unknown-a-image"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-unknown-b-markdown"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "dingtalk-c02-unknown-b-image"},
		},
	}
	var db *sql.DB
	var artifact k12.GradingFinalArtifact
	h := newServerWithReceiptTransport(t, delivery, func(conn *sql.DB) {
		db = conn
		artifact = seedAnnotatedGradingFinalArtifact(
			t, conn, artifactID, "c02-outcome-unknown", "14 道正确 / 2 道过程问题",
		)
	})
	request := fmt.Sprintf(`{
		"agent":"mingming",
		"final_artifact_id":"grading-final-c02-outcome-unknown",
		"final_artifact_digest":"%s"
	}`, artifact.ArtifactDigest)
	wantReceipts := 2 * len(httpBatchTargets())

	rec, first := do(t, h, http.MethodPost, "/tutoring-tips/send", request)
	firstReceipts, _ := first["receipts"].([]any)
	if rec.Code != http.StatusOK || first["status"] != string(k12.DeliveryBatchOutcomeUnknown) ||
		len(firstReceipts) != wantReceipts || len(delivery.sends) != wantReceipts ||
		len(delivery.queries) != 0 {
		t.Fatalf("first all-bound outcome_unknown: status=%d body=%v sends=%d queries=%d",
			rec.Code, first, len(delivery.sends), len(delivery.queries))
	}

	rec, replay := do(t, h, http.MethodPost, "/tutoring-tips/send", request)
	if rec.Code != http.StatusOK || replay["batch_id"] != first["batch_id"] ||
		len(delivery.sends) != wantReceipts || len(delivery.queries) != 0 {
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
		len(delivery.sends) != wantReceipts ||
		len(delivery.queries) != wantReceipts ||
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
	if batchCount != 1 || receiptCount != wantReceipts ||
		attempts != wantReceipts || delivered != wantReceipts {
		t.Errorf("query-only ledger drift: batches=%d receipts=%d attempts=%d delivered=%d",
			batchCount, receiptCount, attempts, delivered)
	}
}
