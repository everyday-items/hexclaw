package knowledge

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/toolkit/util/logger"
)

// RAG 辅助 LLM 预算 + 熔断（BUG-20260704）
//
// 查询扩展（multi-query / HyDE）与 contextual chunk 摘要是 RAG 的可选增强。前者跑在
// 聊天关键路径上（react.go 自动注入前的 kb.Query），后者跑在入库路径；二者均经
// router.Route(ctx) 路由到默认 provider。实机默认常是本地大模型（qwen3.5:9b 单次补全
// 实测 43s），且此前无时间预算，连续辅助调用会阻塞前台主聊天。
//
// 修复对齐 engine 记忆召回熔断（memory_recall.go）：每次辅助 LLM 调用带小预算，超时/
// 失败即降级（expand→原查询、contextual→确定性标题/章节定位）；连续失败达阈值即开闸
// 冷却，期间直接跳过辅助 LLM。provider 无关——本地慢模型或偶发慢云端同样兜底。
const (
	// ragAuxLLMTimeout 单次辅助 LLM 调用预算。健康云端补全 <2s；超此即判慢，掐断降级。
	ragAuxLLMTimeout = 2500 * time.Millisecond
	// ragAuxLLMBreakerThreshold 连续失败/超时阈值，达到即开闸。
	// 取 2：一次检索的 multi-query 与 HyDE 两路并发超时即可开闸，使后续辅助调用快速跳过；
	// 同时需两次失败确认，避免一次抖动即误判，健康快 provider（<预算）不触发。
	ragAuxLLMBreakerThreshold = 2
	// ragAuxLLMBreakerCooldown 开闸冷却期：期间辅助 LLM 直接跳过（纯确定性检索），
	// 到期后放行一次探测恢复。
	ragAuxLLMBreakerCooldown = 60 * time.Second
)

// errRAGAuxLLMBreakerOpen 熔断开闸期间返回：调用方（查询生成器 / contextualizer）据此降级。
var errRAGAuxLLMBreakerOpen = errors.New("knowledge: rag aux llm breaker open (cooling down)")

// auxLLMBreaker 是 RAG 辅助 LLM 的 lock-free 熔断器：连续失败达阈值即开闸冷却，
// 期间 allow 返回 false（调用方快速失败并降级），避免每条消息重付慢 provider 的预算。
// 零值可用。
type auxLLMBreaker struct {
	failStreak atomic.Int64
	openUntil  atomic.Int64 // unix nano；>now 表示开闸冷却中
}

// allow 报告此刻是否放行辅助 LLM 调用（未开闸 / 冷却已过）。
func (b *auxLLMBreaker) allow(now time.Time) bool {
	until := b.openUntil.Load()
	return until == 0 || now.UnixNano() >= until
}

// record 记账一次调用结果：成功复位；失败累计，达阈值即开闸冷却。
func (b *auxLLMBreaker) record(ok bool, now time.Time) {
	if ok {
		b.failStreak.Store(0)
		b.openUntil.Store(0)
		return
	}
	if streak := b.failStreak.Add(1); streak >= ragAuxLLMBreakerThreshold {
		b.openUntil.Store(now.Add(ragAuxLLMBreakerCooldown).UnixNano())
	}
}

// budgetedRerankLLM 给底层 RerankLLM 包上"每次调用预算 + 熔断"，实现同一 RerankLLM
// 接口，可透明替换 multi-query / HyDE / contextual 摘要所用的 LLM。预算 ctx 是请求 ctx 的
// 子 ctx（仅约束本次辅助调用），绝不消耗父 ctx 留给向量/BM25 检索的余量。
type budgetedRerankLLM struct {
	inner   RerankLLM
	breaker *auxLLMBreaker
}

// Complete 实现 RerankLLM。
func (b *budgetedRerankLLM) Complete(ctx context.Context, prompt string) (string, error) {
	now := time.Now()
	if !b.breaker.allow(now) {
		return "", errRAGAuxLLMBreakerOpen // 冷却期内零延迟跳过，触发调用方降级
	}
	cctx, cancel := context.WithTimeout(ctx, ragAuxLLMTimeout)
	defer cancel()

	start := time.Now()
	out, err := b.inner.Complete(ragEnrichContext(cctx), prompt)
	if err != nil {
		b.breaker.record(false, time.Now())
		logger.Warn("[knowledge] 辅助 LLM 超预算/失败，降级确定性检索",
			"error", err, "elapsed", time.Since(start).String(), "budget", ragAuxLLMTimeout.String())
		return "", err
	}
	b.breaker.record(true, time.Now())
	return out, nil
}
