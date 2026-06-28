package recall

import "strings"

// LexicalSim 是字符二元组 Jaccard 相似度 [0,1]，对 CJK 与拉丁均可用、确定性、零依赖。
//
// 用作降级纯 BM25 路径（无 embedding）的全文相关度近似：CandidateSource 适配器可用它把
// (query, content) 映射成 [0,1] higher=better 的 BM25Score 填入 Candidate。eval 护城河与引擎
// 桥接共用此实现，确保「评测所测 == 线上所跑」。
func LexicalSim(a, b string) float64 {
	sa, sb := bigrams(a), bigrams(b)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for g := range sa {
		if _, ok := sb[g]; ok {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func bigrams(s string) map[string]struct{} {
	s = strings.ToLower(strings.Join(strings.Fields(s), ""))
	rs := []rune(s)
	out := make(map[string]struct{}, len(rs))
	for i := 0; i+1 < len(rs); i++ {
		out[string(rs[i:i+2])] = struct{}{}
	}
	if len(rs) == 1 {
		out[string(rs)] = struct{}{}
	}
	return out
}
