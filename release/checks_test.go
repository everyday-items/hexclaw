package release

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckVersionBump_Pass(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"0.4.0"}`), 0644)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`version = 0.4.0 here`), 0644)

	c := CheckVersionBump(dir, "0.4.0", "package.json", "config.json")
	res := c.Run(context.Background())
	if res.Status != StatusPass {
		t.Errorf("应 PASS；got %s: %s", res.Status, res.Message)
	}
}

func TestCheckVersionBump_FailMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte(`old version 0.3.5`), 0644)
	os.WriteFile(filepath.Join(dir, "b"), []byte(`v0.4.0 here`), 0644)

	c := CheckVersionBump(dir, "0.4.0", "a", "b")
	res := c.Run(context.Background())
	if res.Status != StatusFail {
		t.Errorf("应 FAIL（a 没含）；got %s", res.Status)
	}
	if !strings.Contains(res.Detail, "a") {
		t.Errorf("Detail 应列出 a；got %s", res.Detail)
	}
}

func TestCheckVersionBump_SkipEmptyVersion(t *testing.T) {
	c := CheckVersionBump(t.TempDir(), "")
	res := c.Run(context.Background())
	if res.Status != StatusSkip {
		t.Errorf("空 version 应 SKIP；got %s", res.Status)
	}
}

func TestCheckChangelogCurrent_Pass(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("# Changelog\n## [0.4.0] - 2026-04-28\n\nfeat: x"), 0644)
	c := CheckChangelogCurrent(dir, "0.4.0", "CHANGELOG.md")
	res := c.Run(context.Background())
	if res.Status != StatusPass {
		t.Errorf("应 PASS；got %s: %s", res.Status, res.Message)
	}
}

func TestCheckChangelogCurrent_PassVPrefix(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("## v0.4.0\n\nrelease"), 0644)
	c := CheckChangelogCurrent(dir, "0.4.0", "CHANGELOG.md")
	res := c.Run(context.Background())
	if res.Status != StatusPass {
		t.Errorf("v0.4.0 形式应 PASS；got %s", res.Status)
	}
}

func TestCheckChangelogCurrent_FailMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "CHANGELOG.md"), []byte("## v0.3.0\n\nold release"), 0644)
	c := CheckChangelogCurrent(dir, "0.4.0", "CHANGELOG.md")
	res := c.Run(context.Background())
	if res.Status != StatusFail {
		t.Errorf("应 FAIL（无 0.4.0 章节）；got %s", res.Status)
	}
}

func TestCheckChangelogCurrent_FailMissingFile(t *testing.T) {
	c := CheckChangelogCurrent(t.TempDir(), "0.4.0", "CHANGELOG.md")
	res := c.Run(context.Background())
	if res.Status != StatusFail {
		t.Errorf("文件缺失应 FAIL；got %s", res.Status)
	}
}

func TestBuildDefault10WithReal_ReplacesFour(t *testing.T) {
	dir := t.TempDir()
	checks := BuildDefault10WithReal(dir, "0.4.0", []string{"x"})
	if len(checks) != 10 {
		t.Errorf("应仍是 10 项；got %d", len(checks))
	}
	expected := map[string]string{
		"tests-pass":   "tests-pass",
		"lint-clean":   "lint-clean",
		"version-bump": "version-bump",
		"docs-current": "docs-current",
	}
	found := 0
	for _, c := range checks {
		if _, ok := expected[c.Name()]; ok {
			// FuncCheck 会有 N 字段，这里通过 description 含具体描述判断已被替换
			if !strings.Contains(c.Description(), "skip") {
				found++
			}
		}
	}
	if found < 4 {
		t.Errorf("应至少替换了 4 项；got %d", found)
	}
}

func TestTruncate(t *testing.T) {
	s := "abcdefghij"
	if got := truncate(s, 100); got != s {
		t.Errorf("max>=len 应返回原文；got %s", got)
	}
	if got := truncate(s, 4); !strings.HasPrefix(got, "abcd") || !strings.Contains(got, "truncated") {
		t.Errorf("应截断 + 标记；got %s", got)
	}
}
