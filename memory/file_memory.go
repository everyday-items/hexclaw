// Package memory 提供文件驱动的记忆系统
//
// 对标 OpenClaw 的 MEMORY.md + 每日日记机制：
//   - MEMORY.md: 长期记忆（用户偏好、项目约定、关键决策）
//   - YYYY-MM-DD.md: 每日日记（当日活动、决策、上下文）
//
// 核心设计理念："文件即记忆，磁盘即真相"
//   - 所有记忆以 Markdown 文件存储，可人工审查和编辑
//   - 支持 Git 版本控制
//   - Agent 可以读写记忆文件
//   - 启动时自动加载 MEMORY.md + 最近两天的日记
//
// 这些记忆会被注入到 Agent 的 system prompt 中，
// 让 Agent 具有跨会话的持久记忆能力。
package memory

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Options 记忆配置选项
type Options struct {
	Enabled    bool   `yaml:"enabled"`     // 是否启用文件记忆
	Dir        string `yaml:"dir"`         // 记忆文件目录，默认 ~/.hexclaw/memory/
	MaxMemory  int    `yaml:"max_memory"`  // MEMORY.md 最大行数，默认 200
	DailyDays  int    `yaml:"daily_days"`  // 加载最近几天的日记，默认 2
}

// FileMemory 文件驱动的记忆系统
//
// 管理 MEMORY.md 长期记忆和每日日记文件。
// 提供记忆的读、写、搜索能力。
type FileMemory struct {
	mu     sync.RWMutex
	config Options
	dir    string // 记忆目录绝对路径
}

// New 创建文件记忆系统
func New(cfg Options) (*FileMemory, error) {
	dir := cfg.Dir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("获取用户主目录失败: %w", err)
		}
		dir = filepath.Join(home, ".hexclaw", "memory")
	}

	// 展开 ~
	if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	}

	// 确保目录存在
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建记忆目录失败: %w", err)
	}

	if cfg.MaxMemory <= 0 {
		cfg.MaxMemory = 200
	}
	if cfg.DailyDays <= 0 {
		cfg.DailyDays = 2
	}

	fm := &FileMemory{
		config: cfg,
		dir:    dir,
	}

	log.Printf("文件记忆系统已初始化: %s", dir)
	return fm, nil
}

// LoadContext 加载记忆上下文
//
// 加载 MEMORY.md + 最近几天的日记，拼接为可注入 system prompt 的字符串。
// 这是启动时和每次会话开始时调用的核心方法。
func (fm *FileMemory) LoadContext() string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	var sb strings.Builder

	// 加载 MEMORY.md
	memoryContent := fm.readFile("MEMORY.md")
	if memoryContent != "" {
		// 截断到最大行数
		lines := strings.Split(memoryContent, "\n")
		if len(lines) > fm.config.MaxMemory {
			lines = lines[:fm.config.MaxMemory]
		}
		sb.WriteString("## 长期记忆\n\n")
		sb.WriteString(strings.Join(lines, "\n"))
		sb.WriteString("\n\n")
	}

	// 加载最近几天的日记
	now := time.Now()
	for i := 0; i < fm.config.DailyDays; i++ {
		date := now.AddDate(0, 0, -i)
		filename := date.Format("2006-01-02") + ".md"
		content := fm.readFile(filename)
		if content != "" {
			label := "今天"
			if i == 1 {
				label = "昨天"
			} else if i > 1 {
				label = fmt.Sprintf("%d天前", i)
			}
			sb.WriteString(fmt.Sprintf("## 日记 (%s %s)\n\n", label, date.Format("2006-01-02")))
			sb.WriteString(content)
			sb.WriteString("\n\n")
		}
	}

	return sb.String()
}

// SaveMemory 保存长期记忆
//
// 追加内容到 MEMORY.md。Agent 在对话中学到的用户偏好、
// 重要决策等信息应该保存到这里。
func (fm *FileMemory) SaveMemory(content string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	return fm.appendFile("MEMORY.md", content)
}

// SaveDaily 保存每日日记
//
// 追加内容到当天的日记文件。记录当日的活动、决策、上下文。
// 每天自动创建新文件。
func (fm *FileMemory) SaveDaily(content string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	filename := time.Now().Format("2006-01-02") + ".md"
	return fm.appendFile(filename, content)
}

// GetMemory 获取 MEMORY.md 全部内容
func (fm *FileMemory) GetMemory() string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.readFile("MEMORY.md")
}

// GetDaily 获取指定日期的日记
func (fm *FileMemory) GetDaily(date time.Time) string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	filename := date.Format("2006-01-02") + ".md"
	return fm.readFile(filename)
}

// Search 搜索记忆文件
//
// 在所有记忆文件中搜索包含关键词的行。
// 返回匹配的行及其来源文件名。
func (fm *FileMemory) Search(query string) []SearchResult {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	keywords := strings.Fields(strings.ToLower(query))
	if len(keywords) == 0 {
		return nil
	}

	var results []SearchResult

	// 遍历目录中的所有 .md 文件
	entries, err := os.ReadDir(fm.dir)
	if err != nil {
		return nil
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}

		content := fm.readFile(entry.Name())
		if content == "" {
			continue
		}

		lines := strings.Split(content, "\n")
		for lineNum, line := range lines {
			lineLower := strings.ToLower(line)
			matchCount := 0
			for _, kw := range keywords {
				if strings.Contains(lineLower, kw) {
					matchCount++
				}
			}
			if matchCount > 0 {
				results = append(results, SearchResult{
					File:    entry.Name(),
					Line:    lineNum + 1,
					Content: line,
					Score:   float64(matchCount) / float64(len(keywords)),
				})
			}
		}
	}

	// 按分数降序排序
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Score > results[j-1].Score; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}

	return results
}

// SearchResult 记忆搜索结果
type SearchResult struct {
	File    string  `json:"file"`    // 文件名
	Line    int     `json:"line"`    // 行号
	Content string  `json:"content"` // 匹配的行内容
	Score   float64 `json:"score"`   // 匹配分数 (0-1)
}

// Dir 返回记忆目录路径
func (fm *FileMemory) Dir() string {
	return fm.dir
}

// --- 内部方法 ---

// readFile 读取记忆文件
func (fm *FileMemory) readFile(filename string) string {
	path := filepath.Join(fm.dir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// appendFile 追加内容到记忆文件
func (fm *FileMemory) appendFile(filename, content string) error {
	path := filepath.Join(fm.dir, filename)
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开记忆文件失败: %w", err)
	}
	defer f.Close()

	// 添加时间戳
	timestamp := time.Now().Format("15:04")
	entry := fmt.Sprintf("\n- [%s] %s\n", timestamp, content)

	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("写入记忆失败: %w", err)
	}

	return nil
}
