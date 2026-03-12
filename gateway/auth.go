package gateway

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
)

// AuthLayer 身份认证层 (Layer 1)
//
// 支持的认证方式：
//   - token: 通过消息 Metadata 中的 "auth_token" 字段验证（HMAC-SHA256 签名）
//   - anonymous: 允许匿名访问（仅开发模式）
//
// 平台适配器已通过平台 OAuth 验证了用户身份，
// 所以平台消息默认视为已认证。API 调用需要 Token。
type AuthLayer struct {
	cfg    config.AuthConfig
	tokens map[string]bool // 预配置的合法 Token 集合
}

// NewAuthLayer 创建认证层
func NewAuthLayer(cfg config.AuthConfig) *AuthLayer {
	tokens := make(map[string]bool)
	for _, t := range cfg.Tokens {
		tokens[t] = true
	}
	return &AuthLayer{cfg: cfg, tokens: tokens}
}

func (l *AuthLayer) Name() string { return "auth" }

// Check 执行认证检查
//
// 规则：
//   - 允许匿名模式：直接通过
//   - 平台消息（有 UserID）：视为已认证
//   - API 消息：校验 auth_token（预配置列表 或 HMAC-SHA256 签名验证）
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

	token := msg.Metadata["auth_token"]

	// 验证 Token 有效性
	if !l.validateToken(token) {
		return &GatewayError{
			Layer:   "auth",
			Code:    "invalid_token",
			Message: "Token 无效或已过期",
		}
	}

	return nil
}

// validateToken 验证 Token 有效性
//
// 支持两种模式：
//  1. 预配置 Token 列表（适合简单场景）
//  2. HMAC-SHA256 签名 Token（格式: "timestamp.signature"，有效期 24 小时）
//
// 如果既没有配置 Tokens 也没有配置 Secret，则拒绝所有请求（安全优先）。
func (l *AuthLayer) validateToken(token string) bool {
	// 模式 1：预配置 Token 列表匹配
	if len(l.tokens) > 0 {
		return l.tokens[token]
	}

	// 模式 2：HMAC-SHA256 签名验证（格式: "timestamp.signature"）
	if l.cfg.Secret != "" {
		parts := strings.SplitN(token, ".", 2)
		if len(parts) != 2 {
			return false
		}
		timestamp, signature := parts[0], parts[1]

		// 检查时间戳是否在有效期内（24 小时）
		ts, err := time.Parse(time.RFC3339, timestamp)
		if err != nil {
			return false
		}
		if time.Since(ts) > 24*time.Hour {
			return false
		}

		// 验证 HMAC-SHA256 签名
		mac := hmac.New(sha256.New, []byte(l.cfg.Secret))
		mac.Write([]byte(timestamp))
		expected := hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(signature), []byte(expected))
	}

	// 无任何认证配置，拒绝所有请求
	return false
}
