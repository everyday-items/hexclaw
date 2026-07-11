package knowledge

import (
	"context"

	"github.com/hexagon-codes/hexagon"
)

// DefaultEmbedMaxRunes 是单条文本入 embedding API 前的默认截断上限（按 rune 计）。
//
// 取 6000：对 CJK 最坏情况（≈1 token/rune）仍稳在主流 embedding 模型
// （text-embedding-3-small 8191 token、nomic-embed-text 8192 token）上限内；
// 正常 400 字 chunk 远不触发，主要防御超长 query / HyDE 假设文档 / 误传整篇大文。
const DefaultEmbedMaxRunes = 6000

// TruncatingEmbedder 在调用底层 Embedder 前，把每条文本按 rune 截断到 maxRunes。
//
// 这是入 embedding API 前的防御闸：避免单条超长输入触发模型 token 超限错误（413）
// 或超量计费。它实现 hexagon.VectorEmbedder（= ai-core vector.Embedder），
// 可与 CachedEmbedder 自由叠加（推荐置于缓存外层，使缓存键作用于截断后文本）。
type TruncatingEmbedder struct {
	inner    hexagon.VectorEmbedder
	maxRunes int
}

// NewTruncatingEmbedder 包裹 inner，按 maxRunes 截断输入；maxRunes<=0 时用 DefaultEmbedMaxRunes。
func NewTruncatingEmbedder(inner hexagon.VectorEmbedder, maxRunes int) *TruncatingEmbedder {
	if maxRunes <= 0 {
		maxRunes = DefaultEmbedMaxRunes
	}
	return &TruncatingEmbedder{inner: inner, maxRunes: maxRunes}
}

// Embed 实现 vector.Embedder：逐条按 rune 截断后委托底层。
func (e *TruncatingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	ctx = sharedEmbedderContext(ctx)
	if len(texts) == 0 {
		return e.inner.Embed(ctx, texts)
	}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = clampRunes(t, e.maxRunes)
	}
	return e.inner.Embed(ctx, out)
}

// EmbedOne 实现 vector.Embedder。
func (e *TruncatingEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	return e.inner.EmbedOne(sharedEmbedderContext(ctx), clampRunes(text, e.maxRunes))
}

// Dimension 实现 vector.Embedder：透传底层维度。
func (e *TruncatingEmbedder) Dimension() int { return e.inner.Dimension() }

// clampRunes 把 s 截断到最多 max 个 rune（按 rune 切，保证不切坏多字节字符）。
func clampRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
