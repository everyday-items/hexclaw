package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type httpReceiptTransport struct {
	send    []usecase.DeliveryTransportAck
	query   []usecase.DeliveryTransportAck
	sendN   int
	queryN  int
	content []string
}

func (f *httpReceiptTransport) PrepareText(_ context.Context, _ string, content string) (usecase.PreparedTextDelivery, error) {
	payload, _ := json.Marshal(map[string]string{"text": content})
	f.content = append(f.content, content)
	return usecase.PreparedTextDelivery{
		BindingID: "agent-rule:24",
		Target: k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: "bot-1", ChatID: "staff-1", Label: "钉钉 · 妈妈",
		},
		PayloadJSON: string(payload), RenderJSON: `{}`,
	}, nil
}

func (f *httpReceiptTransport) SendPrepared(_ context.Context, _ k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	f.sendN++
	ack := f.send[0]
	f.send = f.send[1:]
	return ack, nil
}

func (f *httpReceiptTransport) QueryPrepared(_ context.Context, _ k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	f.queryN++
	ack := f.query[0]
	f.query = f.query[1:]
	return ack, nil
}

func newServerWithReceiptTransport(t *testing.T, delivery usecase.DeliveryTransport) http.Handler {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming'),('other')`); err != nil {
		t.Fatal(err)
	}
	rt, err := assembly.Wire(
		db,
		fakeSolveExec{},
		assembly.WithAccumulationMetadataDeriver(fixedAccumulationMetadataDeriver{}),
		assembly.WithDeliveryTransport(delivery),
	)
	if err != nil {
		t.Fatal(err)
	}
	return apihttp.NewHandler(apihttp.Runtime{Views: rt.Registry.Views, Records: rt.Records, Deps: rt.Deps})
}

func onlyBatchReceipt(t *testing.T, batch map[string]any) map[string]any {
	t.Helper()
	items, _ := batch["receipts"].([]any)
	if len(items) != 1 {
		t.Fatalf("batch receipts=%v want singleton fixture", batch["receipts"])
	}
	receipt, _ := items[0].(map[string]any)
	return receipt
}

func TestAccumulationSendReturnsDurableReceiptAndQueryIsOnlyDeliveredProof(t *testing.T) {
	delivery := &httpReceiptTransport{
		send:  []usecase.DeliveryTransportAck{{Status: k12.DeliverySending, ExternalMessageID: "pqk-http-1"}},
		query: []usecase.DeliveryTransportAck{{Status: k12.DeliveryDelivered, ExternalMessageID: "pqk-http-1"}},
	}
	h := newServerWithReceiptTransport(t, delivery)
	id := addAccumulationHTTP(t, h, "海内存知己，天涯若比邻")

	rec, out := do(t, h, "POST", "/accumulation/"+id+"/send", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || out["status"] != "sending" {
		t.Fatalf("provider acceptance must return a sending DeliveryBatch: %d %v", rec.Code, out)
	}
	child := onlyBatchReceipt(t, out)
	deliveryID, _ := child["delivery_id"].(string)
	if deliveryID == "" || child["external_message_id"] != "pqk-http-1" ||
		child["binding_id"] != "agent-rule:24" || child["payload_digest"] == "" ||
		child["render_manifest_json"] == "" {
		t.Fatalf("receipt evidence incomplete: %v", child)
	}
	if delivery.sendN != 1 {
		t.Fatalf("send count=%d", delivery.sendN)
	}

	rec, got := do(t, h, "GET", "/delivery-receipts/"+deliveryID+"?agent=mingming", "")
	if rec.Code != http.StatusOK || got["status"] != "sending" {
		t.Fatalf("GET receipt: %d %v", rec.Code, got)
	}
	rec, resolved := do(t, h, "POST", "/delivery-receipts/"+deliveryID+"/query", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || resolved["status"] != "delivered" || delivery.queryN != 1 {
		t.Fatalf("query must establish delivery: %d %v queries=%d", rec.Code, resolved, delivery.queryN)
	}
	if rec, _ := do(t, h, "GET", "/delivery-receipts/"+deliveryID+"?agent=other", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner receipt read must be 404, got %d", rec.Code)
	}
}

func TestFailedReceiptHasExplicitSafeRetryAndUnknownRejectsIt(t *testing.T) {
	delivery := &httpReceiptTransport{send: []usecase.DeliveryTransportAck{
		{Status: k12.DeliveryFailed, Detail: "平台明确拒绝"},
		{Status: k12.DeliverySending, ExternalMessageID: "pqk-http-retry"},
		{Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "pqk-http-unknown", Detail: "结果未知"},
	}}
	h := newServerWithReceiptTransport(t, delivery)
	failedID := addAccumulationHTTP(t, h, "欲穷千里目，更上一层楼")

	_, failed := do(t, h, "POST", "/accumulation/"+failedID+"/send", `{"agent":"mingming"}`)
	if failed["status"] != "failed" {
		t.Fatalf("failed batch not returned: %v", failed)
	}
	deliveryID := onlyBatchReceipt(t, failed)["delivery_id"].(string)
	rec, retried := do(t, h, "POST", "/delivery-receipts/"+deliveryID+"/retry", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || retried["status"] != "sending" || retried["delivery_id"] != deliveryID {
		t.Fatalf("failed retry must reuse receipt: %d %v", rec.Code, retried)
	}

	unknownIDRecord := addAccumulationHTTP(t, h, "会当凌绝顶，一览众山小")
	_, unknown := do(t, h, "POST", "/accumulation/"+unknownIDRecord+"/send", `{"agent":"mingming"}`)
	if unknown["status"] != "outcome_unknown" {
		t.Fatalf("unknown batch not returned: %v", unknown)
	}
	unknownID := onlyBatchReceipt(t, unknown)["delivery_id"].(string)
	rec, _ = do(t, h, "POST", "/delivery-receipts/"+unknownID+"/retry", `{"agent":"mingming"}`)
	if rec.Code != http.StatusConflict || delivery.sendN != 3 {
		t.Fatalf("unknown blind retry must be rejected without send: status=%d sends=%d", rec.Code, delivery.sendN)
	}
}

func TestTutoringTipsSendUsesTheSameDurableReceiptProtocol(t *testing.T) {
	delivery := &httpReceiptTransport{send: []usecase.DeliveryTransportAck{{
		Status: k12.DeliverySending, ExternalMessageID: "pqk-tips",
	}}}
	h := newServerWithReceiptTransport(t, delivery)
	rec, out := do(t, h, "POST", "/tutoring-tips/send", `{
		"agent":"mingming","content":"【这份作业的辅导要点】五年级下\n知识点回顾\n小数乘法"
	}`)
	if rec.Code != http.StatusOK || out["status"] != "sending" || out["object_kind"] != "tutoring_tips" {
		t.Fatalf("tutoring-tips send must return durable batch: %d %v", rec.Code, out)
	}
	if len(delivery.content) != 1 || delivery.content[0] == "" {
		t.Fatalf("tutoring-tips content was not sent: %v", delivery.content)
	}
}
