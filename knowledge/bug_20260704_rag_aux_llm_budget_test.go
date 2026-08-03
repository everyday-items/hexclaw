package knowledge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// BUG-20260704：会话消息回复特别慢——根因在 RAG 检索的辅助 LLM 调用无预算。
//
// 真机取证：聊天用 SF(快)，但 KB 检索路径的 expandQueries（并行 multi-query+HyDE）
// 经 router.Route(ctx) 路由到**默认 provider**（实机默认=本地 ollama qwen3.5:9b，单次
// 补全实测 43s），且此前**无时间预算**、跑在聊天关键路径上。辅助生成占住本地单槽后，
// 主聊天只能排队，最终卡满客户端超时。
//
// 契约：RAG 辅助 LLM（查询扩展 / contextual-ingest）是**增强**，绝不能把聊天关键路径拖过预算——
//   ① 每次辅助 LLM 调用必须带时间预算（不继承整请求 ctx 的漫长余量）；
//   ② 慢/失败时降级到确定性检索（向量+BM25+MMR），聊天照常出结果；
//   ③ 连续慢/失败达阈值即熔断开闸，冷却期内直接跳过辅助 LLM（零延迟），不每条消息重付预算。
//
// 文档重排不复用该聊天 LLM；仅显式专用 cross-encoder 可以执行重排，无 executor 时走 MMR。
//
// 修复对齐同生态 engine 记忆召回熔断（[[project_session_2026_07_03_fullstack_review_graceful_exit]]），
// provider 无关：无论辅助 LLM 路由到本地慢模型还是偶发慢的云端，都不阻塞聊天。

// fakeAuxLLM 记录每次 Complete 收到的 ctx 是否有 deadline + 剩余预算，并可模拟
// 三种上游行为：block（挂到 ctx 取消，模拟慢模型）/ err（立即错）/ ok（正常）。
type fakeAuxLLM struct {
	mode   string // "block" | "err" | "ok"
	okResp string

	calls       atomic.Int64
	mu          sync.Mutex
	hadDeadline []bool
	remaining   []time.Duration
}

func (f *fakeAuxLLM) Complete(ctx context.Context, _ string) (string, error) {
	f.calls.Add(1)
	dl, ok := ctx.Deadline()
	f.mu.Lock()
	f.hadDeadline = append(f.hadDeadline, ok)
	if ok {
		f.remaining = append(f.remaining, time.Until(dl))
	}
	f.mu.Unlock()

	switch f.mode {
	case "block":
		<-ctx.Done()
		return "", ctx.Err()
	case "err":
		return "", errors.New("aux llm boom")
	default:
		return f.okResp, nil
	}
}

// ragAuxMgr 构造一个 expand 开启、rerank 未注入专用 executor 的 Manager，并塞 ≥2 篇文档
// 以同时覆盖查询扩展和 MMR 降级。embedder 用确定性 scriptedEmbedder。
func ragAuxMgr(t *testing.T, llm RerankLLM) *Manager {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	emb := &scriptedEmbedder{dim: 4, vecs: map[string][]float32{
		"hexclaw architecture overview":      {1, 0, 0, 0},
		"hexclaw local first privacy design": {1, 0, 0, 0},
	}}
	cfg := HybridConfig{
		VectorWeight: 0.7, TextWeight: 0.3, MMRLambda: 0.7, TimeDecayDays: 0,
		MinScore: 0.0, CandidateK: 50, RRFK: 60, UseRRF: true,
		RerankEnabled: true, ExpandEnabled: true,
	}
	mgr := NewManager(store, store, emb, WithSplitter(testSplitter()), WithHybridConfig(cfg), WithLLM(llm))
	for i, body := range []string{
		"hexclaw architecture overview",
		"hexclaw local first privacy design",
	} {
		if _, err := mgr.AddDocument(context.Background(), "doc"+string(rune('A'+i)), body, "test"); err != nil {
			t.Fatalf("add doc: %v", err)
		}
	}
	return mgr
}

// ① 每次辅助 LLM 调用必须带（小的）时间预算。RED：未修改代码把整请求 ctx 直接下传，
// 收到的 ctx 无 deadline（本测试传 Background）→ hadDeadline=false → FAIL。
func TestRAGAuxLLM_BoundedCtx(t *testing.T) {
	f := &fakeAuxLLM{mode: "ok", okResp: "hexclaw architecture overview\nhexclaw local first privacy design"}
	mgr := ragAuxMgr(t, f)

	// Background：无 deadline——模拟真实聊天请求的漫长 ctx。
	_, err := mgr.Search(context.Background(), "hexclaw architecture", 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	if f.calls.Load() == 0 {
		t.Fatal("辅助 LLM 未被调用——脚手架未触发 query-expand，测试无效")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, has := range f.hadDeadline {
		if !has {
			t.Fatalf("第 %d 次辅助 LLM 调用收到无 deadline 的 ctx——无预算，慢模型会拖垮聊天关键路径", i)
		}
	}
	for i, rem := range f.remaining {
		if rem <= 0 || rem > 10*time.Second {
			t.Fatalf("第 %d 次辅助 LLM 调用预算 %v 不在合理小预算区间（应 (0,10s]）", i, rem)
		}
	}
}

// ② 慢模型不得阻塞聊天关键路径。RED：未修改代码下 block 型 fake 挂到 parent ctx
// 的 12s deadline 才返回（≈12s）→ 超过 10s 断言 → FAIL。修复后每次预算内掐断 → <10s。
func TestRAGAuxLLM_SlowModelDoesNotBlock(t *testing.T) {
	f := &fakeAuxLLM{mode: "block"}
	mgr := ragAuxMgr(t, f)

	// parent 带 12s deadline，模拟"请求 ctx 终会超时但很久"。
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	start := time.Now()
	res, err := mgr.Search(ctx, "hexclaw architecture", 3)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("检索应降级出结果而非报错: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("辅助 LLM 慢应降级到确定性检索并仍出结果（增强不阻断）")
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("辅助 LLM 慢时检索耗时 %v ≥ 10s——聊天关键路径被拖垮（预算未生效）", elapsed)
	}
}

// ③ 连续慢/失败触发熔断，冷却期内后续检索直接跳过辅助 LLM（零延迟）。
// RED：无熔断时每次 Search 都调 fake，两轮调用数翻倍；修复后第二轮不再新增调用。
func TestRAGAuxLLM_BreakerSkipsAfterStreak(t *testing.T) {
	f := &fakeAuxLLM{mode: "err"} // 立即错，快速累计失败
	mgr := ragAuxMgr(t, f)

	if _, err := mgr.Search(context.Background(), "hexclaw architecture", 3); err != nil {
		t.Fatalf("search1: %v", err)
	}
	c1 := f.calls.Load()
	if c1 == 0 {
		t.Fatal("首轮辅助 LLM 未被调用——脚手架无效")
	}

	if _, err := mgr.Search(context.Background(), "hexclaw privacy", 3); err != nil {
		t.Fatalf("search2: %v", err)
	}
	c2 := f.calls.Load()

	if c2 > c1 {
		t.Fatalf("熔断未生效：首轮 %d 次辅助 LLM 调用后连续失败，第二轮又新增 %d 次——冷却期内应零调用", c1, c2-c1)
	}
}

// ④ 降级正确性守卫：辅助 LLM 立即报错时，检索仍走确定性路径出结果（不吞成空/不报错）。
func TestRAGAuxLLM_DegradesToResults(t *testing.T) {
	f := &fakeAuxLLM{mode: "err"}
	mgr := ragAuxMgr(t, f)

	res, err := mgr.Search(context.Background(), "hexclaw architecture", 3)
	if err != nil {
		t.Fatalf("辅助 LLM 错不应让检索报错（应软降级）: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("辅助 LLM 错时检索应回退向量+BM25+MMR 仍出结果")
	}
}

// ⑤ 无回归守卫：辅助 LLM 快且正常时仍被调用（预算/熔断不误伤健康路径）。
func TestRAGAuxLLM_FastModelStillInvoked(t *testing.T) {
	f := &fakeAuxLLM{mode: "ok", okResp: "hexclaw architecture overview"}
	mgr := ragAuxMgr(t, f)

	if _, err := mgr.Search(context.Background(), "hexclaw architecture", 3); err != nil {
		t.Fatalf("search: %v", err)
	}
	if f.calls.Load() == 0 {
		t.Fatal("健康快模型下 query-expand 辅助 LLM 应照常调用——预算/熔断误伤了正常路径")
	}
}
