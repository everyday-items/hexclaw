package main

import (
	"context"
	"errors"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/knowledge"
)

// retrievalRouter 是构造 RAG 辅助 LLM 所需的最小路由能力面（便于单测替身）。
type retrievalRouter interface {
	Route(ctx context.Context) (hexagon.Provider, string, error)
	IsLocalProviderName(name string) bool
}

// errRetrievalAuxSkippedLocal 表示 RAG 辅助 LLM 因路由到本地 provider 而被跳过。
// 调用方（multi-query / HyDE / LLM 重排）据此优雅降级到确定性检索（向量+BM25+MMR）。
var errRetrievalAuxSkippedLocal = errors.New("knowledge: 辅助 LLM 跳过——路由到本地单槽 provider（避让前台主聊天）")

// newRetrievalRerankLLM 构造 KB 检索的辅助 LLM（查询扩展 / LLM 重排），复用 Agent 的 LLM router。
//
// ★前台/后台资源隔离（BUG-20260704）：查询扩展与 LLM 重排是**后台检索增强**，绝不能与**前台
// 主聊天生成**争抢同一稀缺算力。本地部署模型（如 Ollama，常 `-np 1` 单槽）尤甚——辅助调用即使
// 客户端预算取消，服务端仍占槽跑完，主回复被迫排在其后（实测本地 TTFT 0.66s→57s 头阻塞）。
// 故辅助 LLM 一旦路由到**本地 provider** 直接跳过，退化为确定性检索；云端 provider 有并行、
// 无自争用，照常启用。与 rag_aux_llm.go 的预算+熔断是同一「增强绝不阻塞主路径」纪律的互补一环。
func newRetrievalRerankLLM(router retrievalRouter) knowledge.RerankLLMFunc {
	return func(ctx context.Context, prompt string) (string, error) {
		ctx = ragEnrichEgressContext(ctx)
		provider, name, rErr := router.Route(ctx)
		if rErr != nil {
			return "", rErr
		}
		if router.IsLocalProviderName(name) {
			return "", errRetrievalAuxSkippedLocal
		}
		resp, cErr := provider.Complete(ctx, hexagon.CompletionRequest{
			Messages: []hexagon.Message{{Role: hexagon.RoleUser, Content: prompt}},
		})
		if cErr != nil {
			return "", cErr
		}
		return resp.Content, nil
	}
}
