package marketplace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// BUG-20260712 版本感知 re-seed 回归锁。
//
// 背景：`publishSeedNoReplace` 已存在不覆盖 → bundled skill 版本升级（如 homework-checker
// 1.1.0→1.2.0 内联回显）永远到不了「已 seed 的老安装」（磁盘停在旧版）。修：version-aware——
// bundled 版本更高 **且** 磁盘文件与上次 seed 内容一致（未被用户改）时才升级覆盖；
// 用户改过的文件保护不动；无 manifest 的 legacy 未改老安装也能收到升级。

func reseedVer(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := parseFrontmatter(string(b))
	return m.Version
}

// #1 未被改动的老安装：bundled 升版 → 自动升级覆盖。
func TestSeedFromFS_VersionAware_UpgradesUnmodified(t *testing.T) {
	dir := t.TempDir()
	m := NewMarketplace(dir)
	dest := filepath.Join(dir, "hw.md")

	v11 := fstest.MapFS{"skills/hw.md": {Data: []byte("---\nname: hw\nversion: \"1.1.0\"\n---\nOLD 两轮门")}}
	if _, err := m.SeedFromFS(v11, "skills"); err != nil {
		t.Fatal(err)
	}
	if got := reseedVer(t, dest); got != "1.1.0" {
		t.Fatalf("首 seed 应 1.1.0, got %s", got)
	}

	// bundled 升 1.2.0，用户未改 → 应升级覆盖（RED：no-replace 保持 1.1.0）。
	v12 := fstest.MapFS{"skills/hw.md": {Data: []byte("---\nname: hw\nversion: \"1.2.0\"\n---\nNEW 内联回显")}}
	if _, err := m.SeedFromFS(v12, "skills"); err != nil {
		t.Fatal(err)
	}
	if got := reseedVer(t, dest); got != "1.2.0" {
		t.Fatalf("未改动老安装应自动升级到 1.2.0, got %s", got)
	}
	body, _ := os.ReadFile(dest)
	if !strings.Contains(string(body), "内联回显") {
		t.Fatalf("内容应更新为新版, got %q", string(body))
	}
}

// #2 用户改过的文件：bundled 升版也不覆盖（保护自定义）。
func TestSeedFromFS_VersionAware_PreservesUserEdit(t *testing.T) {
	dir := t.TempDir()
	m := NewMarketplace(dir)
	dest := filepath.Join(dir, "hw.md")

	v11 := fstest.MapFS{"skills/hw.md": {Data: []byte("---\nname: hw\nversion: \"1.1.0\"\n---\n原版")}}
	if _, err := m.SeedFromFS(v11, "skills"); err != nil {
		t.Fatal(err)
	}

	custom := "---\nname: hw\nversion: \"1.1.0\"\n---\n我的自定义辅导话术"
	if err := os.WriteFile(dest, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	v12 := fstest.MapFS{"skills/hw.md": {Data: []byte("---\nname: hw\nversion: \"1.2.0\"\n---\n新版官方话术")}}
	if _, err := m.SeedFromFS(v12, "skills"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != custom {
		t.Fatalf("用户改过的 skill 不得被升级覆盖, got %q", string(got))
	}
}

// #3 legacy 无 manifest（本特性上线前就已 no-replace seed）：未改的老安装升级仍应到达。
func TestSeedFromFS_VersionAware_LegacyNoManifestUpgrades(t *testing.T) {
	dir := t.TempDir()
	m := NewMarketplace(dir)
	dest := filepath.Join(dir, "hw.md")
	if err := os.WriteFile(dest, []byte("---\nname: hw\nversion: \"1.1.0\"\n---\n旧"), 0o644); err != nil {
		t.Fatal(err)
	}

	v12 := fstest.MapFS{"skills/hw.md": {Data: []byte("---\nname: hw\nversion: \"1.2.0\"\n---\n新")}}
	if _, err := m.SeedFromFS(v12, "skills"); err != nil {
		t.Fatal(err)
	}
	if got := reseedVer(t, dest); got != "1.2.0" {
		t.Fatalf("legacy 未改老安装应升级到 1.2.0, got %s", got)
	}
}

// #4 版本未升（相等/更低）：不动磁盘（含用户改动）。
func TestSeedFromFS_VersionAware_NotNewerLeavesDiskAlone(t *testing.T) {
	dir := t.TempDir()
	m := NewMarketplace(dir)
	dest := filepath.Join(dir, "hw.md")

	v12 := fstest.MapFS{"skills/hw.md": {Data: []byte("---\nname: hw\nversion: \"1.2.0\"\n---\n磁盘更新")}}
	if _, err := m.SeedFromFS(v12, "skills"); err != nil {
		t.Fatal(err)
	}
	// bundled 反而是更旧的 1.1.0 → 不得回退覆盖。
	v11 := fstest.MapFS{"skills/hw.md": {Data: []byte("---\nname: hw\nversion: \"1.1.0\"\n---\n旧包")}}
	if _, err := m.SeedFromFS(v11, "skills"); err != nil {
		t.Fatal(err)
	}
	if got := reseedVer(t, dest); got != "1.2.0" {
		t.Fatalf("更旧 bundled 不得回退覆盖磁盘 1.2.0, got %s", got)
	}
}
