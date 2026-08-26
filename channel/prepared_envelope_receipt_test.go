package channel_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

// legacyPartReceiptPort 只实现既有逐 part 合同；新增 envelope 能力不得反向扩大旧接口。
type legacyPartReceiptPort struct{}

func (*legacyPartReceiptPort) Name() string { return "legacy" }

func (*legacyPartReceiptPort) SendText(context.Context, channel.Target, string) error { return nil }

func (*legacyPartReceiptPort) SendMessage(context.Context, channel.Target, channel.Message) error {
	return nil
}

func (*legacyPartReceiptPort) SendMessageWithReceipt(
	context.Context,
	channel.Target,
	channel.Message,
) (channel.DeliveryAck, error) {
	return channel.DeliveryAck{}, nil
}

func (*legacyPartReceiptPort) QueryReceipt(
	context.Context,
	channel.Target,
	string,
) (channel.DeliveryAck, error) {
	return channel.DeliveryAck{}, nil
}

func (*legacyPartReceiptPort) PrepareDeliveryPartResource(
	context.Context,
	channel.Target,
	channel.DeliveryPart,
) (string, error) {
	return "prepared", nil
}

func (*legacyPartReceiptPort) SendPreparedPartWithReceipt(
	context.Context,
	channel.Target,
	channel.DeliveryPart,
) (channel.DeliveryAck, error) {
	return channel.DeliveryAck{}, nil
}

var _ channel.PartReceiptPort = (*legacyPartReceiptPort)(nil)
var _ channel.PreparedEnvelopeReceiptPort = channel.NewDingTalk()
var _ channel.PreparedEnvelopePreflightPort = channel.NewDingTalk()

func TestDingTalkPreparedEnvelopePreflightUsesSideEffectFreeCallback(t *testing.T) {
	direct := channel.Target{Platform: "dingtalk", InstanceID: "pi-family", ChatID: "parent-1"}
	ding := channel.NewDingTalk()
	calls := 0
	ding.SetPreparedEnvelopePreflight(func(
		_ context.Context,
		gotTarget channel.Target,
		envelope channel.PreparedEnvelope,
	) error {
		calls++
		if gotTarget != direct {
			t.Fatalf("preflight target=%+v want=%+v", gotTarget, direct)
		}
		if envelope.Parts[1].PreparedResourceID != "@prepared-image" {
			return fmt.Errorf("invalid DingTalk prepared image reference")
		}
		return nil
	})

	valid := preparedImageEnvelopeFixture(t, "## 点评", "art.png", []byte("art-image"))
	if err := ding.PreflightPreparedEnvelope(context.Background(), direct, valid); err != nil {
		t.Fatalf("合法 envelope preflight 失败: %v", err)
	}
	invalid := preparedImageEnvelopeFixture(t, "## 点评", "art.png", []byte("art-image"))
	invalid.Parts[1].PreparedResourceID = "asset://bad"
	if err := ding.PreflightPreparedEnvelope(context.Background(), direct, invalid); err == nil {
		t.Fatal("非法平台媒体引用必须在 preflight 失败")
	}
	if calls != 2 {
		t.Fatalf("preflight callback calls=%d want 2", calls)
	}
}

func TestDingTalkPreparedEnvelopePreflightFailsClosedWithoutCallback(t *testing.T) {
	direct := channel.Target{Platform: "dingtalk", InstanceID: "pi-family", ChatID: "parent-1"}
	ding := channel.NewDingTalk()
	envelope := preparedImageEnvelopeFixture(t, "## 点评", "art.png", []byte("art-image"))

	if err := ding.PreflightPreparedEnvelope(context.Background(), direct, envelope); err == nil {
		t.Fatal("missing platform preflight callback must fail closed")
	}
}

func TestDingTalkPreparedEnvelopeSendsOneOrderedMarkdownImageCallback(t *testing.T) {
	envelope := preparedEnvelopeFixture(t, "## 美术作品点评", "art.png", "image/png", []byte("art-image"))
	direct := channel.Target{Platform: "dingtalk", InstanceID: "family-bot", ChatID: "parent-1"}
	ding := channel.NewDingTalk()

	prepareCalls := 0
	ding.SetDeliveryPartTransport(
		func(_ context.Context, gotTarget channel.Target, part channel.DeliveryPart) (string, error) {
			prepareCalls++
			if gotTarget != direct || part.Kind != messagecontent.PartArtifact || part.Ordinal != 2 {
				t.Fatalf("prepared wrong target/part: target=%+v part=%+v", gotTarget, part)
			}
			return "@prepared-art-image", nil
		},
		func(context.Context, channel.Target, channel.DeliveryPart) (channel.DeliveryAck, error) {
			t.Fatal("prepared envelope must not fall back to the legacy per-part sender")
			return channel.DeliveryAck{}, nil
		},
	)
	resourceID, err := ding.PrepareDeliveryPartResource(context.Background(), direct, envelope.Parts[1])
	if err != nil {
		t.Fatalf("prepare image: %v", err)
	}
	envelope.Parts[1].PreparedResourceID = resourceID

	callbackCalls := 0
	ding.SetPreparedEnvelopeTransport(func(
		_ context.Context,
		gotTarget channel.Target,
		got channel.PreparedEnvelope,
	) (channel.DeliveryAck, error) {
		callbackCalls++
		if gotTarget != direct || len(got.Parts) != 2 {
			t.Fatalf("callback target/envelope drifted: target=%+v envelope=%+v", gotTarget, got)
		}
		markdown, image := got.Parts[0], got.Parts[1]
		if markdown.Kind != messagecontent.PartMarkdown || markdown.Ordinal != 1 ||
			image.Kind != messagecontent.PartArtifact || image.Ordinal != 2 ||
			image.MIME != "image/png" || image.PreparedResourceID != "@prepared-art-image" {
			t.Fatalf("callback did not receive ordered prepared markdown+image: %+v", got.Parts)
		}
		return channel.DeliveryAck{
			ExternalMessageID: "dingtalk-envelope-1",
			Status:            channel.DeliveryAccepted,
		}, nil
	})

	ack, err := ding.SendPreparedEnvelopeWithReceipt(context.Background(), direct, envelope)
	if err != nil {
		t.Fatalf("send prepared envelope: %v", err)
	}
	if prepareCalls != 1 || callbackCalls != 1 {
		t.Fatalf("prepare calls=%d callback calls=%d, want 1/1", prepareCalls, callbackCalls)
	}
	if ack.ExternalMessageID != "dingtalk-envelope-1" || ack.Status != channel.DeliveryAccepted ||
		ack.Target != direct {
		t.Fatalf("prepared envelope ack=%+v", ack)
	}
}

func TestDingTalkPreparedEnvelopeAllowsImagePrefixBeforeIndependentPDF(t *testing.T) {
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 美术作品点评",
		"## 美术作品点评",
		"",
		[]channel.Attachment{
			{Name: "art.png", MIME: "image/png", Data: []byte("art-image")},
			{Name: "source.pdf", MIME: "application/pdf", Data: []byte("%PDF-1.7\n%%EOF\n")},
		},
	)
	if err != nil {
		t.Fatalf("build canonical message: %v", err)
	}
	parts, err := message.DeliveryParts()
	if err != nil {
		t.Fatalf("freeze delivery parts: %v", err)
	}
	parts[1].PreparedResourceID = "@prepared-art-image"
	envelope := channel.PreparedEnvelope{Parts: parts[:2]}

	direct := channel.Target{Platform: "dingtalk", InstanceID: "family-bot", ChatID: "parent-1"}
	ding := channel.NewDingTalk()
	callbackCalls := 0
	ding.SetPreparedEnvelopeTransport(func(
		_ context.Context,
		gotTarget channel.Target,
		got channel.PreparedEnvelope,
	) (channel.DeliveryAck, error) {
		callbackCalls++
		if gotTarget != direct || len(got.Parts) != 2 || got.Parts[1].MIME != "image/png" {
			t.Fatalf("callback must receive only markdown+image prefix: target=%+v envelope=%+v", gotTarget, got)
		}
		return channel.DeliveryAck{ExternalMessageID: "dingtalk-envelope-pdf-1", Status: channel.DeliveryAccepted}, nil
	})

	ack, err := ding.SendPreparedEnvelopeWithReceipt(context.Background(), direct, envelope)
	if err != nil {
		t.Fatalf("send image prefix before independent PDF: %v", err)
	}
	if callbackCalls != 1 || ack.ExternalMessageID != "dingtalk-envelope-pdf-1" || ack.Target != direct {
		t.Fatalf("callback calls=%d ack=%+v", callbackCalls, ack)
	}
}

func TestDingTalkPreparedEnvelopeFailsClosedBeforeCallback(t *testing.T) {
	direct := channel.Target{Platform: "dingtalk", InstanceID: "family-bot", ChatID: "parent-1"}
	group := channel.Target{Platform: "dingtalk", InstanceID: "family-bot", ChatID: "\x00dingtalk-group:cid"}

	tests := []struct {
		name     string
		target   channel.Target
		envelope func(*testing.T) channel.PreparedEnvelope
	}{
		{
			name:   "group target",
			target: group,
			envelope: func(t *testing.T) channel.PreparedEnvelope {
				return preparedImageEnvelopeFixture(t, "## 点评", "art.png", []byte("art-image"))
			},
		},
		{
			name:   "PDF artifact",
			target: direct,
			envelope: func(t *testing.T) channel.PreparedEnvelope {
				return preparedEnvelopeFixture(
					t, "## 练习卷", "practice.pdf", "application/pdf", []byte("%PDF-1.7\n%%EOF\n"),
				)
			},
		},
		{
			name:   "out of order",
			target: direct,
			envelope: func(t *testing.T) channel.PreparedEnvelope {
				envelope := preparedImageEnvelopeFixture(t, "## 点评", "art.png", []byte("art-image"))
				envelope.Parts[0], envelope.Parts[1] = envelope.Parts[1], envelope.Parts[0]
				return envelope
			},
		},
		{
			name:   "unprepared image",
			target: direct,
			envelope: func(t *testing.T) channel.PreparedEnvelope {
				return preparedEnvelopeFixture(t, "## 点评", "art.png", "image/png", []byte("art-image"))
			},
		},
		{
			name:   "cross canonical root",
			target: direct,
			envelope: func(t *testing.T) channel.PreparedEnvelope {
				first := preparedImageEnvelopeFixture(t, "## 第一份点评", "first.png", []byte("first-image"))
				second := preparedImageEnvelopeFixture(t, "## 第二份点评", "second.png", []byte("second-image"))
				first.Parts[1] = second.Parts[1]
				return first
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ding := channel.NewDingTalk()
			callbackCalls := 0
			ding.SetPreparedEnvelopeTransport(func(
				context.Context,
				channel.Target,
				channel.PreparedEnvelope,
			) (channel.DeliveryAck, error) {
				callbackCalls++
				return channel.DeliveryAck{Status: channel.DeliveryAccepted}, nil
			})

			ack, err := ding.SendPreparedEnvelopeWithReceipt(context.Background(), tt.target, tt.envelope(t))
			if err == nil {
				t.Fatalf("invalid prepared envelope unexpectedly succeeded: ack=%+v", ack)
			}
			if callbackCalls != 0 {
				t.Fatalf("invalid prepared envelope crossed callback: calls=%d err=%v", callbackCalls, err)
			}
			if ack.Status != channel.DeliveryFailed || ack.Target != tt.target {
				t.Fatalf("fail-closed ack=%+v target=%+v", ack, tt.target)
			}
		})
	}
}

func TestDingTalkPreparedEnvelopeRejectsNilCallback(t *testing.T) {
	direct := channel.Target{Platform: "dingtalk", InstanceID: "family-bot", ChatID: "parent-1"}
	ding := channel.NewDingTalk()
	envelope := preparedImageEnvelopeFixture(t, "## 点评", "art.png", []byte("art-image"))

	ack, err := ding.SendPreparedEnvelopeWithReceipt(context.Background(), direct, envelope)
	if err == nil {
		t.Fatalf("nil prepared-envelope callback unexpectedly succeeded: ack=%+v", ack)
	}
	if ack.Status != channel.DeliveryFailed || ack.Target != direct {
		t.Fatalf("nil callback fail-closed ack=%+v", ack)
	}
}

func preparedImageEnvelopeFixture(
	t *testing.T,
	markdown string,
	name string,
	data []byte,
) channel.PreparedEnvelope {
	t.Helper()
	envelope := preparedEnvelopeFixture(t, markdown, name, "image/png", data)
	envelope.Parts[1].PreparedResourceID = "@prepared-image"
	return envelope
}

func preparedEnvelopeFixture(
	t *testing.T,
	markdown string,
	name string,
	mime string,
	data []byte,
) channel.PreparedEnvelope {
	t.Helper()
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		markdown,
		markdown,
		"",
		[]channel.Attachment{{Name: name, MIME: mime, Data: data}},
	)
	if err != nil {
		t.Fatalf("build canonical message: %v", err)
	}
	parts, err := message.DeliveryParts()
	if err != nil {
		t.Fatalf("freeze delivery parts: %v", err)
	}
	return channel.PreparedEnvelope{Parts: parts}
}
