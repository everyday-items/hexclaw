package apihttp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type httpBatchTransport struct {
	targets []usecase.ResolvedDeliveryTarget
	send    []usecase.DeliveryTransportAck
	query   []usecase.DeliveryTransportAck
	sends   []k12.DeliveryReceipt
	queries []k12.DeliveryReceipt
}

func (f *httpBatchTransport) ResolveTextTargets(context.Context, string) ([]usecase.ResolvedDeliveryTarget, error) {
	return append([]usecase.ResolvedDeliveryTarget(nil), f.targets...), nil
}

func (f *httpBatchTransport) PrepareTextForTargets(
	_ context.Context,
	content string,
	targets []usecase.ResolvedDeliveryTarget,
) ([]usecase.PreparedTextDelivery, error) {
	payload, _ := json.Marshal(map[string]string{"text": content})
	out := make([]usecase.PreparedTextDelivery, 0, len(targets))
	for _, target := range targets {
		out = append(out, usecase.PreparedTextDelivery{
			BindingID: target.BindingID, Target: target.Target,
			PayloadJSON: string(payload), RenderJSON: `{}`,
		})
	}
	return out, nil
}

func (*httpBatchTransport) PrepareText(context.Context, string, string) (usecase.PreparedTextDelivery, error) {
	panic("singleton preparation must not be used")
}

func (f *httpBatchTransport) SendPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.sends = append(f.sends, receipt)
	ack := f.send[0]
	f.send = f.send[1:]
	return ack, ack.Err
}

func (f *httpBatchTransport) QueryPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.queries = append(f.queries, receipt)
	ack := f.query[0]
	f.query = f.query[1:]
	return ack, ack.Err
}

func httpBatchTargets() []usecase.ResolvedDeliveryTarget {
	return []usecase.ResolvedDeliveryTarget{
		{
			BindingID: "agent-rule:101",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-a", ChatID: "parent", Label: "dingtalk",
			},
		},
		{
			BindingID: "agent-rule:102",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-b", ChatID: "parent", Label: "dingtalk",
			},
		},
	}
}

func addAccumulationHTTP(t *testing.T, h http.Handler, content string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"agent": "mingming", "subject": "语文", "entry_type": "好词好句", "content": content,
	})
	rec, out := do(t, h, "POST", "/accumulation", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("seed accumulation: status=%d body=%v", rec.Code, out)
	}
	recordID, _ := out["record_id"].(string)
	if recordID == "" {
		t.Fatalf("seed accumulation returned no record_id: %v", out)
	}
	return recordID
}

func TestDeliveryBatchHTTPReplayAndFailedOnlyRetry(t *testing.T) {
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "provider-a"},
			{Status: k12.DeliveryFailed, Detail: "bot-b rejected"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "provider-b"},
		},
	}
	h := newServerWithReceiptTransport(t, delivery)
	id := addAccumulationHTTP(t, h, "海内存知己，天涯若比邻")
	path := "/accumulation/" + id + "/send"

	rec, first := do(t, h, "POST", path, `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || first["status"] != string(k12.DeliveryBatchPartialFailed) {
		t.Fatalf("first batch response: status=%d body=%v", rec.Code, first)
	}
	batchID, _ := first["batch_id"].(string)
	children, _ := first["receipts"].([]any)
	if batchID == "" || len(children) != 2 || len(delivery.sends) != 2 {
		t.Fatalf("batch must expose two frozen children: body=%v sends=%d", first, len(delivery.sends))
	}

	rec, replay := do(t, h, "POST", path, `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || replay["batch_id"] != batchID || len(delivery.sends) != 2 {
		t.Fatalf("HTTP replay resent or changed batch: status=%d body=%v sends=%d",
			rec.Code, replay, len(delivery.sends))
	}
	rec, got := do(t, h, "GET", "/delivery-batches/"+batchID+"?agent=mingming", "")
	if rec.Code != http.StatusOK || got["status"] != string(k12.DeliveryBatchPartialFailed) {
		t.Fatalf("GET batch: status=%d body=%v", rec.Code, got)
	}
	rec, retried := do(t, h, "POST", "/delivery-batches/"+batchID+"/retry", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || retried["status"] != string(k12.DeliveryBatchDelivered) ||
		len(delivery.sends) != 3 || delivery.sends[2].Target.InstanceID != "bot-b" {
		t.Fatalf("failed-only retry: status=%d body=%v sends=%+v", rec.Code, retried, delivery.sends)
	}
}

func TestFinalizeSendHasNoTargetAndZeroBindingKeepsDraft(t *testing.T) {
	delivery := &httpBatchTransport{}
	h := newServerWithReceiptTransport(t, delivery)
	_, out := do(t, h, "POST", "/practice-sets/basket/items", `{"agent":"mingming",
        "item":{"subject":"数学","added_via":"weekly","question_markdown":"1+1=?",
        "expected_answer_markdown":"2","verification_status":"verified",
        "verification_evidence":"验算"}}`)
	id := out["record_id"].(string)
	path := "/practice-sets/" + id + "/finalize"

	rec, _ := do(t, h, "POST", path, `{"agent":"mingming","via":"send","target":"client-picked"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("send command must reject client target, got %d", rec.Code)
	}
	rec, noBinding := do(t, h, "POST", path, `{"agent":"mingming","via":"send"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("zero binding must fail, status=%d body=%v", rec.Code, noBinding)
	}
	_, draft := do(t, h, "GET", "/practice-sets/"+id+"?agent=mingming", "")
	if draft["status"] != k12.PracticeStatusDraft || draft["paper_no"] != nil ||
		draft["delivery_batch_id"] != nil {
		t.Fatalf("zero binding changed practice domain: %v", draft)
	}

	delivery.targets = httpBatchTargets()
	delivery.send = []usecase.DeliveryTransportAck{
		{Status: k12.DeliveryDelivered, ExternalMessageID: "paper-a"},
		{Status: k12.DeliveryDelivered, ExternalMessageID: "paper-b"},
	}
	rec, finalized := do(t, h, "POST", path, `{"agent":"mingming","via":"send"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("server-resolved finalize: status=%d body=%v", rec.Code, finalized)
	}
	set, _ := finalized["set"].(map[string]any)
	batch, _ := finalized["delivery_batch"].(map[string]any)
	if set["status"] != k12.PracticeStatusAssigned ||
		set["delivery_batch_id"] == "" ||
		set["delivery_target"] != nil ||
		batch["status"] != string(k12.DeliveryBatchDelivered) ||
		len(batch["receipts"].([]any)) != 2 {
		t.Fatalf("finalize must bind assigned set to two-recipient batch: %v", finalized)
	}
}

func TestAccumulationSendReadsServerRecordAndRejectsClientContent(t *testing.T) {
	delivery := &httpBatchTransport{
		targets: httpBatchTargets(),
		send: []usecase.DeliveryTransportAck{
			{Status: k12.DeliveryDelivered, ExternalMessageID: "accum-a"},
			{Status: k12.DeliveryDelivered, ExternalMessageID: "accum-b"},
		},
	}
	h := newServerWithReceiptTransport(t, delivery)
	_, added := do(t, h, "POST", "/accumulation",
		`{"agent":"mingming","subject":"语文","entry_type":"好词好句",`+
			`"content":"落霞与孤鹜齐飞","source":"《滕王阁序》"}`)
	recordID, _ := added["record_id"].(string)
	if recordID == "" {
		t.Fatalf("seed accumulation: %v", added)
	}

	rec, _ := do(t, h, "POST", "/accumulation/"+recordID+"/send",
		`{"agent":"mingming","content":"客户端伪造正文"}`)
	if rec.Code != http.StatusBadRequest || len(delivery.sends) != 0 {
		t.Fatalf("send command must reject client-authored content: status=%d sends=%d",
			rec.Code, len(delivery.sends))
	}

	rec, batch := do(t, h, "POST", "/accumulation/"+recordID+"/send", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK ||
		batch["object_kind"] != "accumulation" ||
		batch["object_id"] != recordID ||
		batch["status"] != string(k12.DeliveryBatchDelivered) ||
		len(batch["receipts"].([]any)) != 2 ||
		len(delivery.sends) != 2 {
		t.Fatalf("server accumulation batch: status=%d body=%v sends=%d",
			rec.Code, batch, len(delivery.sends))
	}
	for _, sent := range delivery.sends {
		var payload map[string]string
		if err := json.Unmarshal([]byte(sent.PayloadJSON), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["text"] != "落霞与孤鹜齐飞" {
			t.Fatalf("provider payload must come from stored accumulation: %v", payload)
		}
	}

	rec, replay := do(t, h, "POST", "/accumulation/"+recordID+"/send", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || replay["batch_id"] != batch["batch_id"] || len(delivery.sends) != 2 {
		t.Fatalf("accumulation replay changed batch or resent: status=%d body=%v sends=%d",
			rec.Code, replay, len(delivery.sends))
	}
}
