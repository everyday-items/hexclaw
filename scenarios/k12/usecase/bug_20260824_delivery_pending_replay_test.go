package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type cancelFirstBatchTransport struct {
	batchTransport
	cancel context.CancelFunc
}

func (f *cancelFirstBatchTransport) SendPrepared(
	_ context.Context,
	receipt k12.DeliveryReceipt,
) (usecase.DeliveryTransportAck, error) {
	f.sends = append(f.sends, receipt)
	if len(f.sends) == 1 {
		f.cancel()
		return usecase.DeliveryTransportAck{
			Status:            k12.DeliveryOutcomeUnknown,
			ExternalMessageID: "provider-unknown-a",
			Detail:            "request canceled after provider send started",
		}, context.Canceled
	}
	return usecase.DeliveryTransportAck{
		Status:            k12.DeliveryDelivered,
		ExternalMessageID: "provider-delivered-b",
	}, nil
}

func TestDeliveryBatchReplayStartsOnlyNeverAttemptedPendingChildAfterRequestCancellation(t *testing.T) {
	d := newDataDeps(t)
	requestCtx, cancel := context.WithCancel(context.Background())
	fake := &cancelFirstBatchTransport{
		batchTransport: batchTransport{targets: batchTargets()},
		cancel:         cancel,
	}
	d.Delivery = fake

	first, created, firstErr := d.PrepareAndSendTextBatch(
		requestCtx,
		"xiaoming",
		"tutoring_tips",
		"tips-canceled-mid-batch",
		"辅导要点",
	)
	if !created || first.BatchID == "" {
		t.Fatalf("first command did not freeze a batch: created=%v batch=%+v err=%v", created, first, firstErr)
	}
	if firstErr == nil || !errors.Is(firstErr, context.Canceled) {
		t.Logf("first command returned a non-cancellation storage error after cancellation: %v", firstErr)
	}

	stored, err := d.GetDeliveryBatch(context.Background(), "xiaoming", first.BatchID)
	if err != nil {
		t.Fatalf("read frozen batch after canceled request: %v", err)
	}
	if len(stored.Receipts) != 2 || stored.Receipts[1].Status != k12.DeliveryPending {
		t.Fatalf("test precondition missing one never-attempted pending child: %+v", stored.Receipts)
	}
	if len(fake.sends) != 1 || fake.sends[0].Target.InstanceID != "bot-a" {
		t.Fatalf("canceled first request crossed an unexpected provider boundary: %+v", fake.sends)
	}

	replayed, replayCreated, err := d.PrepareAndSendTextBatch(
		context.Background(),
		"xiaoming",
		"tutoring_tips",
		"tips-canceled-mid-batch",
		"辅导要点",
	)
	if err != nil || replayCreated || replayed.BatchID != first.BatchID {
		t.Fatalf(
			"frozen command replay failed: created=%v first_batch=%q replay=%+v err=%v",
			replayCreated,
			first.BatchID,
			replayed,
			err,
		)
	}
	if len(fake.sends) != 2 {
		t.Fatalf("replay left a never-attempted child pending: sends=%d receipts=%+v", len(fake.sends), replayed.Receipts)
	}
	if fake.sends[1].Target.InstanceID != "bot-b" || fake.sends[1].Attempt != 1 {
		t.Fatalf("replay must start only the never-attempted child once: %+v", fake.sends)
	}
	if replayed.Receipts[1].Status != k12.DeliveryDelivered {
		t.Fatalf("pending child did not converge after frozen replay: %+v", replayed.Receipts[1])
	}
}

func TestDeliveryBatchReplayStateMatrixStartsOnlyPendingAttemptZeroOnce(t *testing.T) {
	d := newDataDeps(t)
	requestCtx, cancel := context.WithCancel(context.Background())
	targets := []usecase.ResolvedDeliveryTarget{
		{
			BindingID: "agent-rule:unknown",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-unknown", ChatID: "parent", Label: "dingtalk",
			},
		},
		{
			BindingID: "agent-rule:pending",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-pending", ChatID: "parent", Label: "dingtalk",
			},
		},
		{
			BindingID: "agent-rule:sending",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-sending", ChatID: "parent", Label: "dingtalk",
			},
		},
		{
			BindingID: "agent-rule:delivered",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-delivered", ChatID: "parent", Label: "dingtalk",
			},
		},
		{
			BindingID: "agent-rule:failed",
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", InstanceID: "bot-failed", ChatID: "parent", Label: "dingtalk",
			},
		},
	}
	fake := &cancelFirstBatchTransport{
		batchTransport: batchTransport{targets: targets},
		cancel:         cancel,
	}
	d.Delivery = fake

	first, created, firstErr := d.PrepareAndSendTextBatch(
		requestCtx,
		"xiaoming",
		"tutoring_tips",
		"tips-pending-state-matrix",
		"辅导要点",
	)
	if !created || first.BatchID == "" {
		t.Fatalf("first command did not freeze a batch: created=%v batch=%+v err=%v", created, first, firstErr)
	}
	if firstErr == nil || !errors.Is(firstErr, context.Canceled) {
		t.Logf("first command returned a non-cancellation storage error after cancellation: %v", firstErr)
	}
	if len(fake.sends) != 1 || fake.resolveCalls != 1 || fake.prepareCalls != 1 {
		t.Fatalf(
			"first command must resolve once and cross one provider boundary: resolve=%d prepare=%d sends=%d",
			fake.resolveCalls,
			fake.prepareCalls,
			len(fake.sends),
		)
	}

	stored, err := d.GetDeliveryBatch(context.Background(), "xiaoming", first.BatchID)
	if err != nil {
		t.Fatalf("read frozen state-matrix batch: %v", err)
	}
	if len(stored.Receipts) != len(targets) {
		t.Fatalf("state-matrix batch receipts=%d want=%d", len(stored.Receipts), len(targets))
	}
	if stored.Receipts[0].Status != k12.DeliveryOutcomeUnknown || stored.Receipts[0].Attempt != 1 {
		t.Fatalf("first attempted child must be outcome_unknown/attempt=1: %+v", stored.Receipts[0])
	}
	for i := 1; i < len(stored.Receipts); i++ {
		if stored.Receipts[i].Status != k12.DeliveryPending || stored.Receipts[i].Attempt != 0 {
			t.Fatalf("never-attempted child %d must remain pending/attempt=0: %+v", i, stored.Receipts[i])
		}
	}

	if _, began, err := d.Records.BeginDeliveryAttempt(
		context.Background(), "xiaoming", stored.Receipts[2].DeliveryID,
	); err != nil || !began {
		t.Fatalf("seed sending child: began=%v err=%v", began, err)
	}
	if _, began, err := d.Records.BeginDeliveryAttempt(
		context.Background(), "xiaoming", stored.Receipts[3].DeliveryID,
	); err != nil || !began {
		t.Fatalf("seed delivered child attempt: began=%v err=%v", began, err)
	}
	if _, err := d.Records.MarkDeliveryAccepted(
		context.Background(), "xiaoming", stored.Receipts[3].DeliveryID, "provider-delivered-seeded",
	); err != nil {
		t.Fatalf("seed delivered child provider identity: %v", err)
	}
	if _, err := d.Records.MarkDeliveryDelivered(
		context.Background(), "xiaoming", stored.Receipts[3].DeliveryID,
	); err != nil {
		t.Fatalf("seed delivered child terminal state: %v", err)
	}
	if _, began, err := d.Records.BeginDeliveryAttempt(
		context.Background(), "xiaoming", stored.Receipts[4].DeliveryID,
	); err != nil || !began {
		t.Fatalf("seed failed child attempt: began=%v err=%v", began, err)
	}
	if _, err := d.Records.MarkDeliveryFailed(
		context.Background(), "xiaoming", stored.Receipts[4].DeliveryID, "provider rejected seeded child",
	); err != nil {
		t.Fatalf("seed failed child terminal state: %v", err)
	}

	beforeReplay, err := d.GetDeliveryBatch(context.Background(), "xiaoming", first.BatchID)
	if err != nil {
		t.Fatalf("read seeded state-matrix batch: %v", err)
	}
	wantBeforeReplay := []struct {
		status  k12.DeliveryReceiptStatus
		attempt int
	}{
		{status: k12.DeliveryOutcomeUnknown, attempt: 1},
		{status: k12.DeliveryPending, attempt: 0},
		{status: k12.DeliverySending, attempt: 1},
		{status: k12.DeliveryDelivered, attempt: 1},
		{status: k12.DeliveryFailed, attempt: 1},
	}
	for i, want := range wantBeforeReplay {
		got := beforeReplay.Receipts[i]
		if got.Status != want.status || got.Attempt != want.attempt {
			t.Fatalf("seeded child %d = %s/attempt=%d want %s/attempt=%d: %+v",
				i, got.Status, got.Attempt, want.status, want.attempt, got)
		}
	}
	pendingID := beforeReplay.Receipts[1].DeliveryID

	replayed, replayCreated, err := d.PrepareAndSendTextBatch(
		context.Background(),
		"xiaoming",
		"tutoring_tips",
		"tips-pending-state-matrix",
		"辅导要点",
	)
	if err != nil || replayCreated || replayed.BatchID != first.BatchID {
		t.Fatalf(
			"first replay failed: created=%v first_batch=%q replay=%+v err=%v",
			replayCreated,
			first.BatchID,
			replayed,
			err,
		)
	}
	if len(fake.sends) != 2 || fake.sends[1].DeliveryID != pendingID ||
		fake.sends[1].Attempt != 1 {
		t.Fatalf("first replay must start only pending/attempt=0 child once: sends=%+v", fake.sends)
	}
	if len(fake.queries) != 0 {
		t.Fatalf("ordinary replay must not query any child: queries=%+v", fake.queries)
	}
	if fake.resolveCalls != 1 || fake.prepareCalls != 1 {
		t.Fatalf("ordinary replay re-resolved frozen targets: resolve=%d prepare=%d",
			fake.resolveCalls, fake.prepareCalls)
	}

	sendsAfterFirstReplay := len(fake.sends)
	queriesAfterFirstReplay := len(fake.queries)
	secondReplay, secondCreated, err := d.PrepareAndSendTextBatch(
		context.Background(),
		"xiaoming",
		"tutoring_tips",
		"tips-pending-state-matrix",
		"辅导要点",
	)
	if err != nil || secondCreated || secondReplay.BatchID != first.BatchID {
		t.Fatalf(
			"second replay failed: created=%v first_batch=%q replay=%+v err=%v",
			secondCreated,
			first.BatchID,
			secondReplay,
			err,
		)
	}
	if len(fake.sends) != sendsAfterFirstReplay || len(fake.queries) != queriesAfterFirstReplay {
		t.Fatalf("second replay crossed provider boundary: sends=%d queries=%d",
			len(fake.sends), len(fake.queries))
	}
	if fake.resolveCalls != 1 || fake.prepareCalls != 1 {
		t.Fatalf("second replay re-resolved frozen targets: resolve=%d prepare=%d",
			fake.resolveCalls, fake.prepareCalls)
	}

	wantAfterReplay := []k12.DeliveryReceiptStatus{
		k12.DeliveryOutcomeUnknown,
		k12.DeliveryDelivered,
		k12.DeliverySending,
		k12.DeliveryDelivered,
		k12.DeliveryFailed,
	}
	for i, wantStatus := range wantAfterReplay {
		got := secondReplay.Receipts[i]
		if got.Status != wantStatus || got.Attempt != 1 {
			t.Fatalf("second replay mutated child %d unexpectedly: got=%s/attempt=%d want=%s/attempt=1",
				i, got.Status, got.Attempt, wantStatus)
		}
	}
}
