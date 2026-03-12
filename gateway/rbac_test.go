package gateway

import (
	"context"
	"testing"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
)

// newTestRBACConfig 创建测试用 RBAC 配置
//
// 包含三个角色：
//   - admin: 全权限，绑定 admin-1
//   - user: 限定平台和工具，绑定 user-1, user-2
//   - guest: 回退角色，禁用 shell 工具
func newTestRBACConfig() config.RBACConfig {
	return config.RBACConfig{
		Enabled: true,
		Roles: []config.RoleConfig{
			{
				Name:    "admin",
				UserIDs: []string{"admin-1"},
				// 空 Platforms 和 AllowTools = 全部允许
			},
			{
				Name:       "user",
				UserIDs:    []string{"user-1", "user-2"},
				Platforms:  []string{"web", "feishu"},
				AllowTools: []string{"search", "weather"},
				DenyTools:  []string{"shell"},
			},
			{
				Name:      "guest",
				DenyTools: []string{"shell", "code"},
			},
		},
	}
}

func TestRBACLayer_AdminAllowsAll(t *testing.T) {
	layer := NewRBACLayer(newTestRBACConfig())
	ctx := context.Background()

	// admin 用户在任意平台使用任意工具都应通过
	msg := &adapter.Message{
		UserID:   "admin-1",
		Platform: adapter.PlatformTelegram,
		Metadata: map[string]string{"tool_name": "shell"},
	}
	if err := layer.Check(ctx, msg); err != nil {
		t.Fatalf("admin 应通过所有检查: %v", err)
	}
}

func TestRBACLayer_UserPlatformAllowed(t *testing.T) {
	layer := NewRBACLayer(newTestRBACConfig())
	ctx := context.Background()

	msg := &adapter.Message{
		UserID:   "user-1",
		Platform: adapter.PlatformWeb,
	}
	if err := layer.Check(ctx, msg); err != nil {
		t.Fatalf("user 在 web 平台应通过: %v", err)
	}
}

func TestRBACLayer_UserPlatformDenied(t *testing.T) {
	layer := NewRBACLayer(newTestRBACConfig())
	ctx := context.Background()

	msg := &adapter.Message{
		UserID:   "user-1",
		Platform: adapter.PlatformTelegram,
	}
	err := layer.Check(ctx, msg)
	if err == nil {
		t.Fatal("user 在 telegram 平台应被拒绝")
	}
	gwErr, ok := err.(*GatewayError)
	if !ok {
		t.Fatalf("应返回 GatewayError，实际 %T", err)
	}
	if gwErr.Code != "PLATFORM_DENIED" {
		t.Fatalf("期望 PLATFORM_DENIED，实际 %s", gwErr.Code)
	}
}

func TestRBACLayer_UserDenyTool(t *testing.T) {
	layer := NewRBACLayer(newTestRBACConfig())
	ctx := context.Background()

	msg := &adapter.Message{
		UserID:   "user-1",
		Platform: adapter.PlatformWeb,
		Metadata: map[string]string{"tool_name": "shell"},
	}
	err := layer.Check(ctx, msg)
	if err == nil {
		t.Fatal("user 使用 shell 工具应被拒绝")
	}
	gwErr, ok := err.(*GatewayError)
	if !ok {
		t.Fatalf("应返回 GatewayError，实际 %T", err)
	}
	if gwErr.Code != "TOOL_DENIED" {
		t.Fatalf("期望 TOOL_DENIED，实际 %s", gwErr.Code)
	}
}

func TestRBACLayer_UserAllowToolWhitelist(t *testing.T) {
	layer := NewRBACLayer(newTestRBACConfig())
	ctx := context.Background()

	// 允许的工具应通过
	msg := &adapter.Message{
		UserID:   "user-2",
		Platform: adapter.PlatformFeishu,
		Metadata: map[string]string{"tool_name": "search"},
	}
	if err := layer.Check(ctx, msg); err != nil {
		t.Fatalf("user 使用 search 工具应通过: %v", err)
	}

	// 不在白名单中的工具应被拒绝
	msg.Metadata["tool_name"] = "code"
	err := layer.Check(ctx, msg)
	if err == nil {
		t.Fatal("user 使用不在白名单中的 code 工具应被拒绝")
	}
	gwErr, ok := err.(*GatewayError)
	if !ok {
		t.Fatalf("应返回 GatewayError，实际 %T", err)
	}
	if gwErr.Code != "TOOL_NOT_ALLOWED" {
		t.Fatalf("期望 TOOL_NOT_ALLOWED，实际 %s", gwErr.Code)
	}
}

func TestRBACLayer_UnknownUserFallsToGuest(t *testing.T) {
	layer := NewRBACLayer(newTestRBACConfig())
	ctx := context.Background()

	// 未知用户应回退到 guest 角色
	msg := &adapter.Message{
		UserID:   "unknown-user",
		Platform: adapter.PlatformWeb,
		Metadata: map[string]string{"tool_name": "shell"},
	}
	err := layer.Check(ctx, msg)
	if err == nil {
		t.Fatal("guest 角色应拒绝 shell 工具")
	}
	gwErr, ok := err.(*GatewayError)
	if !ok {
		t.Fatalf("应返回 GatewayError，实际 %T", err)
	}
	if gwErr.Code != "TOOL_DENIED" {
		t.Fatalf("期望 TOOL_DENIED，实际 %s", gwErr.Code)
	}
}

func TestRBACLayer_UnknownUserNoGuestRole(t *testing.T) {
	// 无 guest 角色的配置
	cfg := config.RBACConfig{
		Enabled: true,
		Roles: []config.RoleConfig{
			{
				Name:    "admin",
				UserIDs: []string{"admin-1"},
			},
		},
	}
	layer := NewRBACLayer(cfg)
	ctx := context.Background()

	// 未知用户且无 guest 角色 → 直接放行
	msg := &adapter.Message{
		UserID:   "unknown-user",
		Platform: adapter.PlatformTelegram,
		Metadata: map[string]string{"tool_name": "shell"},
	}
	if err := layer.Check(ctx, msg); err != nil {
		t.Fatalf("无 guest 角色时未知用户应放行: %v", err)
	}
}

func TestRBACLayer_EmptyConfig(t *testing.T) {
	cfg := config.RBACConfig{Enabled: true}
	layer := NewRBACLayer(cfg)
	ctx := context.Background()

	msg := &adapter.Message{
		UserID:   "anyone",
		Platform: adapter.PlatformWeb,
		Metadata: map[string]string{"tool_name": "anything"},
	}
	if err := layer.Check(ctx, msg); err != nil {
		t.Fatalf("空 RBAC 配置应全部放行: %v", err)
	}
}

func TestRBACLayer_MultipleRolesCorrectMapping(t *testing.T) {
	layer := NewRBACLayer(newTestRBACConfig())

	// 验证用户角色映射正确
	tests := []struct {
		userID string
		want   string
	}{
		{"admin-1", "admin"},
		{"user-1", "user"},
		{"user-2", "user"},
		{"unknown", "guest"},
	}
	for _, tt := range tests {
		got := layer.GetUserRole(tt.userID)
		if got != tt.want {
			t.Errorf("GetUserRole(%q) = %q，期望 %q", tt.userID, got, tt.want)
		}
	}
}

func TestRBACLayer_NilMetadata(t *testing.T) {
	layer := NewRBACLayer(newTestRBACConfig())
	ctx := context.Background()

	// Metadata 为 nil 时不应 panic
	msg := &adapter.Message{
		UserID:   "user-1",
		Platform: adapter.PlatformWeb,
		Metadata: nil,
	}
	if err := layer.Check(ctx, msg); err != nil {
		t.Fatalf("无 Metadata 时应通过: %v", err)
	}
}

func TestRBACLayer_CaseInsensitivePlatform(t *testing.T) {
	// 平台名称大小写不敏感
	cfg := config.RBACConfig{
		Enabled: true,
		Roles: []config.RoleConfig{
			{
				Name:      "user",
				UserIDs:   []string{"u1"},
				Platforms: []string{"WEB", "Feishu"},
			},
		},
	}
	layer := NewRBACLayer(cfg)
	ctx := context.Background()

	msg := &adapter.Message{
		UserID:   "u1",
		Platform: adapter.PlatformWeb, // "web"
	}
	if err := layer.Check(ctx, msg); err != nil {
		t.Fatalf("平台匹配应大小写不敏感: %v", err)
	}
}

func TestRBACLayer_CaseInsensitiveTool(t *testing.T) {
	// 工具名称大小写不敏感
	cfg := config.RBACConfig{
		Enabled: true,
		Roles: []config.RoleConfig{
			{
				Name:      "user",
				UserIDs:   []string{"u1"},
				DenyTools: []string{"Shell"},
			},
		},
	}
	layer := NewRBACLayer(cfg)
	ctx := context.Background()

	msg := &adapter.Message{
		UserID:   "u1",
		Platform: adapter.PlatformWeb,
		Metadata: map[string]string{"tool_name": "shell"},
	}
	err := layer.Check(ctx, msg)
	if err == nil {
		t.Fatal("工具匹配应大小写不敏感")
	}
}

func TestPipeline_WithRBAC(t *testing.T) {
	cfg := &config.SecurityConfig{
		RBAC: config.RBACConfig{
			Enabled: true,
			Roles: []config.RoleConfig{
				{
					Name:      "guest",
					DenyTools: []string{"shell"},
				},
			},
		},
	}
	p := NewPipeline(cfg, nil)

	// 应有 permission 层 + audit 层
	found := false
	for _, l := range p.layers {
		if l.Name() == "permission" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("Pipeline 应包含 permission 层")
	}

	// 验证 RBAC 检查生效
	msg := &adapter.Message{
		UserID:   "any-user",
		Platform: adapter.PlatformWeb,
		Metadata: map[string]string{"tool_name": "shell"},
	}
	err := p.Check(context.Background(), msg)
	if err == nil {
		t.Fatal("guest 角色应拒绝 shell 工具")
	}
}

func TestPipeline_RBACDisabled(t *testing.T) {
	cfg := &config.SecurityConfig{
		RBAC: config.RBACConfig{
			Enabled: false,
			Roles: []config.RoleConfig{
				{Name: "guest", DenyTools: []string{"shell"}},
			},
		},
	}
	p := NewPipeline(cfg, nil)

	// RBAC 禁用时不应有 permission 层
	for _, l := range p.layers {
		if l.Name() == "permission" {
			t.Fatal("RBAC 禁用时不应有 permission 层")
		}
	}
}
