package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 记忆系统设计缺陷取证（RED：未修改代码上跑、确认失败、失败信息反映症状）。修复后转 GREEN 作回归锁。

func adfFMMax(t *testing.T, max int) *FileMemory {
	t.Helper()
	fm, err := New(Options{Enabled: true, Dir: t.TempDir(), MaxMemory: max})
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
func adfIDOf(ents []MemoryEntry, sub string) string {
	for _, e := range ents {
		if strings.Contains(e.Content, sub) {
			return e.ID
		}
	}
	return ""
}

// 缺陷 B（归档无界）→ 修复锁：归档裁剪到 ArchiveMax，不再随 churn 单调增长（373MB 类）。
func TestAuditDesignFlaw_B_ArchiveBoundedByArchiveMax(t *testing.T) {
	const archiveMax = 10
	fm, err := New(Options{Enabled: true, Dir: t.TempDir(), MaxMemory: 5, ArchiveMax: archiveMax})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range 60 { // 活跃≤5 → ~55 条本应进归档；旧实现归档累积 55 永不裁剪
		adfMust(t, fm.SaveEntryForRole(fmt.Sprintf("琐碎事实编号-%02d", i), "fact", "manual", ""))
	}
	archived := fm.parseEntriesFromFile(fm.roleDir(""), memoryArchiveFile, MemoryStatusArchived, "a")
	if len(archived) > archiveMax {
		t.Fatalf("🔴缺陷B：归档应被裁剪到 ArchiveMax=%d，却累积 %d 条（未封顶=无界增长）", archiveMax, len(archived))
	}
	if len(archived) == 0 {
		t.Fatalf("前置：应有条目进归档（裁剪过度？）")
	}
}

// 缺陷 H（行号即 ID，跨「并发」写失效）：删上方条目使行号漂移 → 用过期 ID 删/改打到错条目。
// ★已确证存在（见报告 §12）。修复=稳定内容寻址 ID 替换行号 ID，触及全套 ID 解析 + 6 处变更点，
// 属独立重构；本轮先交付其余 9 项 + 闭环，H 列为专项跟进（避免在核心写路径上仓促大改）。
func TestAuditDesignFlaw_H_LineIndexIDStaleAfterDeleteAbove(t *testing.T) {
	fm := adfFMMax(t, 200)
	adfMust(t, fm.SaveEntryForRole("AAA-第一条", "fact", "manual", ""))
	adfMust(t, fm.SaveEntryForRole("BBB-第二条", "fact", "manual", ""))
	adfMust(t, fm.SaveEntryForRole("CCC-第三条", "fact", "manual", ""))

	ents := fm.ParseEntries()
	idA := adfIDOf(ents, "AAA")
	idC := adfIDOf(ents, "CCC") // 捕获 C 的【当前】行号 ID（UI/工具拿到后会过期）

	// 「并发」写：删 A（在 C 之上）→ B、C 行号整体上移。默认开的 evict/反思/做梦都会这样移行。
	adfMust(t, fm.DeleteEntry(idA))

	// 用户/工具随后用 C 的【过期】行号 ID 删 C：
	_ = fm.DeleteEntry(idC)

	if strings.Contains(fm.GetMemory(), "CCC") {
		t.Fatalf("🔴缺陷H：用过期行号 ID(%s) 删 C，C 仍在——删 A 后行号漂移，过期 ID 打到错条目/落空。ID=行号是不稳定标识。mem=%q", idC, fm.GetMemory())
	}
}

// 缺陷 I（dedup Update 覆盖正文 + 丢 meta）：近义改写经单写器触发 Update → 置顶位被静默清掉。
func TestAuditDesignFlaw_I_DedupUpdateDropsPin(t *testing.T) {
	fm := adfFMMax(t, 200)
	// 置顶一条关键 fact（无 subject，走纯 dedup 而非 supersede）。
	adfMust(t, fm.SaveStructuredEntry("用户对青霉素过敏务必牢记", "fact", "manual", "", EntryMeta{Pinned: true}))

	// 近义改写经单写器 upsert → 触发就地 Update。
	act, err := fm.UpsertStructuredEntryForRole("用户对青霉素过敏务必牢记。", "fact", "chat_extract", "", "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	pinnedStill := false
	for _, e := range fm.ParseEntries() {
		if strings.Contains(e.Content, "青霉素") && e.Pinned {
			pinnedStill = true
		}
	}
	if !pinnedStill {
		t.Fatalf("🔴缺陷I：近义 upsert(act=%q) 触发 Update 就地覆盖→置顶位被静默丢弃（updateEntryUnlocked 不保 meta），关键记忆从常驻降级", act)
	}
}

// 缺陷 J（PII 守卫未下沉到单写器）：凭证经单写器/做梦/画像路径可绕过守卫落库。
func TestAuditDesignFlaw_J_PIIGuardMissingOnSingleWriter(t *testing.T) {
	cred := "我的登录密码是 Hunter2SecretPass"
	if !LooksSensitive(cred) {
		t.Fatalf("前置：该串应被 LooksSensitive 判敏感")
	}
	fm := adfFMMax(t, 200)
	// 单写器（做梦合成/画像/系统写共用的落盘入口）不过 PII 守卫。
	if _, err := fm.UpsertStructuredEntryForRole(cred, "fact", "system", "", ""); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if strings.Contains(fm.GetMemory(), "Hunter2SecretPass") {
		t.Fatalf("🔴缺陷J：凭证经单写器 UpsertStructuredEntryForRole 直接落库——PII 守卫(LooksSensitive)只挂在抽取/工具路径，未下沉到单写器/做梦/画像（pii.go 自称「供所有写入路径复用」名不副实）")
	}
}

// 缺陷 D（合成相持久化未校验的 LLM 输出）：画像蒸馏把拒答原样落为「每轮必注入」的画像。
func TestAuditDesignFlaw_D_ProfilePersistsRefusal(t *testing.T) {
	fm := adfFMMax(t, 200)
	adfMust(t, fm.SaveStructuredEntry("用户名叫小明", "identity", "manual", "", EntryMeta{}))
	adfMust(t, fm.SaveStructuredEntry("用户是 Go 后端工程师", "fact", "manual", "", EntryMeta{}))
	adfMust(t, fm.SaveStructuredEntry("用户偏好简洁回答", "preference", "manual", "", EntryMeta{}))

	// 合成器返回「拒答」（弱模型/长 prompt 常见）。
	refusal := &recordingSyn{out: "抱歉，我无法根据所提供的信息生成用户画像。"}
	act, err := fm.DistillProfileForRole(context.Background(), "", refusal, DistillProfileConfig{MinFacts: 3}, time.Now())
	if err != nil {
		t.Fatalf("distill: %v", err)
	}
	if strings.Contains(fm.GetMemory(), "无法根据所提供的信息生成") {
		t.Fatalf("🔴缺陷D：画像蒸馏把 LLM 拒答原样落库为「每轮必注入」的 Pinned 画像(act=%q)——只查 err/空/==prev，无拒答/长度/接地校验", act)
	}
}
