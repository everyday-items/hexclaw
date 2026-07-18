package main

// ChannelPort 通道收敛（架构设计-v0.5.0 §6.10 / ADR-K12-011）——composition root 侧：
// 原先分散在 main.go 的钉钉直连（k12IMDeliverer 直调 instanceMgr.Send、cron IM 直发）
// 收敛到 channel.Registry（name→ChannelPort）；K12 场景层继续只见 apihttp.IMBinder /
// IMDeliverer 窄缝（AP-1），本文件是这两条缝到通道端口的装配适配。
// 行为对家长逐字节不变：文案保持原文、限速仍在 adapter per-platform SendQueue、
// 限绑语义原样（收敛到 channel.CheckExclusiveBind）。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/cron"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
)

// k12IMDeliverer 把 K12「发送到手机」（作品点评/观察练习卡，§3.10/§3.12）接到通道端口：
// 按 agent 查绑定私聊路由规则（与 bind-im 同一规则源、天然继承限绑裁决）→ 按规则的
// platform 从通道注册表取 ChannelPort 发辅导延伸消息。就绪标记在实例管理器建成后
// 回填（K12 mount 早于 instances.NewManager 的装配顺序使然）；未就绪/未绑定/通道未
// 配置/留缝未实现都诚实报错——文案家长向（§4.11），HTTP 层据此降级（501/409），
// 前端提供复制文本兜底，绝不虚标已发送。
type k12IMDeliverer struct {
	router   *agentrouter.Dispatcher
	channels *channel.Registry
	mu       sync.RWMutex
	ready    bool
}

// MarkReady 标记通道发送链路就绪（instanceMgr 建成、钉钉通道 sender 回填后调用）。
func (d *k12IMDeliverer) MarkReady() {
	d.mu.Lock()
	d.ready = true
	d.mu.Unlock()
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
	content = imLaTeXFallback(content, "k12_send_to_phone")
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
		if err := ch.SendText(ctx, to, content); err != nil {
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
	direct func(ctx context.Context, target, chatID, content string) error,
) func(job *cron.Job, target, content string) error {
	return func(job *cron.Job, target, content string) error {
		if job.ChatID == "" {
			return fmt.Errorf("job %s has no chat_id for IM target %q", job.ID, target)
		}
		// cron IM 投递统一过 LaTeX→Unicode 兜底：在两条分支（通道端口/平台直发回退）
		// 收口之前转换一次，避免多处散接；幂等，无 LaTeX 零改动。
		content = imLaTeXFallback(content, "cron_im")
		if channels != nil {
			if ch, err := channels.Get(target); err == nil {
				serr := ch.SendText(ctx, channel.Target{Platform: target, ChatID: job.ChatID}, content)
				if serr == nil || (!errors.Is(serr, channel.ErrNotImplemented) && !errors.Is(serr, channel.ErrNotReady)) {
					return serr
				}
			}
		}
		return direct(ctx, target, job.ChatID, content)
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

// adapterReplyFromChannelMessage 把 ChannelNeutralMessage（§6.10）投影为平台 adapter.Reply
// （钉钉侧渲染入口；附件现状全为图片，base64 编码与收敛前 k12PhotoReply 逐字节一致）。
func adapterReplyFromChannelMessage(msg channel.Message) *adapter.Reply {
	reply := &adapter.Reply{Content: msg.Text}
	for _, att := range msg.Attachments {
		reply.Attachments = append(reply.Attachments, adapter.Attachment{
			Type: "image",
			Name: att.Name,
			Mime: att.MIME,
			Data: base64.StdEncoding.EncodeToString(att.Data),
		})
	}
	return reply
}
