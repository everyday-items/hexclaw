package memory

import (
	"strings"
	"sync"
	"testing"
)

func newFM(t *testing.T) *FileMemory {
	t.Helper()
	fm, err := New(Options{Dir: t.TempDir(), MaxMemory: 2000})
	if err != nil {
		t.Fatalf("建 FileMemory: %v", err)
	}
	return fm
}

// 单写原子入口：insert / 完全重复 discard / 近义改写 update 就地（增量 4 收口增量 3 的 TOCTOU）。
func TestUpsert_InsertUpdateDiscard(t *testing.T) {
	fm := newFM(t)
	if a, _ := fm.UpsertEntryForRole("用户喜欢深色主题界面", "fact", "chat_extract", ""); a != "insert" {
		t.Fatalf("首条应 insert，得 %s", a)
	}
	if a, _ := fm.UpsertEntryForRole("用户喜欢深色主题界面", "fact", "chat_extract", ""); a != "discard" {
		t.Fatalf("完全重复应 discard，得 %s", a)
	}
	if a, _ := fm.UpsertEntryForRole("用户喜欢简洁的代码风格", "fact", "chat_extract", ""); a != "insert" {
		t.Fatalf("新事实应 insert，得 %s", a)
	}
	if a, _ := fm.UpsertEntryForRole("用户喜欢简洁的代码命名风格", "fact", "chat_extract", ""); a != "update" {
		t.Fatalf("近义改写应 update 就地，得 %s", a)
	}
	entries := fm.ParseEntries()
	if len(entries) != 2 {
		t.Fatalf("应有 2 条（主题 + 被就地更新的代码风格），得 %d", len(entries))
	}
}

// rule/identity 等全局类型强制落 _global（跨角色、常驻保证带），即便传了 role。
func TestUpsert_GlobalRouting(t *testing.T) {
	fm := newFM(t)
	if _, err := fm.UpsertEntryForRole("务必用简体中文回复", "rule", "manual", "hexbot"); err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	if g := fm.GetMemory(); !strings.Contains(g, "简体中文") {
		t.Fatalf("rule 应落 _global，GetMemory 未含: %q", g)
	}
}

// 并发单写：多 goroutine 同时 upsert 互异事实 —— 写锁串行化，全部落盘无丢失、无 race。
// 配合 `go test -race` 验证坑A（多写入者并发盲覆盖）已收口。
func TestUpsert_ConcurrentNoRace(t *testing.T) {
	fm := newFM(t)
	facts := []string{
		"用户叫小明", "用户住在北京", "用户喜欢喝咖啡", "用户养了一只橘猫",
		"项目用 Go 语言", "前端框架是 Vue", "数据库选 PostgreSQL", "部署在 Kubernetes",
		"用户时区 UTC+8", "团队规模五人", "发布周期两周", "代码托管在 GitLab",
	}
	var wg sync.WaitGroup
	for _, f := range facts {
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			_, _ = fm.UpsertEntryForRole(c, "fact", "chat_extract", "")
		}(f)
	}
	wg.Wait()

	if n := len(fm.ParseEntries()); n != len(facts) {
		t.Fatalf("%d 条互异事实并发写入应全部落盘，得 %d（疑似并发丢失）", len(facts), n)
	}
}
