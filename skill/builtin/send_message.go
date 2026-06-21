package builtin

// 多通道送达 Skill（纯发送器）。
//
// 行为：
//   - adapter 包无中心 Manager：复用注入式 MessageSender（main 持有 live adapters + per-platform
//     SendQueue 限速），Skill 不自己 reach into adapters。
//   - 送达审批由统一安全闸（engine.PermissionPolicy 的 send-approve 规则 + 无人值守 LLM
//     风险顾问）在工具执行前统一执行 —— Skill 不再自管确认门（§11.10 统一安全闸）。
//   - 失败明确上抛，非静默。

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

// MessageSender 注入式发送抽象（narrow interface，便于测试 + 复用 main 已持有的 live adapters/SendQueue）。
type MessageSender interface {
	Send(ctx context.Context, channel, target, content string, atts []adapter.Attachment) error
}

// RiskReviewer 无人值守风险自审：对动作判级 low/med/high。统一安全闸（engine 侧）用
// 它判定无人值守 consequential 动作能否放行（仅 low）。真实实现 = NewLLMRiskReviewer。
type RiskReviewer interface {
	Assess(ctx context.Context, action, payload string) (RiskLevel, error)
}

// RiskLevel 风险等级。
type RiskLevel int

const (
	RiskLow RiskLevel = iota
	RiskMedium
	RiskHigh
)

// SendMessageSkill 把消息发到已配置渠道。审批由 engine 统一安全闸前置执行，Skill 只负责发。
type SendMessageSkill struct {
	sender MessageSender
}

func NewSendMessageSkill(sender MessageSender) *SendMessageSkill {
	return &SendMessageSkill{sender: sender}
}

func (s *SendMessageSkill) Name() string        { return "send_message" }
func (s *SendMessageSkill) Description() string { return "Send a message to a configured channel" }

// Match 只走 LLM tool-call 主路径。
func (s *SendMessageSkill) Match(string) bool { return false }

func (s *SendMessageSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("send_message",
		"Send a message to a configured channel (feishu/discord/wechat/email/slack/...). "+
			"Use to deliver a digest/report — merely replying delivers nothing.",
		&llm.Schema{Type: "object", Properties: map[string]*llm.Schema{
			"channel": {Type: "string", Description: "feishu|discord|wechat|email|slack|..."},
			"target":  {Type: "string", Description: "chat/group id or email address"},
			"content": {Type: "string", Description: "message body (required)"},
		}, Required: []string{"channel", "target", "content"}})
}

func (s *SendMessageSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	if s.sender == nil {
		return nil, fmt.Errorf("message sending is not configured")
	}
	channel := firstStringArg(args, "channel")
	target := firstStringArg(args, "target")
	content := firstStringArg(args, "content")
	if channel == "" || target == "" || content == "" {
		return nil, fmt.Errorf("channel, target and content are required")
	}

	// 审批已由 engine 统一安全闸（send-approve 规则 + 无人值守风险顾问）在工具执行前
	// 完成 —— 走到这里即已放行，Skill 不再二次确认。

	// 必经 SendQueue（由 sender 内部封装限速）；失败明确上抛。
	if err := s.sender.Send(ctx, channel, target, content, nil); err != nil {
		return nil, fmt.Errorf("send to %s failed: %w", channel, err)
	}
	return &skill.Result{
		Content:  fmt.Sprintf("Sent to %s (%s)", channel, target),
		Metadata: map[string]string{"channel": channel, "target": target},
	}, nil
}
