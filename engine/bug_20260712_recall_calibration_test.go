package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/memory"
)

// BUG-20260712-O（真实模型 E2E 取证 · 标定缺陷）：nomic-embed-text 真机矩阵显示——
// 「你好」召回了「孩子对花生过敏」记忆（memory_hits 非空）。两个叠加根因之一：
// 记忆检索层没有寒暄门（知识库侧已有 shouldAutoInjectKB，记忆侧漏装）。
// 契约：<4 rune 的超短输入无检索意图 → 检索层跳过（常驻层照常注入）。
func TestBug20260712_MemoryChitchatGate(t *testing.T) {
	fm := adfFM(t)
	adfMust(t, fm.SaveStructuredEntry("孩子对花生过敏，任何含花生的食物都不能吃", "fact", "manual", "", memory.EntryMeta{}))
	adfMust(t, fm.SaveStructuredEntry("用户是一名船长", "identity", "manual", "", memory.EntryMeta{}))
	eng := adfEng(t, fm)

	ctx := withRetrievalHitsSink(context.Background())
	block := eng.buildLongTermMemoryBlock(ctx, "", "你好")

	if strings.Contains(block, "花生") {
		t.Fatalf("寒暄不得触发记忆检索层注入（真机取证：你好→花生过敏）：%q", block)
	}
	if !strings.Contains(block, "船长") {
		t.Fatalf("常驻层（identity）应照常注入：%q", block)
	}
	if _, mem := retrievalHitsFrom(ctx).snapshot(); len(mem) != 0 {
		t.Fatalf("寒暄不得产生记忆命中，got %d", len(mem))
	}
}

// subFloorEmbedder 复刻真机刻度：任意 query↔任意记忆 cos=0.543（年假↔花生过敏实测）——
// hybrid 0.7*0.543=0.380 < 地板 0.5，属应被砍的噪音区间。
type subFloorEmbedder struct{}

func (subFloorEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		if i == 0 {
			out[i] = []float32{1, 0} // query
		} else {
			out[i] = []float32{0.543, 0.8397} // cos(query, doc)=0.543（单位向量）
		}
	}
	return out, nil
}

// BUG-20260712-O②（真机取证）：年假问题带出花生记忆——hybrid 0.380<0.5 本应被地板砍掉，
// 却被 rankFacts「砍空即放宽重试」捞回。标定就位后该兜底只会复活地板下噪音
// （真低分真命中 ≥0.58 直接过地板，无需兜底），予以移除：砍空即空。
func TestBug20260712_SubFloorNotResurrectedByFallback(t *testing.T) {
	fm := adfFM(t)
	adfMust(t, fm.SaveStructuredEntry("孩子对花生过敏，任何含花生的食物都不能吃", "fact", "manual", "", memory.EntryMeta{}))
	eng := adfEng(t, fm)
	eng.SetMemoryEmbedder(subFloorEmbedder{})

	ctx := withRetrievalHitsSink(context.Background())
	block := eng.buildLongTermMemoryBlock(ctx, "", "公司年假制度是怎么规定的")
	if strings.Contains(block, "花生") {
		t.Fatalf("地板下噪音（hybrid 0.380）不得被放宽重试复活：%q", block)
	}
	if _, mem := retrievalHitsFrom(ctx).snapshot(); len(mem) != 0 {
		t.Fatalf("地板下噪音不得产生记忆命中，got %d", len(mem))
	}
}
