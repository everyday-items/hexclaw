package engine

// BUG-20260712 failover×egress 级联：真机暴露的整条链失败。
//
// 复现的真机链路：用户发 "hello" 给 K12 云端辅导助手 →
//   1) 路由先选本地 Ollama(tools=33) → CPU 巨型 prompt header 超时取消 → "context canceled"。
//   2) 引擎回退到云端 provider，但**复用了「为本地构建、带跨会话记忆(memory)」的请求**
//      （buildTurnContext 在本地 locality 下注入了 <memory-context>）→ 云端 cloudEgressProvider
//      守卫按 ctx egress 信封里的 ClassMemory 拦截（"敏感数据类 memory 不允许在用途 general_chat
//      下出本机"）。
//   3) 该 egress 错误不算 "provider 不可用" → 不再继续回退 → 落到友好错误 "模型服务暂时不可用"。
//
// 修复三点（本套件钉死）：
//   A) "context canceled"/"canceled" 纳入 provider 不可用 needle → 本地 header 超时能触发回退。
//   B) 回退时按**目标 provider 的本地/云 locality** 重建 cloud-safe 请求（起干净 egress 信封 +
//      盖云 locality → buildTurnContext 不注入跨会话记忆 → 信封不含 ClassMemory → 守卫放行）。
//   C) runner 内部单跳跨 provider fallback 禁用，多跳 failover 完全交给 react.go 外层循环
//      （那里能重建 cloud-safe 请求）——否则 runner 内部换 provider 仍复用带 memory 的 messages。

import (
	"context"
	"errors"
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

const failoverEgressMemMarker = "FOEGRESS_用户是杭州的K12家长孩子上五年级"

// newFailoverEgressEngine 装配真机同构栈：本地默认 provider（Ollama，不加 egress 守卫）+
// 云端备用 provider（egress-guarded）+ 强制云边界 egress.Policy + 一条跨会话记忆。
func newFailoverEgressEngine(t *testing.T, local, cloud hexagon.Provider, localName, cloudName string) *ReActEngine {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "foegress.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = localName
	cfg.LLM.Routing.Strategy = "default" // 确定性：Route 恒返回默认（本地）provider
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		localName: {Model: "qwen3.5:9b"}, // 名含 ollama/本地 → 本地，不加 egress 守卫、fallback 排最后
		cloudName: {Model: "gpt-x"},      // 无 base_url + 云名 → 云端，加 cloudEgressProvider 守卫
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{
		localName: local,
		cloudName: cloud,
	})
	router.SetEgressPolicy(&egress.Policy{}) // 与生产 main.go 一致，装上强制云边界

	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())

	fm, err := memory.New(memory.Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 200, DailyDays: 0})
	if err != nil {
		t.Fatalf("创建 FileMemory 失败: %v", err)
	}
	if err := fm.SaveMemory("跨会话事实：" + failoverEgressMemMarker); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}
	eng.SetFileMemory(fm)

	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng
}

// TestFailoverEgress_NonStreaming_LocalCanceledFallsBackCloudSafe 非流式路径：
// 本地 "context canceled" → 回退云端，不得被 egress 拦死，云端信封不含 memory。
func TestFailoverEgress_NonStreaming_LocalCanceledFallsBackCloudSafe(t *testing.T) {
	local := &failoverProvider{name: "Ollama (本地)", err: errors.New(`Post "http://localhost:11434/api/chat": context canceled`)}
	cloud := &egressCaptureProvider{}
	eng := newFailoverEgressEngine(t, local, cloud, local.name, "openrouter")

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID: "foe-nonstream-1", Platform: adapter.PlatformAPI,
		UserID: "u-foe-1", ChatID: "c-foe-1",
		Content: "hello",
	})
	if err != nil {
		if strings.Contains(err.Error(), "egress") || strings.Contains(err.Error(), "不允许") {
			t.Fatalf("BUG 复现：本地超时回退云端复用了带 memory 的请求 → 被云 egress 拦死：%v", err)
		}
		t.Fatalf("本地 context canceled 应回退到云端并成功，却失败：%v", err)
	}
	if !strings.Contains(reply.Content, "ok") {
		t.Fatalf("应拿到云端 provider 的正常回答，got %q", reply.Content)
	}
	if local.callCount() == 0 {
		t.Fatalf("本地 provider 应先被调用一次（是回退的起点）")
	}
	// 发给云端的 egress 信封必须不含 memory（根因修复：回退按云 locality 重建，不注入跨会话记忆）。
	reqs := cloud.last(t)
	requireNoEgressClass(t, reqs, egress.ClassMemory)
	requireEgressRequest(t, reqs, egress.PurposeGeneralChat, egress.ClassGeneral)
}

// TestFailoverEgress_Streaming_LocalCanceledFallsBackCloudSafe 流式路径（同构）：
// 覆盖 runner 内部单跳 fallback 直接触发 egress 拦截的真机场景。
func TestFailoverEgress_Streaming_LocalCanceledFallsBackCloudSafe(t *testing.T) {
	local := &failoverProvider{name: "Ollama (本地)", err: errors.New(`context canceled`)}
	cloud := &egressCaptureProvider{}
	eng := newFailoverEgressEngine(t, local, cloud, local.name, "openrouter")

	ch, err := eng.ProcessStream(context.Background(), &adapter.Message{
		ID: "foe-stream-1", Platform: adapter.PlatformAPI,
		UserID: "u-foe-2", ChatID: "c-foe-2",
		Content: "hello",
	})
	if err != nil {
		t.Fatalf("ProcessStream 建流失败: %v", err)
	}
	out, derr := drainStream(t, ch)
	if derr != nil {
		if strings.Contains(derr.Error(), "egress") || strings.Contains(derr.Error(), "不允许") {
			t.Fatalf("BUG 复现：流式本地超时回退云端被 egress 拦死：%v", derr)
		}
		t.Fatalf("流式本地 context canceled 应回退云端并成功，却失败：%v", derr)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("流式应拿到云端 provider 的正常回答，got %q", out)
	}
	if local.callCount() == 0 {
		t.Fatalf("本地 provider 应先被调用一次（是回退的起点）")
	}
	reqs := cloud.last(t)
	requireNoEgressClass(t, reqs, egress.ClassMemory)
	requireEgressRequest(t, reqs, egress.PurposeGeneralChat, egress.ClassGeneral)
}

// TestIsProviderUnavailableError_ContextCanceled 单元锁：本地 header 超时取消属 provider 侧
// 不可用，必须触发回退（补 needle "context canceled"/"canceled"）。
func TestIsProviderUnavailableError_ContextCanceled(t *testing.T) {
	for _, s := range []string{
		"context canceled",
		`Post "http://localhost:11434/api/chat": context canceled`,
		"operation was canceled",
	} {
		if !isProviderUnavailableError(errors.New(s)) {
			t.Fatalf("context canceled 类应判为 provider 不可用（触发回退），却判为可用：%q", s)
		}
	}
}
