package memory

import (
	"strings"
	"testing"
	"time"
)

// currentlyValid 过滤出「当前有效」条目（ValidTo 为空或在未来）—— 镜像 recall.IsCurrentlyValid，
// 但仅用字符串字段，供存储层测试断言 G3 留史语义（旧条留盘但不再有效）。
func currentlyValid(entries []MemoryEntry, now time.Time) []MemoryEntry {
	var out []MemoryEntry
	for _, e := range entries {
		if e.ValidTo == "" {
			out = append(out, e)
			continue
		}
		t, err := time.Parse(time.RFC3339, e.ValidTo)
		if err != nil || now.Before(t) {
			out = append(out, e)
		}
	}
	return out
}

func findByContent(entries []MemoryEntry, substr string) *MemoryEntry {
	for i := range entries {
		if strings.Contains(entries[i].Content, substr) {
			return &entries[i]
		}
	}
	return nil
}

// B-1 单元：内联 meta 原地改写保留正文/前缀、只换 meta，且行数不变（行号 ID 稳定）。
func TestRewriteFirstLineMeta_PreservesContentAndPrefix(t *testing.T) {
	const line = "- [09:30] [fact:chat_extract] 用户住在北京"
	got := rewriteFirstLineMeta(line, func(m EntryMeta) EntryMeta {
		m.ValidTo = "2026-06-26T10:00:00Z"
		m.Subject = "居住地"
		return m
	})
	if !strings.HasPrefix(got, "- [09:30] [fact:chat_extract] 用户住在北京") {
		t.Fatalf("正文/前缀应原样保留，得: %q", got)
	}
	pure, meta := splitEntryMeta(stripEntryPrefix(got))
	if pure != "用户住在北京" {
		t.Fatalf("剥 meta 后正文应为「用户住在北京」，得 %q", pure)
	}
	if meta.ValidTo != "2026-06-26T10:00:00Z" || meta.Subject != "居住地" {
		t.Fatalf("meta 未写入: %+v", meta)
	}
	// 再改一次（叠加 Supersedes）应保留既有字段。
	got2 := rewriteFirstLineMeta(got, func(m EntryMeta) EntryMeta {
		m.Supersedes = "m-3"
		return m
	})
	_, meta2 := splitEntryMeta(stripEntryPrefix(got2))
	if meta2.ValidTo == "" || meta2.Subject != "居住地" || meta2.Supersedes != "m-3" {
		t.Fatalf("叠加改写丢字段: %+v", meta2)
	}
}

// B-2 命脉：同主语异值 → supersede 留史。旧条留盘但失效，新条带 Supersedes/Subject/ValidFrom。
func TestUpsert_SupersedeKeepsHistory(t *testing.T) {
	fm := newFM(t)

	if a, err := fm.UpsertStructuredEntryForRole("用户住在北京", "fact", "chat_extract", "", "居住地"); err != nil || a != "insert" {
		t.Fatalf("首条应 insert，得 %s err=%v", a, err)
	}
	if a, err := fm.UpsertStructuredEntryForRole("用户住在上海", "fact", "chat_extract", "", "居住地"); err != nil || a != "supersede" {
		t.Fatalf("同主语异值应 supersede，得 %s err=%v", a, err)
	}

	all := fm.ParseEntries()
	if len(all) != 2 {
		t.Fatalf("留史：应有 2 条（北京失效 + 上海有效），得 %d: %+v", len(all), all)
	}
	now := time.Now()
	valid := currentlyValid(all, now)
	if len(valid) != 1 || !strings.Contains(valid[0].Content, "上海") {
		t.Fatalf("当前有效应仅「上海」，得 %+v", valid)
	}

	bj := findByContent(all, "北京")
	if bj == nil || bj.ValidTo == "" {
		t.Fatalf("旧条「北京」应留盘且标 ValidTo，得 %+v", bj)
	}
	sh := findByContent(all, "上海")
	if sh == nil || sh.Supersedes == "" || sh.Subject != "居住地" || sh.ValidFrom == "" {
		t.Fatalf("新条「上海」应带 Supersedes/Subject/ValidFrom，得 %+v", sh)
	}
}

// B-2 时效闭环：失效旧条不再参与去重比对 —— 重复取代不会反复命中历史条目；
// 同值重复 discard；再异值 supersede 命中的是「当前有效」条而非历史条。
func TestUpsert_SupersedeSkipsInvalidatedHistory(t *testing.T) {
	fm := newFM(t)
	mustAction := func(content, subj, want string) {
		t.Helper()
		a, err := fm.UpsertStructuredEntryForRole(content, "fact", "chat_extract", "", subj)
		if err != nil {
			t.Fatalf("upsert %q: %v", content, err)
		}
		if a != want {
			t.Fatalf("upsert %q 期望 %s，得 %s", content, want, a)
		}
	}
	mustAction("用户时区 UTC+8", "时区", "insert")
	mustAction("用户时区 UTC+9", "时区", "supersede") // UTC+8 失效
	mustAction("用户时区 UTC+9", "时区", "discard")   // 与当前有效 UTC+9 同值
	mustAction("用户时区 UTC+0", "时区", "supersede") // 取代当前有效 UTC+9（非历史 UTC+8）

	all := fm.ParseEntries()
	if v := currentlyValid(all, time.Now()); len(v) != 1 || !strings.Contains(v[0].Content, "UTC+0") {
		t.Fatalf("当前有效应仅 UTC+0，得 %+v", v)
	}
	if len(all) != 3 { // UTC+8、UTC+9、UTC+0 各留史一条
		t.Fatalf("应留史 3 条，得 %d: %+v", len(all), all)
	}
}

// 回归：无主语路径（既有抽取写链）行为不变 —— 永不 supersede，仅 insert/update/discard。
func TestUpsert_NoSubjectNeverSupersedes(t *testing.T) {
	fm := newFM(t)
	if a, _ := fm.UpsertEntryForRole("用户住在北京", "fact", "chat_extract", ""); a != "insert" {
		t.Fatalf("insert，得 %s", a)
	}
	// 无主语：即便语义矛盾也走 update 就地（不 supersede、不留史）。
	a, _ := fm.UpsertEntryForRole("用户住在上海", "fact", "chat_extract", "")
	if a == "supersede" {
		t.Fatalf("无主语不应 supersede，得 %s", a)
	}
}
