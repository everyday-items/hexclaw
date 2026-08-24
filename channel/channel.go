// Package channel 是 IM 通道端口层（架构设计-v0.5.0 §6.10 ChannelPort / ADR-K12-011）。
//
// 职责边界：
//   - 场景层（scenarios/k12）只产 ChannelNeutralMessage 语义的中立载荷，经窄缝
//     （usecase.DeliveryTransport）触达本层，绝不 import 平台细节（AP-1 / K12-INV-012）；
//   - 本层声明「通道」这一端口（Port）与注册表（Registry），不含任何 K12 领域类型，
//     也不 import adapter/instances——具体发送函数由 composition root（cmd/hexclaw）注入；
//   - 限绑语义（§3.12：同一私聊目标同一时间只绑一个 TutorAgent）归属本层
//     （CheckExclusiveBind），对所有通道渠道中立地生效。
//
// 当前真实通道只有钉钉（DingTalk）；飞书/企微是留缝 stub（方法集齐、诚实「未实现」），
// 接入点见 feishu.go / wecom.go 注释。仅一对一私聊（direct），群 conversation 永不进入
// 业务流（K12-INV-015）——Target 只表达私聊目标。
package channel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/messagecontent"
)

var (
	// ErrNotConfigured 通道未在注册表配置：调用方必须诚实降级
	// （与 K12 HTTP 面既有 501/409 语义一致），不得静默换通道或假装已发送。
	ErrNotConfigured = errors.New("channel not configured")
	// ErrNotImplemented 留缝 stub 通道（飞书/企微）尚未实现：诚实报错，不吞不骗。
	ErrNotImplemented = errors.New("channel not implemented")
	// ErrNotReady 通道发送函数尚未回填（装配顺序缝：通道注册早于平台实例管理器建成）。
	ErrNotReady = errors.New("channel sender not ready")
	// ErrGroupTarget 群 conversation 目标被拒（K12-INV-015：仅 direct，群永不进入业务流）。
	ErrGroupTarget = errors.New("channel: group conversation target rejected (direct only)")
)

// Target 是一对一私聊投递目标（direct conversation，§6.10）。
type Target struct {
	// Platform 通道名（dingtalk/feishu/wecom），与注册表 key 一致。
	Platform string
	// InstanceID 平台实例 ID（同平台多实例时定位具体机器人）；可空。
	InstanceID string
	// ChatID 私聊会话 ID。
	ChatID string
}

// EnsureDirect 校验 Target 是一对一私聊目标（K12-INV-015 通道层契约）。
//
// Target 没有 conversation type 字段——现实现里「群会话」唯一能流到发送层的编码形态，
// 是钉钉发送队列的群哨兵 ChatID（adapter/dingtalk groupQueueTarget：
// "\x00dingtalk-group:<openConversationId>"，以 NUL 控制字符开头）；真实私聊目标
// （staffId / 私聊会话 ID）绝不以控制字符开头。故据此拒群：控制字符前缀 = 非 direct 编码。
// 各 Port 实现（当前唯一真实通道 DingTalk）必须在发送前调用本校验。
func (t Target) EnsureDirect() error {
	if len(t.ChatID) > 0 && t.ChatID[0] < 0x20 {
		return ErrGroupTarget
	}
	return nil
}

// SendKey 平台实例管理器的发送键：实例 ID 优先，未记录实例时退回平台名
// （与收敛前 k12IMDeliverer 的既有规则逐字节一致）。
func (t Target) SendKey() string {
	if t.InstanceID != "" {
		return t.InstanceID
	}
	return t.Platform
}

// Attachment 通道中立附件（批注图、练习卷图/PDF 等）。Data 为原始字节，
// 各通道投影层自行决定编码（钉钉侧 base64 进 adapter.Reply）。
type Attachment struct {
	Name string
	MIME string
	Data []byte
}

// Message 是 ChannelNeutralMessage 的通道层载荷（§6.10：K12 只产中立消息，
// 渠道 Renderer 负责呈现降级）。现状真实用途只有两种形态，方法集据此收敛（YAGNI）：
//   - 纯文本：作品点评/观察练习卡/cron 提醒（Text）；
//   - 图文/呈现物：批改结果 Markdown + 批注图、练习卷图片/PDF（Text + Attachments）。
type Message struct {
	// Text 文本或 Markdown 正文；渠道适配层负责 Markdown/纯文本投影降级。
	Text string
	// Content is the immutable canonical Markdown/LaTeX source. Text is only
	// the channel-specific visible projection and must be traceable through
	// RenderManifest; both remain optional solely for legacy callers.
	Content        *messagecontent.MessageContent
	RenderManifest *messagecontent.RenderManifest
	// Attachments 可选附件（现状全为图片；PDF 呈现物走同一形态）。
	Attachments []Attachment
}

// DeliveryPart 是一次外发只处理一个冻结 part 的通道中立载荷。
// 完整 canonical 证据随每个 part 保留，但 Text 与 Attachment 只允许二选一。
type DeliveryPart struct {
	Kind               messagecontent.PartKind        `json:"kind"`
	MIME               string                         `json:"mime,omitempty"`
	Ordinal            int                            `json:"ordinal"`
	Digest             string                         `json:"digest"`
	Text               string                         `json:"text,omitempty"`
	Attachment         *Attachment                    `json:"attachment,omitempty"`
	MessageContent     *messagecontent.MessageContent `json:"message_content"`
	RenderManifest     *messagecontent.RenderManifest `json:"render_manifest"`
	PreparedResourceID string                         `json:"-"`
}

// DeliveryParts 把一份 canonical 消息冻结为 Markdown 在前、附件随后的一组单 part 载荷。
func (m Message) DeliveryParts() ([]DeliveryPart, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.Content == nil || m.RenderManifest == nil {
		return nil, errors.New("channel: canonical delivery evidence is required")
	}
	if len(m.RenderManifest.Parts) != len(m.Attachments)+1 ||
		m.RenderManifest.Parts[0].Kind != messagecontent.PartMarkdown {
		return nil, errors.New("channel: delivery manifest must contain one markdown part followed by its artifacts")
	}

	content := *m.Content
	content.Attachments = append([]messagecontent.AttachmentRef(nil), m.Content.Attachments...)
	manifest := *m.RenderManifest
	manifest.Parts = append([]messagecontent.RenderPart(nil), m.RenderManifest.Parts...)
	markdownSum := sha256.Sum256([]byte(content.Markdown))
	parts := make([]DeliveryPart, 0, len(manifest.Parts))
	parts = append(parts, DeliveryPart{
		Kind:           messagecontent.PartMarkdown,
		Ordinal:        1,
		Digest:         "sha256:" + hex.EncodeToString(markdownSum[:]),
		Text:           manifest.Parts[0].Text,
		MessageContent: &content,
		RenderManifest: &manifest,
	})
	for i, attachment := range m.Attachments {
		copyAttachment := Attachment{
			Name: attachment.Name,
			MIME: attachment.MIME,
			Data: append([]byte(nil), attachment.Data...),
		}
		parts = append(parts, DeliveryPart{
			Kind:           messagecontent.PartArtifact,
			MIME:           attachment.MIME,
			Ordinal:        i + 2,
			Digest:         content.Attachments[i].Digest,
			Attachment:     &copyAttachment,
			MessageContent: &content,
			RenderManifest: &manifest,
		})
	}
	for i := range parts {
		if err := parts[i].Validate(); err != nil {
			return nil, fmt.Errorf("channel: delivery part %d is invalid: %w", i+1, err)
		}
	}
	return parts, nil
}

// Validate 校验单 part 与完整 canonical source/manifest 的身份一致性。
func (p DeliveryPart) Validate() error {
	if p.MessageContent == nil || p.RenderManifest == nil {
		return errors.New("channel: delivery part requires canonical content and render manifest")
	}
	if err := p.RenderManifest.ValidateFor(*p.MessageContent); err != nil {
		return fmt.Errorf("channel: invalid delivery part render evidence: %w", err)
	}
	if p.Ordinal < 1 || p.Ordinal > len(p.RenderManifest.Parts) {
		return errors.New("channel: delivery part ordinal is out of range")
	}
	if !validSHA256Digest(p.Digest) {
		return errors.New("channel: delivery part digest is invalid")
	}
	selected := p.RenderManifest.Parts[p.Ordinal-1]
	if selected.Kind != p.Kind {
		return errors.New("channel: delivery part kind does not match render manifest")
	}
	switch p.Kind {
	case messagecontent.PartMarkdown:
		if p.Ordinal != 1 || p.MIME != "" || p.Attachment != nil || p.PreparedResourceID != "" {
			return errors.New("channel: markdown delivery part has artifact fields")
		}
		if p.Text != selected.Text {
			return errors.New("channel: markdown delivery part text does not match render manifest")
		}
		sum := sha256.Sum256([]byte(p.MessageContent.Markdown))
		if p.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
			return errors.New("channel: markdown delivery part digest does not match canonical source")
		}
	case messagecontent.PartArtifact:
		if p.Ordinal < 2 || p.Text != "" || p.Attachment == nil {
			return errors.New("channel: artifact delivery part fields are incomplete")
		}
		if err := validateCanonicalAttachment(*p.Attachment); err != nil {
			return err
		}
		attachmentIndex := p.Ordinal - 2
		if attachmentIndex >= len(p.MessageContent.Attachments) {
			return errors.New("channel: artifact delivery part has no canonical attachment")
		}
		ref := p.MessageContent.Attachments[attachmentIndex]
		if p.MIME != p.Attachment.MIME || ref.MIME != p.MIME || ref.Name != p.Attachment.Name ||
			ref.Digest != p.Digest || selected.ArtifactRef != ref.AssetID ||
			selected.ArtifactDigest != p.Digest || selected.AltText != ref.AltText {
			return errors.New("channel: artifact delivery part does not match canonical attachment")
		}
		sum := sha256.Sum256(p.Attachment.Data)
		if p.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
			return errors.New("channel: artifact delivery part digest does not match bytes")
		}
	default:
		return fmt.Errorf("channel: unsupported delivery part kind %q", p.Kind)
	}
	return nil
}

func validSHA256Digest(value string) bool {
	raw, ok := strings.CutPrefix(value, "sha256:")
	decoded, err := hex.DecodeString(raw)
	return ok && err == nil && len(decoded) == sha256.Size
}

func NewCanonicalMessage(producer messagecontent.ProducerKind, locale, markdown, projectedText, fallbackReason string) (Message, error) {
	return NewCanonicalMessageWithAttachments(producer, locale, markdown, projectedText, fallbackReason, nil)
}

func NewCanonicalMessageWithAttachments(producer messagecontent.ProducerKind, locale, markdown, projectedText, fallbackReason string, attachments []Attachment) (Message, error) {
	return newCanonicalMessage(producer, locale, markdown, projectedText, fallbackReason, attachments, messagecontent.PartText, false)
}

func NewCanonicalMarkdownMessageWithAttachments(producer messagecontent.ProducerKind, locale, markdown, projectedMarkdown, fallbackReason string, attachments []Attachment) (Message, error) {
	return newCanonicalMessage(producer, locale, markdown, projectedMarkdown, fallbackReason, attachments, messagecontent.PartMarkdown, true)
}

func newCanonicalMessage(producer messagecontent.ProducerKind, locale, markdown, projectedText, fallbackReason string, attachments []Attachment, partKind messagecontent.PartKind, supportsMarkdown bool) (Message, error) {
	refs := make([]messagecontent.AttachmentRef, 0, len(attachments))
	parts := []messagecontent.RenderPart{{Kind: partKind, Text: projectedText}}
	for _, attachment := range attachments {
		if err := validateCanonicalAttachment(attachment); err != nil {
			return Message{}, err
		}
		sum := sha256.Sum256(attachment.Data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		ref := "inline:" + hex.EncodeToString(sum[:])
		refs = append(refs, messagecontent.AttachmentRef{
			AssetID: ref,
			Name:    attachment.Name,
			MIME:    attachment.MIME,
			Digest:  digest,
			AltText: attachment.Name,
		})
		parts = append(parts, messagecontent.RenderPart{
			Kind:           messagecontent.PartArtifact,
			ArtifactRef:    ref,
			ArtifactDigest: digest,
			AltText:        attachment.Name,
		})
	}
	content, err := messagecontent.New(producer, locale, markdown, refs)
	if err != nil {
		return Message{}, err
	}
	rendererVersion := "channel-readable-text-v1"
	if supportsMarkdown {
		rendererVersion = "channel-markdown-readable-math-v1"
	}
	manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceChannel,
		RendererVersion: rendererVersion,
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown:    supportsMarkdown,
			UnicodeMath: true,
			Attachments: len(attachments) > 0,
		},
		Parts:          parts,
		FallbackReason: fallbackReason,
	})
	if err != nil {
		return Message{}, err
	}
	msg := Message{Text: projectedText, Content: &content, RenderManifest: &manifest, Attachments: append([]Attachment(nil), attachments...)}
	return msg, msg.Validate()
}

func (m Message) Validate() error {
	if (m.Content == nil) != (m.RenderManifest == nil) {
		return errors.New("channel: canonical content and render manifest must be supplied together")
	}
	if m.Content == nil {
		return nil
	}
	if err := m.RenderManifest.ValidateFor(*m.Content); err != nil {
		return fmt.Errorf("channel: invalid render evidence: %w", err)
	}
	if len(m.Attachments) != len(m.Content.Attachments) {
		return errors.New("channel: attachment projection does not match canonical references")
	}
	for i, attachment := range m.Attachments {
		if err := validateCanonicalAttachment(attachment); err != nil {
			return err
		}
		sum := sha256.Sum256(attachment.Data)
		wantDigest := "sha256:" + hex.EncodeToString(sum[:])
		ref := m.Content.Attachments[i]
		if ref.Digest != wantDigest || ref.MIME != attachment.MIME || ref.Name != attachment.Name {
			return fmt.Errorf("channel: attachment %d does not match canonical digest", i)
		}
	}
	var visible strings.Builder
	for _, part := range m.RenderManifest.Parts {
		switch part.Kind {
		case messagecontent.PartMarkdown, messagecontent.PartText:
			visible.WriteString(part.Text)
		}
	}
	if strings.TrimSpace(m.Text) != strings.TrimSpace(visible.String()) {
		return errors.New("channel: visible text does not match render manifest parts")
	}
	return nil
}

func validateCanonicalAttachment(attachment Attachment) error {
	name := strings.TrimSpace(attachment.Name)
	if name == "" {
		return errors.New("channel: attachment name is required")
	}
	lowerName := strings.ToLower(name)
	for _, prefix := range []string{"asset:", "file:", "http:", "https:", "blob:", "data:"} {
		if strings.HasPrefix(lowerName, prefix) {
			return errors.New("channel: attachment name must not contain a URL or path")
		}
	}
	if strings.ContainsAny(name, `/\`) {
		return errors.New("channel: attachment name must not contain a URL or path")
	}
	if len(attachment.Data) == 0 {
		return errors.New("channel: attachment bytes are required")
	}
	mime := strings.ToLower(strings.TrimSpace(attachment.MIME))
	if mime == "application/pdf" || (strings.HasPrefix(mime, "image/") && mime != "image/") {
		return nil
	}
	return fmt.Errorf("channel: unsupported attachment MIME %q", attachment.MIME)
}

// Port 是通道端口（ChannelPort）。方法集从现状消费点提炼：
//   - SendText：发文本（点评/练习卡发送、cron 投递文本）；
//   - SendMessage：发图文/呈现物（批改结果+批注图、练习卷）。
//
// 绑定校验不在 Port 上——限绑是渠道中立语义（见 CheckExclusiveBind），绑定持久化
// 归平台路由规则（router.Dispatcher），不随通道实现漂移。
// 回执/能力协商（§6.10 delivery receipt、载荷上限）留待真实第二通道接入时再上，
// 不预先抽象（YAGNI：飞书/企微当前是留缝不是实现）。
type Port interface {
	// Name 通道名（注册表 key；与 Target.Platform 同一命名空间）。
	Name() string
	// SendText 发送纯文本到私聊目标。
	SendText(ctx context.Context, to Target, text string) error
	// SendMessage 发送图文/呈现物（文本+附件）到私聊目标。
	SendMessage(ctx context.Context, to Target, msg Message) error
}

type DeliveryStatus string

const (
	DeliveryAccepted       DeliveryStatus = "accepted"
	DeliveryDelivered      DeliveryStatus = "delivered"
	DeliveryFailed         DeliveryStatus = "failed"
	DeliveryOutcomeUnknown DeliveryStatus = "outcome_unknown"
)

type DeliveryAck struct {
	ExternalMessageID string         `json:"external_message_id"`
	Status            DeliveryStatus `json:"status"`
	Target            Target         `json:"target"`
}

// ReceiptPort is the optional ChannelPort capability used by user-facing
// "send to phone" flows. SendMessageWithReceipt returns provider acceptance;
// callers must query until a terminal status before claiming delivery.
type ReceiptPort interface {
	Port
	SendMessageWithReceipt(ctx context.Context, to Target, msg Message) (DeliveryAck, error)
	QueryReceipt(ctx context.Context, to Target, externalMessageID string) (DeliveryAck, error)
}

// PartReceiptPort 是逐 part 媒体准备与可核验外发的可选通道能力。
type PartReceiptPort interface {
	ReceiptPort
	PrepareDeliveryPartResource(ctx context.Context, to Target, part DeliveryPart) (preparedResourceID string, err error)
	SendPreparedPartWithReceipt(ctx context.Context, to Target, part DeliveryPart) (DeliveryAck, error)
}
