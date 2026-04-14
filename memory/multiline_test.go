package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ━━━ 修复前的旧逻辑（用于对比） ━━━

// parseEntriesOldBehavior 模拟修复前的逐行解析逻辑。
// 每一个非空行都被当作独立的记忆条目 — 这是 Bug 的根因。
func parseEntriesOldBehavior(raw string) []MemoryEntry {
	lines := strings.Split(raw, "\n")
	var entries []MemoryEntry
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		e := MemoryEntry{
			ID:     fmt.Sprintf("m-%d", i),
			Type:   "fact",
			Source: "manual",
			Status: "active",
		}
		if strings.HasPrefix(line, "- ") {
			line = line[2:]
		}
		if strings.HasPrefix(line, "[") {
			if idx := strings.Index(line, "]"); idx > 0 && idx <= 6 {
				line = strings.TrimSpace(line[idx+1:])
			}
		}
		if strings.HasPrefix(line, "[") {
			if idx := strings.Index(line, "]"); idx > 0 {
				meta := line[1:idx]
				if parts := strings.SplitN(meta, ":", 2); len(parts) == 2 {
					e.Type = parts[0]
					e.Source = parts[1]
					line = strings.TrimSpace(line[idx+1:])
				}
			}
		}
		e.Content = line
		if e.Content != "" {
			entries = append(entries, e)
		}
	}
	return entries
}

// parseEntriesNewBehavior 使用修复后的 splitEntryBlocks 块解析逻辑。
func parseEntriesNewBehavior(raw string) []MemoryEntry {
	blocks := splitEntryBlocks(raw)
	var entries []MemoryEntry
	for _, block := range blocks {
		e := MemoryEntry{
			ID:     fmt.Sprintf("m-%d", block.startLine),
			Type:   "fact",
			Source: "manual",
			Status: "active",
		}
		line := block.text
		if strings.HasPrefix(line, "- ") {
			line = line[2:]
		}
		if strings.HasPrefix(line, "[") {
			if idx := strings.Index(line, "]"); idx > 0 && idx <= 6 {
				line = strings.TrimSpace(line[idx+1:])
			}
		}
		if strings.HasPrefix(line, "[") {
			if idx := strings.Index(line, "]"); idx > 0 {
				meta := line[1:idx]
				if parts := strings.SplitN(meta, ":", 2); len(parts) == 2 {
					e.Type = parts[0]
					e.Source = parts[1]
					line = strings.TrimSpace(line[idx+1:])
				}
			}
		}
		e.Content = line
		if e.Content != "" {
			entries = append(entries, e)
		}
	}
	return entries
}

// ━━━ 修复前 vs 修复后 对比测试 ━━━

func TestBeforeAfter_MultilineEntry(t *testing.T) {
	// 模拟 SaveEntry 写入多行内容后的文件内容
	raw := "\n- [16:05] [fact:manual] 第一行\n第二行\n第三行\n"

	t.Run("修复前：多行被拆成 3 条", func(t *testing.T) {
		entries := parseEntriesOldBehavior(raw)
		if len(entries) != 3 {
			t.Fatalf("修复前期望 3 条（每行一条），得到 %d", len(entries))
		}
		t.Logf("修复前结果（BUG）:")
		for i, e := range entries {
			t.Logf("  条目 %d: %q", i, e.Content)
		}
		// 验证每行确实被当成独立条目
		if entries[0].Content != "第一行" {
			t.Errorf("第 1 条应该是 '第一行'，得到 %q", entries[0].Content)
		}
		if entries[1].Content != "第二行" {
			t.Errorf("第 2 条应该是 '第二行'，得到 %q", entries[1].Content)
		}
		if entries[2].Content != "第三行" {
			t.Errorf("第 3 条应该是 '第三行'，得到 %q", entries[2].Content)
		}
	})

	t.Run("修复后：多行归为 1 条", func(t *testing.T) {
		entries := parseEntriesNewBehavior(raw)
		if len(entries) != 1 {
			t.Fatalf("修复后期望 1 条，得到 %d", len(entries))
		}
		t.Logf("修复后结果（正确）:")
		t.Logf("  条目 0: %q", entries[0].Content)
		// 多行内容完整保留
		if !strings.Contains(entries[0].Content, "第一行") ||
			!strings.Contains(entries[0].Content, "第二行") ||
			!strings.Contains(entries[0].Content, "第三行") {
			t.Errorf("多行内容不完整: %q", entries[0].Content)
		}
	})
}

func TestBeforeAfter_MixedEntries(t *testing.T) {
	// 3 条记忆：单行 + 多行 + 单行
	raw := `
- [16:05] [fact:manual] 单行条目

- [16:06] [fact:manual] 多行第一行
多行第二行
多行第三行

- [16:07] [fact:manual] 最后一条
`
	t.Run("修复前：5 条（多行被拆散）", func(t *testing.T) {
		entries := parseEntriesOldBehavior(raw)
		if len(entries) != 5 {
			t.Fatalf("修复前期望 5 条，得到 %d", len(entries))
		}
		t.Logf("修复前结果（BUG）:")
		for i, e := range entries {
			t.Logf("  条目 %d: %q", i, e.Content)
		}
	})

	t.Run("修复后：3 条（多行归为一条）", func(t *testing.T) {
		entries := parseEntriesNewBehavior(raw)
		if len(entries) != 3 {
			t.Fatalf("修复后期望 3 条，得到 %d", len(entries))
		}
		t.Logf("修复后结果（正确）:")
		for i, e := range entries {
			t.Logf("  条目 %d: %q", i, e.Content)
		}
		if entries[0].Content != "单行条目" {
			t.Errorf("第 1 条应该是 '单行条目'，得到 %q", entries[0].Content)
		}
		if !strings.Contains(entries[1].Content, "多行第一行") ||
			!strings.Contains(entries[1].Content, "多行第三行") {
			t.Errorf("第 2 条多行内容不完整: %q", entries[1].Content)
		}
		if entries[2].Content != "最后一条" {
			t.Errorf("第 3 条应该是 '最后一条'，得到 %q", entries[2].Content)
		}
	})
}

func TestBeforeAfter_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	fm := newTestFileMemory(t, dir)

	// 写入 3 条记忆（第 2 条是多行）
	fm.SaveEntry("用户偏好：简洁风格", "preference", "manual")
	fm.SaveEntry("项目说明：\n- 前端用 Vue 3\n- 后端用 Go\n- 数据库用 SQLite", "fact", "manual")
	fm.SaveEntry("不要自动提交 Git", "instruction", "manual")

	// 读取并验证
	entries := fm.ParseEntries()

	t.Logf("写入 3 条后实际解析结果（%d 条）:", len(entries))
	for i, e := range entries {
		t.Logf("  条目 %d [%s:%s]: %q", i, e.Type, e.Source, e.Content)
	}

	if len(entries) != 3 {
		t.Fatalf("期望 3 条记忆，得到 %d 条（修复前会得到 6 条）", len(entries))
	}

	// 验证第 2 条多行内容完整
	if !strings.Contains(entries[1].Content, "项目说明：") ||
		!strings.Contains(entries[1].Content, "前端用 Vue 3") ||
		!strings.Contains(entries[1].Content, "后端用 Go") ||
		!strings.Contains(entries[1].Content, "数据库用 SQLite") {
		t.Errorf("多行内容不完整: %q", entries[1].Content)
	}

	// 删除多行条目后验证
	if err := fm.DeleteEntry(entries[1].ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	remaining := fm.ParseEntries()
	if len(remaining) != 2 {
		t.Fatalf("删除后期望 2 条，得到 %d 条", len(remaining))
	}
	if !strings.Contains(remaining[0].Content, "简洁风格") {
		t.Errorf("删除后第 1 条不匹配: %q", remaining[0].Content)
	}
	if !strings.Contains(remaining[1].Content, "不要自动提交") {
		t.Errorf("删除后第 2 条不匹配: %q", remaining[1].Content)
	}

	t.Logf("删除多行条目后（%d 条）:", len(remaining))
	for i, e := range remaining {
		t.Logf("  条目 %d: %q", i, e.Content)
	}
}

// ━━━ splitEntryBlocks 单元测试 ━━━

func TestSplitEntryBlocks_SingleLine(t *testing.T) {
	raw := "\n- [16:05] [fact:manual] 单行记忆\n"
	blocks := splitEntryBlocks(raw)
	if len(blocks) != 1 {
		t.Fatalf("期望 1 个块，得到 %d", len(blocks))
	}
	if !strings.Contains(blocks[0].text, "单行记忆") {
		t.Errorf("内容不匹配: %s", blocks[0].text)
	}
}

func TestSplitEntryBlocks_MultiLine(t *testing.T) {
	raw := "\n- [16:05] [fact:manual] 第一行\n第二行\n第三行\n"
	blocks := splitEntryBlocks(raw)
	if len(blocks) != 1 {
		t.Fatalf("期望 1 个块（多行内容应归为一条），得到 %d", len(blocks))
	}
	if !strings.Contains(blocks[0].text, "第一行") {
		t.Errorf("缺少第一行: %s", blocks[0].text)
	}
	if !strings.Contains(blocks[0].text, "第二行") {
		t.Errorf("缺少第二行: %s", blocks[0].text)
	}
	if !strings.Contains(blocks[0].text, "第三行") {
		t.Errorf("缺少第三行: %s", blocks[0].text)
	}
}

func TestSplitEntryBlocks_MultiEntries(t *testing.T) {
	raw := "\n- [16:05] [fact:manual] 条目一\n续行一\n\n- [16:06] [fact:manual] 条目二\n\n- [16:07] [fact:manual] 条目三\n续行三a\n续行三b\n"
	blocks := splitEntryBlocks(raw)
	if len(blocks) != 3 {
		t.Fatalf("期望 3 个块，得到 %d", len(blocks))
	}
	if !strings.Contains(blocks[0].text, "续行一") {
		t.Errorf("条目一缺少续行: %s", blocks[0].text)
	}
	if strings.Contains(blocks[1].text, "续行") {
		t.Errorf("条目二不应有续行: %s", blocks[1].text)
	}
	if !strings.Contains(blocks[2].text, "续行三b") {
		t.Errorf("条目三缺少续行: %s", blocks[2].text)
	}
}

// ━━━ 端到端集成测试 ━━━

func TestMultilineMemory_BugReproduction(t *testing.T) {
	dir := t.TempDir()
	fm := newTestFileMemory(t, dir)

	multilineContent := "第一行\n第二行\n第三行"
	if err := fm.SaveEntry(multilineContent, "fact", "manual"); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	entries := fm.ParseEntries()
	if len(entries) != 1 {
		t.Errorf("多行内容应保存为 1 条记忆，实际得到 %d 条", len(entries))
		for i, e := range entries {
			t.Logf("  条目 %d: %q", i, e.Content)
		}
	}

	if !strings.Contains(entries[0].Content, "第一行") ||
		!strings.Contains(entries[0].Content, "第二行") ||
		!strings.Contains(entries[0].Content, "第三行") {
		t.Errorf("多行内容不完整: %q", entries[0].Content)
	}
}

func TestMultilineMemory_DeletePreservesOthers(t *testing.T) {
	dir := t.TempDir()
	fm := newTestFileMemory(t, dir)

	fm.SaveEntry("单行条目", "fact", "manual")
	fm.SaveEntry("多行一\n多行二\n多行三", "fact", "manual")
	fm.SaveEntry("最后一条", "fact", "manual")

	entries := fm.ParseEntries()
	if len(entries) != 3 {
		t.Fatalf("期望 3 条记忆，得到 %d", len(entries))
	}

	if err := fm.DeleteEntry(entries[1].ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	remaining := fm.ParseEntries()
	if len(remaining) != 2 {
		t.Fatalf("删除后期望 2 条，得到 %d", len(remaining))
	}
	if !strings.Contains(remaining[0].Content, "单行条目") {
		t.Errorf("第一条不匹配: %q", remaining[0].Content)
	}
	if !strings.Contains(remaining[1].Content, "最后一条") {
		t.Errorf("第二条不匹配: %q", remaining[1].Content)
	}
}

func TestMultilineMemory_UpdatePreservesMultiline(t *testing.T) {
	dir := t.TempDir()
	fm := newTestFileMemory(t, dir)

	fm.SaveEntry("旧内容一\n旧内容二", "fact", "manual")

	entries := fm.ParseEntries()
	if len(entries) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(entries))
	}

	if err := fm.UpdateEntry(entries[0].ID, "新内容一\n新内容二\n新内容三"); err != nil {
		t.Fatalf("更新失败: %v", err)
	}

	updated := fm.ParseEntries()
	if len(updated) != 1 {
		t.Fatalf("更新后期望 1 条，得到 %d", len(updated))
	}
	if !strings.Contains(updated[0].Content, "新内容一") {
		t.Errorf("更新后内容不匹配: %q", updated[0].Content)
	}
}

func TestMultilineMemory_ArchiveAndRestore(t *testing.T) {
	dir := t.TempDir()
	fm := newTestFileMemory(t, dir)

	fm.SaveEntry("归档测试\n第二行\n第三行", "fact", "manual")

	entries := fm.ParseEntries()
	if len(entries) != 1 {
		t.Fatalf("期望 1 条，得到 %d", len(entries))
	}

	if err := fm.ArchiveEntry(entries[0].ID); err != nil {
		t.Fatalf("归档失败: %v", err)
	}

	active := fm.ParseEntries()
	if len(active) != 0 {
		t.Errorf("归档后活跃列表应为空，得到 %d", len(active))
	}

	archivePath := filepath.Join(dir, "_global", memoryArchiveFile)
	archiveData, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("读取归档文件失败: %v", err)
	}
	archiveContent := string(archiveData)
	if !strings.Contains(archiveContent, "归档测试") {
		t.Errorf("归档文件缺少内容: %s", archiveContent)
	}
	if !strings.Contains(archiveContent, "第二行") {
		t.Errorf("归档文件缺少续行: %s", archiveContent)
	}
}

// ━━━ 辅助函数 ━━━

func newTestFileMemory(t *testing.T, dir string) *FileMemory {
	t.Helper()
	globalDir := filepath.Join(dir, "_global")
	os.MkdirAll(globalDir, 0755)
	fm, err := New(Options{
		Enabled:   true,
		Dir:       dir,
		MaxMemory: 100,
	})
	if err != nil {
		t.Fatalf("创建 FileMemory 失败: %v", err)
	}
	return fm
}
