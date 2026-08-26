package main

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// creativeEnvelopeChannel 只记录 composite envelope 与旧发送入口的物理调用次数。
type creativeEnvelopeChannel struct {
	envelopes       [][]channel.DeliveryPart
	preflights      [][]channel.DeliveryPart
	legacyMessages  int
	legacyPartSends int
}

func (c *creativeEnvelopeChannel) Name() string { return "dingtalk" }

func (c *creativeEnvelopeChannel) SendText(context.Context, channel.Target, string) error {
	c.legacyMessages++
	return nil
}

func (c *creativeEnvelopeChannel) SendMessage(
	context.Context,
	channel.Target,
	channel.Message,
) error {
	c.legacyMessages++
	return nil
}

func (c *creativeEnvelopeChannel) SendMessageWithReceipt(
	context.Context,
	channel.Target,
	channel.Message,
) (channel.DeliveryAck, error) {
	c.legacyMessages++
	return channel.DeliveryAck{Status: channel.DeliveryAccepted}, nil
}

func (c *creativeEnvelopeChannel) QueryReceipt(
	context.Context,
	channel.Target,
	string,
) (channel.DeliveryAck, error) {
	return channel.DeliveryAck{Status: channel.DeliveryDelivered}, nil
}

func (c *creativeEnvelopeChannel) PrepareDeliveryPartResource(
	context.Context,
	channel.Target,
	channel.DeliveryPart,
) (string, error) {
	return "", fmt.Errorf("unexpected media preparation")
}

func (c *creativeEnvelopeChannel) SendPreparedPartWithReceipt(
	context.Context,
	channel.Target,
	channel.DeliveryPart,
) (channel.DeliveryAck, error) {
	c.legacyPartSends++
	return channel.DeliveryAck{Status: channel.DeliveryAccepted}, nil
}

func (c *creativeEnvelopeChannel) SendPreparedEnvelopeWithReceipt(
	_ context.Context,
	to channel.Target,
	envelope channel.PreparedEnvelope,
) (channel.DeliveryAck, error) {
	copyParts := append([]channel.DeliveryPart(nil), envelope.Parts...)
	c.envelopes = append(c.envelopes, copyParts)
	return channel.DeliveryAck{
		ExternalMessageID: "creative-envelope-query-key",
		Status:            channel.DeliveryAccepted,
		Target:            to,
	}, nil
}

func (c *creativeEnvelopeChannel) PreflightPreparedEnvelope(
	_ context.Context,
	_ channel.Target,
	envelope channel.PreparedEnvelope,
) error {
	copyParts := append([]channel.DeliveryPart(nil), envelope.Parts...)
	c.preflights = append(c.preflights, copyParts)
	if envelope.Parts[1].PreparedResourceID != "@creative-image-media" {
		return fmt.Errorf("invalid DingTalk prepared image reference")
	}
	return nil
}

func creativeEnvelopeFixture(
	t *testing.T,
	attachment k12usecase.DeliveryAttachment,
) (*k12IMDeliverer, *creativeEnvelopeChannel, []k12.DeliveryReceipt) {
	t.Helper()
	d, dispatcher, registry := newDelivererFixture(t)
	transport := &creativeEnvelopeChannel{}
	registry.Register(transport)
	bindRule(t, dispatcher, "dingtalk", "bot-creative", "parent-creative", "child-a")
	d.MarkReady()

	targets, err := d.ResolveTextTargets(context.Background(), "child-a")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := d.PrepareMessageForTargets(
		context.Background(),
		k12usecase.DeliveryMessage{
			Content:     "## 可见证据\n\n- 彩虹颜色清楚。\n\n## 先这样肯定\n\n- 配色很完整。",
			Attachments: []k12usecase.DeliveryAttachment{attachment},
		},
		targets,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 2 {
		t.Fatalf("fixture must freeze markdown plus one artifact, got %d", len(prepared))
	}

	receipts := make([]k12.DeliveryReceipt, 0, len(prepared))
	for i, item := range prepared {
		receipt := k12.DeliveryReceipt{
			DeliveryID:    fmt.Sprintf("creative-envelope-part-%d", i+1),
			BatchID:       "creative-envelope-batch",
			BatchOrdinal:  i + 1,
			PartKind:      item.PartKind,
			PartMIME:      item.PartMIME,
			PartOrdinal:   item.PartOrdinal,
			PartDigest:    item.PartDigest,
			AgentName:     "child-a",
			ObjectKind:    "creative_work",
			ObjectID:      "creative-work-1",
			BindingID:     item.BindingID,
			Target:        item.Target,
			Status:        k12.DeliverySending,
			DedupeKey:     fmt.Sprintf("creative-envelope-dedupe-%d", i+1),
			PayloadDigest: deliveryPayloadDigest(item.PayloadJSON),
			PayloadJSON:   item.PayloadJSON,
			RenderJSON:    item.RenderJSON,
			Attempt:       1,
		}
		if item.PartKind == messagecontent.PartArtifact {
			receipt.PreparedResourceID = "@creative-image-media"
		}
		receipts = append(receipts, receipt)
	}
	return d, transport, receipts
}

func TestK12IMDelivererCreativeWorkRestoresOnePreparedEnvelope(t *testing.T) {
	d, transport, receipts := creativeEnvelopeFixture(t, k12usecase.DeliveryAttachment{
		Name: "美术作品.png",
		MIME: "image/png",
		Data: []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x01},
	})

	ack, err := d.SendPreparedEnvelope(context.Background(), receipts)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != k12.DeliverySending || ack.ExternalMessageID != "creative-envelope-query-key" {
		t.Fatalf("provider acceptance must remain sending with one query key: %+v", ack)
	}
	if len(transport.envelopes) != 1 || transport.legacyMessages != 0 || transport.legacyPartSends != 0 {
		t.Fatalf("creative work must use one envelope and zero legacy sends: envelopes=%d messages=%d parts=%d",
			len(transport.envelopes), transport.legacyMessages, transport.legacyPartSends)
	}

	parts := transport.envelopes[0]
	if len(parts) != 2 || parts[0].Kind != messagecontent.PartMarkdown ||
		parts[1].Kind != messagecontent.PartArtifact || parts[1].MIME != "image/png" {
		t.Fatalf("envelope order must remain markdown then image: %#v", parts)
	}
	if parts[0].Ordinal != 1 || parts[1].Ordinal != 2 ||
		parts[0].Digest != receipts[0].PartDigest || parts[1].Digest != receipts[1].PartDigest {
		t.Fatalf("canonical part identity changed: %#v", parts)
	}
	if parts[1].PreparedResourceID != "@creative-image-media" {
		t.Fatalf("prepared image resource was not restored: %#v", parts[1])
	}
	if parts[0].MessageContent == nil || parts[1].MessageContent == nil ||
		parts[0].RenderManifest == nil || parts[1].RenderManifest == nil ||
		!reflect.DeepEqual(parts[0].MessageContent, parts[1].MessageContent) ||
		!reflect.DeepEqual(parts[0].RenderManifest, parts[1].RenderManifest) {
		t.Fatalf("all envelope parts must preserve one canonical source and manifest: %#v", parts)
	}
}

func TestK12IMDelivererPreflightsCreativeEnvelopeBeforeGroupCAS(t *testing.T) {
	d, transport, receipts := creativeEnvelopeFixture(t, k12usecase.DeliveryAttachment{
		Name: "美术作品.png",
		MIME: "image/png",
		Data: []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x01},
	})
	for i := range receipts {
		receipts[i].Status = k12.DeliveryPending
		receipts[i].Attempt = 0
	}

	if err := d.PreflightPreparedEnvelope(context.Background(), receipts); err != nil {
		t.Fatalf("pending envelope preflight 失败: %v", err)
	}
	if len(transport.preflights) != 1 || len(transport.envelopes) != 0 ||
		transport.legacyMessages != 0 || transport.legacyPartSends != 0 {
		t.Fatalf("pre-CAS preflight crossed send boundary: %+v", transport)
	}
	for i := range receipts {
		receipts[i].Status = k12.DeliveryFailed
		receipts[i].Attempt = 1
	}
	if err := d.PreflightPreparedEnvelope(context.Background(), receipts); err != nil {
		t.Fatalf("retryable failed envelope preflight 失败: %v", err)
	}
	for i := range receipts {
		receipts[i].Status = k12.DeliveryPending
		receipts[i].Attempt = 0
	}

	receipts[1].PreparedResourceID = "asset://bad"
	if err := d.PreflightPreparedEnvelope(context.Background(), receipts); err == nil {
		t.Fatal("invalid platform image reference must fail pre-CAS preflight")
	}
	if len(transport.envelopes) != 0 || transport.legacyMessages != 0 || transport.legacyPartSends != 0 {
		t.Fatalf("failed preflight crossed provider send boundary: %+v", transport)
	}

	receipts[1].PreparedResourceID = "@creative-image-media"
	receipts[1].Target.InstanceID = "pi-another-instance"
	if err := d.PreflightPreparedEnvelope(context.Background(), receipts); err == nil {
		t.Fatal("cross-instance components must fail before platform preflight")
	}
	if len(transport.preflights) != 3 || len(transport.envelopes) != 0 {
		t.Fatalf("cross-instance components crossed platform boundary: %+v", transport)
	}
}

func TestK12IMDelivererCreativeEnvelopeRejectsMixedOrIncompleteComponents(t *testing.T) {
	image := k12usecase.DeliveryAttachment{
		Name: "美术作品.png",
		MIME: "image/png",
		Data: []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x01},
	}
	tests := []struct {
		name   string
		mutate func([]k12.DeliveryReceipt)
	}{
		{
			name: "non creative object",
			mutate: func(receipts []k12.DeliveryReceipt) {
				for i := range receipts {
					receipts[i].ObjectKind = "grading_final_artifact"
				}
			},
		},
		{
			name: "cross target",
			mutate: func(receipts []k12.DeliveryReceipt) {
				receipts[1].Target.ChatID = "another-parent"
			},
		},
		{
			name: "cross binding",
			mutate: func(receipts []k12.DeliveryReceipt) {
				receipts[1].BindingID = "agent-rule:another-binding"
			},
		},
		{
			name: "cross batch",
			mutate: func(receipts []k12.DeliveryReceipt) {
				receipts[1].BatchID = "another-batch"
			},
		},
		{
			name: "missing prepared resource",
			mutate: func(receipts []k12.DeliveryReceipt) {
				receipts[1].PreparedResourceID = ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, transport, receipts := creativeEnvelopeFixture(t, image)
			tt.mutate(receipts)
			ack, err := d.SendPreparedEnvelope(context.Background(), receipts)
			if err == nil || ack.Status != k12.DeliveryFailed {
				t.Fatalf("invalid envelope must fail before send: ack=%+v err=%v", ack, err)
			}
			if len(transport.envelopes) != 0 || transport.legacyMessages != 0 || transport.legacyPartSends != 0 {
				t.Fatalf("invalid envelope must have zero provider sends: envelopes=%d messages=%d parts=%d",
					len(transport.envelopes), transport.legacyMessages, transport.legacyPartSends)
			}
		})
	}
}

func TestK12IMDelivererCreativeEnvelopeRejectsPDFWithoutLegacyFallback(t *testing.T) {
	d, transport, receipts := creativeEnvelopeFixture(t, k12usecase.DeliveryAttachment{
		Name: "作品.pdf",
		MIME: "application/pdf",
		Data: []byte("%PDF-1.7\n%%EOF\n"),
	})
	receipts[1].PreparedResourceID = "@creative-pdf-media"

	ack, err := d.SendPreparedEnvelope(context.Background(), receipts)
	if err == nil || ack.Status != k12.DeliveryFailed {
		t.Fatalf("PDF must stay outside the creative composite envelope: ack=%+v err=%v", ack, err)
	}
	if len(transport.envelopes) != 0 || transport.legacyMessages != 0 || transport.legacyPartSends != 0 {
		t.Fatalf("rejected PDF envelope must not fall back to legacy sends: envelopes=%d messages=%d parts=%d",
			len(transport.envelopes), transport.legacyMessages, transport.legacyPartSends)
	}
}
