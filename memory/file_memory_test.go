package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNew 测试创建文件记忆系统
func TestNew(t *testing.T) {
	dir := t.TempDir()

	fm, err := New(Options{
		Enabled: true,
		Dir:     dir,
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	if fm.Dir() != dir {
		t.Errorf("目录不匹配: got %s, want %s", fm.Dir(), dir)
	}
	if fm.config.MaxMemory != 200 {
		t.Errorf("默认 MaxMemory 应为 200，实际 %d", fm.config.MaxMemory)
	}
	if fm.config.DailyDays != 2 {
		t.Errorf("默认 DailyDays 应为 2，实际 %d", fm.config.DailyDays)
	}
}

// TestNew_DefaultDir 测试默认目录
func TestNew_DefaultDir(t *testing.T) {
	fm, err := New(Options{Enabled: true})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	home, _ := os.UserHomeDir()
	expected := filepath.Join(home, ".hexclaw", "memory")
	if fm.Dir() != expected {
		t.Errorf("默认目录不匹配: got %s, want %s", fm.Dir(), expected)
	}
}

// TestSaveAndGetMemory 测试保存和获取长期记忆
func TestSaveAndGetMemory(t *testing.T) {
	dir := t.TempDir()
	fm, _ := New(Options{Dir: dir})

	// 保存记忆
	if err := fm.SaveMemory("用户偏好：使用中文"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := fm.SaveMemory("项目使用 Go 语言"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 获取记忆
	content := fm.GetMemory()
	if content == "" {
		t.Fatal("记忆内容不应为空")
	}
	if !strings.Contains(content, "用户偏好：使用中文") {
		t.Error("记忆应包含第一条记录")
	}
	if !strings.Contains(content, "项目使用 Go 语言") {
		t.Error("记忆应包含第二条记录")
	}
}

// TestSaveAndGetDaily 测试保存和获取每日日记
func TestSaveAndGetDaily(t *testing.T) {
	dir := t.TempDir()
	fm, _ := New(Options{Dir: dir})

	// 保存今日日记
	if err := fm.SaveDaily("今天完成了 Phase 3 开发"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// 获取今日日记
	content := fm.GetDaily(time.Now())
	if content == "" {
		t.Fatal("日记内容不应为空")
	}
	if !strings.Contains(content, "今天完成了 Phase 3 开发") {
		t.Error("日记应包含记录内容")
	}
}

// TestLoadContext 测试加载记忆上下文
func TestLoadContext(t *testing.T) {
	dir := t.TempDir()
	fm, _ := New(Options{Dir: dir, DailyDays: 2})

	// 保存长期记忆
	fm.SaveMemory("长期记忆内容")

	// 保存今日日记
	fm.SaveDaily("今日活动记录")

	// 加载上下文
	ctx := fm.LoadContext()
	if ctx == "" {
		t.Fatal("上下文不应为空")
	}
	if !strings.Contains(ctx, "长期记忆") {
		t.Error("上下文应包含长期记忆标题")
	}
	if !strings.Contains(ctx, "长期记忆内容") {
		t.Error("上下文应包含长期记忆内容")
	}
	if !strings.Contains(ctx, "日记") {
		t.Error("上下文应包含日记标题")
	}
	if !strings.Contains(ctx, "今日活动记录") {
		t.Error("上下文应包含日记内容")
	}
}

// TestLoadContext_MaxMemoryTruncation 测试长期记忆截断
func TestLoadContext_MaxMemoryTruncation(t *testing.T) {
	dir := t.TempDir()
	fm, _ := New(Options{Dir: dir, MaxMemory: 3})

	// 写入超过 3 行的内容
	path := filepath.Join(dir, "MEMORY.md")
	content := "第一行\n第二行\n第三行\n第四行\n第五行\n"
	os.WriteFile(path, []byte(content), 0644)

	ctx := fm.LoadContext()
	// 应该只包含前 3 行
	if strings.Contains(ctx, "第四行") {
		t.Error("截断后不应包含第四行")
	}
	if strings.Contains(ctx, "第五行") {
		t.Error("截断后不应包含第五行")
	}
	if !strings.Contains(ctx, "第一行") {
		t.Error("应包含第一行")
	}
}

// TestSearch 测试搜索记忆
func TestSearch(t *testing.T) {
	dir := t.TempDir()
	fm, _ := New(Options{Dir: dir})

	// 保存多条记忆
	fm.SaveMemory("Go 语言项目")
	fm.SaveMemory("Python 脚本工具")
	fm.SaveMemory("Go 并发编程")

	// 搜索 Go
	results := fm.Search("Go")
	if len(results) == 0 {
		t.Fatal("应找到包含 Go 的结果")
	}

	goCount := 0
	for _, r := range results {
		if strings.Contains(r.Content, "Go") {
			goCount++
		}
	}
	if goCount < 2 {
		t.Errorf("应至少找到 2 条 Go 相关结果，实际 %d", goCount)
	}
}

// TestSearch_MultiKeyword 测试多关键词搜索
func TestSearch_MultiKeyword(t *testing.T) {
	dir := t.TempDir()
	fm, _ := New(Options{Dir: dir})

	fm.SaveMemory("Go 语言并发编程")
	fm.SaveMemory("Go 语言 Web 开发")
	fm.SaveMemory("Python 机器学习")

	// 搜索 "Go 并发"
	results := fm.Search("Go 并发")
	if len(results) == 0 {
		t.Fatal("应找到结果")
	}

	// 包含两个关键词的结果应排在前面
	if results[0].Score < 0.5 {
		t.Error("最佳匹配分数应 >= 0.5")
	}
}

// TestSaveMemory_EmptyContent 测试空内容不保存
func TestSaveMemory_EmptyContent(t *testing.T) {
	dir := t.TempDir()
	fm, _ := New(Options{Dir: dir})

	// 空内容应该不报错
	if err := fm.SaveMemory(""); err != nil {
		t.Errorf("空内容不应报错: %v", err)
	}
	if err := fm.SaveMemory("   "); err != nil {
		t.Errorf("空白内容不应报错: %v", err)
	}

	// 不应创建文件
	content := fm.GetMemory()
	if content != "" {
		t.Errorf("空内容不应创建文件，实际内容: %q", content)
	}
}

// TestSearch_EmptyQuery 测试空查询
func TestSearch_EmptyQuery(t *testing.T) {
	dir := t.TempDir()
	fm, _ := New(Options{Dir: dir})

	results := fm.Search("")
	if results != nil {
		t.Error("空查询应返回 nil")
	}
}
