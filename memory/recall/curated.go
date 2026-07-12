package recall

import (
	"context"
	"sort"
	"strings"
	"time"
)

// Candidate 是一条记忆作为检索候选，附**已归一到 [0,1]、higher=better** 的两路相似度分。
//
// 契约：VectorScore（cosine）与 BM25Score（全文相关度）均须由 CandidateSource/适配器
// 归一到 [0,1]、越大越相关。真实 FTS 适配器需把 SQLite bm25()（越负越相关）做符号翻转 +
// 饱和变换后再填入。本包据此做加权 hybrid，保留绝对语义以便 MinScore 作绝对阈值。
type Candidate struct {
	Entry
	VectorScore float64
	BM25Score   float64
	HasVector   bool // 该候选是否带向量分（区分「向量 0 分」与「无向量降级」）
}

// CandidateSource 产出候选（适配 memory.FileMemory.Search / VectorMemory.Search / storage FTS）。
// 实现须**按 userID 过滤**（多租户不串场，方案 §4.9 / 修 P4），并可超额返回（candidateMultiplier）。
type CandidateSource interface {
	Candidates(ctx context.Context, userID, role, query string, k int) ([]Candidate, error)
}

// QueryExpander 召回前改写/扩展 query（方案 §7-核心 重点二：漏召缓解）。
// 默认 IdentityExpander 原样返回；LLM/同义词扩展为可插拔实现。
type QueryExpander interface {
	Expand(query string) string
}

// IdentityExpander 原样返回（确定性默认；不引入 LLM 到热路径）。
type IdentityExpander struct{}

func (IdentityExpander) Expand(q string) string { return strings.TrimSpace(q) }

// Reranker 可选精排（cross-encoder/轻量），nil 时用三维打分序（方案 §7-核心 重点二）。
type Reranker interface {
	Rerank(query string, results []Result) []Result
}

// Result 是一条召回结果，带归一 relevance 与最终 score（便于可观测/调参）。
type Result struct {
	Entry
	Relevance float64 // 归一后的 hybrid 相关度 [0,1]
	Score     float64 // 三维打分（rerank 后可能被覆盖为 rerank 分序）
}

// hybrid 比例（OpenClaw 源码确认 hybrid.ts:125：0.7 向量 + 0.3 BM25，归一化加权，非 RRF）。
const (
	wVector = 0.7
	wBM25   = 0.3
)

// CuratedRetriever 是策展事实检索器（方案 §7bis memory/curated.go，坑B 通道①）。
//
//	query 改写 → hybrid(0.7/0.3) → 三维打分 → rerank → minScore → top-K
//
// 无向量分（全部候选 HasVector=false）时**软降级纯 BM25**（memory-core 式，非硬 unavailable）。
type CuratedRetriever struct {
	Source       CandidateSource
	Expander     QueryExpander // nil → IdentityExpander
	Reranker     Reranker      // nil → 三维打分序
	Weights      Weights       // 零值 → DefaultWeights
	LambdaPerDay float64       // <=0 → DefaultLambdaPerDay
	MinScore     float64       // relevance 绝对阈值，砍低相关（防误召污染）
	TopK         int           // <=0 → 5
	CandidateMul int           // 候选放大倍数，<=0 → 4（OpenClaw candidateMultiplier）
	Now          func() time.Time
}

func (r *CuratedRetriever) weights() Weights {
	if r.Weights == (Weights{}) {
		return DefaultWeights()
	}
	return r.Weights
}

func (r *CuratedRetriever) topK() int {
	if r.TopK <= 0 {
		return 5
	}
	return r.TopK
}

func (r *CuratedRetriever) candidateMul() int {
	if r.CandidateMul <= 0 {
		return 4
	}
	return r.CandidateMul
}

func (r *CuratedRetriever) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Retrieve 执行策展事实召回，返回按 score 降序的 top-K。
//
// 纯读：不改 RecallCount/AccessedAt（自校正由调用方经 Writer 落盘，避免本函数产生副作用）。
// 调用方可据返回的 IDs 提交一次 RecallCount++ 写操作。
func (r *CuratedRetriever) Retrieve(ctx context.Context, userID, role, query string) ([]Result, error) {
	expander := QueryExpander(IdentityExpander{})
	if r.Expander != nil {
		expander = r.Expander
	}
	q := expander.Expand(query)
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}

	cands, err := r.Source.Candidates(ctx, userID, role, q, r.topK()*r.candidateMul())
	if err != nil {
		return nil, err
	}
	now := r.now()

	// 过滤：当前有效（G3）+ 非空正文 + 租户匹配（双保险，Source 应已过滤）。
	filtered := cands[:0:0]
	anyVector := false
	for _, c := range cands {
		if strings.TrimSpace(c.Content) == "" {
			continue
		}
		if !sameUser(c.UserID, userID) {
			continue
		}
		if !IsCurrentlyValid(c.Entry, now) {
			continue
		}
		if c.HasVector {
			anyVector = true
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		return nil, nil
	}

	w := r.weights()
	out := make([]Result, 0, len(filtered))
	for _, c := range filtered {
		rel := relevance(c, anyVector)
		// 零证据剔除（BUG-20260712-L）：rel=0 = 词法零重叠且无向量证据 = 「无相关」而非
		// 「低相关」，不受 MinScore=0（关地板）豁免——真机取证：问 1+1 召回「大大阿达」。
		// rel>0 的低分候选不受影响（S2「花生酱→花生过敏」场景由 MinScore 语义管）。
		if rel <= 0 {
			continue
		}
		if rel < r.MinScore { // 绝对阈值砍低相关，防误召污染
			continue
		}
		out = append(out, Result{
			Entry:     c.Entry,
			Relevance: rel,
			Score:     Score(c.Entry, rel, now, w, r.LambdaPerDay),
		})
	}
	if len(out) == 0 {
		return nil, nil
	}

	// 稳定排序：score 降序，平分按 importance、再按 ID 保证确定性。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		ii, ij := Importance(out[i].Entry), Importance(out[j].Entry)
		if ii != ij {
			return ii > ij
		}
		return out[i].ID < out[j].ID
	})

	if r.Reranker != nil {
		out = r.Reranker.Rerank(q, out)
	}

	if k := r.topK(); len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// relevance 计算单候选的 hybrid 相关度 [0,1]。
// 有向量层 → 0.7·cosine + 0.3·bm25；全程无向量 → 软降级纯 bm25（方案 D6）。
func relevance(c Candidate, anyVector bool) float64 {
	bm := clamp01(c.BM25Score)
	if !anyVector {
		return bm
	}
	vec := 0.0
	if c.HasVector {
		vec = clamp01(c.VectorScore)
	}
	return clamp01(wVector*vec + wBM25*bm)
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
