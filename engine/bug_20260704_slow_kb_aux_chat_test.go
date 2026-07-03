package engine

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// BUG-20260704 引擎层集成：模拟真实用户请求，验证 KB 辅助 LLM（查询扩展/LLM 重排）
// 慢时，聊天关键路径（eng.Process / eng.ProcessStream）不被拖垮。
//
// 真机现象：主聊天走 SF（快，"思考 4s"），但每条消息的 KB 检索把 expand/rerank 路由到
// 本地 43s 慢模型（无预算）→ 模型开始生成前卡 180s。这里用**慢辅助 LLM + 快主 provider**
// 复刻该场景，走真实 Process/ProcessStream 全链路断言修复生效。
//
// 与 knowledge 单元锁（bug_20260704_rag_aux_llm_budget_test.go）互补：单元证明 Manager
// 层预算+熔断，本集成证明它在引擎注入点（react.go e.kb.Query）真实兜住聊天。

// slowAuxLLM 模拟本地慢模型：挂到 ctx 取消才返回（预算 ctx 会在 2.5s 掐断）。
type slowAuxLLM struct{ calls atomic.Int64 }

func (s *slowAuxLLM) Complete(ctx context.Context, _ string) (string, error) {
	s.calls.Add(1)
	<-ctx.Done()
	return "", ctx.Err()
}

// fastKBChatProvider 模拟 SF 快主聊天：Complete/Stream 立即出回复；记录发给模型的消息
// （用于断言 KB 注入内容是否到达模型 = 降级不杀检索质量）。
type fastKBChatProvider struct {
	mu   sync.Mutex
	seen []string
}

func (p *fastKBChatProvider) Name() string { return "test" }

func (p *fastKBChatProvider) record(req hexagon.CompletionRequest) {
	p.mu.Lock()
	for _, m := range req.Messages {
		p.seen = append(p.seen, m.Content)
	}
	p.mu.Unlock()
}

func (p *fastKBChatProvider) Complete(_ context.Context, req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	p.record(req)
	return &hexagon.CompletionResponse{Content: "我是小蟹。", Usage: hexagon.Usage{TotalTokens: 6}}, nil
}

func (p *fastKBChatProvider) Stream(_ context.Context, req hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	p.record(req)
	body := `data: {"choices":[{"index":0,"delta":{"content":"我是小蟹。"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	return llm.NewStream(strings.NewReader(body), llm.StreamOpenAIFormat), nil
}

func (p *fastKBChatProvider) Models() []llm.ModelInfo {
	return []llm.ModelInfo{{ID: "mock-model", Name: "Mock Model"}}
}
func (p *fastKBChatProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

func (p *fastKBChatProvider) sawText(sub string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.seen {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

type slowAuxEmbedder struct{ vecs map[string][]float32 }

func (e *slowAuxEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.lookup(t)
	}
	return out, nil
}
func (e *slowAuxEmbedder) EmbedOne(_ context.Context, t string) ([]float32, error) {
	return e.lookup(t), nil
}
func (e *slowAuxEmbedder) Dimension() int { return 4 }
func (e *slowAuxEmbedder) lookup(t string) []float32 {
	if v, ok := e.vecs[t]; ok {
		return v
	}
	return []float32{0, 0, 0, 1} // 未知文本：与 query 基正交
}

// newEngineWithSlowAuxKB 构造 engine + KB（rerank+expand 开、注入 slowAuxLLM、含 2 篇
// 与 query 对齐的文档以触发 rerank 池 >1），并用快主 provider。
func newEngineWithSlowAuxKB(t *testing.T, provider hexagon.Provider, aux knowledge.RerankLLM) *ReActEngine {
	t.Helper()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}
	kbStore := knowledge.NewSQLiteStore(store.DB())
	if err := kbStore.Init(context.Background()); err != nil {
		t.Fatalf("初始化知识库存储失败: %v", err)
	}

	const q = "你是谁"
	emb := &slowAuxEmbedder{vecs: map[string][]float32{
		q: {1, 0, 0, 0},
		"小蟹是本地优先的 AI 搭子，数据不出门":   {1, 0, 0, 0},
		"HexClaw 河蟹 钳子硬壳也硬 隐私优先": {1, 0, 0, 0},
	}}
	kb := knowledge.NewManager(kbStore, kbStore, emb,
		knowledge.WithSplitter(splitter.NewRecursiveSplitter(
			splitter.WithRecursiveChunkSize(400), splitter.WithRecursiveChunkOverlap(80))),
		knowledge.WithHybridConfig(knowledge.HybridConfig{
			VectorWeight: 0.7, TextWeight: 0.3, MMRLambda: 0.7, TimeDecayDays: 0,
			MinScore: 0.3, CandidateK: 50, RRFK: 60, UseRRF: true,
			RerankEnabled: true, ExpandEnabled: true,
		}),
		knowledge.WithLLM(aux))
	for i, body := range []string{"小蟹是本地优先的 AI 搭子，数据不出门", "HexClaw 河蟹 钳子硬壳也硬 隐私优先"} {
		if _, err := kb.AddDocument(context.Background(), "doc"+string(rune('A'+i)), body, "test"); err != nil {
			t.Fatalf("add doc: %v", err)
		}
	}

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.Knowledge.Enabled = true
	cfg.Knowledge.TopK = 3
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"test": {Model: "mock-model"}}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"test": provider})
	eng := NewReActEngine(cfg, router, store, skill.NewRegistry())
	eng.SetKnowledgeBase(kb)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng
}

// R1（真实用户请求·同步）：KB 有文档 + 辅助 LLM 慢 + 主聊天快 → Process 快速出回复，
// 不被辅助 LLM 拖到 180s。
func TestSlowKBAux_R1_ProcessNotBlocked(t *testing.T) {
	eng := newEngineWithSlowAuxKB(t, &fastKBChatProvider{}, &slowAuxLLM{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	reply, err := eng.Process(ctx, &adapter.Message{
		ID: "m-r1", Platform: adapter.PlatformAPI, UserID: "u1", SessionID: "s-r1", Content: "你是谁",
	})
	el := time.Since(start)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reply == nil || reply.Content == "" {
		t.Fatal("应有回复")
	}
	if el >= 15*time.Second {
		t.Fatalf("辅助 LLM 慢时 Process 耗时 %v ≥ 15s——聊天关键路径被拖垮", el)
	}
	t.Logf("R1 Process elapsed=%v（并行 expand 两路 2.5s 预算超时→阈值2 开闸→rerank 跳过 + 快主回复）", el)
}

// R2（多轮·熔断）：连续多条消息，辅助 LLM 持续慢 → 熔断开闸，后续消息不再打辅助 LLM，
// 越来越快；辅助调用总数随熔断收敛（不随消息数线性增长）。
func TestSlowKBAux_R2_BreakerAcrossMessages(t *testing.T) {
	aux := &slowAuxLLM{}
	eng := newEngineWithSlowAuxKB(t, &fastKBChatProvider{}, aux)

	var firstEl, lastEl time.Duration
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		if _, err := eng.Process(ctx, &adapter.Message{
			ID: "m-r2-" + string(rune('a'+i)), Platform: adapter.PlatformAPI,
			UserID: "u1", SessionID: "s-r2", Content: "你是谁 第" + string(rune('0'+i)) + "问",
		}); err != nil {
			cancel()
			t.Fatalf("Process %d: %v", i, err)
		}
		cancel()
		el := time.Since(start)
		if i == 0 {
			firstEl = el
		}
		lastEl = el
	}
	calls := aux.calls.Load()
	// 熔断后后续消息应零/极少辅助调用：4 条消息若无熔断至少 4×3=12 次；熔断后应远少于此。
	if calls >= 12 {
		t.Fatalf("熔断未生效：4 条消息累计 %d 次辅助 LLM 调用（无熔断应线性增长），冷却期内应收敛", calls)
	}
	// 末条消息应明显快于首条（熔断已开，辅助路径零延迟）。
	if lastEl > firstEl {
		t.Logf("R2 注意：末条 %v 未快于首条 %v（可容忍，主看 calls 收敛=%d）", lastEl, firstEl, calls)
	}
	t.Logf("R2 首条=%v 末条=%v 辅助调用累计=%d（熔断收敛）", firstEl, lastEl, calls)
}

// R3（真实用户请求·流式 SSE，桌面实际路径）：辅助 LLM 慢 → ProcessStream 仍快速吐完整回复。
func TestSlowKBAux_R3_ProcessStreamNotBlocked(t *testing.T) {
	eng := newEngineWithSlowAuxKB(t, &fastKBChatProvider{}, &slowAuxLLM{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	ch, err := eng.ProcessStream(ctx, &adapter.Message{
		ID: "m-r3", Platform: adapter.PlatformAPI, UserID: "u1", SessionID: "s-r3", Content: "你是谁",
	})
	if err != nil {
		t.Fatalf("ProcessStream: %v", err)
	}
	var content strings.Builder
	for chunk := range ch {
		if chunk.Error != nil {
			t.Fatalf("流式错误: %v", chunk.Error)
		}
		content.WriteString(chunk.Content)
		if chunk.Done {
			break
		}
	}
	el := time.Since(start)
	if content.Len() == 0 {
		t.Fatal("流式应产出回复内容")
	}
	if el >= 15*time.Second {
		t.Fatalf("辅助 LLM 慢时 ProcessStream 耗时 %v ≥ 15s——SSE 关键路径被拖垮", el)
	}
	t.Logf("R3 ProcessStream elapsed=%v content=%q", el, content.String())
}

// R4（降级不杀检索质量）：强 KB 命中 + 辅助 LLM 慢 → 回复仍带 KB 注入内容
// （向量+BM25 检索路径在辅助 LLM 降级后照常跑，文档仍到达模型）。
func TestSlowKBAux_R4_DegradePreservesRetrieval(t *testing.T) {
	prov := &fastKBChatProvider{}
	eng := newEngineWithSlowAuxKB(t, prov, &slowAuxLLM{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := eng.Process(ctx, &adapter.Message{
		ID: "m-r4", Platform: adapter.PlatformAPI, UserID: "u1", SessionID: "s-r4", Content: "你是谁",
	}); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// 辅助 LLM 全程超预算降级，但确定性检索仍应把文档注入模型消息。
	if !prov.sawText("本地优先") && !prov.sawText("河蟹") {
		t.Fatal("R4: 辅助 LLM 降级后 KB 注入内容未到达模型——降级杀掉了检索质量")
	}
	t.Log("R4 强命中文档在辅助 LLM 降级后仍注入模型（确定性检索存活）")
}

// R5（并发·多用户同时提问）：辅助 LLM 慢 + 多请求并发 → 全部返回、无死锁、总时限内完成，
// 熔断状态跨并发共享安全（配合 -race）。
func TestSlowKBAux_R5_ConcurrentRequests(t *testing.T) {
	eng := newEngineWithSlowAuxKB(t, &fastKBChatProvider{}, &slowAuxLLM{})

	const n = 4
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, errs[idx] = eng.Process(ctx, &adapter.Message{
				ID: "m-r5-" + string(rune('a'+idx)), Platform: adapter.PlatformAPI,
				UserID: "u" + string(rune('1'+idx)), SessionID: "s-r5-" + string(rune('a'+idx)), Content: "你是谁",
			})
		}(i)
	}
	wg.Wait()
	el := time.Since(start)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发请求 %d 失败: %v", i, err)
		}
	}
	if el >= 20*time.Second {
		t.Fatalf("并发慢辅助 LLM 下 %d 请求耗时 %v ≥ 20s——疑似串行阻塞/死锁", n, el)
	}
	t.Logf("R5 %d 并发请求 elapsed=%v 全部成功", n, el)
}
