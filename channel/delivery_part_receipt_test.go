package channel_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func TestDingTalkDeliveryPartTransportPreparesAndSendsExactlyOnePart(t *testing.T) {
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 本周练习",
		"## 本周练习",
		"",
		[]channel.Attachment{
			{Name: "page.png", MIME: "image/png", Data: []byte("image-bytes")},
			{Name: "practice.pdf", MIME: "application/pdf", Data: []byte("%PDF-1.7\n%%EOF\n")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := message.DeliveryParts()
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 || parts[0].Kind != messagecontent.PartMarkdown ||
		parts[1].MIME != "image/png" || parts[2].MIME != "application/pdf" {
		t.Fatalf("delivery parts=%+v", parts)
	}

	ding := channel.NewDingTalk()
	var prepared []channel.DeliveryPart
	var sent []channel.DeliveryPart
	ding.SetDeliveryPartTransport(
		func(_ context.Context, _ channel.Target, part channel.DeliveryPart) (string, error) {
			prepared = append(prepared, part)
			return "@media-pdf", nil
		},
		func(_ context.Context, to channel.Target, part channel.DeliveryPart) (channel.DeliveryAck, error) {
			sent = append(sent, part)
			return channel.DeliveryAck{
				ExternalMessageID: "pqk-part", Status: channel.DeliveryAccepted, Target: to,
			}, nil
		},
	)
	target := channel.Target{Platform: "dingtalk", InstanceID: "ding-main", ChatID: "parent-1"}
	resourceID, err := ding.PrepareDeliveryPartResource(context.Background(), target, parts[2])
	if err != nil || resourceID != "@media-pdf" || len(prepared) != 1 || prepared[0].Ordinal != 3 {
		t.Fatalf("prepare resource=%q parts=%+v err=%v", resourceID, prepared, err)
	}
	parts[2].PreparedResourceID = resourceID
	ack, err := ding.SendPreparedPartWithReceipt(context.Background(), target, parts[2])
	if err != nil || ack.ExternalMessageID != "pqk-part" || len(sent) != 1 || sent[0].Ordinal != 3 {
		t.Fatalf("send ack=%+v parts=%+v err=%v", ack, sent, err)
	}
	if sent[0].Text != "" || sent[0].Attachment == nil || sent[0].Attachment.Name != "practice.pdf" {
		t.Fatalf("PDF receipt resent another part: %+v", sent[0])
	}
}

func TestDingTalkDeliveryPartTransportRejectsGroupAndUnpreparedArtifact(t *testing.T) {
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12, "zh-CN", "## 练习", "## 练习", "",
		[]channel.Attachment{{Name: "practice.pdf", MIME: "application/pdf", Data: []byte("%PDF-1.7\n")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := message.DeliveryParts()
	if err != nil {
		t.Fatal(err)
	}
	ding := channel.NewDingTalk()
	called := 0
	ding.SetDeliveryPartTransport(
		func(context.Context, channel.Target, channel.DeliveryPart) (string, error) {
			called++
			return "@media", nil
		},
		func(context.Context, channel.Target, channel.DeliveryPart) (channel.DeliveryAck, error) {
			called++
			return channel.DeliveryAck{}, nil
		},
	)
	group := channel.Target{Platform: "dingtalk", ChatID: "\x00dingtalk-group:cid"}
	if _, err := ding.PrepareDeliveryPartResource(context.Background(), group, parts[1]); err == nil || called != 0 {
		t.Fatalf("group prepare crossed transport: calls=%d err=%v", called, err)
	}
	direct := channel.Target{Platform: "dingtalk", ChatID: "parent-1"}
	if _, err := ding.SendPreparedPartWithReceipt(context.Background(), direct, parts[1]); err == nil || called != 0 {
		t.Fatalf("unprepared artifact crossed transport: calls=%d err=%v", called, err)
	}
}
