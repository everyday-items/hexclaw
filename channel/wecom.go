package channel

import (
	"context"
	"fmt"
)

// WeCom 企业微信通道留缝 stub（同 feishu.go：方法集齐、诚实「未实现」）。
//
// 接入点（真实实现时）：
//  1. 仿 dingtalk.go：struct{ send SendFunc } + SetSender 后置回填；
//  2. composition root（cmd/hexclaw/main.go 通道注册处）注入
//     instances.Manager.Send 闭包 + Message→adapter.Reply 投影
//     （企微消息渲染细节在 adapter/wecom，限速走其 SendQueue，本层不重复）；
//  3. Markdown/卡片投影降级按 §6.10 WeComRenderer 语义补充；
//  4. 群消息忽略并记录「不支持群聊」（K12-INV-015），不得按私聊悄悄处理。
type WeCom struct{}

// NewWeCom 建企微留缝 stub。
func NewWeCom() WeCom { return WeCom{} }

// Name 实现 Port。
func (WeCom) Name() string { return "wecom" }

// SendText 实现 Port（未实现，诚实报错）。
func (WeCom) SendText(ctx context.Context, to Target, text string) error {
	return fmt.Errorf("企业微信通道尚未实现: %w", ErrNotImplemented)
}

// SendMessage 实现 Port（未实现，诚实报错）。
func (WeCom) SendMessage(ctx context.Context, to Target, msg Message) error {
	return fmt.Errorf("企业微信通道尚未实现: %w", ErrNotImplemented)
}
