package gateway

import (
	"context"
	"log"

	"github.com/everyday-items/hexclaw/adapter"
)

// AuditLayer 审计记录层 (Layer 6)
//
// 记录所有通过安全检查的请求，用于：
//   - 操作日志留痕
//   - 异常行为告警
//   - 合规审计
//
// 此层始终通过（不拒绝请求），仅做记录。
// 后续可接入 hexagon/security/audit 的 AuditLogger 实现持久化存储。
type AuditLayer struct{}

// NewAuditLayer 创建审计层
func NewAuditLayer() *AuditLayer {
	return &AuditLayer{}
}

func (l *AuditLayer) Name() string { return "audit" }

// Check 记录审计日志
//
// 审计层不拒绝请求，只记录日志。
// 后续可替换为 hexagon/security/audit 的结构化审计存储。
func (l *AuditLayer) Check(_ context.Context, msg *adapter.Message) error {
	log.Printf("[审计] 请求通过安全检查: platform=%s, user=%s, content_len=%d",
		msg.Platform, msg.UserID, len(msg.Content))
	return nil
}
