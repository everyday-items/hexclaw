package channel

import (
	"context"
	"fmt"
)

// Feishu 飞书通道留缝 stub（§3.12：支持「实现了 ChannelPort 的渠道」；v0.5.0 只有
// 钉钉是真实通道）。方法集齐但全部返回 ErrNotImplemented——诚实「未实现」，绝不
// 假装已发送，也不静默改走别的通道。
//
// 接入点（真实实现时）：
//  1. 仿 dingtalk.go：定义 struct{ send SendFunc } + SetSender 后置回填；
//  2. composition root（cmd/hexclaw/main.go 通道注册处）注入
//     instances.Manager.Send 闭包 + Message→adapter.Reply 投影
//     （飞书消息渲染细节在 adapter/feishu，限速走其 SendQueue，本层不重复）；
//  3. Markdown/卡片投影降级按 §6.10 FeishuRenderer 语义补充；
//  4. 群消息忽略并记录「不支持群聊」（K12-INV-015），不得按私聊悄悄处理。
type Feishu struct{}

// NewFeishu 建飞书留缝 stub。
func NewFeishu() Feishu { return Feishu{} }

// Name 实现 Port。
func (Feishu) Name() string { return "feishu" }

// SendText 实现 Port（未实现，诚实报错）。
func (Feishu) SendText(ctx context.Context, to Target, text string) error {
	return fmt.Errorf("飞书通道尚未实现: %w", ErrNotImplemented)
}

// SendMessage 实现 Port（未实现，诚实报错）。
func (Feishu) SendMessage(ctx context.Context, to Target, msg Message) error {
	return fmt.Errorf("飞书通道尚未实现: %w", ErrNotImplemented)
}
