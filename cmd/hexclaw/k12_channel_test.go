package main

// ChannelPort 收敛后的 composition root 契约（架构设计-v0.5.0 §6.10 / §3.12）：
//   - k12IMDeliverer 按绑定规则的 platform 路由到注册表里的正确通道（fake channel 证据）；
//   - 未绑定 → 诚实拒绝（家长向文案，HTTP 层映射 409）；
//   - 通道未就绪 / 未配置 / 留缝 stub 未实现 → 各自诚实降级文案，绝不虚标已发送；
//   - 限绑语义保持（复用 im_bind_exclusive_test.go 既有契约，binder 改走 channel.CheckExclusiveBind）；
//   - cron IM 投递走通道：已注册通道经 ChannelPort 发送；未注册目标/留缝 stub 回退
//     平台通用直发（cron 是平台通用面，不因 K12 留缝停摆）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// recordChannel 契约替身：记录经通道发出的消息。
type recordChannel struct {
	name       string
	sent       []recordedSend
	prepared   []channel.DeliveryPart
	sentParts  []channel.DeliveryPart
	fail       error
	sendAck    channel.DeliveryAck
	queryAck   channel.DeliveryAck
	queryCalls int
}

type recordedSend struct {
	to   channel.Target
	text string
}

func (c *recordChannel) Name() string { return c.name }

func (c *recordChannel) SendText(ctx context.Context, to channel.Target, text string) error {
	if c.fail != nil {
		return c.fail
	}
	c.sent = append(c.sent, recordedSend{to: to, text: text})
	return nil
}

func (c *recordChannel) SendMessage(ctx context.Context, to channel.Target, msg channel.Message) error {
	return c.SendText(ctx, to, msg.Text)
}

func (c *recordChannel) SendMessageWithReceipt(ctx context.Context, to channel.Target, msg channel.Message) (channel.DeliveryAck, error) {
	if err := c.SendMessage(ctx, to, msg); err != nil {
		return channel.DeliveryAck{Status: channel.DeliveryFailed, Target: to}, err
	}
	ack := c.sendAck
	if ack.Status == "" {
		ack = channel.DeliveryAck{ExternalMessageID: "process-query-key", Status: channel.DeliveryAccepted}
	}
	ack.Target = to
	return ack, nil
}

func (c *recordChannel) QueryReceipt(_ context.Context, to channel.Target, externalMessageID string) (channel.DeliveryAck, error) {
	c.queryCalls++
	ack := c.queryAck
	if ack.Status == "" {
		ack = channel.DeliveryAck{Status: channel.DeliveryDelivered}
	}
	if ack.ExternalMessageID == "" {
		ack.ExternalMessageID = externalMessageID
	}
	ack.Target = to
	return ack, nil
}

func (c *recordChannel) PrepareDeliveryPartResource(
	_ context.Context,
	_ channel.Target,
	part channel.DeliveryPart,
) (string, error) {
	if c.fail != nil {
		return "", c.fail
	}
	c.prepared = append(c.prepared, part)
	return "@media-part-" + strconv.Itoa(part.Ordinal), nil
}

func (c *recordChannel) SendPreparedPartWithReceipt(
	_ context.Context,
	to channel.Target,
	part channel.DeliveryPart,
) (channel.DeliveryAck, error) {
	if c.fail != nil {
		return channel.DeliveryAck{Status: channel.DeliveryFailed, Target: to}, c.fail
	}
	c.sentParts = append(c.sentParts, part)
	ack := c.sendAck
	if ack.Status == "" {
		ack = channel.DeliveryAck{
			ExternalMessageID: fmt.Sprintf("part:%s:%d", to.ChatID, part.Ordinal),
			Status:            channel.DeliveryAccepted,
		}
	}
	ack.Target = to
	return ack, nil
}

func newDelivererFixture(t *testing.T) (*k12IMDeliverer, *agentrouter.Dispatcher, *channel.Registry) {
	t.Helper()
	dispatcher := agentrouter.New()
	for _, name := range []string{"child-a", "child-b"} {
		if err := dispatcher.Register(agentrouter.AgentConfig{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	reg := channel.NewRegistry()
	d := &k12IMDeliverer{router: dispatcher, channels: reg}
	return d, dispatcher, reg
}

func bindRule(t *testing.T, dispatcher *agentrouter.Dispatcher, platform, instanceID, chatID, agent string) {
	t.Helper()
	if err := dispatcher.AddRule(agentrouter.Rule{Platform: platform, InstanceID: instanceID, ChatID: chatID, AgentName: agent, Priority: 50}); err != nil {
		t.Fatal(err)
	}
}

func TestK12IMDeliverer_RoutesToBoundChannel(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	ding := &recordChannel{name: "dingtalk"}
	other := &recordChannel{name: "feishu"}
	reg.Register(ding)
	reg.Register(other)
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()

	target, err := d.DeliverText(context.Background(), "child-a", "点评要点")
	if err != nil {
		t.Fatal(err)
	}
	if target != "dingtalk" {
		t.Fatalf("返回投递目标应为绑定平台, got %q", target)
	}
	if len(other.sent) != 0 || len(ding.sent) != 1 {
		t.Fatalf("发送必须路由到绑定通道: dingtalk=%d feishu=%d", len(ding.sent), len(other.sent))
	}
	got := ding.sent[0]
	if got.to.ChatID != "mom-chat" || got.to.InstanceID != "bot-1" || got.to.Platform != "dingtalk" || got.text != "点评要点" {
		t.Fatalf("目标与内容必须原样透传: %+v", got)
	}
}

func TestK12IMDeliverer_UnboundHonestRefusal(t *testing.T) {
	d, _, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk"})
	d.MarkReady()
	if _, err := d.DeliverText(context.Background(), "child-a", "x"); err == nil || !strings.Contains(err.Error(), "还没绑定") {
		t.Fatalf("未绑定必须诚实拒绝（家长向文案）, got %v", err)
	}
}

func TestK12IMDeliverer_NotReadyHonest(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk"})
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	// 未 MarkReady（instanceMgr 尚未建成）：保持既有「还没就绪」文案。
	if _, err := d.DeliverText(context.Background(), "child-a", "x"); err == nil || !strings.Contains(err.Error(), "还没就绪") {
		t.Fatalf("通道未就绪必须诚实报错, got %v", err)
	}
}

func TestK12IMDeliverer_StubChannelHonestDegrade(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(channel.NewFeishu())
	bindRule(t, dispatcher, "feishu", "fs-1", "mom-chat", "child-a")
	d.MarkReady()
	_, err := d.DeliverText(context.Background(), "child-a", "x")
	if err == nil || !strings.Contains(err.Error(), "还没有开通") {
		t.Fatalf("留缝 stub 通道必须诚实「未开通」降级, got %v", err)
	}
}

func TestK12IMDeliverer_UnconfiguredChannelHonestDegrade(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk"})
	bindRule(t, dispatcher, "telegram", "tg-1", "mom-chat", "child-a")
	d.MarkReady()
	_, err := d.DeliverText(context.Background(), "child-a", "x")
	if err == nil || !strings.Contains(err.Error(), "还没有接入") {
		t.Fatalf("未配置通道必须诚实「未接入」降级, got %v", err)
	}
}

func TestK12IMDeliverer_SendFailureKeepsParentFacingCopy(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk", fail: context.DeadlineExceeded})
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()
	_, err := d.DeliverText(context.Background(), "child-a", "x")
	if err == nil || !strings.Contains(err.Error(), "发送没有成功（dingtalk）") {
		t.Fatalf("发送失败文案必须保持既有家长向措辞, got %v", err)
	}
}

func TestAdapterDeliveryAckMapsAcceptedWithoutClaimingDelivered(t *testing.T) {
	target := channel.Target{Platform: "dingtalk", InstanceID: "pi-1", ChatID: "user-1"}
	got := channelAckFromAdapter(adapter.DeliveryAck{
		ExternalMessageID: "pqk-1",
		Status:            adapter.DeliveryAccepted,
	}, target)
	if got.Status != channel.DeliveryAccepted || got.Status == channel.DeliveryDelivered || got.ExternalMessageID != "pqk-1" || got.Target != target {
		t.Fatalf("mapped ack=%+v", got)
	}
}

func TestK12IMDelivererFreezesReceiptPayloadBeforeProviderSend(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	ding := &recordChannel{name: "dingtalk", sendAck: channel.DeliveryAck{
		ExternalMessageID: "pqk-24", Status: channel.DeliveryAccepted,
	}}
	reg.Register(ding)
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()

	prepared, err := d.PrepareText(context.Background(), "child-a", "计算 $x^2$，长度 $12 \\, \\mathrm{cm}$ 的点评")
	if err != nil {
		t.Fatal(err)
	}
	if len(ding.sent) != 0 {
		t.Fatal("PrepareText must be side-effect free until receipt persistence")
	}
	if !strings.HasPrefix(prepared.BindingID, "agent-rule:") || prepared.Target.ChatID != "mom-chat" ||
		prepared.Target.InstanceID != "bot-1" || prepared.PayloadJSON == "" || prepared.RenderJSON == "" {
		t.Fatalf("prepared delivery evidence incomplete: %+v", prepared)
	}
	receipt := k12.DeliveryReceipt{
		DeliveryID: "delivery-24", AgentName: "child-a", BindingID: prepared.BindingID,
		Target: prepared.Target, PartKind: prepared.PartKind, PartMIME: prepared.PartMIME,
		PartOrdinal: prepared.PartOrdinal, PartDigest: prepared.PartDigest,
		PayloadJSON: prepared.PayloadJSON, RenderJSON: prepared.RenderJSON,
		PayloadDigest: deliveryPayloadDigest(prepared.PayloadJSON), Status: k12.DeliverySending,
	}
	ack, err := d.SendPrepared(context.Background(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != k12.DeliverySending || ack.ExternalMessageID != "pqk-24" {
		t.Fatalf("provider acceptance must map to domain sending: %+v", ack)
	}
	if len(ding.sent) != 0 || len(ding.sentParts) != 1 || !strings.Contains(ding.sentParts[0].Text, "x²") ||
		!strings.Contains(ding.sentParts[0].Text, "12 cm") ||
		strings.Contains(ding.sentParts[0].Text, "\\,") ||
		strings.Contains(ding.sentParts[0].Text, "\\mathrm") {
		t.Fatalf("send must reuse one frozen readable part exactly once: %+v", ding.sentParts)
	}
	tampered := receipt
	tampered.BindingID = "agent-rule:rebound"
	badAck, badErr := d.SendPrepared(context.Background(), tampered)
	if badErr == nil || badAck.Status != k12.DeliveryFailed || len(ding.sentParts) != 1 {
		t.Fatalf("stale/rebound receipt must fail before send: ack=%+v sends=%d err=%v", badAck, len(ding.sentParts), badErr)
	}

	ding.queryAck = channel.DeliveryAck{Status: channel.DeliveryDelivered}
	queried, err := d.QueryPrepared(context.Background(), k12.DeliveryReceipt{
		Target: prepared.Target, ExternalMessageID: "pqk-24",
	})
	if err != nil || queried.Status != k12.DeliveryDelivered || queried.ExternalMessageID != "pqk-24" || ding.queryCalls != 1 {
		t.Fatalf("query must preserve provider evidence: ack=%+v calls=%d err=%v", queried, ding.queryCalls, err)
	}
}

func TestK12IMDelivererMigratedWholeMessageReceiptSendsCanonicalMarkdownPart(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	ding := &recordChannel{name: "dingtalk"}
	reg.Register(ding)
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()

	targets, err := d.ResolveTextTargets(context.Background(), "child-a")
	if err != nil {
		t.Fatal(err)
	}
	legacyMessage, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 旧版辅导内容\n\n请复习 **两位数加法**。",
		"## 旧版辅导内容\n\n请复习 **两位数加法**。",
		"",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := legacyMessage.DeliveryParts()
	if err != nil {
		t.Fatal(err)
	}
	legacyPayload, err := json.Marshal(legacyMessage)
	if err != nil {
		t.Fatal(err)
	}
	legacyRender, err := json.Marshal(legacyMessage.RenderManifest)
	if err != nil {
		t.Fatal(err)
	}
	receipt := k12.DeliveryReceipt{
		DeliveryID: "delivery-v84", AgentName: "child-a", BindingID: targets[0].BindingID,
		Target: targets[0].Target, Status: k12.DeliverySending,
		PartKind: parts[0].Kind, PartMIME: parts[0].MIME,
		PartOrdinal: parts[0].Ordinal, PartDigest: parts[0].Digest,
		PayloadDigest: deliveryPayloadDigest(string(legacyPayload)),
		PayloadJSON:   string(legacyPayload), RenderJSON: string(legacyRender),
	}

	ack, err := d.SendPrepared(context.Background(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != k12.DeliverySending || len(ding.sentParts) != 1 || len(ding.sent) != 0 {
		t.Fatalf("migrated whole-message receipt must send exactly one canonical part: ack=%+v parts=%d legacy=%d",
			ack, len(ding.sentParts), len(ding.sent))
	}
	if got := ding.sentParts[0]; got.Kind != parts[0].Kind || got.Ordinal != 1 ||
		got.Digest != parts[0].Digest || got.Text != parts[0].Text {
		t.Fatalf("migrated canonical Markdown part changed: got=%+v want=%+v", got, parts[0])
	}

	tampered := receipt
	tampered.PartDigest = tampered.PayloadDigest
	badAck, badErr := d.SendPrepared(context.Background(), tampered)
	if badErr == nil || badAck.Status != k12.DeliveryFailed || len(ding.sentParts) != 1 {
		t.Fatalf("whole-message digest must fail before provider send: ack=%+v sends=%d err=%v",
			badAck, len(ding.sentParts), badErr)
	}
}

func TestK12IMDelivererCreativeImageBridgePreservesMarkdownAndBytes(t *testing.T) {
	d := &k12IMDeliverer{}
	canonical := "## 美术作品\n\n### 可见证据\n\n- 彩虹的七种颜色层次清楚。"
	imageBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x01, 0x02, 0x03}
	prepared, err := d.PrepareMessageForTargets(context.Background(), k12usecase.DeliveryMessage{
		Content: canonical,
		Attachments: []k12usecase.DeliveryAttachment{{
			Name: "美术作品.png",
			MIME: "image/png",
			Data: imageBytes,
		}},
	}, []k12usecase.ResolvedDeliveryTarget{{
		BindingID: "agent-rule:creative-image",
		Target: k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: "bot-1", ChatID: "parent-1",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 2 {
		t.Fatalf("一个物理目标的 Markdown 与图片必须冻结两个 part，got %d", len(prepared))
	}

	var markdownPart, imagePart channel.DeliveryPart
	if err := json.Unmarshal([]byte(prepared[0].PayloadJSON), &markdownPart); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(prepared[1].PayloadJSON), &imagePart); err != nil {
		t.Fatal(err)
	}
	if markdownPart.Text != canonical || markdownPart.MessageContent == nil || markdownPart.MessageContent.Markdown != canonical {
		t.Fatalf("创作作品 Markdown 必须完整穿过冻结载荷: %#v", markdownPart.MessageContent)
	}
	if imagePart.RenderManifest == nil || !imagePart.RenderManifest.CapabilitySnapshot.Markdown ||
		!imagePart.RenderManifest.CapabilitySnapshot.Attachments {
		t.Fatalf("冻结载荷必须声明 Markdown 与附件能力: %#v", imagePart.RenderManifest)
	}
	if imagePart.Attachment == nil || imagePart.Attachment.Name != "美术作品.png" ||
		imagePart.Attachment.MIME != "image/png" || !bytes.Equal(imagePart.Attachment.Data, imageBytes) {
		t.Fatalf("冻结载荷必须保留单一图片名称、MIME 与原始字节: %#v", imagePart.Attachment)
	}

	adapterPart := adapterDeliveryPartFromChannelPart(imagePart)
	if adapterPart.Text != "" || adapterPart.MessageContent == nil || adapterPart.RenderManifest == nil ||
		adapterPart.Attachment == nil || adapterPart.Attachment.Type != "image" ||
		adapterPart.Attachment.Name != "美术作品.png" || adapterPart.Attachment.Mime != "image/png" ||
		adapterPart.Attachment.URL != "" {
		t.Fatalf("adapter bridge 必须只生成当前图片 part: %#v", adapterPart)
	}
	decoded, err := base64.StdEncoding.DecodeString(adapterPart.Attachment.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, imageBytes) {
		t.Fatalf("adapter 图片附件字节发生变化: got %x want %x", decoded, imageBytes)
	}

	for _, candidate := range []struct {
		name  string
		value string
	}{
		{name: "冻结载荷", value: prepared[1].PayloadJSON},
		{name: "通道正文", value: markdownPart.Text},
		{name: "adapter 正文", value: adapterPart.Text},
		{name: "adapter 附件 URL", value: adapterPart.Attachment.URL},
	} {
		for _, forbidden := range []string{"asset://", "file://", "/Users/", `C:\\`} {
			if strings.Contains(candidate.value, forbidden) {
				t.Fatalf("%s 不得暴露 asset URI 或本地路径 %q: %q", candidate.name, forbidden, candidate.value)
			}
		}
	}
}

func TestK12IMDelivererPDFBridgePreservesCanonicalArtifact(t *testing.T) {
	d := &k12IMDeliverer{}
	canonical := "## 本周练习卷\n\n请完成后拍照提交。"
	pdfBytes := []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n%%EOF\n")
	prepared, err := d.PrepareMessageForTargets(context.Background(), k12usecase.DeliveryMessage{
		Content: canonical,
		Attachments: []k12usecase.DeliveryAttachment{{
			Name: "本周练习卷.pdf",
			MIME: "application/pdf",
			Data: pdfBytes,
		}},
	}, []k12usecase.ResolvedDeliveryTarget{{
		BindingID: "agent-rule:weekly-pdf",
		Target: k12.DeliveryTarget{
			Platform: "dingtalk", InstanceID: "bot-1", ChatID: "parent-1",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 2 {
		t.Fatalf("一个物理目标的 Markdown 与 PDF 必须冻结两个 part，got %d", len(prepared))
	}

	var pdfPart channel.DeliveryPart
	if err := json.Unmarshal([]byte(prepared[1].PayloadJSON), &pdfPart); err != nil {
		t.Fatal(err)
	}
	if pdfPart.MessageContent == nil || pdfPart.RenderManifest == nil || len(pdfPart.MessageContent.Attachments) != 1 ||
		len(pdfPart.RenderManifest.Parts) != 2 || pdfPart.Attachment == nil {
		t.Fatalf("PDF canonical/manifest 证据不完整: part=%#v", pdfPart)
	}
	sum := sha256.Sum256(pdfBytes)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	ref := pdfPart.MessageContent.Attachments[0]
	artifact := pdfPart.RenderManifest.Parts[1]
	if ref.Name != "本周练习卷.pdf" || ref.MIME != "application/pdf" || ref.Digest != wantDigest ||
		artifact.Kind != messagecontent.PartArtifact || artifact.ArtifactRef != ref.AssetID ||
		artifact.ArtifactDigest != wantDigest {
		t.Fatalf("PDF PartArtifact 与冻结 bytes/MIME 不一致: ref=%#v artifact=%#v", ref, artifact)
	}

	adapterPart := adapterDeliveryPartFromChannelPart(pdfPart)
	if adapterPart.Text != "" || adapterPart.MessageContent == nil || adapterPart.RenderManifest == nil ||
		adapterPart.Attachment == nil {
		t.Fatalf("PDF adapter bridge 丢失 canonical 证据: %#v", adapterPart)
	}
	attachment := *adapterPart.Attachment
	if attachment.Type != "file" || attachment.Name != "本周练习卷.pdf" ||
		attachment.Mime != "application/pdf" || attachment.URL != "" {
		t.Fatalf("PDF adapter bridge 必须生成内联 file 附件: %#v", attachment)
	}
	decoded, err := base64.StdEncoding.DecodeString(attachment.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, pdfBytes) {
		t.Fatalf("PDF adapter bridge bytes 发生变化: got %x want %x", decoded, pdfBytes)
	}
}

func TestK12IMDelivererRejectsUnsafeAttachmentsBeforeFreezingTargets(t *testing.T) {
	tests := []struct {
		name       string
		attachment k12usecase.DeliveryAttachment
	}{
		{name: "URL", attachment: k12usecase.DeliveryAttachment{Name: "https://internal.invalid/work.png", MIME: "image/png", Data: []byte("image")}},
		{name: "POSIX path", attachment: k12usecase.DeliveryAttachment{Name: "/Users/private/work.png", MIME: "image/png", Data: []byte("image")}},
		{name: "Windows path", attachment: k12usecase.DeliveryAttachment{Name: `C:\Users\private\work.png`, MIME: "image/png", Data: []byte("image")}},
		{name: "unsupported MIME", attachment: k12usecase.DeliveryAttachment{Name: "archive.zip", MIME: "application/zip", Data: []byte("archive")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &k12IMDeliverer{}
			prepared, err := d.PrepareMessageForTargets(context.Background(), k12usecase.DeliveryMessage{
				Content:     "## 学习资料",
				Attachments: []k12usecase.DeliveryAttachment{tt.attachment},
			}, []k12usecase.ResolvedDeliveryTarget{{
				BindingID: "agent-rule:unsafe-attachment",
				Target: k12.DeliveryTarget{
					Platform: "dingtalk", InstanceID: "bot-1", ChatID: "parent-1",
				},
			}})
			if err == nil {
				t.Fatalf("不安全附件必须在冻结前失败: %#v", prepared)
			}
			if len(prepared) != 0 {
				t.Fatalf("冻结失败不得返回任何可发送载荷: %#v", prepared)
			}
		})
	}
}

func TestK12IMDelivererFreezesAndSendsTargetByPartWithoutWholeMessageReplay(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	ding := &recordChannel{name: "dingtalk"}
	reg.Register(ding)
	bindRule(t, dispatcher, "dingtalk", "bot-1", "parent-1", "child-a")
	bindRule(t, dispatcher, "dingtalk", "bot-2", "parent-2", "child-a")
	d.MarkReady()

	targets, err := d.ResolveTextTargets(context.Background(), "child-a")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := d.PrepareMessageForTargets(
		context.Background(),
		k12usecase.DeliveryMessage{
			Content: "## 本周练习\n\n请完成后订正。",
			Attachments: []k12usecase.DeliveryAttachment{
				{Name: "page.png", MIME: "image/png", Data: []byte("image-bytes")},
				{Name: "practice.pdf", MIME: "application/pdf", Data: []byte("%PDF-1.7\n%%EOF\n")},
			},
		},
		targets,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 6 {
		t.Fatalf("2 targets × 3 parts must freeze six payloads, got %d", len(prepared))
	}
	wantMIME := []string{"", "image/png", "application/pdf", "", "image/png", "application/pdf"}
	for i, item := range prepared {
		var part channel.DeliveryPart
		if err := json.Unmarshal([]byte(item.PayloadJSON), &part); err != nil {
			t.Fatalf("decode part %d: %v", i, err)
		}
		if err := part.Validate(); err != nil {
			t.Fatalf("validate part %d: %v", i, err)
		}
		if part.Ordinal != i%3+1 || part.MIME != wantMIME[i] || item.PartOrdinal != part.Ordinal ||
			item.PartKind != part.Kind || item.PartMIME != part.MIME || item.PartDigest != part.Digest {
			t.Fatalf("part identity %d changed: prepared=%+v part=%+v", i, item, part)
		}
		receipt := k12.DeliveryReceipt{
			DeliveryID:    fmt.Sprintf("delivery-%d", i),
			AgentName:     "child-a",
			BindingID:     item.BindingID,
			Target:        item.Target,
			PartKind:      item.PartKind,
			PartMIME:      item.PartMIME,
			PartOrdinal:   item.PartOrdinal,
			PartDigest:    item.PartDigest,
			PayloadJSON:   item.PayloadJSON,
			RenderJSON:    item.RenderJSON,
			PayloadDigest: deliveryPayloadDigest(item.PayloadJSON),
			Status:        k12.DeliverySending,
		}
		if part.Kind == messagecontent.PartArtifact {
			resourceID, prepareErr := d.PrepareDeliveryPartResource(context.Background(), receipt)
			if prepareErr != nil {
				t.Fatalf("prepare artifact %d: %v", i, prepareErr)
			}
			receipt.PreparedResourceID = resourceID
		}
		if _, sendErr := d.SendPrepared(context.Background(), receipt); sendErr != nil {
			t.Fatalf("send part %d: %v", i, sendErr)
		}
	}
	if len(ding.sent) != 0 || len(ding.sentParts) != 6 || len(ding.prepared) != 4 {
		t.Fatalf("whole message replayed or part counts changed: legacy=%d parts=%d prepared=%d", len(ding.sent), len(ding.sentParts), len(ding.prepared))
	}
	for i, part := range ding.sentParts {
		if part.Ordinal != i%3+1 {
			t.Fatalf("target-major part order changed at %d: %+v", i, part)
		}
		if part.Kind == messagecontent.PartMarkdown && (part.Attachment != nil || part.PreparedResourceID != "") {
			t.Fatalf("markdown part carried artifact data: %+v", part)
		}
		if part.Kind == messagecontent.PartArtifact && (part.Text != "" || part.Attachment == nil || part.PreparedResourceID == "") {
			t.Fatalf("artifact part is incomplete: %+v", part)
		}
	}
}

func TestK12FinalArtifactIMProjectionKeepsMarkdownAndOmitsInternalJSONEvidence(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk"})
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()

	assessment, err := json.Marshal(k12usecase.PhotoGradeItem{
		Recognized: k12usecase.RecognizedQuestion{
			ProblemID: "internal-only", AttemptID: "attempt-1", InputDigest: "sha256:input-1",
			ConfirmedVersion: 1, CanonicalMarkdown: "$\\\\frac{3}{4}$",
		},
		Status: k12usecase.PhotoCorrect,
		Grade:  k12usecase.GradeResult{Solution: "3/4 × 8 = 6"},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical := "# 作业批改结果\n\n## 第 1 题\n\n$\\\\frac{3}{4} \\\\times 8 = 6$\n\n**Grading status:** `correct`\n\n```json\n" + string(assessment) + "\n```\n\n# 这份作业的辅导要点\n\n1. **先审题**\n2. 再验算"
	prepared, err := d.PrepareText(context.Background(), "child-a", canonical)
	if err != nil {
		t.Fatal(err)
	}
	var payload channel.DeliveryPart
	if err := json.Unmarshal([]byte(prepared.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.MessageContent == nil || payload.MessageContent.Markdown != canonical {
		t.Fatal("冻结 canonical source 必须保持完整且可追溯")
	}
	if payload.RenderManifest == nil || len(payload.RenderManifest.Parts) != 1 ||
		payload.RenderManifest.Parts[0].Kind != "markdown" {
		t.Fatalf("钉钉解题消息必须保持 Markdown part: %#v", payload.RenderManifest)
	}
	for _, want := range []string{"# 作业批改结果", "## 第 1 题", "3/4 × 8 = 6", "1. **先审题**"} {
		if !strings.Contains(payload.Text, want) {
			t.Fatalf("Markdown 投影缺少 %q: %q", want, payload.Text)
		}
	}
	for _, internal := range []string{"```json", "problem_id", "internal-only"} {
		if strings.Contains(payload.Text, internal) {
			t.Fatalf("内部评估 JSON 不得进入家长钉钉消息: %q", payload.Text)
		}
	}
}

func TestK12FinalArtifactIMProjectionKeepsActionableSolutionAndUserJSON(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk"})
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()

	assessment, err := json.Marshal(k12usecase.PhotoGradeItem{
		Recognized: k12usecase.RecognizedQuestion{
			ProblemID: "problem-1", AttemptID: "attempt-1", InputDigest: "sha256:input-1",
			ConfirmedVersion: 1, Question: "3/4 × 8 = ?", StudentAnswer: "5",
		},
		Status: k12usecase.PhotoWrong,
		Grade: k12usecase.GradeResult{
			Solution: "## 解答\n先算 3 ÷ 4，再乘 8，得到 6。\n\n## 答案\n6",
			Outcome: k12usecase.GradeOutcome{
				Verdict: k12usecase.VerdictDisagree, WrongStep: "把 3/4 当成了 3/8", ErrorCause: "分数含义理解错误",
			},
		},
		ParentGuide: &k12usecase.ParentTeachingGuide{
			Answer: "6", FullSolutionSteps: []string{"3/4 × 8 = 6"},
			GradeLevelMethod: "先约分或先算 8 ÷ 4", LikelyMistakes: []string{"把分母也乘 8"},
			ParentTeachingSequence: []string{"先让孩子说出四分之三的含义", "再让孩子独立计算"},
			FollowUpQuestions:      []string{"怎样验算结果？"}, CheckingMethod: "用 6 ÷ 8 = 3/4 反向检查",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical := "# 作业批改结果\n\n## 第 1 题\n\n题目给出的数据示例必须保留：\n\n```json\n{\"student_visible\":true}\n```\n\n**Grading status:** `wrong`\n\n```json\n" + string(assessment) + "\n```"
	prepared, err := d.PrepareText(context.Background(), "child-a", canonical)
	if err != nil {
		t.Fatal(err)
	}
	var payload channel.DeliveryPart
	if err := json.Unmarshal([]byte(prepared.PayloadJSON), &payload); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"```json\n{\"student_visible\":true}\n```",
		"### 订正参考", "先算 3 ÷ 4，再乘 8，得到 6。",
		"**第一个错步：** 把 3/4 当成了 3/8", "**错因：** 分数含义理解错误",
		"### 家长怎么讲", "**答案：** 6", "**本年级方法：** 先约分或先算 8 ÷ 4",
		"**易错点：**", "把分母也乘 8", "**家长怎么讲：**", "先让孩子说出四分之三的含义",
		"**可以追问：**", "怎样验算结果？", "**怎么检查：** 用 6 ÷ 8 = 3/4 反向检查",
	} {
		if !strings.Contains(payload.Text, want) {
			t.Fatalf("钉钉 Markdown 投影缺少用户可见内容 %q:\n%s", want, payload.Text)
		}
	}
	for _, internal := range []string{"\"Recognized\"", "\"ParentGuide\"", "\"ResultKind\""} {
		if strings.Contains(payload.Text, internal) {
			t.Fatalf("内部评估字段不得进入钉钉消息 %q:\n%s", internal, payload.Text)
		}
	}
	if payload.RenderManifest == nil || payload.RenderManifest.FallbackReason != "" {
		t.Fatalf("无 LaTeX 的可见投影不得虚标数学降级: %#v", payload.RenderManifest)
	}
}

func TestCronIMDeliver_GoesThroughChannel(t *testing.T) {
	reg := channel.NewRegistry()
	ding := &recordChannel{name: "dingtalk"}
	reg.Register(ding)
	reg.Register(channel.NewFeishu())

	var direct []recordedSend
	deliver := newCronIMDeliver(context.Background(), reg, func(ctx context.Context, target, chatID string, msg channel.Message) error {
		direct = append(direct, recordedSend{to: channel.Target{Platform: target, ChatID: chatID}, text: msg.Text})
		return nil
	})

	job := &cron.Job{ID: "job-1", ChatID: "mom-chat"}
	// 已注册通道：投递必须走 ChannelPort，不再直连。
	if err := deliver(job, "dingtalk", "本周练习卷"); err != nil {
		t.Fatal(err)
	}
	if len(ding.sent) != 1 || len(direct) != 0 {
		t.Fatalf("cron 投递应走通道: channel=%d direct=%d", len(ding.sent), len(direct))
	}
	if ding.sent[0].to.ChatID != "mom-chat" || ding.sent[0].text != "本周练习卷" {
		t.Fatalf("cron 投递目标/内容必须原样透传: %+v", ding.sent[0])
	}
	// 未注册目标（其他平台/实例 ID）：回退平台通用直发，行为不变。
	if err := deliver(job, "tg-instance-1", "提醒"); err != nil {
		t.Fatal(err)
	}
	if len(direct) != 1 || direct[0].to.Platform != "tg-instance-1" {
		t.Fatalf("未注册目标应回退通用直发: %+v", direct)
	}
	// 留缝 stub（未实现）：cron 是平台通用面，回退直发不停摆。
	if err := deliver(job, "feishu", "提醒"); err != nil {
		t.Fatal(err)
	}
	if len(direct) != 2 {
		t.Fatalf("stub 未实现应回退通用直发: %+v", direct)
	}
	// 无 chat_id：保持既有报错。
	if err := deliver(&cron.Job{ID: "job-2"}, "dingtalk", "x"); err == nil || !strings.Contains(err.Error(), "no chat_id") {
		t.Fatalf("无 chat_id 应报错, got %v", err)
	}
}
