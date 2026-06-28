package memory

import (
	"strings"
	"testing"
)

// 缺陷H 修复回归锁：稳定内容寻址 ID 取代「行号即 ID」。
// 不变量：一旦拿到某条目的 ID，删/改/移动它**上方**的条目（evict/反思/做梦默认开都会这样移行）后，
// 用该 ID 做的任何变更仍精确命中原条目，绝不漂移到错条目/落空。

func sidFM(t *testing.T) *FileMemory {
	t.Helper()
	fm, err := New(Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 200})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return fm
}

func sidIDOf(ents []MemoryEntry, sub string) string {
	for _, e := range ents {
		if strings.Contains(e.Content, sub) {
			return e.ID
		}
	}
	return ""
}

func sidEntryOf(ents []MemoryEntry, sub string) *MemoryEntry {
	for i := range ents {
		if strings.Contains(ents[i].Content, sub) {
			return &ents[i]
		}
	}
	return nil
}

// 每条单行新条目都铸得稳定 ID（前缀 e、与行号 ID 形态正交）。
func TestStableID_EveryEntryGetsDistinctStableID(t *testing.T) {
	fm := sidFM(t)
	for _, c := range []string{"事实甲", "事实乙", "事实丙"} {
		if err := fm.SaveEntryForRole(c, "fact", "manual", ""); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	seen := map[string]bool{}
	for _, e := range fm.ParseEntries() {
		if !strings.HasPrefix(e.ID, "e") {
			t.Fatalf("条目 %q 应有稳定 ID（e 前缀），得 %q", e.Content, e.ID)
		}
		if looksLikeLineID(e.ID) {
			t.Fatalf("稳定 ID 不应被误判为行号 ID：%q", e.ID)
		}
		if seen[e.ID] {
			t.Fatalf("稳定 ID 应唯一，重复：%q", e.ID)
		}
		seen[e.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("应 3 个不同稳定 ID，得 %d", len(seen))
	}
}

// pin / bump / update / delete 四类变更，在「删上方条目致行号上移」后仍精确命中目标条，且不误伤旁条。
func TestStableID_MutationsHitTargetAfterLineShift(t *testing.T) {
	fm := sidFM(t)
	for _, c := range []string{"AAA-顶部", "BBB-次", "CCC-目标", "DDD-旁条", "EEE-末尾"} {
		if err := fm.SaveEntryForRole(c, "fact", "manual", ""); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	idTarget := sidIDOf(fm.ParseEntries(), "CCC-目标")
	idDDD := sidIDOf(fm.ParseEntries(), "DDD-旁条")
	if idTarget == "" || !strings.HasPrefix(idTarget, "e") {
		t.Fatalf("目标条应有稳定 ID，得 %q", idTarget)
	}

	// 制造行号漂移：删 CCC 上方的两条 → CCC/DDD/EEE 行号整体上移。
	if err := fm.DeleteEntry(sidIDOf(fm.ParseEntries(), "AAA-顶部")); err != nil {
		t.Fatalf("del AAA: %v", err)
	}
	if err := fm.DeleteEntry(sidIDOf(fm.ParseEntries(), "BBB-次")); err != nil {
		t.Fatalf("del BBB: %v", err)
	}

	// 用最初捕获的（现已"过期行号"对应的）稳定 ID 做各种变更：
	if err := fm.SetPinned(idTarget, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	fm.BumpHitCount([]string{idTarget})
	if err := fm.UpdateEntry(idTarget, "CCC-目标-已更新"); err != nil {
		t.Fatalf("update: %v", err)
	}

	got := sidEntryOf(fm.ParseEntries(), "CCC-目标")
	if got == nil {
		t.Fatalf("目标条 CCC 丢失，mem=%q", fm.GetMemory())
	}
	if got.ID != idTarget {
		t.Fatalf("内容更新后稳定 ID 应不变：want %q got %q", idTarget, got.ID)
	}
	if !got.Pinned {
		t.Fatalf("SetPinned 未命中 CCC（打到错条目/落空）")
	}
	if got.HitCount < 1 {
		t.Fatalf("BumpHitCount 未命中 CCC，HitCount=%d", got.HitCount)
	}
	if !strings.Contains(got.Content, "已更新") {
		t.Fatalf("UpdateEntry 未命中 CCC：%q", got.Content)
	}
	// 旁条 DDD 不应被任何变更误伤。
	ddd := sidEntryOf(fm.ParseEntries(), "DDD-旁条")
	if ddd == nil {
		t.Fatalf("旁条 DDD 不应消失")
	}
	if ddd.Pinned || ddd.HitCount > 0 || ddd.ID != idDDD {
		t.Fatalf("变更误伤旁条 DDD：pinned=%v hit=%d id(%q→%q)", ddd.Pinned, ddd.HitCount, idDDD, ddd.ID)
	}

	// 最终用稳定 ID 删目标条，精确命中。
	if err := fm.DeleteEntry(idTarget); err != nil {
		t.Fatalf("del target: %v", err)
	}
	if strings.Contains(fm.GetMemory(), "CCC-目标") {
		t.Fatalf("DeleteEntry 未命中 CCC：%q", fm.GetMemory())
	}
	if !strings.Contains(fm.GetMemory(), "DDD-旁条") {
		t.Fatalf("删目标条误删旁条 DDD：%q", fm.GetMemory())
	}
}

// 稳定 ID 跨「归档 → 恢复」往返保持不变（归档移动整块、eid 标签随行迁移）。
func TestStableID_PreservedThroughArchiveRestore(t *testing.T) {
	fm := sidFM(t)
	if err := fm.SaveEntryForRole("可归档可恢复的事实", "fact", "manual", ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	id0 := sidIDOf(fm.ParseEntries(), "可归档可恢复")
	if id0 == "" {
		t.Fatalf("应有稳定 ID")
	}
	// 用稳定 ID 归档。
	if err := fm.ArchiveEntry(id0); err != nil {
		t.Fatalf("archive by stable id: %v", err)
	}
	arch, err := fm.ListEntries(ListOptions{View: MemoryViewArchived, Limit: 10})
	if err != nil {
		t.Fatalf("list archived: %v", err)
	}
	if len(arch.Entries) != 1 || arch.Entries[0].ID != id0 {
		t.Fatalf("归档后稳定 ID 应不变：want %q got %+v", id0, arch.Entries)
	}
	// 用同一稳定 ID 恢复（归档态由 resolver 识别为 archive 文件）。
	if err := fm.RestoreEntry(id0); err != nil {
		t.Fatalf("restore by stable id: %v", err)
	}
	active := fm.ParseEntries()
	if len(active) != 1 || active[0].ID != id0 {
		t.Fatalf("恢复后稳定 ID 应不变：want %q got %+v", id0, active)
	}
}

// 向后兼容：旧式行号 ID（无内联 eid 的历史文件）仍按行号解析、可删/可改。
func TestStableID_LegacyLineIDStillResolves(t *testing.T) {
	fm := sidFM(t)
	// 直接写入一份「旧格式」文件：无 eid 标签。
	dir := fm.roleDir("")
	if err := fm.UpdateMemory(""); err != nil { // 确保目录存在
		t.Fatalf("init: %v", err)
	}
	raw := "\n- [09:00] [fact:manual] 旧格式条目一\n\n- [09:01] [fact:manual] 旧格式条目二\n"
	if err := atomicWriteFile(dir+"/"+memoryActiveFile, []byte(raw), 0644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	ents := fm.ParseEntries()
	if len(ents) != 2 {
		t.Fatalf("应解析 2 条旧格式，得 %d", len(ents))
	}
	// 旧条目无 eid → 回退行号 ID（m-N）。
	for _, e := range ents {
		if !looksLikeLineID(e.ID) {
			t.Fatalf("旧格式条目应回退行号 ID，得 %q", e.ID)
		}
	}
	// 用行号 ID 删第二条仍可命中（兼容路径）。
	id2 := sidIDOf(ents, "条目二")
	if err := fm.DeleteEntry(id2); err != nil {
		t.Fatalf("delete legacy: %v", err)
	}
	if strings.Contains(fm.GetMemory(), "条目二") {
		t.Fatalf("行号 ID 删旧格式条目失败：%q", fm.GetMemory())
	}
	if !strings.Contains(fm.GetMemory(), "条目一") {
		t.Fatalf("误删了条目一：%q", fm.GetMemory())
	}
}

// 边界回归（审计 fan-out 揪出）：dedup-update 把「单行+稳定ID」条目就地整合为多行正文时，
// 内联 meta 标签绝不能落到续行——否则它违反「eid 仅单行」不变量，后续 BumpHitCount/SetPinned
// 改首行 → 双标签 → 正文被存储层标签污染（泄漏给 LLM/展示）。
func TestStableID_MultilineUpdateDoesNotLeakMetaTag(t *testing.T) {
	fm := sidFM(t)
	if err := fm.SaveEntryForRole("用户喜欢喝美式咖啡", "fact", "manual", ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	id := sidIDOf(fm.ParseEntries(), "美式咖啡")
	if id == "" || !strings.HasPrefix(id, "e") {
		t.Fatalf("前置：应有稳定 ID，得 %q", id)
	}

	// 走单写器就地整合为多行正文（dedup ActionUpdate 真实路径：rewriteEntryContentMetaUnlocked）。
	fm.mu.Lock()
	preserved := fm.readEntryMetaUnlocked(id) // 含 eid
	err := fm.rewriteEntryContentMetaUnlocked(id, "用户喜欢喝美式咖啡\n而且不加糖\n每天三杯", preserved)
	fm.mu.Unlock()
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	// 再召回触发 BumpHitCount（命中即自增）——这是首行改写，会和续行标签冲突。
	fm.BumpHitCount([]string{sidIDOf(fm.ParseEntries(), "美式咖啡")})

	// 不变量：任何条目正文都不得残留存储层 meta 标签。
	for _, e := range fm.ParseEntries() {
		if strings.Contains(e.Content, entryMetaPrefix) {
			t.Fatalf("🔴多行 update+bump 后正文泄漏 meta 标签：%q", e.Content)
		}
	}
	if strings.Count(fm.GetMemory(), entryMetaPrefix) > 1 {
		t.Fatalf("🔴出现双 meta 标签（首行+续行冲突）：%q", fm.GetMemory())
	}
}

// 多行条目的 meta 改写（SetPinned/BumpHitCount）必须落块尾正文行：既要生效、又不污染正文。
// 用户场景：把一条多行手记置顶。
func TestStableID_MultilineEntryPinAndBumpSafe(t *testing.T) {
	fm := sidFM(t)
	if err := fm.SaveEntryForRole("会议纪要：\n- 周三发版\n- 数据库要扩容", "fact", "manual", ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	id := sidIDOf(fm.ParseEntries(), "会议纪要")
	if id == "" {
		t.Fatalf("前置：未找到多行条目")
	}
	if err := fm.SetPinned(id, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	fm.BumpHitCount([]string{sidIDOf(fm.ParseEntries(), "会议纪要")})

	got := sidEntryOf(fm.ParseEntries(), "会议纪要")
	if got == nil {
		t.Fatalf("多行条目丢失：%q", fm.GetMemory())
	}
	if !got.Pinned {
		t.Fatalf("🔴 SetPinned 对多行条目未生效（标签落首行被 parse 当正文）")
	}
	if got.HitCount < 1 {
		t.Fatalf("🔴 BumpHitCount 对多行条目未生效，HitCount=%d", got.HitCount)
	}
	if strings.Contains(got.Content, entryMetaPrefix) {
		t.Fatalf("🔴多行条目正文泄漏 meta 标签：%q", got.Content)
	}
	// 多行正文三行齐全。
	for _, frag := range []string{"会议纪要", "周三发版", "数据库要扩容"} {
		if !strings.Contains(got.Content, frag) {
			t.Fatalf("多行正文缺片段 %q：%q", frag, got.Content)
		}
	}
}

// GetMemoryForPrompt：注入 LLM 抽取提示的「现有记忆」必须剥掉存储层内联 meta 标签
// （eid/pin/时序锚对模型是噪声且白耗 token），但保留可读正文与 时间/类型 前缀。
func TestStableID_GetMemoryForPromptStripsMetaTags(t *testing.T) {
	fm := sidFM(t)
	for _, c := range []string{"用户住在上海", "用户偏好简洁回答", "用户对花生过敏"} {
		if err := fm.SaveEntryForRole(c, "fact", "manual", ""); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	// 裸 GetMemory 含内联标签（存储真相）。
	if !strings.Contains(fm.GetMemory(), entryMetaPrefix) {
		t.Fatalf("前置：GetMemory 应含内联 eid 标签（每条新条目都铸了 ID）")
	}
	// 注入提示用的渲染必须剥净标签、保留正文。
	forPrompt := fm.GetMemoryForPrompt()
	if strings.Contains(forPrompt, entryMetaPrefix) {
		t.Fatalf("🔴 GetMemoryForPrompt 仍含存储层 meta 标签（噪声/token 浪费注入模型）：%q", forPrompt)
	}
	for _, frag := range []string{"上海", "简洁回答", "花生过敏"} {
		if !strings.Contains(forPrompt, frag) {
			t.Fatalf("剥标签后正文缺片段 %q：%q", frag, forPrompt)
		}
	}
}

// looksLikeLineID 形态判定：行号 ID 命中、稳定 ID / 杂串不命中。
func TestStableID_LooksLikeLineID(t *testing.T) {
	line := []string{"m-0", "m-7", "a-12", "r-3"}
	stable := []string{"e1a2b3c4d5e6f708", "e0", "", "m-", "m-x", "mm-1", "x-1", "e-1"}
	for _, s := range line {
		if !looksLikeLineID(s) {
			t.Errorf("%q 应判为行号 ID", s)
		}
	}
	for _, s := range stable {
		if looksLikeLineID(s) {
			t.Errorf("%q 不应判为行号 ID", s)
		}
	}
}
