package marketplace

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

// TestSeedFromFS_WritesAndIsIdempotent 验证首启 seed：新写入 + 已存在不覆盖（幂等）。
func TestSeedFromFS_WritesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	m := NewMarketplace(dir)

	fsys := fstest.MapFS{
		"skills/alpha.md": {Data: []byte("---\nname: alpha\n---\nA")},
		"skills/beta.md":  {Data: []byte("---\nname: beta\n---\nB")},
		"skills/note.txt": {Data: []byte("ignored")}, // 非 .md 跳过
	}

	// 首次 seed：两个 .md 写入，.txt 忽略。
	n, err := m.SeedFromFS(fsys, "skills")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("首次应 seed 2 个, got %d", n)
	}
	for _, f := range []string{"alpha.md", "beta.md"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s 应已 seed: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "note.txt")); err == nil {
		t.Error("非 .md 不应写入")
	}

	// 用户改了 alpha.md（模拟本地修改）→ 再 seed 不应覆盖、不应重复写。
	custom := "---\nname: alpha\n---\nUSER EDIT"
	if err := os.WriteFile(filepath.Join(dir, "alpha.md"), []byte(custom), 0644); err != nil {
		t.Fatal(err)
	}
	n2, err := m.SeedFromFS(fsys, "skills")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("二次 seed 应 0（全已存在）, got %d", n2)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "alpha.md"))
	if string(got) != custom {
		t.Errorf("幂等 seed 不得覆盖用户修改, got %q", string(got))
	}
}

func TestSeedFromFS_ConcurrentNoReplaceHasSingleWinner(t *testing.T) {
	dir := t.TempDir()
	fsys := fstest.MapFS{
		"skills/alpha.md": {Data: []byte("---\nname: alpha\n---\nCOMPLETE")},
	}

	const workers = 32
	start := make(chan struct{})
	results := make(chan int, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, err := NewMarketplace(dir).SeedFromFS(fsys, "skills")
			results <- n
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent seed: %v", err)
		}
	}
	total := 0
	for n := range results {
		total += n
	}
	if total != 1 {
		t.Fatalf("total successful seeds = %d, want exactly 1", total)
	}
	got, err := os.ReadFile(filepath.Join(dir, "alpha.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "---\nname: alpha\n---\nCOMPLETE" {
		t.Fatalf("published seed is partial/corrupt: %q", got)
	}
	assertNoSeedTemps(t, dir)
}

func TestSeedFromFS_CleansStaleCrashTempAndRecovers(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".hexclaw-seed-alpha.md-stale.tmp")
	if err := os.WriteFile(stale, []byte("PARTIAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	fsys := fstest.MapFS{
		"skills/alpha.md": {Data: []byte("---\nname: alpha\n---\nCOMPLETE")},
	}
	n, err := NewMarketplace(dir).SeedFromFS(fsys, "skills")
	if err != nil || n != 1 {
		t.Fatalf("recover seed: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale crash temp remains: %v", err)
	}
	assertNoSeedTemps(t, dir)
}

func assertNoSeedTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".hexclaw-seed-") && strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("seed temporary file leaked: %s", entry.Name())
		}
	}
}

// TestSeedFromFS_MissingSubdir 子目录不存在时报错（非 panic）。
func TestSeedFromFS_MissingSubdir(t *testing.T) {
	m := NewMarketplace(t.TempDir())
	if _, err := m.SeedFromFS(fstest.MapFS{}, "skills"); err == nil {
		t.Error("缺子目录应返回 error")
	}
}
