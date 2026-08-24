package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func TestPrepareAndSendMessageBatchForTargetsKeepsInboundDirectTargetLocked(t *testing.T) {
	d := newDataDeps(t)
	fake := newMessagePartTransport()
	d.Delivery = fake
	locked := []usecase.ResolvedDeliveryTarget{fake.targets[1]}
	message := usecase.DeliveryMessage{
		Content: "## 作业批改\n\n14 道正确 / 2 道过程问题",
		Attachments: []usecase.DeliveryAttachment{{
			Name: "批改后的作业.png", MIME: "image/png", Data: []byte("annotated-image"),
		}},
	}

	batch, created, err := d.PrepareAndSendMessageBatchForTargets(
		context.Background(),
		"xiaoming", "dingtalk_photo_grading_reply", "inbound-1/final-digest",
		message, locked,
	)
	if err != nil || !created {
		t.Fatalf("prepare locked photo reply batch: created=%v batch=%+v err=%v", created, batch, err)
	}
	if fake.resolveCalls != 0 {
		t.Fatalf("locked reply re-resolved mutable bindings: calls=%d", fake.resolveCalls)
	}
	if len(batch.Receipts) != 2 || len(fake.sends) != 2 {
		t.Fatalf("one direct target must freeze exactly markdown+image: batch=%+v sends=%+v", batch, fake.sends)
	}
	for _, receipt := range batch.Receipts {
		if receipt.BindingID != locked[0].BindingID || receipt.Target != locked[0].Target ||
			receipt.Status != k12.DeliveryDelivered || receipt.ExternalMessageID == "" {
			t.Fatalf("receipt escaped the inbound direct target or lost provider evidence: %+v", receipt)
		}
	}
}
