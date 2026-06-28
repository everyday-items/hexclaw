package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/memory"
	"github.com/hexagon-codes/hexclaw/memory/recall"
)

// 记忆系统设计缺陷取证（RED：在未修改的代码上跑、确认失败、失败信息反映缺陷具体症状）。
// 这些测试证明评审发现的 bug 真实存在；修复后即转 GREEN 作回归锁。

func adfFM(t *testing.T) *memory.FileMemory {
	t.Helper()
	fm, err := memory.New(memory.Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 200})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return fm
}
func adfMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("save: %v", err)
	}
}
func adfEng(t *testing.T, fm *memory.FileMemory) *ReActEngine {
	t.Helper()
	e := &ReActEngine{}
	e.SetFileMemory(fm)
	return e
}

// 缺陷 A（实时召回映射器丢 ValidTo）：被取代的失效旧值仍被注入 → 模型见矛盾。
func TestAuditDesignFlaw_A1_SupersededFactStillInjected(t *testing.T) {
	fm := adfFM(t)
	past := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	// 旧值已被取代（ValidTo 已设=失效留史，仍在活跃文件待反思归档）。
	adfMust(t, fm.SaveStructuredEntry("用户住在北京", "fact", "manual", "", memory.EntryMeta{Subject: "居住地", ValidTo: past}))
	// 新值有效。
	adfMust(t, fm.SaveStructuredEntry("用户住在上海", "fact", "manual", "", memory.EntryMeta{Subject: "居住地"}))

	block := adfEng(t, fm).buildLongTermMemoryBlock(context.Background(), "", "")

	if !strings.Contains(block, "上海") {
		t.Fatalf("前置：新值应被注入，block=%q", block)
	}
	if strings.Contains(block, "北京") {
		t.Fatalf("🔴缺陷A：被取代的失效旧值「北京」仍被实时注入——toRecallEntries 丢 ValidTo → IsCurrentlyValid 恒真 → 旧+新矛盾同时进上下文。block=%q", block)
	}
}

// 缺陷 A（实时召回映射器丢 Pinned）：置顶的关键 fact 未进常驻保证带 → 置顶失灵、可被相关性地板砍。
func TestAuditDesignFlaw_A2_PinnedFactNotResident(t *testing.T) {
	fm := adfFM(t)
	adfMust(t, fm.SaveStructuredEntry("用户对青霉素过敏", "fact", "manual", "", memory.EntryMeta{Pinned: true, Subject: "过敏"}))

	all := toRecallEntries(fm.ParseEntriesForRole(""))
	resident, _ := recall.SelectResident(all, time.Now(), residentBudgetRunes)

	found := false
	for _, e := range resident {
		if strings.Contains(e.Content, "青霉素") {
			found = true
		}
	}
	if !found {
		t.Fatalf("🔴缺陷A：置顶的关键 fact「青霉素过敏」未进常驻保证带——toRecallEntries 丢 Pinned → DerivedTier 非 resident → 置顶失灵、落入检索层可被地板砍。resident 条数=%d", len(resident))
	}
}

// 缺陷 F（HitCount 从不自增）：召回反馈环是死代码 → 重要度自校正/晋升(≥3)/做梦保护(≥3) 全失效。
func TestAuditDesignFlaw_F_RecallCountNeverIncremented(t *testing.T) {
	fm := adfFM(t)
	adfMust(t, fm.SaveStructuredEntry("用户最喜欢的语言是 Rust", "fact", "manual", "", memory.EntryMeta{}))
	eng := adfEng(t, fm)

	for range 3 { // 召回同一条 3 次
		_ = eng.buildLongTermMemoryBlock(context.Background(), "", "Rust 语言")
	}

	for _, e := range fm.ParseEntriesForRole("") {
		if strings.Contains(e.Content, "Rust") && e.HitCount == 0 {
			t.Fatalf("🔴缺陷F：召回 3 次后 HitCount 仍为 0——召回路径从不自增 → recallAlpha·RecallCount 恒 0、fact 晋升(≥3)/做梦热点保护(≥3) 永不触发（整个行为反馈环是死代码）")
		}
	}
}
