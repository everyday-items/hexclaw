// pipeline.go 实现 v0.4.0 H7 RAG 完整 pipeline（feature-flag gated）。
//
// 现状 knowledge.Manager.Search 已经做了"hybrid + MMR"召回，但 RAG 的完整链
// （查询改写 / 重排 / context 拼装 / 答案生成）还散在各调用方手写。H7 把这
// 5 阶段抽成显式 Pipeline，便于后续插拔（cross-encoder rerank / LLM query rewrite /
// citation-aware context builder）。
//
// 5 阶段：
//
//	1. QueryRewriter      — 把用户原 query 改写成更适合检索的形式（同义词 / 拆分）
//	2. Retriever          — 调 Manager.Search 返回候选 chunks
//	3. Reranker           — 用更精细的 scorer（cross-encoder / LLM）重排
//	4. ContextBuilder     — 把 chunks 拼成 prompt 用 context（含分隔符 / 引用 ID）
//	5. Answerer           — 把 context 喂给 LLM 生成最终答案；可空（让调用方自己组 prompt）
//
// flag rag.pipeline.v1 控制是否启用。flag 关闭时 RunRAG 立即返回 ErrPipelineDisabled，
// 调用方应直接用 Manager.Query / Search 老 API。
package knowledge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagRAGPipelineV1 控制 H7 RAG pipeline 是否启用。
const FlagRAGPipelineV1 = "rag.pipeline.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagRAGPipelineV1,
		Default:      true, // alpha 强制 OFF
		Description:  "Run knowledge retrieval through the v0.4.0 5-stage RAG pipeline (rewrite/retrieve/rerank/context/answer).",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// QueryRewriter 把用户原 query 转成（一个或多个）检索 query。
type QueryRewriter interface {
	Rewrite(ctx context.Context, original string) ([]string, error)
}

// Retriever 接收（多个）检索 query，返回去重的候选 SearchHit 列表。
type Retriever interface {
	Retrieve(ctx context.Context, queries []string, topK int) ([]SearchHit, error)
}

// Reranker 对候选 hits 做精细打分排序。返回新排序后的 hits（长度可裁剪）。
type Reranker interface {
	Rerank(ctx context.Context, query string, hits []SearchHit, topK int) ([]SearchHit, error)
}

// ContextBuilder 把 hits 拼成 prompt 友好的 context 字符串（含引用标记）。
type ContextBuilder interface {
	Build(hits []SearchHit) (string, error)
}

// Answerer 接收 query + context，生成最终答案。返回空字符串表示"由调用方自己生成"。
type Answerer interface {
	Answer(ctx context.Context, query, contextStr string) (string, error)
}

// Pipeline 把 5 阶段组合成一个可执行流水线。任一字段为 nil 时使用本包提供的默认实现。
type Pipeline struct {
	Rewriter       QueryRewriter
	Retriever      Retriever
	Reranker       Reranker
	ContextBuilder ContextBuilder
	Answerer       Answerer
}

// PipelineResult 是 5 阶段执行后的全量产物。
type PipelineResult struct {
	Original string
	Queries  []string    // QueryRewriter 输出
	Hits     []SearchHit // Reranker 输出
	Context  string      // ContextBuilder 输出
	Answer   string      // Answerer 输出（可空）
}

// ErrPipelineDisabled 在 flag OFF 时返回，调用方据此 fallback。
var ErrPipelineDisabled = errors.New("RAG pipeline disabled (flag rag.pipeline.v1 OFF)")

// RunRAG 执行 5 阶段流水线。任一阶段返回 error 立即短路。
func (p *Pipeline) RunRAG(ctx context.Context, query string, topK int) (*PipelineResult, error) {
	if !featureflag.Enabled(ctx, FlagRAGPipelineV1) {
		return nil, ErrPipelineDisabled
	}
	if topK <= 0 {
		topK = 5
	}

	res := &PipelineResult{Original: query}

	rewriter := p.Rewriter
	if rewriter == nil {
		rewriter = IdentityQueryRewriter{}
	}
	queries, err := rewriter.Rewrite(ctx, query)
	if err != nil {
		return res, fmt.Errorf("rag: rewrite: %w", err)
	}
	if len(queries) == 0 {
		queries = []string{query}
	}
	res.Queries = queries

	if p.Retriever == nil {
		return res, fmt.Errorf("rag: Retriever is required")
	}
	hits, err := p.Retriever.Retrieve(ctx, queries, topK*2) // 多取一些，给 rerank 更大空间
	if err != nil {
		return res, fmt.Errorf("rag: retrieve: %w", err)
	}

	reranker := p.Reranker
	if reranker == nil {
		reranker = ScoreReranker{}
	}
	hits, err = reranker.Rerank(ctx, query, hits, topK)
	if err != nil {
		return res, fmt.Errorf("rag: rerank: %w", err)
	}
	res.Hits = hits

	cb := p.ContextBuilder
	if cb == nil {
		cb = SimpleContextBuilder{}
	}
	ctxStr, err := cb.Build(hits)
	if err != nil {
		return res, fmt.Errorf("rag: build context: %w", err)
	}
	res.Context = ctxStr

	if p.Answerer != nil {
		answer, err := p.Answerer.Answer(ctx, query, ctxStr)
		if err != nil {
			return res, fmt.Errorf("rag: answer: %w", err)
		}
		res.Answer = answer
	}

	return res, nil
}

// ============== 默认实现 ==============

// IdentityQueryRewriter 把 query 原样返回。
type IdentityQueryRewriter struct{}

// Rewrite 实现 QueryRewriter，返回 [query]。
func (IdentityQueryRewriter) Rewrite(_ context.Context, q string) ([]string, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, errors.New("empty query")
	}
	return []string{q}, nil
}

// ManagerRetriever 把 knowledge.Manager.Search 适配为 Retriever。多 query 时
// 各自检索后去重合并（按 chunk_id 去重，保留首次出现）。
type ManagerRetriever struct {
	Manager *Manager
}

// NewManagerRetriever 构造一个使用 *Manager 的 Retriever。
func NewManagerRetriever(m *Manager) *ManagerRetriever {
	return &ManagerRetriever{Manager: m}
}

// Retrieve 实现 Retriever。
func (r *ManagerRetriever) Retrieve(ctx context.Context, queries []string, topK int) ([]SearchHit, error) {
	if r == nil || r.Manager == nil {
		return nil, errors.New("ManagerRetriever: nil Manager")
	}
	seen := map[string]bool{}
	var out []SearchHit
	for _, q := range queries {
		hits, err := r.Manager.Search(ctx, q, topK)
		if err != nil {
			return nil, fmt.Errorf("search %q: %w", q, err)
		}
		for _, h := range hits {
			if seen[h.ChunkID] {
				continue
			}
			seen[h.ChunkID] = true
			out = append(out, h)
		}
	}
	return out, nil
}

// ScoreReranker 按 SearchHit.Score 降序排序，截断到 topK。这是默认 / 占位实现；
// 真实 cross-encoder rerank 留给 v0.4.x 后续。
type ScoreReranker struct{}

// Rerank 实现 Reranker。
func (ScoreReranker) Rerank(_ context.Context, _ string, hits []SearchHit, topK int) ([]SearchHit, error) {
	out := make([]SearchHit, len(hits))
	copy(out, hits)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out, nil
}

// SimpleContextBuilder 把 hits 拼成 markdown-friendly context，每段带引用 ID。
//
// 输出形如：
//
//	[1] (doc=abc123 chunk=0)
//	chunk content...
//
//	[2] (doc=def456 chunk=1)
//	...
type SimpleContextBuilder struct {
	// MaxChars 单段 context 的最大字符数；超过时截断并加 "..."。0 = 不限。
	MaxChars int
}

// Build 实现 ContextBuilder。
func (b SimpleContextBuilder) Build(hits []SearchHit) (string, error) {
	var sb strings.Builder
	for i, h := range hits {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "[%d] (doc=%s chunk=%d)\n", i+1, h.DocID, h.ChunkIndex)
		body := h.Content
		if b.MaxChars > 0 && len(body) > b.MaxChars {
			body = body[:b.MaxChars] + "..."
		}
		sb.WriteString(body)
	}
	return sb.String(), nil
}

// AnswerFunc 把一个普通函数适配为 Answerer。
type AnswerFunc func(ctx context.Context, query, contextStr string) (string, error)

// Answer 实现 Answerer。
func (f AnswerFunc) Answer(ctx context.Context, query, contextStr string) (string, error) {
	if f == nil {
		return "", nil
	}
	return f(ctx, query, contextStr)
}
