package memory

import (
	"strings"
	"testing"
)

// 评审findings A/B 的回归测试（修复后应 GREEN）。范围：单用户桌面（多 Agent）。
// 修复：淘汰统一到 recall.Score（单一真相源、含 salience/recency）+ 硬保 Pinned/rule/identity/instruction。

// ── A：用户显式 Pinned 的健康关键事实不再被淘汰（Pinned 进硬保护带）────────────────
func TestAuditA_EvictionProtectsPinnedAllergy(t *testing.T) {
	fm, err := New(Options{Dir: t.TempDir(), MaxMemory: 3})
	if err != nil {
		t.Fatal(err)
	}
	// 用户用 manage_memory 置顶的健康关键事实（source=ai_managed, Pinned=true）。
	if err := fm.SaveStructuredEntry("用户对青霉素严重过敏", "fact", "ai_managed", "", EntryMeta{Pinned: true}); err != nil {
		t.Fatal(err)
	}
	// 之后日常对话自动抽取的琐碎事实，制造淘汰压力。
	for _, c := range []string{"用户今天喝了奶茶", "用户喜欢看科幻片", "用户周末去了商场", "用户买了新手机"} {
		if err := fm.SaveEntryForRole(c, "fact", "chat_extract", ""); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(fm.GetMemory(), "青霉素") {
		t.Fatalf("🔴 BUG-A 未修复：Pinned 健康事实仍被淘汰\n%s", fm.GetMemory())
	}
}

// ── A2：rule 硬规则不再被淘汰（rule 进硬保护带）──────────────────────────────────
func TestAuditA2_EvictionProtectsRule(t *testing.T) {
	fm, _ := New(Options{Dir: t.TempDir(), MaxMemory: 3})
	_ = fm.SaveStructuredEntry("永远用简体中文回复", "rule", "manual", "", EntryMeta{})
	for _, c := range []string{"用户喜欢咖啡", "用户用 MacBook", "用户在杭州", "用户养猫"} {
		_ = fm.SaveEntryForRole(c, "fact", "manual", "")
	}
	hasRule := false
	for _, e := range fm.ParseEntries() {
		if e.Type == "rule" {
			hasRule = true
		}
	}
	if !hasRule {
		t.Fatalf("🔴 BUG-A 未修复：rule 硬规则被淘汰\n%s", fm.GetMemory())
	}
}

// ── B：淘汰尊重召回重要性——高显著性事实在压力下存活，优于普通事实（两套打分已统一）──
func TestAuditB_EvictionRespectsRecallImportance(t *testing.T) {
	fm, _ := New(Options{Dir: t.TempDir(), MaxMemory: 3})
	_ = fm.SaveEntryForRole("用户对花生严重过敏", "fact", "chat_extract", "") // 高 salience（含「过敏」）
	for _, c := range []string{"用户喜欢蓝色", "用户喜欢绿色", "用户喜欢红色"} {
		_ = fm.SaveEntryForRole(c, "fact", "chat_extract", "") // 普通事实
	}
	if !strings.Contains(fm.GetMemory(), "过敏") {
		t.Fatalf("🔴 BUG-B 未修复：高显著性「过敏」应在淘汰压力下存活（recall.Score 含 salience），却被淘汰\n%s", fm.GetMemory())
	}
}
