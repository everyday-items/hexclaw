// Package gateway 提供六层安全网关
//
// 安全网关是 HexClaw 的核心安全组件，所有消息在到达 Agent 引擎之前
// 必须通过六层安全检查：
//
//	消息 → Auth → RateLimit → CostCheck → InputSafety → Permission → Audit → Engine
//
// 每一层都可独立配置开关和参数。任意一层拒绝则直接返回错误，
// 不会继续传递到后续层。
package gateway

import (
	"context"

	"github.com/everyday-items/hexclaw/internal/adapter"
)

// Gateway 安全网关接口
//
// 对消息进行多层安全检查，通过后才允许到达 Engine。
// 每一层检查失败都会返回对应的 GatewayError。
type Gateway interface {
	// Check 执行全部安全检查
	// 通过返回 nil，被拒绝返回 *GatewayError
	Check(ctx context.Context, msg *adapter.Message) error

	// RecordUsage 记录一次请求的资源使用（Token、成本等）
	// 在 Engine 处理完成后调用，用于成本控制和审计
	RecordUsage(ctx context.Context, msg *adapter.Message, usage *Usage) error
}

// Usage 资源使用记录
type Usage struct {
	Provider    string  // LLM Provider 名称
	Model       string  // 模型名称
	InputTokens int     // 输入 Token 数
	OutputTokens int    // 输出 Token 数
	Cost        float64 // 费用（美元）
}

// GatewayError 网关拒绝错误
type GatewayError struct {
	Layer   string // 拒绝层: auth / rate_limit / cost / input_safety / permission / audit
	Code    string // 错误码
	Message string // 用户可见的错误消息
}

func (e *GatewayError) Error() string {
	return e.Layer + ": " + e.Message
}
