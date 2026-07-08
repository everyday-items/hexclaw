package marketplace

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
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

// TestSeedFromFS_MissingSubdir 子目录不存在时报错（非 panic）。
func TestSeedFromFS_MissingSubdir(t *testing.T) {
	m := NewMarketplace(t.TempDir())
	if _, err := m.SeedFromFS(fstest.MapFS{}, "skills"); err == nil {
		t.Error("缺子目录应返回 error")
	}
}
