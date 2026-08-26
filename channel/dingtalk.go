package channel

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/messagecontent"
)

// SendFunc 平台发送函数：由 composition root 注入（cmd/hexclaw 包
// instances.Manager.Send + ChannelNeutralMessage→adapter.Reply 投影）。
// 限速在平台 adapter 的 per-platform SendQueue 内同源生效，本层不重复实现。
type SendFunc func(ctx context.Context, to Target, msg Message) error

type ReceiptSendFunc func(ctx context.Context, to Target, msg Message) (DeliveryAck, error)
type ReceiptQueryFunc func(ctx context.Context, to Target, externalMessageID string) (DeliveryAck, error)
type DeliveryPartPrepareFunc func(ctx context.Context, to Target, part DeliveryPart) (preparedResourceID string, err error)
type DeliveryPartSendFunc func(ctx context.Context, to Target, part DeliveryPart) (DeliveryAck, error)
type PreparedEnvelopePreflightFunc func(ctx context.Context, to Target, envelope PreparedEnvelope) error
type PreparedEnvelopeSendFunc func(ctx context.Context, to Target, envelope PreparedEnvelope) (DeliveryAck, error)

// DingTalk 钉钉通道：v0.5.0 唯一真实通道（§3.12「当前真实测试优先钉钉」）。
// 它是既有钉钉直连路径的收敛点——不新增行为，只把「经 instanceMgr.Send 直发」
// 装进端口形状；文案/限速/限绑全部保持在原归属层，不在此复制。
//
// 装配顺序缝：K12 mount 早于 instances.NewManager，故 sender 允许后置回填
// （SetSender）；未回填前发送返回 ErrNotReady（诚实，不静默丢）。
type DingTalk struct {
	mu                sync.RWMutex
	send              SendFunc
	receiptSend       ReceiptSendFunc
	receiptQuery      ReceiptQueryFunc
	partPrepare       DeliveryPartPrepareFunc
	partSend          DeliveryPartSendFunc
	envelopePreflight PreparedEnvelopePreflightFunc
	envelopeSend      PreparedEnvelopeSendFunc
}

// NewDingTalk 建钉钉通道（sender 待回填）。
func NewDingTalk() *DingTalk {
	return &DingTalk{}
}

// SetSender 回填真实发送函数（instanceMgr 建成后，composition root 调用一次）。
func (d *DingTalk) SetSender(fn SendFunc) {
	d.mu.Lock()
	d.send = fn
	d.mu.Unlock()
}

// SetReceiptTransport wires the provider's send acknowledgement and status
// query endpoints. They remain separate so HTTP acceptance cannot be mistaken
// for a delivered terminal.
func (d *DingTalk) SetReceiptTransport(send ReceiptSendFunc, query ReceiptQueryFunc) {
	d.mu.Lock()
	d.receiptSend = send
	d.receiptQuery = query
	d.mu.Unlock()
}

// SetDeliveryPartTransport 回填逐 part 的媒体准备与可核验发送函数。
func (d *DingTalk) SetDeliveryPartTransport(prepare DeliveryPartPrepareFunc, send DeliveryPartSendFunc) {
	d.mu.Lock()
	d.partPrepare = prepare
	d.partSend = send
	d.mu.Unlock()
}

// SetPreparedEnvelopeTransport 回填作品点评图文同卡的可核验发送函数。
func (d *DingTalk) SetPreparedEnvelopeTransport(send PreparedEnvelopeSendFunc) {
	d.mu.Lock()
	d.envelopeSend = send
	d.mu.Unlock()
}

// SetPreparedEnvelopePreflight 回填组 CAS 前的平台只读校验函数。
func (d *DingTalk) SetPreparedEnvelopePreflight(preflight PreparedEnvelopePreflightFunc) {
	d.mu.Lock()
	d.envelopePreflight = preflight
	d.mu.Unlock()
}

// Name 实现 Port。
func (d *DingTalk) Name() string { return "dingtalk" }

// SendText 实现 Port：纯文本按图文的空附件形态走同一发送路径。
func (d *DingTalk) SendText(ctx context.Context, to Target, text string) error {
	return d.SendMessage(ctx, to, Message{Text: text})
}

// SendMessage 实现 Port：透传给注入的平台发送函数。
// 发送前强制 EnsureDirect（K12-INV-015）：群编码目标在通道层被拒，绝不触达平台发送函数。
func (d *DingTalk) SendMessage(ctx context.Context, to Target, msg Message) error {
	if err := to.EnsureDirect(); err != nil {
		return fmt.Errorf("钉钉通道拒绝群会话目标（仅支持一对一私聊）: %w", err)
	}
	if err := msg.Validate(); err != nil {
		return err
	}
	d.mu.RLock()
	send := d.send
	d.mu.RUnlock()
	if send == nil {
		return fmt.Errorf("钉钉通道发送函数未回填: %w", ErrNotReady)
	}
	return send(ctx, to, msg)
}

func (d *DingTalk) SendMessageWithReceipt(ctx context.Context, to Target, msg Message) (DeliveryAck, error) {
	failed := DeliveryAck{Status: DeliveryFailed, Target: to}
	if err := to.EnsureDirect(); err != nil {
		return failed, fmt.Errorf("钉钉通道拒绝群会话目标（仅支持一对一私聊）: %w", err)
	}
	if err := msg.Validate(); err != nil {
		return failed, err
	}
	d.mu.RLock()
	send := d.receiptSend
	d.mu.RUnlock()
	if send == nil {
		return failed, fmt.Errorf("钉钉通道投递回执发送函数未回填: %w", ErrNotReady)
	}
	ack, err := send(ctx, to, msg)
	if ack.Target.Platform == "" {
		ack.Target = to
	}
	return ack, err
}

func (d *DingTalk) QueryReceipt(ctx context.Context, to Target, externalMessageID string) (DeliveryAck, error) {
	unknown := DeliveryAck{ExternalMessageID: externalMessageID, Status: DeliveryOutcomeUnknown, Target: to}
	if err := to.EnsureDirect(); err != nil {
		return unknown, fmt.Errorf("钉钉通道拒绝群会话目标（仅支持一对一私聊）: %w", err)
	}
	d.mu.RLock()
	query := d.receiptQuery
	d.mu.RUnlock()
	if query == nil {
		return unknown, fmt.Errorf("钉钉通道投递回执查询函数未回填: %w", ErrNotReady)
	}
	ack, err := query(ctx, to, externalMessageID)
	if ack.Target.Platform == "" {
		ack.Target = to
	}
	return ack, err
}

func (d *DingTalk) PrepareDeliveryPartResource(ctx context.Context, to Target, part DeliveryPart) (string, error) {
	if err := to.EnsureDirect(); err != nil {
		return "", fmt.Errorf("DingTalk delivery parts support direct targets only: %w", err)
	}
	if err := part.Validate(); err != nil {
		return "", err
	}
	if part.Kind != messagecontent.PartArtifact || strings.TrimSpace(part.PreparedResourceID) != "" {
		return "", fmt.Errorf("DingTalk media preparation requires one unprepared artifact part")
	}
	d.mu.RLock()
	prepare := d.partPrepare
	d.mu.RUnlock()
	if prepare == nil {
		return "", fmt.Errorf("DingTalk delivery part preparation is unavailable: %w", ErrNotReady)
	}
	resourceID, err := prepare(ctx, to, part)
	if err != nil {
		return "", err
	}
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return "", fmt.Errorf("DingTalk media preparation returned an empty resource id")
	}
	return resourceID, nil
}

func (d *DingTalk) SendPreparedPartWithReceipt(ctx context.Context, to Target, part DeliveryPart) (DeliveryAck, error) {
	failed := DeliveryAck{Status: DeliveryFailed, Target: to}
	if err := to.EnsureDirect(); err != nil {
		return failed, fmt.Errorf("DingTalk delivery parts support direct targets only: %w", err)
	}
	if err := part.Validate(); err != nil {
		return failed, err
	}
	if part.Kind == messagecontent.PartArtifact && strings.TrimSpace(part.PreparedResourceID) == "" {
		return failed, fmt.Errorf("DingTalk artifact delivery part has no prepared resource")
	}
	d.mu.RLock()
	send := d.partSend
	d.mu.RUnlock()
	if send == nil {
		return failed, fmt.Errorf("DingTalk delivery part sender is unavailable: %w", ErrNotReady)
	}
	ack, err := send(ctx, to, part)
	if ack.Target.Platform == "" {
		ack.Target = to
	}
	return ack, err
}

// SendPreparedEnvelopeWithReceipt 一次发送一份已准备完成的 CreativeWork 图文组合消息。
// 组合能力未装配或载荷不合法时直接失败，不退回既有逐 part 发送路径。
func (d *DingTalk) SendPreparedEnvelopeWithReceipt(ctx context.Context, to Target, envelope PreparedEnvelope) (DeliveryAck, error) {
	failed := DeliveryAck{Status: DeliveryFailed, Target: to}
	if err := to.EnsureDirect(); err != nil {
		return failed, fmt.Errorf("DingTalk prepared envelopes support direct targets only: %w", err)
	}
	if err := envelope.Validate(); err != nil {
		return failed, err
	}
	d.mu.RLock()
	send := d.envelopeSend
	d.mu.RUnlock()
	if send == nil {
		return failed, fmt.Errorf("DingTalk prepared envelope sender is unavailable: %w", ErrNotReady)
	}
	ack, err := send(ctx, to, envelope)
	if ack.Target.Platform == "" {
		ack.Target = to
	}
	return ack, err
}

// PreflightPreparedEnvelope 在发送状态 CAS 前完成平台组合消息校验。
func (d *DingTalk) PreflightPreparedEnvelope(ctx context.Context, to Target, envelope PreparedEnvelope) error {
	if err := to.EnsureDirect(); err != nil {
		return fmt.Errorf("DingTalk prepared envelopes support direct targets only: %w", err)
	}
	if err := envelope.Validate(); err != nil {
		return err
	}
	d.mu.RLock()
	preflight := d.envelopePreflight
	d.mu.RUnlock()
	if preflight == nil {
		return fmt.Errorf("DingTalk prepared envelope preflight is unavailable: %w", ErrNotReady)
	}
	return preflight(ctx, to, envelope)
}
