package usecase_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type receiptTransport struct {
	prepared usecase.PreparedTextDelivery
	send     []usecase.DeliveryTransportAck
	query    []usecase.DeliveryTransportAck
	sends    []k12.DeliveryReceipt
	queries  []k12.DeliveryReceipt
}

func (f *receiptTransport) PrepareText(_ context.Context, _, _ string) (usecase.PreparedTextDelivery, error) {
	return f.prepared, nil
}

func (f *receiptTransport) SendPrepared(_ context.Context, receipt k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	f.sends = append(f.sends, receipt)
	if len(f.send) == 0 {
		return usecase.DeliveryTransportAck{}, errors.New("unexpected send")
	}
	ack := f.send[0]
	f.send = f.send[1:]
	return ack, ack.Err
}

func (f *receiptTransport) QueryPrepared(_ context.Context, receipt k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	f.queries = append(f.queries, receipt)
	if len(f.query) == 0 {
		return usecase.DeliveryTransportAck{}, errors.New("unexpected query")
	}
	ack := f.query[0]
	f.query = f.query[1:]
	return ack, ack.Err
}

func deliveryTransportFixture() *receiptTransport {
	return &receiptTransport{prepared: usecase.PreparedTextDelivery{
		BindingID:   "agent-rule:17",
		Target:      k12.DeliveryTarget{Platform: "dingtalk", InstanceID: "dt-main", ChatID: "staff-1", Label: "钉钉 · 妈妈"},
		PayloadJSON: `{"content":{"producer":"k12"},"text":"家长向内容"}`,
		RenderJSON:  `{"surface":"channel","renderer_version":"channel-markdown-readable-math-v1"}`,
	}}
}

func TestDeliveryReceiptProviderAcceptanceIsNotDeliveredAndReplayDoesNotResend(t *testing.T) {
	d := newDataDeps(t, "xiaoming", "other")
	fake := deliveryTransportFixture()
	fake.send = []usecase.DeliveryTransportAck{{
		Status: k12.DeliverySending, ExternalMessageID: "process-query-key-1",
	}}
	d.Delivery = fake

	got, created, err := d.PrepareAndSendText(context.Background(), "xiaoming", "tutoring_tips", "tips-1", "辅导要点")
	if err != nil || !created {
		t.Fatalf("first send: created=%v err=%v", created, err)
	}
	if got.Status != k12.DeliverySending || got.ExternalMessageID != "process-query-key-1" {
		t.Fatalf("provider acceptance must remain sending with evidence: %+v", got)
	}
	if got.BindingID != fake.prepared.BindingID || got.Target != fake.prepared.Target ||
		!strings.HasPrefix(got.PayloadDigest, "sha256:") || got.PayloadJSON != fake.prepared.PayloadJSON ||
		got.RenderJSON != fake.prepared.RenderJSON || got.Attempt != 1 {
		t.Fatalf("durable receipt did not freeze complete evidence: %+v", got)
	}

	replay, created, err := d.PrepareAndSendText(context.Background(), "xiaoming", "tutoring_tips", "tips-1", "辅导要点")
	if err != nil || created || replay.DeliveryID != got.DeliveryID {
		t.Fatalf("same delivery must replay one receipt: created=%v receipt=%+v err=%v", created, replay, err)
	}
	if len(fake.sends) != 1 {
		t.Fatalf("HTTP replay must not send again, sends=%d", len(fake.sends))
	}
	if _, err := d.GetDeliveryReceipt(context.Background(), "other", got.DeliveryID); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("receipt must be owner scoped, got %v", err)
	}
}

func TestDeliveryReceiptFailedRetryReusesFrozenReceipt(t *testing.T) {
	d := newDataDeps(t)
	fake := deliveryTransportFixture()
	fake.send = []usecase.DeliveryTransportAck{
		{Status: k12.DeliveryFailed, Detail: "平台明确拒绝"},
		{Status: k12.DeliverySending, ExternalMessageID: "process-query-key-2"},
	}
	d.Delivery = fake

	failed, _, err := d.PrepareAndSendText(context.Background(), "xiaoming", "tutoring_tips", "tutoring-tips-1", "辅导要点正文")
	if err != nil || failed.Status != k12.DeliveryFailed || failed.Attempt != 1 {
		t.Fatalf("first explicit failure must be durable: %+v err=%v", failed, err)
	}
	retried, err := d.RetryDeliveryReceipt(context.Background(), "xiaoming", failed.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.DeliveryID != failed.DeliveryID || retried.Status != k12.DeliverySending || retried.Attempt != 2 {
		t.Fatalf("failed retry must reuse row and advance attempt: %+v", retried)
	}
	if len(fake.sends) != 2 || fake.sends[0].PayloadJSON != fake.sends[1].PayloadJSON ||
		fake.sends[0].RenderJSON != fake.sends[1].RenderJSON || fake.sends[0].Target != fake.sends[1].Target {
		t.Fatalf("retry changed frozen evidence: %+v", fake.sends)
	}
}

func TestDeliveryReceiptOutcomeUnknownCannotBlindRetryAndOnlyQueryCanResolve(t *testing.T) {
	d := newDataDeps(t)
	fake := deliveryTransportFixture()
	fake.send = []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryOutcomeUnknown, ExternalMessageID: "process-query-key-3", Detail: "网络中断，结果未知",
	}}
	fake.query = []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryDelivered, ExternalMessageID: "process-query-key-3",
	}}
	d.Delivery = fake

	unknown, _, err := d.PrepareAndSendText(context.Background(), "xiaoming", "tutoring_tips", "tips-2", "另一份辅导要点")
	if err != nil || unknown.Status != k12.DeliveryOutcomeUnknown {
		t.Fatalf("unknown outcome must be durable: %+v err=%v", unknown, err)
	}
	if _, err := d.RetryDeliveryReceipt(context.Background(), "xiaoming", unknown.DeliveryID); err == nil {
		t.Fatal("outcome_unknown must reject blind resend")
	}
	if len(fake.sends) != 1 {
		t.Fatalf("blind retry reached transport, sends=%d", len(fake.sends))
	}

	resolved, err := d.QueryDeliveryReceipt(context.Background(), "xiaoming", unknown.DeliveryID)
	if err != nil || resolved.Status != k12.DeliveryDelivered {
		t.Fatalf("provider query must resolve unknown: %+v err=%v", resolved, err)
	}
	if len(fake.queries) != 1 || fake.queries[0].ExternalMessageID != "process-query-key-3" {
		t.Fatalf("query did not reuse external evidence: %+v", fake.queries)
	}
	replayed, err := d.QueryDeliveryReceipt(context.Background(), "xiaoming", unknown.DeliveryID)
	if err != nil || replayed != resolved || len(fake.queries) != 1 {
		t.Fatalf("terminal query replay must not call provider again: replay=%+v queries=%d err=%v", replayed, len(fake.queries), err)
	}
}

func TestDeliveryReceiptRestartRecoveryQueriesInFlightWithoutResend(t *testing.T) {
	d := newDataDeps(t)
	fake := deliveryTransportFixture()
	fake.send = []usecase.DeliveryTransportAck{{
		Status: k12.DeliverySending, ExternalMessageID: "process-query-key-restart",
	}}
	d.Delivery = fake
	started, _, err := d.PrepareAndSendText(context.Background(), "xiaoming", "tutoring_tips", "tutoring-tips-restart", "辅导要点正文")
	if err != nil || started.Status != k12.DeliverySending {
		t.Fatalf("seed in-flight receipt: %+v err=%v", started, err)
	}

	// A fresh Deps value models process reconstruction around the same durable store.
	restarted := d
	restartedTransport := deliveryTransportFixture()
	restartedTransport.query = []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryDelivered, ExternalMessageID: "process-query-key-restart",
	}}
	restarted.Delivery = restartedTransport
	recovered, err := restarted.RecoverDeliveryReceipts(context.Background(), "xiaoming")
	if err != nil || recovered != 1 {
		t.Fatalf("restart recovery: recovered=%d err=%v", recovered, err)
	}
	terminal, err := restarted.GetDeliveryReceipt(context.Background(), "xiaoming", started.DeliveryID)
	if err != nil || terminal.Status != k12.DeliveryDelivered {
		t.Fatalf("restart query did not converge: %+v err=%v", terminal, err)
	}
	if len(restartedTransport.sends) != 0 || len(restartedTransport.queries) != 1 {
		t.Fatalf("restart must query, never resend in-flight: sends=%d queries=%d", len(restartedTransport.sends), len(restartedTransport.queries))
	}
}
