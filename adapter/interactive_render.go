// interactive_render.go 实现 v0.4.0 F6 跨平台交互组件标准化（feature-flag gated）。
//
// 把 G3 的 InteractivePayload 翻译成各 IM 平台原生形态：
//   - 飞书 → 卡片按钮
//   - Slack → Block Kit
//   - Discord → Embed Components
//   - Telegram → Inline Keyboard
//   - 其它（不支持原生交互的平台）→ 降级为"1) 是  2) 不是"文本提示
//
// flag interactive.render.v1 默认 OFF。flag 关闭时 RenderTextFallback 永远生效，
// 各 IM 适配器走文本 fallback；flag 开启后调用方可在适配器里 switch payload.Type
// 调对应 RenderXxx 平台原生 API。
//
// 本期最小实现：仅提供 RenderTextFallback —— 各 IM 适配器一行 fallback 即可
// 让交互在所有平台基础可用。原生卡片渲染留待 v0.4.x 后续。
package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagInteractiveRenderV1 控制 F6 跨平台 renderer 是否启用原生路径。alpha 默认 OFF。
const FlagInteractiveRenderV1 = "interactive.render.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagInteractiveRenderV1,
		Default:      true, // alpha 强制 OFF
		Description:  "Render InteractivePayload to native IM components (cards / Block Kit / Embed / Inline KB). flag OFF uses text fallback.",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// PayloadRenderer 是平台原生 renderer 接口。各 IM 适配器实现它把抽象 payload
// 翻译成自己平台的卡片 / Block Kit / Embed / Inline Keyboard 等。
type PayloadRenderer interface {
	RenderButtons(prompt string, buttons []InteractiveButton) (any, error)
	RenderSelect(prompt string, options []InteractiveOption) (any, error)
	RenderApproval(approval *InteractiveApproval) (any, error)
	RenderCard(card *InteractiveCard) (any, error)
}

// RenderTextFallback 把任意 InteractivePayload 渲染成纯文本，用于不支持原生
// 交互的 IM 平台 / 调试 / 测试。永远不返回 error（本期保证可用）。
//
// 输出形式：
//
//	prompt
//	1) 是  2) 不是  3) 还行
//
// 用户回复"1"/"是"/"yes"等 → 平台 webhook 解析为 metadata.interactive_action。
func RenderTextFallback(p *InteractivePayload) string {
	if p == nil {
		return ""
	}
	var sb strings.Builder
	if p.Prompt != "" {
		sb.WriteString(p.Prompt)
		sb.WriteString("\n")
	}
	switch p.Type {
	case "buttons":
		writeButtons(&sb, p.Buttons)
	case "select":
		writeOptions(&sb, p.Options)
	case "approval":
		if p.Approval != nil {
			fmt.Fprintf(&sb, "%s\n", p.Approval.Subject)
			if p.Approval.Summary != "" {
				fmt.Fprintf(&sb, "%s\n", p.Approval.Summary)
			}
			approve := p.Approval.ApproveLabel
			if approve == "" {
				approve = "同意"
			}
			reject := p.Approval.RejectLabel
			if reject == "" {
				reject = "拒绝"
			}
			fmt.Fprintf(&sb, "1) %s  2) %s", approve, reject)
		}
	case "card":
		if p.Card != nil {
			sb.WriteString(p.Card.Title)
			sb.WriteString("\n")
			for _, f := range p.Card.Fields {
				fmt.Fprintf(&sb, "  • %s: %s\n", f.Label, f.Value)
			}
			if len(p.Card.Buttons) > 0 {
				writeButtons(&sb, p.Card.Buttons)
			}
			if p.Card.Footer != "" {
				sb.WriteString("\n")
				sb.WriteString(p.Card.Footer)
			}
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func writeButtons(sb *strings.Builder, btns []InteractiveButton) {
	parts := make([]string, 0, len(btns))
	for i, b := range btns {
		parts = append(parts, fmt.Sprintf("%d) %s", i+1, b.Label))
	}
	sb.WriteString(strings.Join(parts, "  "))
}

func writeOptions(sb *strings.Builder, opts []InteractiveOption) {
	for i, o := range opts {
		fmt.Fprintf(sb, "%d) %s", i+1, o.Label)
		if o.Description != "" {
			fmt.Fprintf(sb, " — %s", o.Description)
		}
		sb.WriteString("\n")
	}
}

// ShouldUseNativeRenderer 由 IM 适配器调用，决定是否走原生平台 renderer 路径。
// flag interactive.render.v1 OFF 时永远返回 false → 所有适配器走文本 fallback。
func ShouldUseNativeRenderer(flags featureflag.Flags) bool {
	if flags == nil {
		return false
	}
	return flags.IsEnabled(FlagInteractiveRenderV1)
}

// MaybeApplyTextFallback 在 IM 适配器 Send 入口调用：当 reply 含 InteractivePayload
// 且 flag OFF（或平台未实现原生 renderer）时，把 RenderTextFallback 结果追加到
// reply.Content 末尾，使按钮 / 选项 / 审批 / 卡片在所有平台基础可用。
//
// 行为：
//   - reply == nil 或 reply.Interactive == nil：no-op，返回 false
//   - flag interactive.render.v1 ON：no-op（让适配器自行调原生 renderer），返回 false
//   - flag OFF：追加文本 fallback 到 reply.Content（保留 \n\n 分隔），返回 true
//
// 不修改 reply.Interactive（保留以便上层观察 / persistence）。
func MaybeApplyTextFallback(ctx context.Context, reply *Reply) bool {
	if reply == nil || reply.Interactive == nil {
		return false
	}
	if featureflag.Enabled(ctx, FlagInteractiveRenderV1) {
		return false
	}
	fallback := RenderTextFallback(reply.Interactive)
	if fallback == "" {
		return false
	}
	if reply.Content == "" {
		reply.Content = fallback
	} else {
		reply.Content = strings.TrimRight(reply.Content, "\n") + "\n\n" + fallback
	}
	return true
}
