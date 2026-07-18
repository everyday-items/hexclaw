package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// BUG-20260712 provider 韧性回归锁（治本·第 2 部分）：
// 当 agent 绑定的 provider 在 router 里找不到（配置漂移/被抹/命名不一致）时，resolveProvider
// 必须①报**可操作**错误（点名缺失的 provider + 怎么恢复/改绑）②**绝不静默回退**到默认/云端
// provider——本地会话被悄悄转发云端会击穿隐私出口边界（egress）。修前是笼统「provider 不存在」。
func TestResolveProvider_MissingProviderActionableNoSilentFallback(t *testing.T) {
	cfg := config.DefaultConfig()
	// router 里没有 "Ollama (本地)"（模拟绑定本地模型的 agent 遇上配置被抹）。
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{})
	dir := t.TempDir()
	store, _ := sqlitestore.New(filepath.Join(dir, "test.db"))
	t.Cleanup(func() { _ = store.Close() })
	_ = store.Init(context.Background())
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())

	// hint 为显式非 auto → 跳过 agent 路由，直接解析该 provider；未注册即命中错误分支。
	p, name, err := eng.resolveProvider(context.Background(), "Ollama (本地)", nil)

	if err == nil {
		t.Fatal("绑定的 provider 缺失应报错，绝不静默回退")
	}
	// 核心 egress 护栏：绝不返回任何 provider（否则可能把本地会话悄悄转发云端）。
	if p != nil || name != "" {
		t.Fatalf("不得返回任何 provider（防静默 local→cloud 回退），got name=%q", name)
	}
	msg := err.Error()
	if !strings.Contains(msg, "Ollama (本地)") {
		t.Fatalf("错误应点名缺失的 provider，got: %s", msg)
	}
	if !strings.Contains(msg, "改绑") && !strings.Contains(msg, "恢复") {
		t.Fatalf("错误应可操作（怎么恢复/改绑），got: %s", msg)
	}
	var providerErr *ProviderUnavailableError
	if !errors.As(err, &providerErr) || providerErr.Provider != "Ollama (本地)" {
		t.Fatalf("missing provider must be a typed ProviderUnavailableError, got %T: %v", err, err)
	}
	if err := eng.ValidateProvider("Ollama (本地)"); !errors.As(err, &providerErr) {
		t.Fatalf("boundary validation must reject the same provider without routing: %v", err)
	}
}
