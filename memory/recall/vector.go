package recall

import "math"

// Cosine 计算两向量的余弦相似度 ∈ [-1,1]（higher=more similar）。
//
// 用作向量召回路径的语义相似度：CandidateSource 适配器对 (queryVec, contentVec) 求 Cosine
// 后填入 Candidate.VectorScore（hybrid 加权再 clamp01，负相关→0，不污染排序）。
// 与 LexicalSim 一样是确定性、零依赖的打分原语，引擎桥接与 eval 共用，确保「评测所测==线上所跑」。
//
// 安全降级：维度不一致或任一为零向量时返回 0（调用方据此降级到纯 BM25，绝不 panic）。
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
