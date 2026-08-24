package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type canceledSendContextTransport struct {
	cancel         context.CancelFunc
	sawLiveSendCtx bool
	sendCalls      int
	queryCalls     int
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

func (f *canceledSendContextTransport) QueryPrepared(context.Context, k12.DeliveryReceipt) (usecase.DeliveryTransportAck, error) {
	f.queryCalls++
	return usecase.DeliveryTransportAck{
		ExternalMessageID: "process-query-key-after-response-loss",
		Status:            k12.DeliveryDelivered,
	}, nil
}

func TestDeliveryPostSendResponseLossStaysParkedWithoutBlindResendWhenProviderHasNoQueryKey(t *testing.T) {
	d := newDataDeps(t)
	ctx, cancel := context.WithCancel(context.Background())
	fake := &canceledSendContextTransport{cancel: cancel}
	d.Delivery = fake

	unknown, created, err := d.PrepareAndSendText(
		ctx, "xiaoming", "tutoring_tips", "response-lost-after-provider-send", "辅导要点",
	)
	if err != nil {
		t.Fatalf("persist outcome after response loss: %v", err)
	}
	if !created || unknown.Status != k12.DeliveryOutcomeUnknown {
		t.Fatalf("response loss must first persist outcome_unknown: created=%v receipt=%+v", created, unknown)
	}
	if unknown.ExternalMessageID != "" {
		t.Fatalf("response loss must not invent a provider query identity, got %q", unknown.ExternalMessageID)
	}

	queried, err := d.QueryDeliveryReceipt(context.Background(), "xiaoming", unknown.DeliveryID)
	if !errors.Is(err, usecase.ErrDeliveryQueryUnavailable) {
		t.Fatalf("query without a provider identity error=%v, want ErrDeliveryQueryUnavailable", err)
	}
	if queried.Status != k12.DeliveryOutcomeUnknown {
		t.Fatalf("unsafe query must keep the receipt parked, got %+v", queried)
	}

	replayed, replayCreated, err := d.PrepareAndSendText(
		context.Background(), "xiaoming", "tutoring_tips", "response-lost-after-provider-send", "辅导要点",
	)
	if err != nil {
		t.Fatalf("replay parked delivery: %v", err)
	}
	if replayCreated || replayed.DeliveryID != unknown.DeliveryID || replayed.Status != k12.DeliveryOutcomeUnknown {
		t.Fatalf("replay must return the same parked receipt: created=%v receipt=%+v", replayCreated, replayed)
	}

	processed, err := d.RecoverDeliveryReceipts(context.Background(), "xiaoming")
	if err != nil {
		t.Fatalf("restart recovery: %v", err)
	}
	if processed != 1 {
		t.Fatalf("restart recovery processed=%d, want 1 parked receipt", processed)
	}
	if fake.sendCalls != 1 || fake.queryCalls != 0 {
		t.Fatalf("provider identity loss must never blind-send or fake-query, sends=%d queries=%d", fake.sendCalls, fake.queryCalls)
	}
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
