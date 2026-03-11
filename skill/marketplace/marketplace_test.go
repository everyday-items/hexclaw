package marketplace

import (
	"os"
	"path/filepath"
	"testing"
)

// TestParseFrontmatter 测试 frontmatter 解析
func TestParseFrontmatter(t *testing.T) {
	input := `---
name: web-search
description: 搜索网页并返回结果
author: hexclaw-team
version: "1.0"
triggers:
  - 搜索
  - search
  - 查找
tags:
  - search
  - web
---

# 网络搜索技能

你是一个网络搜索助手，请根据用户的查询执行搜索。`

	meta, content := parseFrontmatter(input)

	if meta.Name != "web-search" {
		t.Errorf("name: got %q, want %q", meta.Name, "web-search")
	}
	if meta.Description != "搜索网页并返回结果" {
		t.Errorf("description: got %q", meta.Description)
	}
	if meta.Author != "hexclaw-team" {
		t.Errorf("author: got %q", meta.Author)
	}
	if meta.Version != "1.0" {
		t.Errorf("version: got %q", meta.Version)
	}
	if len(meta.Triggers) != 3 {
		t.Fatalf("triggers: got %d, want 3", len(meta.Triggers))
	}
	if meta.Triggers[0] != "搜索" || meta.Triggers[1] != "search" || meta.Triggers[2] != "查找" {
		t.Errorf("triggers: got %v", meta.Triggers)
	}
	if len(meta.Tags) != 2 {
		t.Fatalf("tags: got %d, want 2", len(meta.Tags))
	}
	if content == "" {
		t.Error("content 不应为空")
	}
	if content != "# 网络搜索技能\n\n你是一个网络搜索助手，请根据用户的查询执行搜索。" {
		t.Errorf("content 不匹配: %q", content)
	}
}

// TestParseFrontmatter_NoFrontmatter 测试无 frontmatter 的文件
func TestParseFrontmatter_NoFrontmatter(t *testing.T) {
	input := "# 简单技能\n\n这是一个没有 frontmatter 的技能文件。"

	meta, content := parseFrontmatter(input)

	if meta.Name != "" {
		t.Errorf("无 frontmatter 时 name 应为空，got %q", meta.Name)
	}
	if content != input {
		t.Errorf("内容应为原始文本")
	}
}

// TestParseFrontmatter_InlineList 测试内联列表格式
func TestParseFrontmatter_InlineList(t *testing.T) {
	input := `---
name: test
triggers: [a, b, c]
tags: ["tag1", "tag2"]
---

内容`

	meta, _ := parseFrontmatter(input)

	if len(meta.Triggers) != 3 {
		t.Fatalf("triggers: got %d, want 3", len(meta.Triggers))
	}
	if meta.Triggers[0] != "a" || meta.Triggers[1] != "b" || meta.Triggers[2] != "c" {
		t.Errorf("triggers: got %v", meta.Triggers)
	}
	if len(meta.Tags) != 2 {
		t.Fatalf("tags: got %d, want 2", len(meta.Tags))
	}
}

// TestMarkdownSkill_Match 测试技能匹配
func TestMarkdownSkill_Match(t *testing.T) {
	skill := &MarkdownSkill{
		Meta: SkillMeta{
			Name:     "search",
			Triggers: []string{"搜索", "search", "查找"},
		},
	}

	tests := []struct {
		content string
		want    bool
	}{
		{"搜索 Go 语言教程", true},
		{"search golang", true},
		{"查找文件", true},
		{"帮我翻译", false},
		{"天气怎么样", false},
		{"", false},
	}

	for _, tt := range tests {
		got := skill.Match(tt.content)
		if got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.content, got, tt.want)
		}
	}
}

// TestMarkdownSkill_NoTriggers 测试无触发词的技能
func TestMarkdownSkill_NoTriggers(t *testing.T) {
	skill := &MarkdownSkill{
		Meta: SkillMeta{Name: "assistant"},
	}

	if skill.Match("any content") {
		t.Error("无触发词的技能不应匹配任何内容")
	}
}

// TestLoadSkillFromFile 测试从文件加载技能
func TestLoadSkillFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-skill.md")

	content := `---
name: test
description: 测试技能
author: tester
version: "2.0"
---

# 测试技能

这是测试内容。`

	os.WriteFile(path, []byte(content), 0644)

	skill, err := LoadSkillFromFile(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	if skill.Meta.Name != "test" {
		t.Errorf("name: got %q", skill.Meta.Name)
	}
	if skill.Meta.Description != "测试技能" {
		t.Errorf("description: got %q", skill.Meta.Description)
	}
	if skill.Meta.Version != "2.0" {
		t.Errorf("version: got %q", skill.Meta.Version)
	}

	// 内容应未加载（按需加载）
	if skill.loaded {
		t.Error("加载后不应立即读取内容")
	}
}

// TestMarkdownSkill_LazyLoad 测试懒加载
func TestMarkdownSkill_LazyLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lazy.md")

	os.WriteFile(path, []byte(`---
name: lazy
---

懒加载内容`), 0644)

	skill, _ := LoadSkillFromFile(path)

	// 首次 LoadContent 应从文件读取
	content, err := skill.LoadContent()
	if err != nil {
		t.Fatalf("懒加载失败: %v", err)
	}
	if content != "懒加载内容" {
		t.Errorf("内容不匹配: %q", content)
	}
	if !skill.loaded {
		t.Error("加载后应标记已加载")
	}

	// 再次调用应使用缓存
	content2, _ := skill.LoadContent()
	if content2 != content {
		t.Error("缓存内容应一致")
	}
}

// TestLoadSkillsFromDir 测试从目录加载技能
func TestLoadSkillsFromDir(t *testing.T) {
	dir := t.TempDir()

	// 创建单文件技能
	os.WriteFile(filepath.Join(dir, "search.md"), []byte(`---
name: search
description: 搜索
---
搜索内容`), 0644)

	// 创建目录技能
	subDir := filepath.Join(dir, "translator")
	os.MkdirAll(subDir, 0755)
	os.WriteFile(filepath.Join(subDir, "SKILL.md"), []byte(`---
name: translator
description: 翻译
---
翻译内容`), 0644)

	// 创建非 .md 文件（应忽略）
	os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("ignore"), 0644)

	skills, err := LoadSkillsFromDir(dir)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	if len(skills) != 2 {
		t.Fatalf("应加载 2 个技能，实际 %d", len(skills))
	}

	names := make(map[string]bool)
	for _, s := range skills {
		names[s.Meta.Name] = true
	}
	if !names["search"] {
		t.Error("应包含 search 技能")
	}
	if !names["translator"] {
		t.Error("应包含 translator 技能")
	}
}

// TestLoadSkillsFromDir_NotExist 测试不存在的目录
func TestLoadSkillsFromDir_NotExist(t *testing.T) {
	skills, err := LoadSkillsFromDir("/nonexistent/path")
	if err != nil {
		t.Errorf("不存在的目录不应报错: %v", err)
	}
	if skills != nil {
		t.Error("不存在的目录应返回 nil")
	}
}

// TestMarketplace_InstallAndList 测试安装和列出技能
func TestMarketplace_InstallAndList(t *testing.T) {
	skillDir := t.TempDir()
	sourceDir := t.TempDir()

	// 创建源技能文件
	sourcePath := filepath.Join(sourceDir, "my-skill.md")
	os.WriteFile(sourcePath, []byte(`---
name: my-skill
description: 我的技能
version: "1.0"
---
技能内容`), 0644)

	mp := NewMarketplace(skillDir)
	mp.Init()

	// 安装
	skill, err := mp.Install(sourcePath)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if skill.Meta.Name != "my-skill" {
		t.Errorf("名称不匹配: %q", skill.Meta.Name)
	}

	// 列表
	list := mp.List()
	if len(list) != 1 {
		t.Fatalf("应有 1 个技能，实际 %d", len(list))
	}

	// 获取
	s, ok := mp.Get("my-skill")
	if !ok {
		t.Fatal("应能获取已安装技能")
	}
	if s.Meta.Description != "我的技能" {
		t.Errorf("描述不匹配: %q", s.Meta.Description)
	}
}

// TestMarketplace_InstallDir 测试安装目录技能
func TestMarketplace_InstallDir(t *testing.T) {
	skillDir := t.TempDir()
	sourceDir := t.TempDir()

	// 创建源目录技能
	skillSubDir := filepath.Join(sourceDir, "complex-skill")
	os.MkdirAll(skillSubDir, 0755)
	os.WriteFile(filepath.Join(skillSubDir, "SKILL.md"), []byte(`---
name: complex-skill
description: 复杂技能
---
# 复杂技能

多文件技能内容`), 0644)
	os.WriteFile(filepath.Join(skillSubDir, "helper.txt"), []byte("辅助文件"), 0644)

	mp := NewMarketplace(skillDir)
	mp.Init()

	skill, err := mp.Install(skillSubDir)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if skill.Meta.Name != "complex-skill" {
		t.Errorf("名称不匹配: %q", skill.Meta.Name)
	}

	// 检查文件是否已复制
	destHelper := filepath.Join(skillDir, "complex-skill", "helper.txt")
	if _, err := os.Stat(destHelper); err != nil {
		t.Error("辅助文件应已复制")
	}
}

// TestMarketplace_Uninstall 测试删除技能
func TestMarketplace_Uninstall(t *testing.T) {
	skillDir := t.TempDir()

	// 预先创建一个已安装的技能
	os.WriteFile(filepath.Join(skillDir, "to-remove.md"), []byte(`---
name: to-remove
---
内容`), 0644)

	mp := NewMarketplace(skillDir)
	mp.Init()

	if len(mp.List()) != 1 {
		t.Fatal("应有 1 个技能")
	}

	err := mp.Uninstall("to-remove")
	if err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	if len(mp.List()) != 0 {
		t.Error("删除后应无技能")
	}

	// 文件应已删除
	if _, err := os.Stat(filepath.Join(skillDir, "to-remove.md")); !os.IsNotExist(err) {
		t.Error("文件应已删除")
	}
}

// TestMarketplace_UninstallNotExist 测试删除不存在的技能
func TestMarketplace_UninstallNotExist(t *testing.T) {
	mp := NewMarketplace(t.TempDir())
	mp.Init()

	err := mp.Uninstall("nonexistent")
	if err == nil {
		t.Error("删除不存在的技能应报错")
	}
}

// TestMarkdownSkill_Execute 测试技能执行
func TestMarkdownSkill_Execute(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "exec.md")
	os.WriteFile(path, []byte(`---
name: exec-test
author: tester
---
执行测试内容`), 0644)

	skill, _ := LoadSkillFromFile(path)

	result, err := skill.Execute(t.Context(), map[string]any{
		"query": "测试查询",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	if result.Content != "执行测试内容" {
		t.Errorf("内容不匹配: %q", result.Content)
	}
	if result.Metadata["skill_name"] != "exec-test" {
		t.Errorf("元数据不匹配: %v", result.Metadata)
	}
	if result.Metadata["query"] != "测试查询" {
		t.Errorf("查询参数不匹配: %v", result.Metadata)
	}
}
