package memory

import (
	"strings"
	"testing"
)

// 存储格式地基（A）：FileMemory 行格式持久化 Pinned/Subject/ValidFrom/ValidTo/Supersedes/Confidence，
// 用 markdown 不可见的内联 meta 标签，向后兼容（旧条目无标签 → 字段取零值）。
// 解锁：时序取代留史（G3）、Pinned（manage_memory/UI）。

func TestStructuredEntry_RoundTrip(t *testing.T) {
	fm := newFM(t)
	meta := EntryMeta{
		Pinned:     true,
		Subject:    "居住地",
		ValidFrom:  "2026-06-25T10:00:00Z",
		Supersedes: "m-3",
		Confidence: 0.9,
	}
	if err := fm.SaveStructuredEntry("用户现在住上海", "fact", "ai_managed", "", meta); err != nil {
		t.Fatalf("SaveStructuredEntry: %v", err)
	}

	entries := fm.ParseEntries()
	var got *MemoryEntry
	for i := range entries {
		if strings.Contains(entries[i].Content, "住上海") {
			got = &entries[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("未解析到结构化条目，得 %+v", entries)
	}
	// 正文不含 meta 标签（标签被分离）。
	if strings.Contains(got.Content, "<!--m:") || strings.Contains(got.Content, "-->") {
		t.Errorf("正文不应残留 meta 标签：%q", got.Content)
	}
	if !got.Pinned {
		t.Errorf("Pinned 未持久化/还原")
	}
	if got.Subject != "居住地" {
		t.Errorf("Subject 未还原：%q", got.Subject)
	}
	if got.ValidFrom != "2026-06-25T10:00:00Z" {
		t.Errorf("ValidFrom 未还原：%q", got.ValidFrom)
	}
	if got.Supersedes != "m-3" {
		t.Errorf("Supersedes 未还原：%q", got.Supersedes)
	}
}

// 向后兼容：旧格式条目（无 meta 标签）解析后新字段为零值，正文不变。
func TestStructuredEntry_BackwardCompatible(t *testing.T) {
	fm := newFM(t)
	if err := fm.SaveEntryForRole("用户喜欢深色主题", "preference", "manual", ""); err != nil {
		t.Fatalf("SaveEntryForRole: %v", err)
	}
	entries := fm.ParseEntries()
	if len(entries) != 1 {
		t.Fatalf("应有 1 条，得 %d", len(entries))
	}
	e := entries[0]
	if e.Content != "用户喜欢深色主题" {
		t.Errorf("旧格式正文应原样，得 %q", e.Content)
	}
	if e.Pinned || e.Subject != "" || e.ValidTo != "" {
		t.Errorf("旧格式新字段应为零值，得 pinned=%v subj=%q vto=%q", e.Pinned, e.Subject, e.ValidTo)
	}
}
