package hub

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestHubInstallNotFound(t *testing.T) {
	h := New(HubConfig{}, t.TempDir())
	h.catalog = &Catalog{Skills: []SkillMeta{}}

	err := h.Install(context.Background(), "nonexistent")
	if err == nil {
		t.Error("安装不存在的技能应报错")
	}
}

func TestCatalogURL(t *testing.T) {
	h := New(HubConfig{
		RepoURL: "https://github.com/hexagon-codes/hexclaw-hub",
		Branch:  "main",
	}, "")

	expected := "https://raw.githubusercontent.com/hexagon-codes/hexclaw-hub/main/index.json"
	if got := h.catalogURL(); got != expected {
		t.Errorf("catalogURL 不匹配:\n期望: %s\n得到: %s", expected, got)
	}
}
