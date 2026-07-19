package engine

import (
	"context"
	"sync"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/memory/recall"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

// retrievalHitsSink 收集本轮请求的 RAG/记忆检索命中，供 finalize（done chunk）与
// 非流式 Reply 组装点回传前端（U9：命中标签+详情）。
//
// 为什么用 context sink 而非透传参数：知识命中在顶层 RAG 注入块产出，记忆命中在
// 更深的 buildTurnContext→buildLongTermMemoryBlock 产出，两处相隔多层函数（且流式
// 路径跨 goroutine）。用挂在 ctx 上的单例 sink 汇聚，避免把 hits 参数穿过
// completeWithTools / processStreamRuntime / buildCompletionRequest 一长串签名。
// 与既有 withToolReplyMetaSink 同模式。
type retrievalHitsSink struct {
	mu        sync.Mutex
	knowledge []adapter.KnowledgeHit
	memory    []adapter.MemoryHit
}

type retrievalHitsSinkKey struct{}

// withRetrievalHitsSink 在 ctx 上挂一个（幂等）命中收集 sink。
func withRetrievalHitsSink(ctx context.Context) context.Context {
	if ctx.Value(retrievalHitsSinkKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, retrievalHitsSinkKey{}, &retrievalHitsSink{})
}

func retrievalHitsFrom(ctx context.Context) *retrievalHitsSink {
	s, _ := ctx.Value(retrievalHitsSinkKey{}).(*retrievalHitsSink)
	return s
}

// recordKnowledgeHits 把一次知识库检索的命中记入本轮 sink（best-effort，无 sink 静默跳过）。
func recordKnowledgeHits(ctx context.Context, hits []knowledge.SearchHit) {
	if len(hits) == 0 {
		return
	}
	s := retrievalHitsFrom(ctx)
	if s == nil {
		return
	}
	mapped := make([]adapter.KnowledgeHit, 0, len(hits))
	for _, h := range hits {
		mapped = append(mapped, adapter.KnowledgeHit{
			DocTitle:       h.DocTitle,
			Source:         h.Source,
			Content:        h.Content,
			Score:          h.Score,
			MessageContent: canonicalProducerContent(messagecontent.ProducerRAG, h.Content, "und"),
		})
	}
	s.mu.Lock()
	s.knowledge = append(s.knowledge, mapped...)
	s.mu.Unlock()
}

// recordMemoryHits 把一次长期记忆召回的命中记入本轮 sink。
func recordMemoryHits(ctx context.Context, role string, entries []recall.Entry) {
	if len(entries) == 0 {
		return
	}
	s := retrievalHitsFrom(ctx)
	if s == nil {
		return
	}
	mapped := make([]adapter.MemoryHit, 0, len(entries))
	for _, en := range entries {
		mapped = append(mapped, adapter.MemoryHit{
			Content:        en.Content,
			Source:         role,
			MessageContent: canonicalProducerContent(messagecontent.ProducerRAG, en.Content, "und"),
		})
	}
	s.mu.Lock()
	s.memory = append(s.memory, mapped...)
	s.mu.Unlock()
}

// snapshot 返回本轮已收集命中的拷贝（供回复组装点读取）。
func (s *retrievalHitsSink) snapshot() ([]adapter.KnowledgeHit, []adapter.MemoryHit) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var k []adapter.KnowledgeHit
	if len(s.knowledge) > 0 {
		k = append(k, s.knowledge...)
	}
	var m []adapter.MemoryHit
	if len(s.memory) > 0 {
		m = append(m, s.memory...)
	}
	return k, m
}

// retrievalHitsSnapshot 读取 ctx 上 sink 的当前命中快照（无 sink 返回空）。
func retrievalHitsSnapshot(ctx context.Context) ([]adapter.KnowledgeHit, []adapter.MemoryHit) {
	return retrievalHitsFrom(ctx).snapshot()
}
