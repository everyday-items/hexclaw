package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// BUG-20260711 全链路 E2E：复现截图那条「多轮云端对话被 egress 硬拦死」。
//
// 装配真实栈：engine → resolveLLMSelection → 云端 provider（egress-guarded facade）→
// buildStreamMessages（盖 egress 类）→ provider.Stream（守卫在此校验 ctx 信封）。
// provider 名不含 "ollama" + 无 base_url → 被判云端 → SetEgressPolicy 后每次
// Complete/Stream 都过 cloudEgressProvider 守卫。
//
// 旧行为：第 2 轮带 history → 信封获得 ClassMemory → general_chat+memory 被守卫拒绝 →
// "runtime stream 失败: cloud provider ... egress: egress 拦截"。
// 新行为：history 归 general + 云端不注入跨会话记忆 → 守卫放行，多轮对话正常。
func newEgressGuardedCloudEngine(t *testing.T, provider hexagon.Provider, mem *memory.FileMemory) *ReActEngine {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "e2e.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "cloud-openai"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"cloud-openai": {
			Model:          "gpt-x", // 无 base_url + 名不含 ollama → 云端
			ModelSpecsMode: config.LLMModelSpecsModeExplicit,
			ModelSpecs: []config.LLMProviderModelSpec{{
				ID: "gpt-x",
				Capabilities: []string{
					config.LLMModelCapabilityText,
					config.LLMModelCapabilityVision,
				},
			}},
		},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{
		"cloud-openai": provider,
	})
	router.SetEgressPolicy(&egress.Policy{}) // 装上强制云边界（与生产 main.go 一致）
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	if mem != nil {
		eng.SetFileMemory(mem)
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng
}

func drainStream(t *testing.T, ch <-chan *adapter.ReplyChunk) (string, error) {
	t.Helper()
	var sb strings.Builder
	for chunk := range ch {
		if chunk == nil {
			continue
		}
		if chunk.Error != nil {
			return sb.String(), chunk.Error
		}
		sb.WriteString(chunk.Content)
	}
	return sb.String(), nil
}

// 多轮云端对话：第一轮建立历史，第二轮带 history 再问——全链路不得被 egress 拦死。
func TestE2E_MultiTurnCloudChat_NotBlockedByEgress(t *testing.T) {
	provider := &egressCaptureProvider{}
	eng := newEgressGuardedCloudEngine(t, provider, nil)
	ctx := context.Background()

	turn1 := &adapter.Message{ID: "e2e-1", SessionID: "s-cloud", UserID: "u1", Platform: adapter.PlatformAPI, Content: "你好"}
	ch1, err := eng.ProcessStream(ctx, turn1)
	if err != nil {
		t.Fatalf("第一轮 ProcessStream 建流失败: %v", err)
	}
	if _, err := drainStream(t, ch1); err != nil {
		t.Fatalf("第一轮不应被拦截: %v", err)
	}

	// 第二轮：同 session → 引擎从库里加载 history（len>0）。旧代码在此被 egress 拦死。
	turn2 := &adapter.Message{ID: "e2e-2", SessionID: "s-cloud", UserID: "u1", Platform: adapter.PlatformAPI, Content: "你都会什么？"}
	ch2, err := eng.ProcessStream(ctx, turn2)
	if err != nil {
		t.Fatalf("第二轮 ProcessStream 建流失败: %v", err)
	}
	out, err := drainStream(t, ch2)
	if err != nil {
		if strings.Contains(err.Error(), "egress") {
			t.Fatalf("BUG 复现：多轮云端对话被 egress 拦死（截图症状）：%v", err)
		}
		t.Fatalf("第二轮意外失败: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("第二轮应正常拿到云端回复，got %q", out)
	}

	// 守卫必须真的被走到（provider 被调用 = 云边界放行），且信封是纯 general（无 memory）。
	reqs := provider.last(t)
	requireEgressRequest(t, reqs, egress.PurposeGeneralChat, egress.ClassGeneral)
	requireNoEgressClass(t, reqs, egress.ClassMemory)
}

// 云端 + 开启长期记忆：记忆不出本机（不注入），但对话照样不被拦死（优雅降级）。
func TestE2E_CloudChatWithMemory_DropsMemoryButSucceeds(t *testing.T) {
	fm, err := memory.New(memory.Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 200, DailyDays: 0})
	if err != nil {
		t.Fatalf("创建 FileMemory 失败: %v", err)
	}
	const marker = "E2EMEM_用户住在杭州"
	if err := fm.SaveMemory("跨会话事实：" + marker); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}

	provider := &egressCaptureProvider{}
	eng := newEgressGuardedCloudEngine(t, provider, fm)

	msg := &adapter.Message{ID: "e2e-mem", SessionID: "s-mem", UserID: "u1", Platform: adapter.PlatformAPI, Content: "介绍下我"}
	ch, err := eng.ProcessStream(context.Background(), msg)
	if err != nil {
		t.Fatalf("ProcessStream 建流失败: %v", err)
	}
	out, err := drainStream(t, ch)
	if err != nil {
		t.Fatalf("云端 + 记忆开启不应被拦死（应静默丢记忆），got err=%v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("应正常拿到云端回复，got %q", out)
	}
	// 记忆没出本机：发给 provider 的信封不含 memory 类。
	requireNoEgressClass(t, provider.last(t), egress.ClassMemory)
}
