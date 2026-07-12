package engine

// BUG-20260712-e K12 识题（拍照识题）走 cost-aware 路由而非配置的视觉模型：桌面识题经
// visionFn → router.Route(ctx)，cost-aware 抓本地免费 provider（Ollama），无视用户为视觉配置的
// 云端默认模型（glm-4v-flash）。后果：①既慢（本地 9B 视觉 CPU 上龟速）②曾因本地默认模型指向
// 未安装的 qwen2.5:3b 直接「ollama api error: 404 model not found」。用户诉求「设置哪个模型走哪个
// 模型」——识题应当用**配置的默认**视觉模型。
//
// 修复：识题改用 e.RouteForVision（取 router.Default 配置默认），而非 cost-aware router.Route。

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// newCostAwareEngine 装配一个 cost-aware 策略的引擎（真机复现 bug 的配置：cost-aware 会优先
// 本地免费 provider）。默认 provider = 配置的默认。
func newCostAwareEngine(t *testing.T, providers map[string]hexagon.Provider, cfgs map[string]config.LLMProviderConfig, defaultProvider string) *ReActEngine {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "vroute.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = defaultProvider
	cfg.LLM.Providers = cfgs
	cfg.LLM.Routing.Enabled = true
	cfg.LLM.Routing.Strategy = "cost-aware" // 真机配置：cost-aware 优先本地免费 provider
	router := llmrouter.NewWithProviders(cfg.LLM, providers)
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng
}

// TestRouteForVision_UsesConfiguredDefaultNotCostAware cost-aware 策略下，识题视觉路由必须落到
// **配置的默认**（智谱 glm-4v-flash），而非 cost-aware 抓到的本地 Ollama。
func TestRouteForVision_UsesConfiguredDefaultNotCostAware(t *testing.T) {
	local := &numCtxCaptureProvider{}
	cloud := &numCtxCaptureProvider{}
	eng := newCostAwareEngine(t,
		map[string]hexagon.Provider{
			"Ollama (本地)": local,
			"智谱 AI":       cloud,
		},
		map[string]config.LLMProviderConfig{
			"Ollama (本地)": {Model: "qwen3.5:9b"},
			"智谱 AI":       {Model: "glm-4v-flash"},
		},
		"智谱 AI", // 配置的默认 provider
	)

	// 先确认这套配置下 cost-aware Route 确实抓的是本地（否则 RED 无意义）。
	if _, routeName, err := eng.router.Route(context.Background()); err != nil || routeName != "Ollama (本地)" {
		t.Fatalf("前置：cost-aware Route 应抓本地 Ollama（证明与默认有区分），got name=%q err=%v", routeName, err)
	}

	provider, model, err := eng.RouteForVision(context.Background())
	if err != nil {
		t.Fatalf("RouteForVision: %v", err)
	}
	if model != "glm-4v-flash" {
		t.Fatalf("BUG 复现：识题未用配置的默认视觉模型 glm-4v-flash，got %q（cost-aware 抓了本地）", model)
	}
	if provider != hexagon.Provider(cloud) {
		t.Fatalf("BUG 复现：识题路由到了非默认 provider（应为配置默认智谱 AI）")
	}
}
