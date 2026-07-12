package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// newEngineForMemoryAudit 构建一个最小可用的引擎（与 TestBuildStreamMessages 同款）。
func newEngineForMemoryAudit(t *testing.T) *ReActEngine {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {APIKey: "sk-test", Model: "test-model"},
	}
	router, _ := llmrouter.New(cfg.LLM)
	dir := t.TempDir()
	store, _ := sqlitestore.New(filepath.Join(dir, "test.db"))
	t.Cleanup(func() { store.Close() })
	store.Init(context.Background())
	skills := skill.NewRegistry()
	return NewReActEngine(cfg, router, store, skills)
}

// AUDIT bug#3b: 长期记忆注入被硬截断到 500 字符 —— 排在后面的记忆（如"剀哥是谁"）被丢掉，问答答不上。
// 复现：保存 >500 字符的记忆，独特标记排在末尾；构造 system prompt 后标记应当被注入，但实际被截断丢失。
func TestAudit_LongTermMemoryNotTruncated_20260623(t *testing.T) {
	eng := newEngineForMemoryAudit(t)

	dir := t.TempDir()
	fm, err := memory.New(memory.Options{Enabled: true, Dir: dir, MaxMemory: 200, DailyDays: 0})
	if err != nil {
		t.Fatalf("创建 FileMemory 失败: %v", err)
	}

	// 先写入足够多的填充记忆（确保超过 500 字符），标记记忆最后写（O_APPEND → 落在文件末尾）。
	for i := range 12 {
		if err := fm.SaveMemory("这是一条用于填充长期记忆的占位内容编号" + string(rune('A'+i)) + "确保整体长度超过五百字符以触发截断"); err != nil {
			t.Fatalf("写入填充记忆失败: %v", err)
		}
	}
	const marker = "KAIGE_MARKER_7788_剀哥是HexClaw的核心代码贡献者"
	if err := fm.SaveMemory("用户的好朋友：" + marker); err != nil {
		t.Fatalf("写入标记记忆失败: %v", err)
	}

	// 前置校验：原始记忆里标记确实排在第 500 字符之后（否则这条测试不成立）。
	raw := fm.LoadContextForRole("")
	idx := strings.Index(raw, marker)
	if idx < 0 {
		t.Fatalf("标记未写入原始记忆，测试前提不成立")
	}
	if idx <= 500 {
		t.Fatalf("标记位于第 %d 字符（<=500），无法验证截断 bug，请增加填充量", idx)
	}

	eng.SetFileMemory(fm)

	// 长期记忆召回应被注入到当轮 user 消息（前缀缓存优化后从 system 迁出），标记应当出现。
	msgs := eng.buildStreamMessages(context.Background(), "", nil, "", "介绍下剀哥", nil, nil)
	turn := msgs[len(msgs)-1].Content
	if !strings.Contains(turn, "memory-context") {
		t.Fatalf("当轮 user 消息未注入任何长期记忆块")
	}
	if !strings.Contains(turn, marker) {
		t.Errorf("BUG#3b: 长期记忆被截断，排在 500 字符后的记忆 %q 丢失 —— 问答无法回答该记忆内容", marker)
	}
}

// AUDIT bug#3a: 引擎在"挂载时记忆为空、之后才写入"的场景下也必须能注入。
// （main.go 用 memCtxLen>0 当挂载闸门 → 首次添加的记忆要等下次重启才生效；本测试钉死引擎侧能力，
//
//	配合 main.go 改为"只要 fileMem 创建成功就挂载"。）
func TestAudit_MemoryInjectedWhenEmptyAtAttach_20260623(t *testing.T) {
	eng := newEngineForMemoryAudit(t)

	dir := t.TempDir()
	fm, err := memory.New(memory.Options{Enabled: true, Dir: dir, MaxMemory: 200, DailyDays: 0})
	if err != nil {
		t.Fatalf("创建 FileMemory 失败: %v", err)
	}

	// 挂载时记忆为空
	eng.SetFileMemory(fm)

	// 之后才写入记忆（模拟用户在会话中通过 UI 新增）
	const marker = "POSTATTACH_MARKER_4521"
	if err := fm.SaveMemory("会话中新增的记忆：" + marker); err != nil {
		t.Fatalf("写入记忆失败: %v", err)
	}

	// query 与新增记忆词法相关（BUG-20260712-L 起零相关检索层不注入——本测试意图是
	// 「挂载后新增可见」，不是「无关也注入」）。
	msgs := eng.buildStreamMessages(context.Background(), "", nil, "", "会话中新增的记忆是什么", nil, nil)
	if !strings.Contains(msgs[len(msgs)-1].Content, marker) {
		t.Errorf("BUG#3a: 挂载后新增的记忆未被注入：%q", marker)
	}
}
