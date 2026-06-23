package hub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"net/http"

	"github.com/hexagon-codes/hexclaw/internal/testutil/httpmock"
)

func TestHubRefreshAndSearch(t *testing.T) {
	catalog := Catalog{
		Version:   "1.0",
		UpdatedAt: time.Now(),
		Skills: []SkillMeta{
			{Name: "web-search", DisplayName: "网络搜索", Description: "Search the web", Category: "search", Tags: []string{"web", "search"}},
			{Name: "translator", DisplayName: "翻译助手", Description: "Translate text", Category: "language", Tags: []string{"translate", "i18n"}},
			{Name: "code-review", DisplayName: "代码审查", Description: "Review code quality", Category: "dev", Tags: []string{"code", "review"}},
		},
	}

	h := New(HubConfig{
		Enabled: true,
		RepoURL: "https://clawhub.test/repo",
		Branch:  "main",
	}, t.TempDir())
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(catalog)
	}))
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh 失败: %v", err)
	}

	// 搜索
	results := h.Search("search")
	if len(results) != 1 || results[0].Name != "web-search" {
		t.Errorf("搜索 'search' 期望 1 个结果，得到 %d", len(results))
	}

	results = h.Search("code")
	if len(results) != 1 || results[0].Name != "code-review" {
		t.Errorf("搜索 'code' 期望 1 个结果，得到 %d", len(results))
	}

	// 按标签搜索
	results = h.Search("i18n")
	if len(results) != 1 || results[0].Name != "translator" {
		t.Errorf("搜索 'i18n' 期望 1 个结果，得到 %d", len(results))
	}

	// 空搜索
	results = h.Search("nonexistent")
	if len(results) != 0 {
		t.Errorf("搜索不存在的关键词期望 0 个结果，得到 %d", len(results))
	}
}

func TestHubListByCategory(t *testing.T) {
	h := &Hub{
		catalog: &Catalog{
			Skills: []SkillMeta{
				{Name: "a", Category: "dev"},
				{Name: "b", Category: "dev"},
				{Name: "c", Category: "search"},
			},
		},
	}

	results := h.ListByCategory("dev")
	if len(results) != 2 {
		t.Errorf("期望 2 个 dev 技能，得到 %d", len(results))
	}

	results = h.ListByCategory("SEARCH")
	if len(results) != 1 {
		t.Errorf("期望 1 个 search 技能，得到 %d", len(results))
	}
}

func TestHubInstallAndUninstall(t *testing.T) {
	skillContent := "---\nname: test-skill\n---\n# Test Skill"
	dir := t.TempDir()
	h := New(HubConfig{RepoURL: "https://clawhub.test/repo", Branch: "main"}, dir)
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(skillContent))
	}))
	h.catalog = &Catalog{
		Skills: []SkillMeta{
			{Name: "test-skill", URL: "https://clawhub.test/repo/skills/test-skill.md"},
		},
	}

	ctx := context.Background()

	// 安装
	if err := h.Install(ctx, "test-skill"); err != nil {
		t.Fatalf("安装失败: %v", err)
	}

	path := filepath.Join(dir, "test-skill.md")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取安装的技能失败: %v", err)
	}
	if string(content) != skillContent {
		t.Errorf("技能内容不匹配")
	}

	// 卸载
	if err := h.Uninstall("test-skill"); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("技能文件应已被删除")
	}

	// 卸载不存在的
	if err := h.Uninstall("ghost"); err == nil {
		t.Error("卸载不存在的技能应报错")
	}
}

func TestHubContent(t *testing.T) {
	skillContent := "---\nname: test-skill\n---\n# Test Skill"
	dir := t.TempDir()
	h := New(HubConfig{RepoURL: "https://clawhub.test/repo", Branch: "main"}, dir)
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(skillContent))
	}))
	h.catalog = &Catalog{
		Skills: []SkillMeta{
			{Name: "test-skill", URL: "https://clawhub.test/repo/skills/test-skill.md"},
		},
	}

	content, err := h.Content(context.Background(), "test-skill")
	if err != nil {
		t.Fatalf("Content 失败: %v", err)
	}
	if string(content) != skillContent {
		t.Errorf("内容不匹配:\n期望=%q\n得到=%q", skillContent, string(content))
	}

	// 预览语义：绝不落盘——skillsDir 必须为空
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("Content 不应写盘，但 skillsDir 出现 %d 个文件", len(entries))
	}

	// 未知技能应报错
	if _, err := h.Content(context.Background(), "ghost"); err == nil {
		t.Error("未知技能 Content 应报错")
	}
}

// TestHubContentMatchesInstall 差分不变量：预览（Content）拿到的字节必须与安装
// （Install 写盘）的字节逐字节一致——两者复用同一取数路径，预览所见即安装所得。
func TestHubContentMatchesInstall(t *testing.T) {
	skillContent := "---\nname: diff-skill\ndescription: d\n---\n# Diff\n\nbody\n"
	dir := t.TempDir()
	h := New(HubConfig{RepoURL: "https://clawhub.test/repo", Branch: "main"}, dir)
	h.client = httpmock.NewClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(skillContent))
	}))
	h.catalog = &Catalog{
		Skills: []SkillMeta{
			{Name: "diff-skill", URL: "https://clawhub.test/repo/skills/diff-skill.md"},
		},
	}

	previewed, err := h.Content(context.Background(), "diff-skill")
	if err != nil {
		t.Fatalf("Content 失败: %v", err)
	}
	if err := h.Install(context.Background(), "diff-skill"); err != nil {
		t.Fatalf("Install 失败: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(dir, "diff-skill.md"))
	if err != nil {
		t.Fatalf("读取安装文件失败: %v", err)
	}
	if string(previewed) != string(installed) {
		t.Fatalf("预览与安装字节不一致:\n预览=%q\n安装=%q", string(previewed), string(installed))
	}
}

func TestHubInstallNotFound(t *testing.T) {
	h := New(HubConfig{}, t.TempDir())
	h.catalog = &Catalog{Skills: []SkillMeta{}}

	err := h.Install(context.Background(), "nonexistent")
	if err == nil {
		t.Error("安装不存在的技能应报错")
	}
}

func TestNewDefaultsToTaggedHubCatalog(t *testing.T) {
	h := New(HubConfig{}, t.TempDir())

	if h.cfg.RepoURL != "https://github.com/hexagon-codes/hexclaw-hub" {
		t.Fatalf("RepoURL 默认值不匹配: %s", h.cfg.RepoURL)
	}
	if h.cfg.Branch != "v0.0.5" {
		t.Fatalf("Branch 默认值不匹配: %s", h.cfg.Branch)
	}
	if got := h.catalogURL(); got != "https://raw.githubusercontent.com/hexagon-codes/hexclaw-hub/v0.0.5/index.json" {
		t.Fatalf("catalogURL 默认值不匹配: %s", got)
	}
}

func TestCatalogURL(t *testing.T) {
	h := New(HubConfig{
		RepoURL: "https://github.com/hexagon-codes/hexclaw-hub",
		Branch:  "v0.0.2",
	}, "")

	expected := "https://raw.githubusercontent.com/hexagon-codes/hexclaw-hub/v0.0.2/index.json"
	if got := h.catalogURL(); got != expected {
		t.Errorf("catalogURL 不匹配:\n期望: %s\n得到: %s", expected, got)
	}
}

func TestCatalogURL_LocalFileRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("本地仓库以 file://+POSIX 路径构造 URL, Windows 盘符/反斜杠不兼容 (本地仓库跨平台支持留待专项)")
	}
	dir := t.TempDir()
	h := New(HubConfig{
		RepoURL: "file://" + dir,
		Branch:  "main",
	}, "")

	expected := filepath.Join(dir, "index.json")
	if got := h.catalogURL(); got != expected {
		t.Errorf("catalogURL 本地路径不匹配:\n期望: %s\n得到: %s", expected, got)
	}
}

func TestHubRefreshFromLocalRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("本地仓库以 file://+POSIX 路径构造 URL, Windows 盘符/反斜杠不兼容 (本地仓库跨平台支持留待专项)")
	}
	repoDir := t.TempDir()
	catalog := `{"version":"1.0.0","updated_at":"2026-03-22T00:00:00Z","skills":[{"name":"lawyer","display_name":"Lawyer","description":"desc","category":"productivity","tags":["legal"]}]}`
	if err := os.WriteFile(filepath.Join(repoDir, "index.json"), []byte(catalog), 0o644); err != nil {
		t.Fatalf("写入本地 index.json 失败: %v", err)
	}

	h := New(HubConfig{
		RepoURL: "file://" + repoDir,
		Branch:  "main",
	}, t.TempDir())

	if err := h.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh 本地目录失败: %v", err)
	}

	results := h.Search("lawyer")
	if len(results) != 1 || results[0].Name != "lawyer" {
		t.Fatalf("本地目录搜索失败，结果=%v", results)
	}
}

func TestHubInstallFromLocalRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("本地仓库以 file://+POSIX 路径构造 URL, Windows 盘符/反斜杠不兼容 (本地仓库跨平台支持留待专项)")
	}
	repoDir := t.TempDir()
	skillsDir := t.TempDir()
	skillContent := "---\nname: lawyer\n---\n# Lawyer Skill"

	if err := os.MkdirAll(filepath.Join(repoDir, "skills"), 0o755); err != nil {
		t.Fatalf("创建 skills 目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "skills", "lawyer.md"), []byte(skillContent), 0o644); err != nil {
		t.Fatalf("写入本地 skill 失败: %v", err)
	}

	h := New(HubConfig{
		RepoURL: "file://" + repoDir,
		Branch:  "main",
	}, skillsDir)
	h.catalog = &Catalog{
		Skills: []SkillMeta{
			{Name: "lawyer"},
		},
	}

	if err := h.Install(context.Background(), "lawyer"); err != nil {
		t.Fatalf("从本地 hub 安装失败: %v", err)
	}

	installed, err := os.ReadFile(filepath.Join(skillsDir, "lawyer.md"))
	if err != nil {
		t.Fatalf("读取已安装 skill 失败: %v", err)
	}
	if string(installed) != skillContent {
		t.Fatalf("安装内容不匹配:\n期望=%q\n得到=%q", skillContent, string(installed))
	}
}
