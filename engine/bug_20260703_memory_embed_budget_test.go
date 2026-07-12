package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/memory/recall"
)

// BUG-20260703 会话 pre-LLM 静默阻塞回归（真机日志：26 字回复 95s、37 字 115s，
// 「← 收到消息」→「Runtime Stream 调用准备」之间 110s 零日志）。根因三宗罪：
//   ① memEntrySource.Candidates 的 Embed 无 per-call 预算——继承整请求 ctx（web 适配器
//      给 10min），底层 HTTP 客户端又只设 ResponseHeaderTimeout=120s → 慢 embedding
//      端点可拖住会话上百秒；
//   ② 相关性地板砍空时 rankFacts 重试 Retrieve → 同一轮内第二次全量 Embed（双打）；
//   ③ 无熔断——端点持续慢/坏时，每条消息都重复付满额代价。
// 修复对齐同包 ActiveRecall 的既有防护模式：短预算 + 熔断 + 失败可观测。

// blockingEmbedder 模拟慢 embedding 端点：阻塞 delay 或 ctx 取消（先到者胜）。
// 尊重 ctx 是关键——预算修复正是靠短 ctx 把它掐断。
type blockingEmbedder struct {
	delay time.Duration
	calls atomic.Int32
}

func (b *blockingEmbedder) Dimension() int { return 3 }

func (b *blockingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	b.calls.Add(1)
	select {
	case <-time.After(b.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

// countingErrEmbedder 永远失败并计数（触发地板砍空 fallback 与熔断累计）。
type countingErrEmbedder struct{ calls atomic.Int32 }

func (c *countingErrEmbedder) Dimension() int { return 3 }

func (c *countingErrEmbedder) Embed(_ context.Context, _ []string) ([][]float32, error) {
	c.calls.Add(1)
	return nil, errors.New("embed endpoint down")
}

func budgetTestFacts() []recall.Entry {
	// m-1 与测试 query 有字面重叠（软降级 BM25 后仍有证据可召回）；m-2/m-3 零证据
	// （BUG-20260712-L 起零证据不注入，故不再用「全零重叠」fixture 断言非空）。
	return []recall.Entry{
		factEntry("m-1", "预算 alpha"),
		factEntry("m-2", "beta"),
		factEntry("m-3", "gamma"),
	}
}

// ① 预算：慢 embedding 端点绝不能拖垮会话——rankFacts 必须在预算级时间内返回
// （软降级 BM25），而不是陪端点等满 10 分钟 ctx / 120s header timeout。
func TestRankFacts_SlowEmbedderBoundedByBudget(t *testing.T) {
	emb := &blockingEmbedder{delay: 10 * time.Second}
	e := &ReActEngine{memEmbedder: emb}

	start := time.Now()
	got := e.rankFacts(context.Background(), budgetTestFacts(), "预算内返回", time.Now())
	elapsed := time.Since(start)

	if elapsed >= 5*time.Second {
		t.Fatalf("BUG-20260703①: 慢 embedder 拖垮召回，rankFacts 耗时 %v（应受预算约束 <5s，软降级 BM25）", elapsed)
	}
	if len(got) == 0 {
		t.Fatal("软降级后有词法证据的事实应仍可召回（预算约束不等于失聪）")
	}
}

// ② 双打：地板砍空的 fallback 重试不得触发同一轮内第二次 Embed——
// 同一 query 同一批文本，第二次必须复用第一次的结果/结论。
func TestRankFacts_NoDoubleEmbedOnFloorFallback(t *testing.T) {
	emb := &countingErrEmbedder{}
	e := &ReActEngine{memEmbedder: emb}

	_ = e.rankFacts(context.Background(), budgetTestFacts(), "毫无字面重叠的查询", time.Now())

	if n := emb.calls.Load(); n != 1 {
		t.Fatalf("BUG-20260703②: 地板 fallback 双打 Embed，同一轮调用了 %d 次（应恰好 1 次）", n)
	}
}

// ③ 熔断：端点连续失败后必须开闸跳过向量路径（纯 BM25），
// 不能让后续每条消息都重复付失败/超时代价。
func TestRankFacts_BreakerOpensAfterConsecutiveFailures(t *testing.T) {
	emb := &countingErrEmbedder{}
	e := &ReActEngine{memEmbedder: emb}

	for range 5 {
		_ = e.rankFacts(context.Background(), budgetTestFacts(), "毫无字面重叠的查询", time.Now())
	}

	// 阈值 3 次连续失败开闸 → 第 4、5 轮不再打端点。
	if n := emb.calls.Load(); n > 3 {
		t.Fatalf("BUG-20260703③: 无熔断，5 轮召回打了 %d 次 Embed（连续失败 3 次后应开闸跳过）", n)
	}
}
