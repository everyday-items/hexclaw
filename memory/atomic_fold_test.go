package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 缺陷C 回归锁：atomicWriteFile 正确覆盖、且不留临时文件残骸（temp+rename 落地）。
func TestAtomicWriteFile_OverwritesAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "MEMORY.md")
	if err := atomicWriteFile(path, []byte("v1-内容"), 0644); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if err := atomicWriteFile(path, []byte("v2-新内容"), 0644); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "v2-新内容" {
		t.Fatalf("应为最新内容，得 %q", got)
	}
	// 不留 .tmp-* 残骸（rename 成功即消失）。
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("遗留临时文件 %q（temp+rename 应清理）", e.Name())
		}
	}
}

// 缺陷E 回归锁：深相折叠的落盘失败 → 活跃文件原文不动（事务化 = all-or-nothing，无「失效但无替代」中间态）。
func TestDeepReflect_FoldAtomic_FailureLeavesOriginalIntact(t *testing.T) {
	fm := newFM(t)
	fake := &fakeConsolidator{reply: "用户喜欢喝各类咖啡（美式/拿铁/卡布奇诺）"}
	fm.WithConsolidator(fake).WithDreamOptions(foldableOpts())
	for _, c := range []string{"用户喜欢喝美式咖啡", "用户喜欢喝拿铁咖啡", "用户喜欢喝卡布奇诺咖啡"} {
		if err := fm.SaveStructuredEntry(c, "fact", "manual", "", EntryMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	before := fm.GetMemory()

	// 注入落盘失败：把记忆目录设只读 → atomicWriteFile 的 temp 创建/rename 失败。
	dir := fm.roleDir("")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skipf("chmod 不支持（容器/FS 限制）：%v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()

	_, _ = fm.DeepReflectRole(context.Background(), "", time.Now()) // 期望写失败

	_ = os.Chmod(dir, 0o755) // 恢复可读写再核对
	after := fm.GetMemory()
	if after != before {
		t.Fatalf("🔴缺陷E：折叠落盘失败本应保持原文不动(事务化)，却出现部分修改——\nbefore=%q\nafter=%q", before, after)
	}
	// 进一步：没有任何原条被标 ValidTo（失效）却无整合替代。
	if n := countWithValidTo(fm.ParseEntries()); n != 0 {
		t.Fatalf("🔴缺陷E：写失败后仍有 %d 条被标失效（无替代）= 非事务残留", n)
	}
}
