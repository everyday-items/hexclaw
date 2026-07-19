package channel

import (
	"context"
	"testing"
)

func TestDingTalkReceiptPortKeepsAcceptedSeparateFromDelivered(t *testing.T) {
	d := NewDingTalk()
	d.SetReceiptTransport(
		func(_ context.Context, to Target, _ Message) (DeliveryAck, error) {
			return DeliveryAck{ExternalMessageID: "pqk-1", Status: DeliveryAccepted, Target: to}, nil
		},
		func(_ context.Context, to Target, externalID string) (DeliveryAck, error) {
			return DeliveryAck{ExternalMessageID: externalID, Status: DeliveryDelivered, Target: to}, nil
		},
	)
	target := Target{Platform: "dingtalk", InstanceID: "pi-1", ChatID: "user-1"}
	ack, err := d.SendMessageWithReceipt(context.Background(), target, Message{Text: "辅导要点"})
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != DeliveryAccepted || ack.ExternalMessageID != "pqk-1" {
		t.Fatalf("send ack=%+v", ack)
	}
	terminal, err := d.QueryReceipt(context.Background(), target, ack.ExternalMessageID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != DeliveryDelivered || terminal.Target.ChatID != "user-1" {
		t.Fatalf("query ack=%+v", terminal)
	}
}

func TestDingTalkReceiptPortRejectsGroupBeforeTransport(t *testing.T) {
	d := NewDingTalk()
	called := false
	d.SetReceiptTransport(
		func(_ context.Context, _ Target, _ Message) (DeliveryAck, error) {
			called = true
			return DeliveryAck{}, nil
		},
		nil,
	)
	_, err := d.SendMessageWithReceipt(context.Background(), Target{Platform: "dingtalk", ChatID: "\x00dingtalk-group:cid"}, Message{Text: "x"})
	if err == nil || called {
		t.Fatalf("group target must fail before transport: called=%v err=%v", called, err)
	}
}
