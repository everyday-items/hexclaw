package gateway

import (
	"context"
	"strings"
	"sync"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
)

// RBACLayer 基于角色的权限控制层
//
// 根据用户 ID 匹配角色，检查平台和工具权限。
// 角色匹配优先级：按用户 ID 精确匹配，未匹配则回退到 guest 角色。
// 未匹配任何角色（且无 guest 角色）的用户直接放行。
//
// 检查流程：
//  1. 根据 msg.UserID 查找角色
//  2. 未匹配 → 查找 guest 角色 → 无 guest 则放行
//  3. 检查平台权限（Platforms 白名单）
//  4. 检查工具权限（DenyTools 黑名单优先，AllowTools 白名单兜底）
type RBACLayer struct {
	cfg   config.RBACConfig
	roles map[string]*config.RoleConfig // 角色名 → 角色配置
	users map[string]*config.RoleConfig // 用户ID → 角色配置
	mu    sync.RWMutex
}

// NewRBACLayer 创建 RBAC 权限检查层
//
// 初始化时建立用户到角色的映射表，后续查找为 O(1)。
// 同一用户出现在多个角色中时，后定义的角色覆盖先定义的。
func NewRBACLayer(cfg config.RBACConfig) *RBACLayer {
	l := &RBACLayer{
		cfg:   cfg,
		roles: make(map[string]*config.RoleConfig, len(cfg.Roles)),
		users: make(map[string]*config.RoleConfig),
	}
	// 建立用户到角色的映射
	for i := range cfg.Roles {
		role := &cfg.Roles[i]
		l.roles[role.Name] = role
		for _, uid := range role.UserIDs {
			l.users[uid] = role
		}
	}
	return l
}

// Name 返回层名称
func (l *RBACLayer) Name() string { return "permission" }

// Check 执行 RBAC 权限检查
//
// 检查流程：
//  1. 根据 msg.UserID 查找角色
//  2. 未匹配角色 → 查找 guest 角色 → 无 guest 角色则放行
//  3. 检查平台权限（Platforms 白名单，空列表=全部允许）
//  4. 检查工具权限（通过 Metadata["tool_name"] 判断）
//     - DenyTools 黑名单优先：命中则拒绝
//     - AllowTools 白名单兜底：非空时，不在列表中则拒绝
func (l *RBACLayer) Check(_ context.Context, msg *adapter.Message) error {
	l.mu.RLock()
	defer l.mu.RUnlock()

	role := l.resolveRole(msg.UserID)
	if role == nil {
		return nil // 无角色配置，放行
	}

	// 检查平台权限
	if len(role.Platforms) > 0 {
		allowed := false
		for _, p := range role.Platforms {
			if strings.EqualFold(p, string(msg.Platform)) {
				allowed = true
				break
			}
		}
		if !allowed {
			return &GatewayError{
				Layer:   "permission",
				Code:    "PLATFORM_DENIED",
				Message: "当前平台无访问权限",
			}
		}
	}

	// 检查工具权限（通过 Metadata 中的 tool_name）
	if msg.Metadata != nil {
		if toolName := msg.Metadata["tool_name"]; toolName != "" {
			// DenyTools 黑名单优先
			for _, deny := range role.DenyTools {
				if strings.EqualFold(deny, toolName) {
					return &GatewayError{
						Layer:   "permission",
						Code:    "TOOL_DENIED",
						Message: "无权使用工具: " + toolName,
					}
				}
			}
			// AllowTools 白名单（非空时，不在列表中则拒绝）
			if len(role.AllowTools) > 0 {
				allowed := false
				for _, allow := range role.AllowTools {
					if strings.EqualFold(allow, toolName) {
						allowed = true
						break
					}
				}
				if !allowed {
					return &GatewayError{
						Layer:   "permission",
						Code:    "TOOL_NOT_ALLOWED",
						Message: "无权使用工具: " + toolName,
					}
				}
			}
		}
	}

	return nil
}

// resolveRole 根据用户 ID 解析角色
//
// 优先精确匹配用户 ID，未匹配则回退到 guest 角色。
// 如果也没有 guest 角色，返回 nil（表示放行）。
func (l *RBACLayer) resolveRole(userID string) *config.RoleConfig {
	if role, ok := l.users[userID]; ok {
		return role
	}
	// 回退到 guest 角色
	if guest, ok := l.roles["guest"]; ok {
		return guest
	}
	return nil
}

// GetUserRole 获取用户的角色名称
//
// 供外部查询用户角色，如审计日志、管理接口等场景。
// 未匹配任何角色时返回空字符串。
func (l *RBACLayer) GetUserRole(userID string) string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if role := l.resolveRole(userID); role != nil {
		return role.Name
	}
	return ""
}
