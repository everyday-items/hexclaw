package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type canceledSendContextTransport struct {
	cancel         context.CancelFunc
	sawLiveSendCtx bool
	sendCalls      int
	sentReceipt    k12.DeliveryReceipt
}

func (f *canceledSendContextTransport) PrepareText(context.Context, string, string) (usecase.PreparedTextDelivery, error) {
	return usecase.PreparedTextDelivery{
		BindingID: "agent-rule:cancel-test",
		Target: k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: "dt-main", ChatID: "staff-1",
		},
		PayloadJSON: `{"text":"timeout regression"}`,
		RenderJSON:  `{"surface":"channel"}`,
	}, nil
}

func (f *canceledSendContextTransport) SendPrepared(ctx context.Context, receipt k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	f.sawLiveSendCtx = ctx.Err() == nil
	f.sendCalls++
	f.sentReceipt = receipt
	f.cancel()
	return usecase.DeliveryTransportAck{}, context.Canceled
}

func (*canceledSendContextTransport) QueryPrepared(context.Context, k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	return usecase.DeliveryTransportAck{}, nil
}

func TestDeliverySendCanceledContextStillPersistsOutcomeUnknown(t *testing.T) {
	d := newDataDeps(t)
	ctx, cancel := context.WithCancel(context.Background())
	fake := &canceledSendContextTransport{cancel: cancel}
	d.Delivery = fake

	result, created, err := d.PrepareAndSendText(
		ctx, "xiaoming", "tutoring_tips", "cancel-after-provider-send", "辅导要点",
	)
	if !fake.sawLiveSendCtx {
		t.Fatal("provider send must begin with a live request context")
	}
	if err != nil {
		t.Errorf("request cancellation must not prevent durable outcome persistence: %v", err)
	}
	if !created || result.Status != k12.DeliveryOutcomeUnknown {
		t.Errorf("canceled provider send must return durable outcome_unknown: created=%v receipt=%+v", created, result)
	}

	persisted, err := d.GetDeliveryReceipt(context.Background(), "xiaoming", fake.sentReceipt.DeliveryID)
	if err != nil {
		t.Fatalf("read durable receipt: %v", err)
	}
	if persisted.Status != k12.DeliveryOutcomeUnknown {
		t.Fatalf("receipt must converge from sending to outcome_unknown after cancellation, got %s", persisted.Status)
	}
	if fake.sendCalls != 1 {
		t.Fatalf("persisting the terminal receipt must not send to the provider again, got %d sends", fake.sendCalls)
	}
}
