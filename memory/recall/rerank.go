package recall

import (
	"sort"
	"strings"
)

// 闭环重点二「rerank 精排」（§7-核心）：cross-encoder 之外的确定性轻量基线，替代 nil Reranker。
// 在三维打分序基础上，给「query 整体命中正文（短语匹配）」或「字面覆盖高」者加成后重排。

// LexicalReranker 确定性词面精排。
type LexicalReranker struct {
	PhraseBonus   float64 // query 整体是正文子串的加成，<=0 → 0.5
	CoverageBonus float64 // 字面相似度加成系数，<=0 → 0.3
}

func (r LexicalReranker) Rerank(query string, results []Result) []Result {
	pb, cb := r.PhraseBonus, r.CoverageBonus
	if pb <= 0 {
		pb = 0.5
	}
	if cb <= 0 {
		cb = 0.3
	}
	q := strings.ToLower(strings.TrimSpace(query))

	bonus := func(content string) float64 {
		b := cb * LexicalSim(query, content)
		if q != "" && strings.Contains(strings.ToLower(content), q) {
			b += pb
		}
		return b
	}

	out := make([]Result, len(results))
	copy(out, results)
	sort.SliceStable(out, func(i, j int) bool {
		si := out[i].Score + bonus(out[i].Content)
		sj := out[j].Score + bonus(out[j].Content)
		if si != sj {
			return si > sj
		}
		return out[i].ID < out[j].ID
	})
	return out
}
