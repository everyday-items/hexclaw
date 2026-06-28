package recall

import (
	"testing"
	"time"
)

func TestSelectResident_OnlyResidentAndValid(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	yesterday := now.AddDate(0, 0, -1)
	entries := []Entry{
		{ID: "rule", Type: TypeRule, Content: "务必用中文回复", AccessedAt: now},
		{ID: "fact", Type: TypeFact, Content: "某检索事实", AccessedAt: now},                  // 非常驻
		{ID: "expired", Type: TypeIdentity, Content: "旧身份", ValidTo: &yesterday},         // 已失效
		{ID: "pinfact", Type: TypeFact, Pinned: true, Content: "钉住的事实", AccessedAt: now}, // pinned 强制常驻
	}
	got, overflow := SelectResident(entries, now, 0)
	if overflow != 0 {
		t.Fatalf("不限预算不应溢出，得 %d", overflow)
	}
	gotIDs := map[string]bool{}
	for _, e := range got {
		gotIDs[e.ID] = true
	}
	if !gotIDs["rule"] || !gotIDs["pinfact"] {
		t.Fatalf("rule 与 pinned 事实应入常驻，得 %v", gotIDs)
	}
	if gotIDs["fact"] || gotIDs["expired"] {
		t.Fatalf("非常驻/失效条目不应入常驻，得 %v", gotIDs)
	}
}

func TestSelectResident_PinnedFirstThenImportance(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{ID: "pref", Type: TypePreference, Content: "偏好简洁", AccessedAt: now},
		{ID: "rule", Type: TypeRule, Content: "硬规则", AccessedAt: now},
		{ID: "pinned", Type: TypePreference, Pinned: true, Content: "钉住偏好", AccessedAt: now},
	}
	got, _ := SelectResident(entries, now, 0)
	if got[0].ID != "pinned" {
		t.Fatalf("pinned 应排第一，得 %v", got[0].ID)
	}
	// rule 先验(1.0) > preference(0.6)，rule 应在非 pinned 中靠前。
	if got[1].ID != "rule" {
		t.Fatalf("rule 应在 preference 之前，得 %v", got[1].ID)
	}
}

func TestSelectResident_BudgetOverflow(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	entries := []Entry{
		{ID: "a", Type: TypeRule, Content: "一二三四五", AccessedAt: now},       // 5 runes
		{ID: "b", Type: TypePreference, Content: "六七八九十", AccessedAt: now}, // 5 runes
	}
	// 预算只够第一条（含 +1 开销 → 6）。
	got, overflow := SelectResident(entries, now, 6)
	if len(got) != 1 || overflow != 1 {
		t.Fatalf("预算应只容 1 条、溢出 1 条，得 selected=%d overflow=%d", len(got), overflow)
	}
	// 至少保留最高优先一条（rule a）。
	if got[0].ID != "a" {
		t.Fatalf("应保留最高优先 rule，得 %v", got[0].ID)
	}
}
