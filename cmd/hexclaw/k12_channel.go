package main

// ChannelPort 通道收敛（架构设计-v0.5.0 §6.10 / ADR-K12-011）——composition root 侧：
// 原先分散在 main.go 的钉钉直连（k12IMDeliverer 直调 instanceMgr.Send、cron IM 直发）
// 收敛到 channel.Registry（name→ChannelPort）；K12 场景层继续只见 apihttp.IMBinder /
// usecase.DeliveryTransport 窄缝（AP-1），本文件是这两条缝到通道端口的装配适配。
// 行为对家长逐字节不变：文案保持原文、限速仍在 adapter per-platform SendQueue、
// 限绑语义原样（收敛到 channel.CheckExclusiveBind）。

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// k12IMDeliverer 把 K12「发送到手机」（辅导要点、练习集、积累内容）接到通道端口：
// 按 agent 查绑定私聊路由规则（与 bind-im 同一规则源、天然继承限绑裁决）→ 按规则的
// platform 从通道注册表取 ChannelPort 发辅导延伸消息。就绪标记在实例管理器建成后
// 回填（K12 mount 早于 instances.NewManager 的装配顺序使然）；未就绪/未绑定/通道未
// 配置/留缝未实现都诚实报错——文案家长向（§4.11），HTTP 层据此降级（501/409）；
// 前端展示持久化回执并仅允许明确失败重试，绝不以复制或平台受理冒充已送达。
type k12IMDeliverer struct {
	router            *agentrouter.Dispatcher
	channels          *channel.Registry
	mu                sync.RWMutex
	ready             bool
	resolveInstanceID func(platform, instanceRef string) (string, error)
}

var _ k12usecase.DeliveryEnvelopeTransport = (*k12IMDeliverer)(nil)

func channelAckFromAdapter(ack adapter.DeliveryAck, target channel.Target) channel.DeliveryAck {
	status := channel.DeliveryOutcomeUnknown
	switch ack.Status {
	case adapter.DeliveryAccepted:
		status = channel.DeliveryAccepted
	case adapter.DeliveryDelivered:
		status = channel.DeliveryDelivered
	case adapter.DeliveryFailed:
		status = channel.DeliveryFailed
	case adapter.DeliveryOutcomeUnknown:
		status = channel.DeliveryOutcomeUnknown
	}
	return channel.DeliveryAck{ExternalMessageID: ack.ExternalMessageID, Status: status, Target: target}
}

// MarkReady 标记通道发送链路就绪（instanceMgr 建成、钉钉通道 sender 回填后调用）。
func (d *k12IMDeliverer) MarkReady() {
	d.mu.Lock()
	d.ready = true
	d.mu.Unlock()
}

// SetInstanceResolver 回填运行实例解析器；绑定目标必须在批次冻结前转换为稳定实例 ID。
func (d *k12IMDeliverer) SetInstanceResolver(
	resolve func(platform, instanceRef string) (string, error),
) {
	d.mu.Lock()
	d.resolveInstanceID = resolve
	d.mu.Unlock()
}

type resolvedDirectBinding struct {
	rule   agentrouter.Rule
	target channel.Target
}

func normalizeDirectRule(rule agentrouter.Rule) agentrouter.Rule {
	rule.Platform = strings.ToLower(strings.TrimSpace(rule.Platform))
	rule.InstanceID = strings.TrimSpace(rule.InstanceID)
	rule.UserID = strings.TrimSpace(rule.UserID)
	rule.ChatID = strings.TrimSpace(rule.ChatID)
	rule.AgentName = strings.TrimSpace(rule.AgentName)
	return rule
}

func (d *k12IMDeliverer) resolveDirectBindings(agentName string) ([]resolvedDirectBinding, error) {
	d.mu.RLock()
	ready := d.ready
	resolveInstanceID := d.resolveInstanceID
	d.mu.RUnlock()
	if !ready {
		return nil, fmt.Errorf("发送通道还没就绪，稍等片刻再试")
	}
	agentName = strings.TrimSpace(agentName)
	candidates := make([]resolvedDirectBinding, 0)
	for _, rule := range d.router.ListRules() {
		rule = normalizeDirectRule(rule)
		if rule.AgentName != agentName || rule.ChatID == "" {
			continue
		}
		target := channel.Target{Platform: rule.Platform, InstanceID: rule.InstanceID, ChatID: rule.ChatID}
		if err := target.EnsureDirect(); err != nil || target.Platform == "" {
			continue
		}
		instanceID := target.InstanceID
		if resolveInstanceID != nil {
			var err error
			instanceID, err = resolveInstanceID(target.Platform, instanceID)
			if err != nil {
				return nil, fmt.Errorf("resolve direct binding instance: %w", err)
			}
			instanceID = strings.TrimSpace(instanceID)
		}
		if instanceID == "" {
			return nil, fmt.Errorf("resolve direct binding instance: stable instance ID is required")
		}
		target.InstanceID = instanceID
		candidates = append(candidates, resolvedDirectBinding{rule: rule, target: target})
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.target.Platform != right.target.Platform {
			return left.target.Platform < right.target.Platform
		}
		if left.target.InstanceID != right.target.InstanceID {
			return left.target.InstanceID < right.target.InstanceID
		}
		if left.target.ChatID != right.target.ChatID {
			return left.target.ChatID < right.target.ChatID
		}
		if left.rule.ID > 0 && right.rule.ID > 0 && left.rule.ID != right.rule.ID {
			return left.rule.ID < right.rule.ID
		}
		return stableBindingID(left.rule) < stableBindingID(right.rule)
	})
	out := candidates[:0]
	for _, candidate := range candidates {
		if len(out) > 0 && out[len(out)-1].target == candidate.target {
			continue
		}
		out = append(out, candidate)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"%w: 这个辅导助手还没绑定手机私聊：先在连接设置里绑定",
			k12usecase.ErrNoActiveDirectBindings,
		)
	}
	return out, nil
}

func (d *k12IMDeliverer) resolveDirectBinding(agentName string) (agentrouter.Rule, channel.Target, error) {
	bindings, err := d.resolveDirectBindings(agentName)
	if err != nil {
		return agentrouter.Rule{}, channel.Target{}, err
	}
	if len(bindings) != 1 {
		return agentrouter.Rule{}, channel.Target{}, fmt.Errorf(
			"这个辅导助手有 %d 个手机私聊绑定，必须使用批次投递", len(bindings),
		)
	}
	return bindings[0].rule, bindings[0].target, nil
}

func stableBindingID(rule agentrouter.Rule) string {
	rule = normalizeDirectRule(rule)
	if rule.ID > 0 {
		return "agent-rule:" + strconv.Itoa(rule.ID)
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		rule.Platform, rule.InstanceID, rule.UserID, rule.ChatID, rule.AgentName,
	}, "\x00")))
	return "agent-rule:sha256:" + hex.EncodeToString(sum[:])
}

func deliveryPayloadDigest(payloadJSON string) string {
	sum := sha256.Sum256([]byte(payloadJSON))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (d *k12IMDeliverer) ResolveTextTargets(
	_ context.Context,
	agentName string,
) ([]k12usecase.ResolvedDeliveryTarget, error) {
	bindings, err := d.resolveDirectBindings(agentName)
	if err != nil {
		return nil, err
	}
	out := make([]k12usecase.ResolvedDeliveryTarget, 0, len(bindings))
	for _, binding := range bindings {
		out = append(out, k12usecase.ResolvedDeliveryTarget{
			BindingID: stableBindingID(binding.rule),
			Target: k12.DeliveryTarget{
				Platform:   binding.target.Platform,
				InstanceID: binding.target.InstanceID,
				ChatID:     binding.target.ChatID,
				Label:      binding.target.Platform,
			},
		})
	}
	return out, nil
}

func (d *k12IMDeliverer) PrepareTextForTargets(
	ctx context.Context,
	content string,
	targets []k12usecase.ResolvedDeliveryTarget,
) ([]k12usecase.PreparedTextDelivery, error) {
	return d.PrepareMessageForTargets(
		ctx, k12usecase.DeliveryMessage{Content: content}, targets,
	)
}

func (d *k12IMDeliverer) PrepareMessageForTargets(
	_ context.Context,
	source k12usecase.DeliveryMessage,
	targets []k12usecase.ResolvedDeliveryTarget,
) ([]k12usecase.PreparedTextDelivery, error) {
	if len(targets) == 0 {
		return nil, k12usecase.ErrNoActiveDirectBindings
	}
	canonical := source.Content
	// 批改最终产物的 canonical Markdown 保留逐题内部评估 JSON 以供审计；家长侧
	// 钉钉消息只投影可读的题目、状态与辅导正文，避免把内部 JSON 送入平台载荷。
	visible := k12FinalArtifactIMMarkdown(canonical)
	projected := imLaTeXFallback(visible, "k12_send_to_phone")
	fallbackReason := ""
	if projected != visible {
		fallbackReason = messagecontent.FallbackMathToReadableText
	}
	attachments := make([]channel.Attachment, 0, len(source.Attachments))
	for _, attachment := range source.Attachments {
		attachments = append(attachments, channel.Attachment{
			Name: attachment.Name, MIME: attachment.MIME,
			Data: append([]byte(nil), attachment.Data...),
		})
	}
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(
		messagecontent.ProducerK12, "zh-CN", canonical, projected, fallbackReason, attachments,
	)
	if err != nil {
		return nil, fmt.Errorf("发送内容校验失败，请重试: %w", err)
	}
	parts, err := message.DeliveryParts()
	if err != nil {
		return nil, fmt.Errorf("freeze delivery parts: %w", err)
	}
	renderJSON, err := json.Marshal(message.RenderManifest)
	if err != nil {
		return nil, fmt.Errorf("冻结渲染证据失败: %w", err)
	}
	out := make([]k12usecase.PreparedTextDelivery, 0, len(targets)*len(parts))
	seen := make(map[channel.Target]struct{}, len(targets))
	for _, resolved := range targets {
		target := channel.Target{
			Platform:   strings.ToLower(strings.TrimSpace(resolved.Target.Platform)),
			InstanceID: strings.TrimSpace(resolved.Target.InstanceID),
			ChatID:     strings.TrimSpace(resolved.Target.ChatID),
		}
		if strings.TrimSpace(resolved.BindingID) == "" || target.Platform == "" || target.ChatID == "" {
			return nil, fmt.Errorf("冻结发送目标失败：绑定或私聊目标不完整")
		}
		if err := target.EnsureDirect(); err != nil {
			return nil, fmt.Errorf("冻结发送目标失败：%w", err)
		}
		if _, duplicate := seen[target]; duplicate {
			return nil, fmt.Errorf("冻结发送目标失败：存在重复私聊目标")
		}
		seen[target] = struct{}{}
		for _, part := range parts {
			payloadJSON, marshalErr := json.Marshal(part)
			if marshalErr != nil {
				return nil, fmt.Errorf("encode frozen delivery part: %w", marshalErr)
			}
			out = append(out, k12usecase.PreparedTextDelivery{
				BindingID: strings.TrimSpace(resolved.BindingID),
				Target: k12.DeliveryTarget{
					Platform: target.Platform, InstanceID: target.InstanceID, ChatID: target.ChatID,
					Label: strings.TrimSpace(resolved.Target.Label),
				},
				PartKind:    part.Kind,
				PartMIME:    part.MIME,
				PartOrdinal: part.Ordinal,
				PartDigest:  part.Digest,
				PayloadJSON: string(payloadJSON),
				RenderJSON:  string(renderJSON),
			})
		}
	}
	return out, nil
}

// PrepareText is retained for compatibility with singleton callers. Production
// K12 commands use ResolveTextTargets + PrepareTextForTargets and never choose
// the first binding from a multi-binding snapshot.
func (d *k12IMDeliverer) PrepareText(ctx context.Context, agentName, content string) (k12usecase.PreparedTextDelivery, error) {
	targets, err := d.ResolveTextTargets(ctx, agentName)
	if err != nil {
		return k12usecase.PreparedTextDelivery{}, err
	}
	if len(targets) != 1 {
		return k12usecase.PreparedTextDelivery{}, fmt.Errorf(
			"这个辅导助手有 %d 个手机私聊绑定，必须使用批次投递", len(targets),
		)
	}
	prepared, err := d.PrepareTextForTargets(ctx, content, targets)
	if err != nil {
		return k12usecase.PreparedTextDelivery{}, err
	}
	return prepared[0], nil
}

func channelTargetFromReceipt(receipt k12.DeliveryReceipt) channel.Target {
	return channel.Target{
		Platform: receipt.Target.Platform, InstanceID: receipt.Target.InstanceID, ChatID: receipt.Target.ChatID,
	}
}

func (d *k12IMDeliverer) receiptBindingIsActive(receipt k12.DeliveryReceipt) bool {
	target := channelTargetFromReceipt(receipt)
	target.Platform = strings.ToLower(strings.TrimSpace(target.Platform))
	target.InstanceID = strings.TrimSpace(target.InstanceID)
	target.ChatID = strings.TrimSpace(target.ChatID)
	if target.Platform == "" || target.InstanceID == "" || target.ChatID == "" {
		return false
	}
	d.mu.RLock()
	resolveInstanceID := d.resolveInstanceID
	d.mu.RUnlock()
	for _, rule := range d.router.ListRules() {
		rule = normalizeDirectRule(rule)
		if rule.AgentName != receipt.AgentName || stableBindingID(rule) != receipt.BindingID {
			continue
		}
		if rule.Platform != target.Platform ||
			(rule.ChatID != "" && rule.ChatID != target.ChatID) {
			return false
		}
		if resolveInstanceID == nil {
			return rule.InstanceID == target.InstanceID
		}
		instanceRef := rule.InstanceID
		if instanceRef == "" {
			instanceRef = target.InstanceID
		}
		resolved, err := resolveInstanceID(rule.Platform, instanceRef)
		return err == nil && strings.TrimSpace(resolved) == target.InstanceID
	}
	return false
}

func preparedChannelPart(receipt k12.DeliveryReceipt) (channel.DeliveryPart, error) {
	if got := deliveryPayloadDigest(receipt.PayloadJSON); got != receipt.PayloadDigest {
		return channel.DeliveryPart{}, fmt.Errorf("frozen delivery payload digest mismatch: want %s got %s", receipt.PayloadDigest, got)
	}
	var part channel.DeliveryPart
	if err := json.Unmarshal([]byte(receipt.PayloadJSON), &part); err != nil {
		return channel.DeliveryPart{}, fmt.Errorf("decode frozen delivery part: %w", err)
	}
	if validationErr := part.Validate(); validationErr != nil {
		var legacyEnvelope struct {
			Content        json.RawMessage `json:"Content"`
			RenderManifest json.RawMessage `json:"RenderManifest"`
		}
		if err := json.Unmarshal([]byte(receipt.PayloadJSON), &legacyEnvelope); err != nil ||
			len(legacyEnvelope.Content) == 0 || len(legacyEnvelope.RenderManifest) == 0 {
			return channel.DeliveryPart{}, validationErr
		}
		// V84 回执冻结的是整份消息；升级后的单一子回执只恢复其首个 canonical Markdown part。
		var legacyMessage channel.Message
		if err := json.Unmarshal([]byte(receipt.PayloadJSON), &legacyMessage); err != nil {
			return channel.DeliveryPart{}, fmt.Errorf("decode legacy frozen delivery message: %w", err)
		}
		legacyParts, err := legacyMessage.DeliveryParts()
		if err != nil {
			return channel.DeliveryPart{}, fmt.Errorf("derive legacy frozen delivery part: %w", err)
		}
		if len(legacyParts) == 0 || legacyParts[0].Kind != messagecontent.PartMarkdown || legacyParts[0].Ordinal != 1 {
			return channel.DeliveryPart{}, fmt.Errorf("legacy frozen delivery message has no canonical Markdown part")
		}
		part = legacyParts[0]
	}
	if err := part.Validate(); err != nil {
		return channel.DeliveryPart{}, err
	}
	if part.Kind != receipt.PartKind || part.MIME != receipt.PartMIME ||
		part.Ordinal != receipt.PartOrdinal || part.Digest != receipt.PartDigest {
		return channel.DeliveryPart{}, fmt.Errorf("frozen delivery part identity does not match receipt")
	}
	renderJSON, err := json.Marshal(part.RenderManifest)
	if err != nil || string(renderJSON) != receipt.RenderJSON {
		return channel.DeliveryPart{}, fmt.Errorf("frozen delivery render evidence mismatch")
	}
	return part, nil
}

func usecaseAckFromChannel(ack channel.DeliveryAck, err error) k12usecase.DeliveryTransportAck {
	status := k12.DeliveryOutcomeUnknown
	switch ack.Status {
	case channel.DeliveryAccepted:
		status = k12.DeliverySending
	case channel.DeliveryDelivered:
		status = k12.DeliveryDelivered
	case channel.DeliveryFailed:
		status = k12.DeliveryFailed
	case channel.DeliveryOutcomeUnknown:
		status = k12.DeliveryOutcomeUnknown
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return k12usecase.DeliveryTransportAck{
		ExternalMessageID: ack.ExternalMessageID,
		Status:            status,
		Detail:            detail,
	}
}

type creativeEnvelopePhase uint8

const (
	creativeEnvelopePreflight creativeEnvelopePhase = iota
	creativeEnvelopeSend
	creativeEnvelopeQuery
)

// preparedCreativeWorkEnvelope 从同一物理目标的 component 回执恢复一次图文同卡外发。
// 恢复过程只读取已冻结的 canonical 证据与媒体引用；任何身份或状态漂移都会在通道调用前失败。
func (d *k12IMDeliverer) preparedCreativeWorkEnvelope(
	receipts []k12.DeliveryReceipt,
	phase creativeEnvelopePhase,
) (channel.Target, channel.PreparedEnvelope, error) {
	if len(receipts) < 2 {
		return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf(
			"creative work delivery envelope requires markdown and at least one image",
		)
	}
	first := receipts[0]
	target := channelTargetFromReceipt(first)
	if strings.TrimSpace(first.ObjectKind) != "creative_work" ||
		strings.TrimSpace(first.BatchID) == "" ||
		strings.TrimSpace(first.AgentName) == "" ||
		strings.TrimSpace(first.ObjectID) == "" ||
		strings.TrimSpace(first.BindingID) == "" ||
		strings.TrimSpace(target.Platform) == "" ||
		strings.TrimSpace(target.InstanceID) == "" ||
		strings.TrimSpace(target.ChatID) == "" {
		return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf(
			"creative work delivery envelope identity is incomplete",
		)
	}
	if err := target.EnsureDirect(); err != nil {
		return channel.Target{}, channel.PreparedEnvelope{}, err
	}
	if phase == creativeEnvelopeQuery {
		if first.Attempt < 1 {
			return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf("creative work delivery envelope has no attempt")
		}
		if (first.Status != k12.DeliverySending && first.Status != k12.DeliveryOutcomeUnknown) ||
			strings.TrimSpace(first.ExternalMessageID) == "" {
			return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf(
				"creative work delivery envelope is not in a queryable shared state",
			)
		}
	} else if phase == creativeEnvelopeSend {
		if first.Attempt < 1 {
			return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf("creative work delivery envelope has no attempt")
		}
		if first.Status != k12.DeliverySending || strings.TrimSpace(first.ExternalMessageID) != "" {
			return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf(
				"creative work delivery envelope is not in a sendable shared state",
			)
		}
		if !d.receiptBindingIsActive(first) {
			return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf(
				"the original delivery binding is no longer active",
			)
		}
	} else {
		if (first.Status != k12.DeliveryPending && first.Status != k12.DeliveryFailed) ||
			strings.TrimSpace(first.ExternalMessageID) != "" {
			return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf(
				"creative work delivery envelope is not in a preflightable shared state",
			)
		}
		if !d.receiptBindingIsActive(first) {
			return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf(
				"the original delivery binding is no longer active",
			)
		}
	}

	parts := make([]channel.DeliveryPart, 0, len(receipts))
	for i, receipt := range receipts {
		if receipt.AgentName != first.AgentName || receipt.ObjectKind != first.ObjectKind ||
			receipt.ObjectID != first.ObjectID || receipt.BatchID != first.BatchID ||
			receipt.BindingID != first.BindingID || channelTargetFromReceipt(receipt) != target ||
			receipt.BatchOrdinal != first.BatchOrdinal+i || receipt.PartOrdinal != i+1 ||
			receipt.Status != first.Status || receipt.Attempt != first.Attempt ||
			receipt.ExternalMessageID != first.ExternalMessageID {
			return channel.Target{}, channel.PreparedEnvelope{}, fmt.Errorf(
				"creative work delivery envelope components do not share one frozen identity",
			)
		}
		part, err := preparedChannelPart(receipt)
		if err != nil {
			return channel.Target{}, channel.PreparedEnvelope{}, err
		}
		part.PreparedResourceID = strings.TrimSpace(receipt.PreparedResourceID)
		parts = append(parts, part)
	}
	envelope := channel.PreparedEnvelope{Parts: parts}
	if err := envelope.Validate(); err != nil {
		return channel.Target{}, channel.PreparedEnvelope{}, err
	}
	return target, envelope, nil
}

// SendPreparedEnvelope 只用于 creative_work 的同一目标 Markdown+图片组合消息。
// 不支持组合能力的通道直接失败，禁止降级为逐 part 外发而制造第二个图片气泡。
func (d *k12IMDeliverer) SendPreparedEnvelope(
	ctx context.Context,
	receipts []k12.DeliveryReceipt,
) (k12usecase.DeliveryTransportAck, error) {
	target, envelope, err := d.preparedCreativeWorkEnvelope(receipts, creativeEnvelopeSend)
	if err != nil {
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryFailed, Detail: err.Error()}, err
	}
	port, err := d.channels.Get(target.Platform)
	if err != nil {
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryFailed, Detail: err.Error()}, err
	}
	envelopePort, ok := port.(channel.PreparedEnvelopeReceiptPort)
	if !ok {
		err = fmt.Errorf("channel %q does not support prepared delivery envelopes", target.Platform)
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryFailed, Detail: err.Error()}, err
	}
	ack, sendErr := envelopePort.SendPreparedEnvelopeWithReceipt(ctx, target, envelope)
	return usecaseAckFromChannel(ack, sendErr), sendErr
}

// PreflightPreparedEnvelope 在组 CAS 前用冻结载荷和已持久化媒体引用执行平台完整校验。
func (d *k12IMDeliverer) PreflightPreparedEnvelope(
	ctx context.Context,
	receipts []k12.DeliveryReceipt,
) error {
	target, envelope, err := d.preparedCreativeWorkEnvelope(receipts, creativeEnvelopePreflight)
	if err != nil {
		return err
	}
	port, err := d.channels.Get(target.Platform)
	if err != nil {
		return err
	}
	preflight, ok := port.(channel.PreparedEnvelopePreflightPort)
	if !ok {
		return fmt.Errorf("channel %q does not support prepared envelope preflight", target.Platform)
	}
	return preflight.PreflightPreparedEnvelope(ctx, target, envelope)
}

// QueryPreparedEnvelope 对共享 external ID 只查询一次，并把 provider 结果交给组状态机统一收敛。
func (d *k12IMDeliverer) QueryPreparedEnvelope(
	ctx context.Context,
	receipts []k12.DeliveryReceipt,
) (k12usecase.DeliveryTransportAck, error) {
	_, _, err := d.preparedCreativeWorkEnvelope(receipts, creativeEnvelopeQuery)
	if err != nil {
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryOutcomeUnknown, Detail: err.Error()}, err
	}
	return d.QueryPrepared(ctx, receipts[0])
}

// SendPrepared is only reached after Store.PrepareDeliveryReceipt and
// BeginDeliveryAttempt. Unsupported channels fail visibly in that same row.
func (d *k12IMDeliverer) SendPrepared(ctx context.Context, receipt k12.DeliveryReceipt) (k12usecase.DeliveryTransportAck, error) {
	if !d.receiptBindingIsActive(receipt) {
		err := fmt.Errorf("原投递绑定已经变更，请重新从当前页面发起发送")
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryFailed, Detail: err.Error()}, err
	}
	part, err := preparedChannelPart(receipt)
	if err != nil {
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryFailed, Detail: err.Error()}, err
	}
	part.PreparedResourceID = strings.TrimSpace(receipt.PreparedResourceID)
	if err := part.Validate(); err != nil {
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryFailed, Detail: err.Error()}, err
	}
	port, err := d.channels.Get(receipt.Target.Platform)
	if err != nil {
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryFailed, Detail: "绑定通道还没有接入"}, err
	}
	receiptPort, ok := port.(channel.PartReceiptPort)
	if !ok {
		err = fmt.Errorf("channel %q does not support delivery part receipts", receipt.Target.Platform)
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryFailed, Detail: err.Error()}, err
	}
	ack, sendErr := receiptPort.SendPreparedPartWithReceipt(ctx, channelTargetFromReceipt(receipt), part)
	return usecaseAckFromChannel(ack, sendErr), sendErr
}

// PrepareDeliveryPartResource 在任何可见 part 发送前准备并返回一个平台媒体引用。
func (d *k12IMDeliverer) PrepareDeliveryPartResource(ctx context.Context, receipt k12.DeliveryReceipt) (string, error) {
	if !d.receiptBindingIsActive(receipt) {
		return "", fmt.Errorf("the original delivery binding is no longer active")
	}
	part, err := preparedChannelPart(receipt)
	if err != nil {
		return "", err
	}
	if part.Kind != messagecontent.PartArtifact || strings.TrimSpace(receipt.PreparedResourceID) != "" {
		return "", fmt.Errorf("delivery media preparation requires one unprepared artifact part")
	}
	port, err := d.channels.Get(receipt.Target.Platform)
	if err != nil {
		return "", err
	}
	partPort, ok := port.(channel.PartReceiptPort)
	if !ok {
		return "", fmt.Errorf("channel %q does not support delivery part preparation", receipt.Target.Platform)
	}
	resourceID, err := partPort.PrepareDeliveryPartResource(ctx, channelTargetFromReceipt(receipt), part)
	if err != nil {
		return "", err
	}
	resourceID = strings.TrimSpace(resourceID)
	if resourceID == "" {
		return "", fmt.Errorf("delivery part preparation returned an empty resource id")
	}
	return resourceID, nil
}

func (d *k12IMDeliverer) QueryPrepared(ctx context.Context, receipt k12.DeliveryReceipt) (k12usecase.DeliveryTransportAck, error) {
	if strings.TrimSpace(receipt.Target.InstanceID) == "" {
		err := fmt.Errorf("query prepared delivery requires a stable instance ID")
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryOutcomeUnknown, Detail: err.Error()}, err
	}
	port, err := d.channels.Get(receipt.Target.Platform)
	if err != nil {
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryOutcomeUnknown, Detail: "绑定通道暂时不可查询"}, err
	}
	receiptPort, ok := port.(channel.ReceiptPort)
	if !ok {
		err = fmt.Errorf("通道 %q 不支持投递结果查询", receipt.Target.Platform)
		return k12usecase.DeliveryTransportAck{Status: k12.DeliveryOutcomeUnknown, Detail: err.Error()}, err
	}
	ack, queryErr := receiptPort.QueryReceipt(ctx, channelTargetFromReceipt(receipt), receipt.ExternalMessageID)
	return usecaseAckFromChannel(ack, queryErr), queryErr
}

func (d *k12IMDeliverer) DeliverText(ctx context.Context, agentName, content string) (string, error) {
	d.mu.RLock()
	ready := d.ready
	d.mu.RUnlock()
	if !ready {
		return "", fmt.Errorf("发送通道还没就绪，稍等片刻再试，或先复制文本")
	}
	// IM 出口 LaTeX→Unicode 确定性兜底（钉钉不渲染 LaTeX；提示词硬禁是软约束，
	// BUG-20260712-U 同源风险）。幂等、无 LaTeX 零改动；桌面 HTTP 面不经此转换。
	canonical := content
	content = imLaTeXFallback(canonical, "k12_send_to_phone")
	fallbackReason := ""
	if content != canonical {
		fallbackReason = messagecontent.FallbackMathToReadableText
	}
	message, err := channel.NewCanonicalMarkdownMessageWithAttachments(messagecontent.ProducerK12, "zh-CN", canonical, content, fallbackReason, nil)
	if err != nil {
		return "", fmt.Errorf("发送内容校验失败，请重试: %w", err)
	}
	for _, rule := range d.router.ListRules() {
		if rule.AgentName != agentName || rule.ChatID == "" {
			continue
		}
		ch, err := d.channels.Get(rule.Platform)
		if err != nil {
			// 未配置通道 → 诚实降级（与 501/409 语义一致），不静默换通道。
			return "", fmt.Errorf("「%s」通道还没有接入，可以先复制文本发给家长", rule.Platform)
		}
		to := channel.Target{Platform: rule.Platform, InstanceID: rule.InstanceID, ChatID: rule.ChatID}
		if err := ch.SendMessage(ctx, to, message); err != nil {
			if errors.Is(err, channel.ErrNotImplemented) {
				// 留缝 stub（飞书/企微）：诚实「未开通」，不冒充普通发送失败。
				return "", fmt.Errorf("「%s」通道还没有开通，可以先复制文本发给家长", rule.Platform)
			}
			return "", fmt.Errorf("发送没有成功（%s）：可以先复制文本发给家长", rule.Platform)
		}
		return rule.Platform, nil
	}
	return "", fmt.Errorf("这个辅导助手还没绑定手机私聊：先在连接设置里绑定，或复制文本发给家长")
}

// k12IMBinder 把平台 router.Dispatcher + store 包成 K12 的 IMBinder 缝（AP-1：K12 不 import router）。
// 绑定 = 内存路由规则（即时生效）+ 持久化（重启存活），chat 级绑定（PRD §3.1.7 各绑各的群）。
// 限绑校验收敛到通道层 channel.CheckExclusiveBind（§3.12 语义归属，渠道中立）。
type k12IMBinder struct {
	router *agentrouter.Dispatcher
	store  *agentrouter.SQLiteStore
	mu     sync.Mutex
}

func (b *k12IMBinder) Bind(ctx context.Context, platform, instanceID, chatID, agentName string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	rules := b.router.ListRules()
	existing := make([]channel.Binding, 0, len(rules))
	for _, r := range rules {
		existing = append(existing, channel.Binding{Platform: r.Platform, InstanceID: r.InstanceID, ChatID: r.ChatID, AgentName: r.AgentName})
	}
	already, err := channel.CheckExclusiveBind(existing, channel.Binding{Platform: platform, InstanceID: instanceID, ChatID: chatID, AgentName: agentName})
	if err != nil {
		// 限绑拒绝（§3.12）打上业务冲突哨兵：HTTP 层映射 409，不冒充服务器故障。
		return fmt.Errorf("%w: %w", apihttp.ErrBindConflict, err)
	}
	if already {
		return nil // 幂等：同一实例重复绑定不报错、不重写
	}
	rule := agentrouter.Rule{
		Platform:   platform,
		InstanceID: instanceID,
		ChatID:     chatID,
		AgentName:  agentName,
		Priority:   50, // 群级显式绑定，优先于平台默认
	}
	return b.router.ReplaceRulePersisted(rule, func(persisted *agentrouter.Rule) error {
		if b.store == nil {
			return nil
		}
		return b.store.ReplaceRuleScope(ctx, persisted)
	})
}

// newCronIMDeliver 把平台 cron 的 IM 投递接到通道注册表：已注册通道（钉钉）的投递走
// ChannelPort（与 K12「发送到手机」同一端口，练习卷/提醒同源限速不变）；未注册目标
// （其他平台名/实例 ID）与留缝 stub（飞书/企微未实现）、通道未就绪时回退平台通用直发
// ——cron 是平台通用面，不因 K12 通道留缝停摆，行为保持收敛前逐字节一致。
func newCronIMDeliver(
	ctx context.Context,
	channels *channel.Registry,
	direct func(ctx context.Context, target, chatID string, msg channel.Message) error,
) func(job *cron.Job, target, content string) error {
	return func(job *cron.Job, target, content string) error {
		if job.ChatID == "" {
			return fmt.Errorf("job %s has no chat_id for IM target %q", job.ID, target)
		}
		// cron IM 投递统一过 LaTeX→Unicode 兜底：在两条分支（通道端口/平台直发回退）
		// 收口之前转换一次，避免多处散接；幂等，无 LaTeX 零改动。
		canonical := content
		content = imLaTeXFallback(canonical, "cron_im")
		fallbackReason := ""
		if content != canonical {
			fallbackReason = messagecontent.FallbackMathToReadableText
		}
		message, err := channel.NewCanonicalMarkdownMessageWithAttachments(messagecontent.ProducerCron, "zh-CN", canonical, content, fallbackReason, nil)
		if err != nil {
			return fmt.Errorf("cron %s 投递内容校验失败: %w", job.ID, err)
		}
		if channels != nil {
			if ch, err := channels.Get(target); err == nil {
				serr := ch.SendMessage(ctx, channel.Target{Platform: target, ChatID: job.ChatID}, message)
				if serr == nil || (!errors.Is(serr, channel.ErrNotImplemented) && !errors.Is(serr, channel.ErrNotReady)) {
					return serr
				}
			}
		}
		return direct(ctx, target, job.ChatID, message)
	}
}

// imLaTeXFallback 在 IM 投递出口应用 LaTeX→Unicode 确定性兜底转换（channel.LaTeXToUnicode，
// 呈现适配归通道层）。触发即 Warn——可观测「模型违反了提示词的 Unicode 数学符号约束」
// （solve 链硬禁声明见 engine/solve.go）。边界申报：桌面 HTTP 响应不经此转换，
// 桌面前端可自行渲染 LaTeX，保持原文（契约见 k12_mathtext_egress_test.go）。
func imLaTeXFallback(text, outlet string) string {
	out, changed := channel.LaTeXToUnicode(text)
	if changed {
		slog.Warn("IM 出口检测到 LaTeX，已兜底转换为 Unicode（模型违反提示词的 Unicode 数学符号约束）",
			"outlet", outlet)
	}
	return out
}

func k12FinalArtifactIMMarkdown(markdown string) string {
	if !strings.HasPrefix(strings.TrimSpace(markdown), "# 作业批改结果") ||
		!strings.Contains(markdown, "```json") {
		return markdown
	}
	lines := strings.Split(markdown, "\n")
	visible := make([]string, 0, len(lines))
	assessmentStatus := k12usecase.PhotoItemStatus("")
	replaced := false
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") {
			assessmentStatus = ""
		}
		if trimmed == "## Grading summary" {
			visible = append(visible, "## 批改摘要")
			replaced = true
			continue
		}
		if summary, ok := k12FinalArtifactLegacySummaryLine(trimmed); ok {
			visible = append(visible, summary)
			replaced = true
			continue
		}
		if trimmed == "> A process issue has a correct final answer and is not recorded as wrong." {
			visible = append(visible, "> 过程问题表示最终答案正确，但书写过程需要核对，不记为错题。")
			replaced = true
			continue
		}
		if status, ok := k12FinalArtifactAssessmentStatus(trimmed); ok {
			assessmentStatus = status
			visible = append(visible, "**批改状态：** "+k12FinalArtifactParentStatus(status))
			replaced = true
			continue
		}
		if strings.HasPrefix(trimmed, "**Process note:**") {
			visible = append(visible, strings.Replace(line, "**Process note:**", "**错误步骤：**", 1))
			replaced = true
			continue
		}
		if strings.HasPrefix(trimmed, "**Cause:**") {
			visible = append(visible, strings.Replace(line, "**Cause:**", "**原因：**", 1))
			replaced = true
			continue
		}
		if trimmed == "### How the parent can explain it" {
			visible = append(visible, "### 家长怎么讲")
			replaced = true
			continue
		}
		if assessmentStatus != "" && trimmed == "```json" {
			closing := index + 1
			for closing < len(lines) && strings.TrimSpace(lines[closing]) != "```" {
				closing++
			}
			if closing == len(lines) {
				return markdown
			}
			resultJSON := strings.Join(lines[index+1:closing], "\n")
			details, status, ok := k12usecase.RenderCanonicalGradingAssessmentDetails(resultJSON)
			if ok && status == assessmentStatus {
				if details != "" {
					visible = append(visible, "", details, "")
				}
				index = closing
				replaced = true
				continue
			}
		}
		visible = append(visible, line)
	}
	if !replaced {
		return markdown
	}
	return strings.TrimSpace(strings.Join(visible, "\n"))
}

func k12FinalArtifactAssessmentStatus(line string) (k12usecase.PhotoItemStatus, bool) {
	if !strings.HasPrefix(line, "**Grading status:**") {
		return "", false
	}
	start := strings.IndexByte(line, '`')
	if start < 0 {
		return "", false
	}
	end := strings.IndexByte(line[start+1:], '`')
	if end < 0 {
		return "", false
	}
	status := k12usecase.PhotoItemStatus(line[start+1 : start+1+end])
	switch status {
	case k12usecase.PhotoCorrect,
		k12usecase.PhotoCorrectWithProcessIssue,
		k12usecase.PhotoWrong,
		k12usecase.PhotoUnanswered,
		k12usecase.PhotoAnswerUnclear,
		k12usecase.PhotoBlankSolved,
		k12usecase.PhotoOutOfScope,
		k12usecase.PhotoUntrusted,
		k12usecase.PhotoFailed:
		return status, true
	default:
		return "", false
	}
}

func k12FinalArtifactParentStatus(status k12usecase.PhotoItemStatus) string {
	switch status {
	case k12usecase.PhotoCorrect:
		return "✅ 正确"
	case k12usecase.PhotoCorrectWithProcessIssue:
		return "⚠ 过程问题（最终答案正确，不记为错题）"
	case k12usecase.PhotoWrong:
		return "❌ 需要订正"
	case k12usecase.PhotoUnanswered:
		return "⏸ 未作答"
	case k12usecase.PhotoAnswerUnclear:
		return "⚠ 作答待补录"
	case k12usecase.PhotoBlankSolved:
		return "📘 已生成家长辅导指南"
	case k12usecase.PhotoOutOfScope:
		return "⛔ 超出当前年级范围"
	default:
		return "⚠ 待核对"
	}
}

func k12FinalArtifactLegacySummaryLine(line string) (string, bool) {
	const prefix = "This run determined **"
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(line, prefix)
	totalEnd := strings.Index(rest, "**")
	if totalEnd < 0 {
		return "", false
	}
	rest = rest[totalEnd+2:]
	const metricsPrefix = " questions: **"
	if !strings.HasPrefix(rest, metricsPrefix) {
		return "", false
	}
	metricsWithSuffix := strings.TrimPrefix(rest, metricsPrefix)
	metricsEnd := strings.Index(metricsWithSuffix, "**")
	if metricsEnd < 0 {
		return "", false
	}
	metrics := metricsWithSuffix[:metricsEnd]
	correct, ok := k12FinalArtifactLegacySummaryCount(metrics, "correct")
	if !ok {
		return "", false
	}
	processIssue, ok := k12FinalArtifactLegacySummaryCount(metrics, "with process issues")
	if !ok {
		return "", false
	}
	wrong, ok := k12FinalArtifactLegacySummaryCount(metrics, "requiring correction")
	if !ok {
		return "", false
	}
	result := fmt.Sprintf("**%d 道正确 / %d 道过程问题**", correct, processIssue)
	if wrong > 0 {
		result += fmt.Sprintf("\n\n另有 **%d 道需要订正**。", wrong)
	}
	return result, true
}

func k12FinalArtifactLegacySummaryCount(metrics, marker string) (int, bool) {
	markerIndex := strings.Index(metrics, marker)
	if markerIndex < 0 {
		return 0, false
	}
	countText := strings.TrimSpace(metrics[:markerIndex])
	if slashIndex := strings.LastIndex(countText, "/"); slashIndex >= 0 {
		countText = strings.TrimSpace(countText[slashIndex+1:])
	}
	count, err := strconv.Atoi(countText)
	return count, err == nil && count >= 0
}

// adapterReplyFromChannelMessage 把 ChannelNeutralMessage（§6.10）投影为平台 adapter.Reply。
// 图片与批准的 PDF 保留各自媒体类型；附件 bytes 只做 base64 编码，不转成本地路径或 URL。
func adapterReplyFromChannelMessage(msg channel.Message) *adapter.Reply {
	reply := &adapter.Reply{Content: msg.Text, MessageContent: msg.Content, RenderManifest: msg.RenderManifest}
	for _, att := range msg.Attachments {
		attachmentType := ""
		mime := strings.ToLower(strings.TrimSpace(att.MIME))
		if strings.HasPrefix(mime, "image/") {
			attachmentType = "image"
		} else if mime == "application/pdf" {
			attachmentType = "file"
		}
		reply.Attachments = append(reply.Attachments, adapter.Attachment{
			Type: attachmentType,
			Name: att.Name,
			Mime: att.MIME,
			Data: base64.StdEncoding.EncodeToString(att.Data),
		})
	}
	return reply
}

func adapterDeliveryPartFromChannelPart(part channel.DeliveryPart) adapter.DeliveryPart {
	result := adapter.DeliveryPart{
		Kind:               part.Kind,
		MIME:               part.MIME,
		Ordinal:            part.Ordinal,
		Digest:             part.Digest,
		Text:               part.Text,
		MessageContent:     part.MessageContent,
		RenderManifest:     part.RenderManifest,
		PreparedResourceID: part.PreparedResourceID,
	}
	if part.Attachment == nil {
		return result
	}
	mime := strings.ToLower(strings.TrimSpace(part.Attachment.MIME))
	attachmentType := ""
	if mime == "application/pdf" {
		attachmentType = "file"
	} else if strings.HasPrefix(mime, "image/") {
		attachmentType = "image"
	}
	result.Attachment = &adapter.Attachment{
		Type: attachmentType,
		Name: part.Attachment.Name,
		Mime: part.Attachment.MIME,
		Data: base64.StdEncoding.EncodeToString(part.Attachment.Data),
	}
	return result
}

func adapterPreparedEnvelopeFromChannelEnvelope(envelope channel.PreparedEnvelope) adapter.PreparedEnvelope {
	parts := make([]adapter.DeliveryPart, 0, len(envelope.Parts))
	for _, part := range envelope.Parts {
		parts = append(parts, adapterDeliveryPartFromChannelPart(part))
	}
	return adapter.PreparedEnvelope{Parts: parts}
}
