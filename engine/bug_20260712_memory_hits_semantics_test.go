package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/memory"
)

// BUG-20260712-L（engine 层，真机取证）：问「1+1=2?」，前端渲染「记忆命中：大大阿达大大阿达」。
//
// 两处语义缺陷叠加：
//
//	① rankFacts 的空结果兜底 `return facts`——检索层全无相关时把**全部** facts 原样注入并
//	   记成命中（注入是增强，空就该是空；配合 recall 零证据剔除后，空结果=真无相关）。
//	② buildLongTermMemoryBlock 把**常驻层**（pinned/rule/preference——恒注入，与 query 无关）
//	   也灌进 recordMemoryHits——「记忆命中」语义=按相关性召回；恒注入不是命中，
//	   pinned 的「大大阿达」对着 1+1 显示命中卡纯属误导。
//
// 契约：
//   - 零相关 query：检索层不注入、命中 sink 为空（常驻层照常注入进 prompt，但不打命中标签）。
//   - 相关 query：检索层照常注入 + 打命中标签（不失聪）。
func TestBug20260712_IrrelevantQuery_NoMemoryHits(t *testing.T) {
	fm := adfFM(t)
	// pinned 垃圾（真机形态：type=fact + pinned → 常驻层恒注入）
	adfMust(t, fm.SaveStructuredEntry("大大阿达大大阿达", "fact", "manual", "", memory.EntryMeta{Pinned: true}))
	// 非 pinned 无关事实（检索层）
	adfMust(t, fm.SaveStructuredEntry("用户喜欢深色主题", "fact", "manual", "", memory.EntryMeta{}))
	eng := adfEng(t, fm)

	ctx := withRetrievalHitsSink(context.Background())
	block := eng.buildLongTermMemoryBlock(ctx, "", "1+1=2?")

	// 常驻层（pinned）恒注入是设计——prompt 里可以有
	if !strings.Contains(block, "大大阿达") {
		t.Fatalf("pinned 常驻条目应恒注入（用户显式意图），block=%q", block)
	}
	// 检索层零相关不得注入
	if strings.Contains(block, "深色主题") {
		t.Fatalf("零相关检索层事实不得注入（rankFacts 空结果兜底泄漏），block=%q", block)
	}
	// 命中 sink 必须为空：常驻≠命中、零相关≠命中（前端不得渲染「记忆命中」卡）
	_, memHits := retrievalHitsFrom(ctx).snapshot()
	if len(memHits) != 0 {
		t.Fatalf("零相关 query 不得产生记忆命中（got %d 条，首条=%q）", len(memHits), memHits[0].Content)
	}
}

func TestBug20260712_RelevantQuery_HitsKeepWorking(t *testing.T) {
	fm := adfFM(t)
	adfMust(t, fm.SaveStructuredEntry("用户最喜欢的语言是 Rust", "fact", "manual", "", memory.EntryMeta{}))
	eng := adfEng(t, fm)

	ctx := withRetrievalHitsSink(context.Background())
	block := eng.buildLongTermMemoryBlock(ctx, "", "Rust 语言怎么样")

	if !strings.Contains(block, "Rust") {
		t.Fatalf("相关事实应照常注入（fail-closed 不等于失聪），block=%q", block)
	}
	_, memHits := retrievalHitsFrom(ctx).snapshot()
	if len(memHits) == 0 {
		t.Fatalf("相关召回应照常产生记忆命中")
	}
}
