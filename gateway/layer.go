package gateway

import (
	"context"

	"github.com/everyday-items/hexclaw/adapter"
)

// Layer 安全检查层接口
//
// 每一层实现此接口，Gateway 管道按顺序执行所有 Layer。
// 任意一层返回错误即终止管道，不继续后续检查。
type Layer interface {
	// Name 返回层名称（用于日志和错误标识）
	Name() string

	// Check 执行安全检查
	// 通过返回 nil，拒绝返回 *GatewayError
	Check(ctx context.Context, msg *adapter.Message) error
}
