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
	"github.com/hexagon-codes/toolkit/util/logger"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	fileutil "github.com/hexagon-codes/toolkit/util/file"
)

const (
	memoryActiveFile  = "MEMORY.md"
	memoryArchiveFile = "MEMORY.archive.md"

	MemoryViewActive   = "active"
	MemoryViewArchived = "archived"
	MemoryViewAll      = "all"

	MemoryStatusActive   = "active"
	MemoryStatusArchived = "archived"

	defaultListLimit = 50
	maxListLimit     = 200
)

// Options 记忆配置选项
type Options struct {
	Enabled   bool   `yaml:"enabled"`    // 是否启用文件记忆
	Dir       string `yaml:"dir"`        // 记忆文件目录，默认 ~/.hexclaw/memory/
	MaxMemory int    `yaml:"max_memory"` // MEMORY.md 最大行数，默认 200
	DailyDays int    `yaml:"daily_days"` // 加载最近几天的日记，默认 2
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
	if err := fileutil.MkdirAll(dir); err != nil {
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

	logger.Info("dir", "dir", dir)
	return fm, nil
}

// LoadContext 加载记忆上下文（向后兼容，只加载 _global）
func (fm *FileMemory) LoadContext() string {
	return fm.LoadContextForRole("")
}

// SaveMemory 保存长期记忆（写入 _global/MEMORY.md）
func (fm *FileMemory) SaveMemory(content string) error {
	return fm.SaveEntry(content, "fact", "manual")
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

// GetMemory 获取 _global/MEMORY.md 全部内容
func (fm *FileMemory) GetMemory() string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.readFileFrom(fm.roleDir(""), memoryActiveFile)
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

	// 收集要搜索的目录：根目录 + 一级子目录（_global, role dirs）
	dirs := []struct{ dir, prefix string }{{fm.dir, ""}}
	if topEntries, err := os.ReadDir(fm.dir); err == nil {
		for _, e := range topEntries {
			if e.IsDir() {
				dirs = append(dirs, struct{ dir, prefix string }{filepath.Join(fm.dir, e.Name()), e.Name() + "/"})
			}
		}
	}

	for _, d := range dirs {
		entries, err := os.ReadDir(d.dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
				continue
			}
			content := fm.readFileFrom(d.dir, entry.Name())
			if content == "" {
				continue
			}
			for lineNum, line := range strings.Split(content, "\n") {
				lineLower := strings.ToLower(line)
				matchCount := 0
				for _, kw := range keywords {
					if strings.Contains(lineLower, kw) {
						matchCount++
					}
				}
				if matchCount > 0 {
					results = append(results, SearchResult{
						File:    d.prefix + entry.Name(),
						Line:    lineNum + 1,
						Content: line,
						Score:   float64(matchCount) / float64(len(keywords)),
					})
				}
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})
	const maxResults = 100
	if len(results) > maxResults {
		results = results[:maxResults]
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

// MemoryEntry 结构化记忆条目（从 MEMORY.md 行解析而来）
type MemoryEntry struct {
	ID         string `json:"id"`         // 行号 hash，如 "m-7"
	Content    string `json:"content"`    // 记忆正文
	Type       string `json:"type"`       // identity/preference/fact/instruction/context
	Source     string `json:"source"`     // manual/chat_explicit/chat_extract/system
	CreatedAt  string `json:"created_at"` // ISO 时间
	UpdatedAt  string `json:"updated_at"` // ISO 时间
	HitCount   int    `json:"hit_count"`  // 命中次数（预留）
	Status     string `json:"status"`     // active/archived
	ArchivedAt string `json:"archived_at,omitempty"`
}

// MemoryCapacity 容量信息
type MemoryCapacity struct {
	Used     int `json:"used"`
	Max      int `json:"max"`
	Archived int `json:"archived,omitempty"`
}

// ListOptions 记忆列表查询选项。
type ListOptions struct {
	View   string
	Limit  int
	Cursor string
	Type   string
	Source string
}

// ListResult 记忆列表分页结果。
type ListResult struct {
	Entries    []MemoryEntry `json:"entries"`
	Total      int           `json:"total"`
	NextCursor string        `json:"next_cursor,omitempty"`
	HasMore    bool          `json:"has_more"`
}

// ParseEntries 将 _global 的 MEMORY.md 解析为结构化条目列表（向后兼容）
func (fm *FileMemory) ParseEntries() []MemoryEntry {
	return fm.parseEntriesFromDir(fm.roleDir(""))
}

// Capacity 返回 _global 的容量信息（向后兼容）
func (fm *FileMemory) Capacity() MemoryCapacity {
	entries := fm.parseEntriesFromDir(fm.roleDir(""))
	archived := fm.parseEntriesFromFile(fm.roleDir(""), memoryArchiveFile, MemoryStatusArchived, "a")
	return MemoryCapacity{
		Used:     len(entries),
		Max:      fm.config.MaxMemory,
		Archived: len(archived),
	}
}

// CapacityForRole 返回合并后的容量信息
func (fm *FileMemory) CapacityForRole(role string) MemoryCapacity {
	entries := fm.ParseEntriesForRole(role)
	archived := fm.parseEntriesFromFile(fm.roleDir(""), memoryArchiveFile, MemoryStatusArchived, "a")
	if role != "" {
		archived = append(archived, fm.parseEntriesFromFile(fm.roleDir(role), memoryArchiveFile, MemoryStatusArchived, "a")...)
	}
	return MemoryCapacity{
		Used:     len(entries),
		Max:      fm.config.MaxMemory,
		Archived: len(archived),
	}
}

// ListEntries 返回 _global 记忆列表。默认只返回活跃记忆，归档记忆需要显式 view=archived。
func (fm *FileMemory) ListEntries(opts ListOptions) (ListResult, error) {
	view := opts.View
	if view == "" {
		view = MemoryViewActive
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	start := 0
	if opts.Cursor != "" {
		n, err := strconv.Atoi(opts.Cursor)
		if err != nil || n < 0 {
			return ListResult{}, fmt.Errorf("无效的 cursor: %s", opts.Cursor)
		}
		start = n
	}

	globalDir := fm.roleDir("")
	var entries []MemoryEntry
	switch view {
	case MemoryViewActive:
		entries = fm.parseEntriesFromDir(globalDir)
	case MemoryViewArchived:
		entries = fm.parseEntriesFromFile(globalDir, memoryArchiveFile, MemoryStatusArchived, "a")
	case MemoryViewAll:
		entries = append(entries, fm.parseEntriesFromDir(globalDir)...)
		entries = append(entries, fm.parseEntriesFromFile(globalDir, memoryArchiveFile, MemoryStatusArchived, "a")...)
	default:
		return ListResult{}, fmt.Errorf("无效的 view: %s", view)
	}
	entries = filterMemoryEntries(entries, opts)

	total := len(entries)
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	result := ListResult{
		Entries: entries[start:end],
		Total:   total,
		HasMore: end < total,
	}
	if result.HasMore {
		result.NextCursor = strconv.Itoa(end)
	}
	if result.Entries == nil {
		result.Entries = []MemoryEntry{}
	}
	return result, nil
}

func filterMemoryEntries(entries []MemoryEntry, opts ListOptions) []MemoryEntry {
	if opts.Type == "" && opts.Source == "" {
		return entries
	}
	filtered := make([]MemoryEntry, 0, len(entries))
	for _, entry := range entries {
		if opts.Type != "" && entry.Type != opts.Type {
			continue
		}
		if opts.Source != "" && entry.Source != opts.Source {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

// SaveEntry 保存带元数据的记忆条目
//
// 写入前执行：1) 语义去重  2) 容量淘汰
// role 为空时写入 _global，否则写入 {role} 子目录
func (fm *FileMemory) SaveEntry(content, memType, source string) error {
	return fm.SaveEntryForRole(content, memType, source, "")
}

// SaveEntryForRole 保存记忆到指定角色的目录
func (fm *FileMemory) SaveEntryForRole(content, memType, source, role string) error {
	if memType == "" {
		memType = "fact"
	}
	if source == "" {
		source = "manual"
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	// 全局类型（identity/preference/instruction）始终写入 _global
	targetDir := fm.roleDir(role)
	if memType == "identity" || memType == "preference" || memType == "instruction" {
		targetDir = fm.roleDir("")
	}

	// Fix: 在同一把写锁内完成去重检查、容量淘汰、追加写入，
	// 消除 isDuplicate/evictIfNeeded 与写入之间的 TOCTOU 竞态。
	fm.mu.Lock()
	defer fm.mu.Unlock()

	// 1. 语义去重（在写锁内直接检查，不调用 isDuplicate 避免死锁）
	if fm.isDuplicateUnlocked(targetDir, content) {
		return nil // 静默跳过重复
	}

	// 2. 容量淘汰（在写锁内直接执行，不调用 evictIfNeeded 避免死锁）
	fm.evictIfNeededUnlocked(targetDir)

	path := filepath.Join(targetDir, memoryActiveFile)
	if err := fileutil.MkdirAll(targetDir); err != nil {
		return fmt.Errorf("创建记忆目录失败: %w", err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("打开记忆文件失败: %w", err)
	}
	defer f.Close()

	timestamp := time.Now().Format("15:04")
	entry := fmt.Sprintf("\n- [%s] [%s:%s] %s\n", timestamp, memType, source, content)
	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("写入记忆失败: %w", err)
	}
	return nil
}

// ─── 工作区隔离 ──────────────────────────────────────────

// roleDir 返回指定角色的记忆目录（空 role 返回 _global）
func (fm *FileMemory) roleDir(role string) string {
	if role == "" {
		return filepath.Join(fm.dir, "_global")
	}
	// 安全处理：只取 basename，防止路径穿越
	safe := filepath.Base(strings.ReplaceAll(role, " ", "-"))
	return filepath.Join(fm.dir, safe)
}

// ParseEntriesForRole 解析指定角色的记忆（_global + role 合并）
func (fm *FileMemory) ParseEntriesForRole(role string) []MemoryEntry {
	globalEntries := fm.parseEntriesFromDir(fm.roleDir(""))
	if role == "" {
		return globalEntries
	}
	roleEntries := fm.parseEntriesFromDir(fm.roleDir(role))
	// role entries 的 ID 加前缀避免与 global 冲突
	for i := range roleEntries {
		roleEntries[i].ID = "r-" + roleEntries[i].ID[2:] // m-N → r-N
	}
	return append(globalEntries, roleEntries...)
}

// LoadContextForRole 加载指定角色的记忆上下文
func (fm *FileMemory) LoadContextForRole(role string) string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()

	var sb strings.Builder

	// 全局记忆
	globalMem := fm.readFileFrom(fm.roleDir(""), memoryActiveFile)
	if globalMem != "" {
		lines := strings.Split(globalMem, "\n")
		if len(lines) > fm.config.MaxMemory {
			lines = lines[:fm.config.MaxMemory]
		}
		sb.WriteString("## 长期记忆\n\n")
		sb.WriteString(strings.Join(lines, "\n"))
		sb.WriteString("\n\n")
	}

	// 角色专属记忆
	if role != "" {
		roleMem := fm.readFileFrom(fm.roleDir(role), memoryActiveFile)
		if roleMem != "" {
			sb.WriteString(fmt.Sprintf("## 角色记忆 (%s)\n\n", role))
			sb.WriteString(roleMem)
			sb.WriteString("\n\n")
		}
	}

	// 日记（不区分角色）
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

func (fm *FileMemory) readFileFrom(dir, filename string) string {
	path := filepath.Join(dir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ─── 语义去重 ────────────────────────────────────────────

// isDuplicate 检查新内容是否与已有记忆高度相似（带 RLock，供外部调用）
func (fm *FileMemory) isDuplicate(dir, content string) bool {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.isDuplicateUnlocked(dir, content)
}

// isDuplicateUnlocked 检查新内容是否与已有记忆高度相似（调用方已持有锁）
func (fm *FileMemory) isDuplicateUnlocked(dir, content string) bool {
	raw := fm.readFileFrom(dir, memoryActiveFile)

	if raw == "" {
		return false
	}

	contentLower := strings.ToLower(strings.TrimSpace(content))
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 提取纯内容部分
		existing := extractContentFromLine(line)
		if existing == "" {
			continue
		}
		existingLower := strings.ToLower(existing)

		// 完全相同
		if contentLower == existingLower {
			return true
		}
		// 包含关系 (一条是另一条的子串)
		if strings.Contains(contentLower, existingLower) || strings.Contains(existingLower, contentLower) {
			return true
		}
		// Jaccard 相似度 > 0.8
		if jaccardSimilarity(contentLower, existingLower) > 0.8 {
			return true
		}
	}
	return false
}

// extractContentFromLine 从记忆行中提取纯内容（去掉 - [HH:MM] [type:source] 前缀）
func extractContentFromLine(line string) string {
	if strings.HasPrefix(line, "- ") {
		line = line[2:]
	}
	// 跳过 [HH:MM]
	if strings.HasPrefix(line, "[") {
		if idx := strings.Index(line, "]"); idx > 0 && idx <= 6 {
			line = strings.TrimSpace(line[idx+1:])
		}
	}
	// 跳过 [type:source]
	if strings.HasPrefix(line, "[") {
		if idx := strings.Index(line, "]"); idx > 0 {
			meta := line[1:idx]
			if parts := strings.SplitN(meta, ":", 2); len(parts) == 2 {
				line = strings.TrimSpace(line[idx+1:])
			}
		}
	}
	return line
}

// jaccardSimilarity 基于词集的 Jaccard 相似度
func jaccardSimilarity(a, b string) float64 {
	setA := make(map[string]struct{})
	for _, w := range strings.Fields(a) {
		setA[w] = struct{}{}
	}
	setB := make(map[string]struct{})
	for _, w := range strings.Fields(b) {
		setB[w] = struct{}{}
	}
	if len(setA) == 0 && len(setB) == 0 {
		return 1.0
	}
	intersection := 0
	for w := range setA {
		if _, ok := setB[w]; ok {
			intersection++
		}
	}
	union := len(setA) + len(setB) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// ─── 容量淘汰（加权评分） ────────────────────────────────

// evictIfNeeded 当容量已满时将评分最低的条目降级到归档（带锁，供外部调用）。
func (fm *FileMemory) evictIfNeeded(dir string) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.evictIfNeededUnlocked(dir)
}

// evictIfNeededUnlocked 当容量已满时将评分最低的条目降级到归档（调用方已持有写锁）。
func (fm *FileMemory) evictIfNeededUnlocked(dir string) {
	entries := fm.parseEntriesFromDirUnlocked(dir)
	if len(entries) < fm.config.MaxMemory {
		return
	}

	// 找评分最低的条目（instruction 类不淘汰）
	lowestScore := float64(1e9)
	lowestIdx := -1
	now := time.Now()

	for _, e := range entries {
		if e.Type == "instruction" {
			continue // instruction 永不淘汰
		}

		score := entryEvictionScore(e, now)
		if score < lowestScore {
			lowestScore = score
			lowestIdx = parseEntryLineIndex(e.ID)
		}
	}

	if lowestIdx < 0 {
		return // 全是 instruction，无法淘汰
	}

	_ = fm.moveEntryLineUnlocked(dir, memoryActiveFile, memoryArchiveFile, lowestIdx)
}

// entryEvictionScore 计算条目的保留评分（越高越不该被淘汰）
func entryEvictionScore(e MemoryEntry, now time.Time) float64 {
	// hit_count 权重 0.3
	hitScore := float64(e.HitCount) * 0.3

	// recency 权重 0.3 (距今天数的倒数，越新越高)
	created, err := time.Parse(time.RFC3339, e.CreatedAt)
	recencyScore := 0.3 // 默认
	if err == nil {
		daysSince := now.Sub(created).Hours() / 24
		if daysSince < 1 {
			daysSince = 1
		}
		recencyScore = (1.0 / daysSince) * 0.3
	}

	// type 权重 0.2
	typeWeights := map[string]float64{
		"identity":   0.6,
		"preference": 0.4,
		"fact":       0.2,
		"context":    0.0,
	}
	typeScore := typeWeights[e.Type] * 0.2

	// source 权重 0.2
	sourceWeights := map[string]float64{
		"manual":        0.6,
		"chat_explicit": 0.4,
		"chat_extract":  0.2,
		"system":        0.0,
	}
	sourceScore := sourceWeights[e.Source] * 0.2

	return hitScore + recencyScore + typeScore + sourceScore
}

// parseEntriesFromDir 从指定目录解析活跃 MEMORY.md（带 RLock）
func (fm *FileMemory) parseEntriesFromDir(dir string) []MemoryEntry {
	return fm.parseEntriesFromFile(dir, memoryActiveFile, MemoryStatusActive, "m")
}

// parseEntriesFromDirUnlocked 从指定目录解析活跃 MEMORY.md（调用方已持有锁）
func (fm *FileMemory) parseEntriesFromDirUnlocked(dir string) []MemoryEntry {
	return fm.parseEntriesFromFileUnlocked(dir, memoryActiveFile, MemoryStatusActive, "m")
}

func (fm *FileMemory) parseEntriesFromFile(dir, filename, status, idPrefix string) []MemoryEntry {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.parseEntriesFromFileUnlocked(dir, filename, status, idPrefix)
}

// parseEntriesFromFileUnlocked 解析记忆文件（调用方已持有锁）。
// Fix 8: 使用文件 ModTime 作为无显式时间戳条目的默认日期，避免全部设为 today 破坏时间衰减淘汰。
func (fm *FileMemory) parseEntriesFromFileUnlocked(dir, filename, status, idPrefix string) []MemoryEntry {
	filePath := filepath.Join(dir, filename)
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil
	}
	raw := string(data)
	if raw == "" {
		return nil
	}

	// Fix 8: 使用文件修改时间作为默认日期（而非 time.Now()），
	// 避免所有无显式时间戳的条目都被当成"今天"创建的，破坏时间衰减淘汰。
	defaultDate := time.Now().Format("2006-01-02")
	if info, statErr := os.Stat(filePath); statErr == nil {
		defaultDate = info.ModTime().Format("2006-01-02")
	}

	lines := strings.Split(raw, "\n")
	var entries []MemoryEntry

	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		e := MemoryEntry{
			ID:        fmt.Sprintf("%s-%d", idPrefix, i),
			Type:      "fact",
			Source:    "manual",
			CreatedAt: defaultDate + "T00:00:00Z",
			UpdatedAt: defaultDate + "T00:00:00Z",
			Status:    status,
		}
		if status == MemoryStatusArchived {
			e.ArchivedAt = e.UpdatedAt
		}

		if strings.HasPrefix(line, "- ") {
			line = line[2:]
		}
		if strings.HasPrefix(line, "[") {
			if idx := strings.Index(line, "]"); idx > 0 && idx <= 6 {
				ts := line[1:idx]
				line = strings.TrimSpace(line[idx+1:])
				e.CreatedAt = defaultDate + "T" + ts + ":00Z"
				e.UpdatedAt = e.CreatedAt
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

func (fm *FileMemory) moveEntryLine(dir, fromFile, toFile string, lineIdx int) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.moveEntryLineUnlocked(dir, fromFile, toFile, lineIdx)
}

// moveEntryLineUnlocked 将一行从 fromFile 移动到 toFile（调用方已持有写锁）。
//
// Fix 1: 使用 write-to-temp-then-rename 模式保证原子性：
//  1. 先将目标文件（含新行）写到临时文件，rename 到目标路径（原子）
//  2. 再更新源文件（删除该行）
//
// 如果步骤 2 失败，条目同时存在于两个文件中（重复优于丢失）。
func (fm *FileMemory) moveEntryLineUnlocked(dir, fromFile, toFile string, lineIdx int) error {
	fromPath := filepath.Join(dir, fromFile)
	raw, err := os.ReadFile(fromPath)
	if err != nil {
		return fmt.Errorf("读取记忆文件失败: %w", err)
	}
	lines := strings.Split(string(raw), "\n")
	if lineIdx < 0 || lineIdx >= len(lines) {
		return fmt.Errorf("记忆条目不存在")
	}

	line := lines[lineIdx]
	if strings.TrimSpace(line) == "" {
		return fmt.Errorf("记忆条目不存在")
	}

	if err := fileutil.MkdirAll(dir); err != nil {
		return fmt.Errorf("创建记忆目录失败: %w", err)
	}

	// Step 1: 将目标文件内容 + 新行写到临时文件，然后 rename（原子操作）
	toPath := filepath.Join(dir, toFile)
	existingDest, _ := os.ReadFile(toPath) // 可能不存在，忽略错误
	newDest := string(existingDest) + strings.TrimRight(line, "\n") + "\n"

	tmpPath := toPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(newDest), 0644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, toPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("原子替换目标文件失败: %w", err)
	}

	// Step 2: 更新源文件（删除该行）
	// 如果此步失败，条目存在于两个文件中（重复优于丢失）
	lines = append(lines[:lineIdx], lines[lineIdx+1:]...)
	if err := os.WriteFile(fromPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return fmt.Errorf("更新记忆文件失败: %w", err)
	}
	return nil
}

// UpdateEntry 更新指定 ID 的记忆条目内容
//
// Fix 4: 根据 ID 前缀确定正确的目录，而非总是使用 _global。
// "r-" 前缀的条目需要搜索角色目录。
func (fm *FileMemory) UpdateEntry(id, content string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	filename, lineIdx := parseEntryFileAndLine(id)
	if lineIdx < 0 {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}

	dir := fm.resolveEntryDir(id)
	path := filepath.Join(dir, filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取记忆文件失败: %w", err)
	}

	lines := strings.Split(string(raw), "\n")
	if lineIdx < 0 || lineIdx >= len(lines) {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}

	// 保留原行的时间戳和元数据前缀，只替换内容
	oldLine := strings.TrimSpace(lines[lineIdx])
	newLine := rebuildEntryLine(oldLine, strings.TrimSpace(content))
	lines[lineIdx] = newLine

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// DeleteEntry 删除指定 ID 的记忆条目
//
// Fix 4: 根据 ID 前缀确定正确的目录，而非总是使用 _global。
func (fm *FileMemory) DeleteEntry(id string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	filename, lineIdx := parseEntryFileAndLine(id)
	if lineIdx < 0 {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}

	dir := fm.resolveEntryDir(id)
	path := filepath.Join(dir, filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取记忆文件失败: %w", err)
	}

	lines := strings.Split(string(raw), "\n")
	if lineIdx < 0 || lineIdx >= len(lines) {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}

	// 删除该行
	lines = append(lines[:lineIdx], lines[lineIdx+1:]...)

	return os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// ArchiveEntry 将活跃记忆降级到归档。
func (fm *FileMemory) ArchiveEntry(id string) error {
	filename, lineIdx := parseEntryFileAndLine(id)
	if filename != memoryActiveFile || lineIdx < 0 {
		return fmt.Errorf("只能归档活跃记忆: %s", id)
	}
	return fm.moveEntryLine(fm.roleDir(""), memoryActiveFile, memoryArchiveFile, lineIdx)
}

// RestoreEntry 将归档记忆恢复为活跃记忆。
func (fm *FileMemory) RestoreEntry(id string) error {
	filename, lineIdx := parseEntryFileAndLine(id)
	if filename != memoryArchiveFile || lineIdx < 0 {
		return fmt.Errorf("只能恢复归档记忆: %s", id)
	}
	fm.evictIfNeeded(fm.roleDir(""))
	return fm.moveEntryLine(fm.roleDir(""), memoryArchiveFile, memoryActiveFile, lineIdx)
}

// parseEntryLineIndex 从 "m-7" 格式的 ID 解析出行号
func parseEntryLineIndex(id string) int {
	_, idx := parseEntryFileAndLine(id)
	return idx
}

func parseEntryFileAndLine(id string) (string, int) {
	var prefix string
	switch {
	case strings.HasPrefix(id, "m-"):
		prefix = "m-"
	case strings.HasPrefix(id, "a-"):
		prefix = "a-"
	case strings.HasPrefix(id, "r-"):
		prefix = "r-"
	default:
		return "", -1
	}

	var n int
	if _, err := fmt.Sscanf(id[len(prefix):], "%d", &n); err != nil {
		return "", -1
	}
	if prefix == "a-" {
		return memoryArchiveFile, n
	}
	return memoryActiveFile, n
}

// resolveEntryDir 根据 ID 前缀确定记忆条目所在的目录。
//
// Fix 4: "r-" 前缀表示角色目录中的条目，需搜索所有角色子目录。
// "m-" 和 "a-" 前缀的条目在 _global 目录中。
func (fm *FileMemory) resolveEntryDir(id string) string {
	if strings.HasPrefix(id, "r-") {
		// 搜索所有角色子目录，找到包含该行号的目录
		_, lineIdx := parseEntryFileAndLine(id)
		topEntries, err := os.ReadDir(fm.dir)
		if err == nil {
			for _, e := range topEntries {
				if !e.IsDir() || e.Name() == "_global" {
					continue
				}
				dir := filepath.Join(fm.dir, e.Name())
				data, readErr := os.ReadFile(filepath.Join(dir, memoryActiveFile))
				if readErr != nil {
					continue
				}
				lines := strings.Split(string(data), "\n")
				if lineIdx >= 0 && lineIdx < len(lines) && strings.TrimSpace(lines[lineIdx]) != "" {
					return dir
				}
			}
		}
	}
	return fm.roleDir("")
}

// rebuildEntryLine 保留行的时间戳和元数据，替换内容部分
func rebuildEntryLine(oldLine, newContent string) string {
	line := oldLine
	prefix := ""

	// 保留 "- " 前缀
	if strings.HasPrefix(line, "- ") {
		prefix = "- "
		line = line[2:]
	}

	// 保留 [HH:MM]
	if strings.HasPrefix(line, "[") {
		if idx := strings.Index(line, "]"); idx > 0 && idx <= 6 {
			prefix += line[:idx+1] + " "
			line = strings.TrimSpace(line[idx+1:])
		}
	}

	// 保留 [type:source]
	if strings.HasPrefix(line, "[") {
		if idx := strings.Index(line, "]"); idx > 0 {
			meta := line[1:idx]
			if parts := strings.SplitN(meta, ":", 2); len(parts) == 2 {
				prefix += line[:idx+1] + " "
			}
		}
	}

	return prefix + newContent
}

// UpdateMemory 替换 MEMORY.md 全部内容
func (fm *FileMemory) UpdateMemory(content string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	dir := fm.roleDir("")
	_ = fileutil.MkdirAll(dir)
	path := filepath.Join(dir, memoryActiveFile)
	return os.WriteFile(path, []byte(content), 0644)
}

// ClearAll 清空所有记忆文件
func (fm *FileMemory) ClearAll() error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	if err := filepath.WalkDir(fm.dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("删除文件 %s 失败: %w", path, err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("清空记忆目录失败: %w", err)
	}
	return nil
}

// DeleteFile 删除指定记忆文件
func (fm *FileMemory) DeleteFile(filename string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	// 安全检查：只允许删除 .md 文件，不允许路径穿越
	if filename == "" || strings.Contains(filename, "..") ||
		strings.ContainsAny(filename, "/\\") || filepath.Base(filename) != filename {
		return fmt.Errorf("不安全的文件名: %s", filename)
	}
	if !strings.HasSuffix(filename, ".md") {
		return fmt.Errorf("只能删除 .md 文件")
	}

	path := filepath.Join(fm.dir, filename)
	// 二次验证：确保最终路径在记忆目录内
	absPath, _ := filepath.Abs(path)
	absDir, _ := filepath.Abs(fm.dir)
	if !strings.HasPrefix(absPath, filepath.Clean(absDir)+string(filepath.Separator)) {
		return fmt.Errorf("路径越界: %s", filename)
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("文件不存在: %s", filename)
	}
	return os.Remove(path)
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
