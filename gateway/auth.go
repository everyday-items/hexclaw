package gateway

import (
	"context"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
)

// AuthLayer 身份认证层 (Layer 1)
//
// 支持的认证方式：
//   - token: 通过消息 Metadata 中的 "auth_token" 字段验证
//   - anonymous: 允许匿名访问（仅开发模式）
//
// 平台适配器已通过平台 OAuth 验证了用户身份，
// 所以平台消息默认视为已认证。API 调用需要 Token。
type AuthLayer struct {
	cfg config.AuthConfig
}

// NewAuthLayer 创建认证层
func NewAuthLayer(cfg config.AuthConfig) *AuthLayer {
	return &AuthLayer{cfg: cfg}
}

func (l *AuthLayer) Name() string { return "auth" }

// Check 执行认证检查
//
// 规则：
//   - 允许匿名模式：直接通过
//   - 平台消息（有 UserID）：视为已认证
//   - API 消息：检查 auth_token
func (l *AuthLayer) Check(_ context.Context, msg *adapter.Message) error {
	// 允许匿名访问
	if l.cfg.AllowAnonymous {
		return nil
	}

	// 平台消息：适配器已验证用户身份
	if msg.Platform != adapter.PlatformAPI {
		if msg.UserID != "" {
			return nil
		}
		return &GatewayError{
			Layer:   "auth",
			Code:    "missing_user_id",
			Message: "无法识别用户身份",
		}
	}

	// API 调用：检查 Token
	if msg.Metadata == nil || msg.Metadata["auth_token"] == "" {
		return &GatewayError{
			Layer:   "auth",
			Code:    "missing_token",
			Message: "请提供认证 Token",
		}
	}

	// TODO: 验证 Token 有效性（签名、过期时间等）
	// 当前版本接受任意非空 Token
	return nil
}
