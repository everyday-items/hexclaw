package main

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// BUG-20260610: 桌面端 UI 提供 Webhooks 管理入口，但 webhook.enabled 默认 false
// 导致 /api/v1/webhooks 路由不注册（404）。desktop 模式仅监听 localhost，
// 应与 Cron / Canvas 一样强制启用，保证 UI 入口与后端能力对齐。
func TestApplyDesktopOverrides(t *testing.T) {
	cfg := &config.Config{}
	cfg.Webhook.Enabled = false // 模拟存量 yaml 显式 false

	applyDesktopOverrides(cfg)

	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("Server.Host = %q, want 127.0.0.1", cfg.Server.Host)
	}
	if !cfg.Platforms.Web.Enabled {
		t.Error("Platforms.Web.Enabled = false, want true")
	}
	if !cfg.Security.Auth.AllowAnonymous {
		t.Error("Security.Auth.AllowAnonymous = false, want true")
	}
	if !cfg.Cron.Enabled {
		t.Error("Cron.Enabled = false, want true")
	}
	if !cfg.Canvas.Enabled {
		t.Error("Canvas.Enabled = false, want true")
	}
	if !cfg.Webhook.Enabled {
		t.Error("Webhook.Enabled = false, want true — UI 提供 Webhooks 入口，desktop 模式必须启用对应路由")
	}
}
