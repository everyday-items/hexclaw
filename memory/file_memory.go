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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/memory/recall"
	fileutil "github.com/hexagon-codes/toolkit/util/file"
	"github.com/hexagon-codes/toolkit/util/logger"
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

// entryBlock 表示文件中一条记忆条目的行范围。
//
// 一条条目以 "- [HH:MM]" 开头，后续不以此模式开头的非空行属于同一条目。
// startLine 和 endLine 为文件行号（含 startLine，不含 endLine）。
type entryBlock struct {
	startLine int    // 起始行号（含）
	endLine   int    // 结束行号（不含）
	text      string // 完整文本（多行拼接，含首行前缀）
}

// splitEntryBlocks 将文件按记忆条目分块。
//
// 规则：以 "- [HH:MM]" 开头的行是新条目的开始，
// 后续行如果不匹配此模式则属于上一条目。
// 兼容旧格式：不以 "- [" 开头的独立行视为独立条目。
func splitEntryBlocks(raw string) []entryBlock {
	lines := strings.Split(raw, "\n")
	var blocks []entryBlock
	var current *entryBlock

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if isEntryHeader(trimmed) {
			// 关闭上一个块
			if current != nil {
				current.endLine = i
				blocks = append(blocks, *current)
			}
			current = &entryBlock{startLine: i, text: trimmed}
		} else if current != nil {
			// 续行：追加到当前块
			current.text += "\n" + trimmed
		} else {
			// 不以 "- [HH:MM]" 开头的独立行（兼容旧格式）
			blocks = append(blocks, entryBlock{startLine: i, endLine: i + 1, text: trimmed})
		}
	}
	if current != nil {
		current.endLine = len(lines)
		blocks = append(blocks, *current)
	}
	return blocks
}

// isEntryHeader 判断一行是否为条目起始（以 "- [HH:MM]" 开头）
func isEntryHeader(line string) bool {
	if !strings.HasPrefix(line, "- [") {
		return false
	}
	// 匹配 "- [H:MM]" 或 "- [HH:MM]"
	rest := line[3:] // 去掉 "- ["
	idx := strings.Index(rest, "]")
	if idx < 0 || idx > 5 {
		return false
	}
	ts := rest[:idx]
	return strings.Contains(ts, ":")
}

// Options 记忆配置选项
type Options struct {
	Enabled    bool   `yaml:"enabled"`     // 是否启用文件记忆
	Dir        string `yaml:"dir"`         // 记忆文件目录，默认 ~/.hexclaw/memory/
	MaxMemory  int    `yaml:"max_memory"`  // MEMORY.md 最大行数，默认 200
	DailyDays  int    `yaml:"daily_days"`  // 加载最近几天的日记，默认 2
	ArchiveMax int    `yaml:"archive_max"` // 归档文件保留最近 N 条（修缺陷B 无界增长），默认 2000；<=0 用默认
}

// defaultArchiveMax 归档保留上限（修缺陷B）：evict/反思归档只追加、原本永不裁剪 → 单调增长 +
// Capacity/列表全量解析延迟恶化。保留最近 N 条（环形），更老的硬删。
const defaultArchiveMax = 2000

// FileMemory 文件驱动的记忆系统
//
// 管理 MEMORY.md 长期记忆和每日日记文件。
// 提供记忆的读、写、搜索能力。
type FileMemory struct {
	mu     sync.RWMutex
	config Options
	dir    string // 记忆目录绝对路径

	// 深度整合（"做梦"）阶段（dreaming.go）：opt-in、默认关。
	consolidator ConsolidateLLM // nil → 深度阶段降级为 no-op（仅机械反思运行）
	dreamOpts    *DreamOptions  // nil → DefaultDreamOptions（调参）

	// 低频后台相墙钟（scheduled_phase.go）：持久化 last_run，支持开机补跑。
	clock *phaseClock
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
	if cfg.ArchiveMax <= 0 {
		cfg.ArchiveMax = defaultArchiveMax
	}

	fm := &FileMemory{
		config: cfg,
		dir:    dir,
		clock:  newPhaseClock(dir),
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

// GetMemoryForPrompt 返回 _global 记忆、但逐行剥掉存储层内联 meta 标签（<!--m:eid/pin/vt…-->）。
// 注入 LLM（抽取提示/上下文）一律用本方法而非裸 GetMemory：标签是存储制品，对模型是噪声且白耗 token
// （缺陷H 后每条都带 eid 标签，问题从"部分结构化条目"放大到"全部"）。保留 时间/类型 前缀与正文。
func (fm *FileMemory) GetMemoryForPrompt() string {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return stripInlineMetaLines(fm.readFileFrom(fm.roleDir(""), memoryActiveFile))
}

// stripInlineMetaLines 逐行剥掉行尾内联 meta 标签，保留前缀与正文。无标签则原样返回。
func stripInlineMetaLines(raw string) string {
	if !strings.Contains(raw, entryMetaPrefix) {
		return raw
	}
	lines := strings.Split(raw, "\n")
	for i, ln := range lines {
		pure, _ := splitEntryMeta(ln)
		lines[i] = pure
	}
	return strings.Join(lines, "\n")
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
	ID         string `json:"id"`         // 稳定内容寻址 ID（如 "e1a2b3…"，缺陷H）；旧条目无内联 ID → 回退行号 ID（如 "m-7"）
	Content    string `json:"content"`    // 记忆正文
	Type       string `json:"type"`       // identity/preference/fact/instruction/context
	Source     string `json:"source"`     // manual/chat_explicit/chat_extract/system
	CreatedAt  string `json:"created_at"` // ISO 时间
	UpdatedAt  string `json:"updated_at"` // ISO 时间
	HitCount   int    `json:"hit_count"`  // 命中次数（预留）
	Status     string `json:"status"`     // active/archived
	ArchivedAt string `json:"archived_at,omitempty"`
	// 地基 A：结构化元数据（内联 meta 标签持久化，G3 时序留史 + Pinned）。
	Pinned     bool    `json:"pinned,omitempty"`
	Subject    string  `json:"subject,omitempty"`
	ValidFrom  string  `json:"valid_from,omitempty"`
	ValidTo    string  `json:"valid_to,omitempty"`
	Supersedes string  `json:"supersedes,omitempty"`
	Confidence float32 `json:"confidence,omitempty"`
	// lineIdx 是本条目在所属文件中的当前起始行号（解析时填入）。包内淘汰/反思/做梦的就地行操作用它定位，
	// 不再从 ID 反解行号——缺陷H 修复后 ID 是稳定内容寻址 ID（与行号脱钩），反解会失效。未导出、不入 JSON。
	lineIdx int
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

	// 全局类型（identity/preference/instruction/rule）始终写入 _global（跨角色、常驻保证带）
	targetDir := fm.roleDir(role)
	if isGlobalMemType(memType) {
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

	// 走结构化单写器：统一铸稳定 ID（缺陷H）+ 过 PII 守卫（缺陷J），不再绕过。
	return fm.appendStructuredEntryUnlocked(targetDir, memType, source, content, EntryMeta{})
}

// isGlobalMemType 报告该类型是否强制写入 _global（跨角色可见、常驻保证带）。
func isGlobalMemType(memType string) bool {
	switch memType {
	case "identity", "preference", "instruction", "rule":
		return true
	}
	return false
}

// appendEntryUnlocked 追加一条结构化条目到目标目录的 MEMORY.md（调用方已持写锁）。
func (fm *FileMemory) appendEntryUnlocked(targetDir, memType, source, content string) error {
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
	// role entries 的行号 ID 加 r- 前缀避免与 global 冲突；稳定 ID（缺陷H）本身全局唯一 → 不改写。
	for i := range roleEntries {
		if looksLikeLineID(roleEntries[i].ID) {
			roleEntries[i].ID = "r-" + roleEntries[i].ID[2:] // m-N → r-N
		}
	}
	return append(globalEntries, roleEntries...)
}

// parseEntriesForRoleUnlocked 解析角色记忆（_global + role 合并，调用方已持写锁）。
func (fm *FileMemory) parseEntriesForRoleUnlocked(role string) []MemoryEntry {
	globalEntries := fm.parseEntriesFromDirUnlocked(fm.roleDir(""))
	if role == "" {
		return globalEntries
	}
	roleEntries := fm.parseEntriesFromDirUnlocked(fm.roleDir(role))
	for i := range roleEntries {
		if looksLikeLineID(roleEntries[i].ID) {
			roleEntries[i].ID = "r-" + roleEntries[i].ID[2:]
		}
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
	// 地基 A：去重比对用纯正文，剥掉行尾内联 meta 标签。
	pure, _ := splitEntryMeta(line)
	return pure
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

// evictIfNeededUnlocked 当容量已满时将评分最低的条目降级到归档（调用方已持有写锁）。
func (fm *FileMemory) evictIfNeededUnlocked(dir string) {
	entries := fm.parseEntriesFromDirUnlocked(dir)
	if len(entries) < fm.config.MaxMemory {
		return
	}

	// 找最该淘汰（保留价值最低）的条目。
	lowestScore := float64(1e18)
	lowestIdx := -1
	now := time.Now()

	for _, e := range entries {
		// 硬保护带（修复 BUG-A）：与 recall.SelectResident 同一白名单 + 用户显式置顶 → 永不淘汰。
		// 旧逻辑只保 instruction，会把 Pinned 画像/置顶事实、rule 硬规则、identity 身份误删（做梦留史之洞）。
		if e.Pinned || e.Type == "rule" || e.Type == "identity" || e.Type == "instruction" {
			continue
		}
		// 单一真相源（修复 BUG-B）：淘汰用与召回同口径的 recall.Score（recency+importance，query 无关、
		// 含 salience/类型先验/召回频次），不再用独立 entryEvictionScore（两套打分会漂移、删掉召回最看重的）。
		score := recall.Score(toRecallEntry(e), 0, now, recall.DefaultWeights(), recall.DefaultLambdaPerDay)
		// 已失效的留史条（ValidTo 过期）优先淘汰：做梦/反思留史会累积历史，空间紧张时先剪历史、保活跃事实。
		if !entryValidAt(e, now) {
			score = -1
		}
		if score < lowestScore {
			lowestScore = score
			lowestIdx = e.lineIdx
		}
	}

	if lowestIdx < 0 {
		return // 全是受保护条目（罕见）：无法淘汰，MaxMemory 应调大
	}

	_ = fm.moveEntryLineUnlocked(dir, memoryActiveFile, memoryArchiveFile, lowestIdx)
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

	blocks := splitEntryBlocks(raw)
	var entries []MemoryEntry

	for _, block := range blocks {
		e := MemoryEntry{
			ID:        fmt.Sprintf("%s-%d", idPrefix, block.startLine),
			lineIdx:   block.startLine, // 真实行号（淘汰/反思/做梦就地行操作用它，与稳定 ID 解耦）
			Type:      "fact",
			Source:    "manual",
			CreatedAt: defaultDate + "T00:00:00Z",
			UpdatedAt: defaultDate + "T00:00:00Z",
			Status:    status,
		}
		if status == MemoryStatusArchived {
			e.ArchivedAt = e.UpdatedAt
		}

		// 解析首行前缀：- [HH:MM] [type:source] content
		line := block.text
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

		// 地基 A：分离行尾内联 meta 标签 → 填充结构化字段（旧条目无标签 → 零值）。
		pure, emeta := splitEntryMeta(line)
		e.Content = pure
		if emeta.ID != "" { // 修缺陷H：有稳定 ID 则用它作条目标识（删/改/移行不漂移）；旧条目无标签 → 回退行号 ID。
			e.ID = emeta.ID
		}
		e.Pinned = emeta.Pinned
		e.Subject = emeta.Subject
		e.ValidFrom = emeta.ValidFrom
		e.ValidTo = emeta.ValidTo
		e.Supersedes = emeta.Supersedes
		e.Confidence = emeta.Confidence
		if emeta.HitCount > 0 {
			e.HitCount = emeta.HitCount
		}
		if e.Content != "" {
			entries = append(entries, e)
		}
	}
	return entries
}

// metaLineIdx 返回块 [start,end) 内**承载内联 meta 标签的行**＝最后一个非空行。
// parse 从块尾（splitEntryMeta 作用于整块拼接文本）读取标签，故所有就地 meta 改写都必须落在这一行；
// 单行条目 == start（与历史行为逐字节一致），多行条目则落最后一条正文行——杜绝标签写到首行而被
// 后续 parse 当作正文（双标签污染 / 元数据丢失）。
func metaLineIdx(lines []string, start, end int) int {
	if end > len(lines) {
		end = len(lines)
	}
	for i := end - 1; i >= start && i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return start
}

// findBlockByStartLine 在文件内容中找到以 startLine 开头的条目块。
// 返回块的起止行号。未找到时返回 -1, -1。
func findBlockByStartLine(raw string, startLine int) (blockStart, blockEnd int) {
	for _, b := range splitEntryBlocks(raw) {
		if b.startLine == startLine {
			return b.startLine, b.endLine
		}
	}
	return -1, -1
}

// removeLineRange 从 lines 切片中删除 [start, end) 范围的行
func removeLineRange(lines []string, start, end int) []string {
	return append(lines[:start], lines[end:]...)
}

// extractLineRange 从 lines 切片中提取 [start, end) 范围的行并拼接
func extractLineRange(lines []string, start, end int) string {
	return strings.Join(lines[start:end], "\n")
}

// moveEntryLineUnlocked 将一条记忆从 fromFile 移动到 toFile（调用方已持有写锁）。
//
// 支持多行条目：根据 startLine 找到完整的条目块，整块移动。
//
// 使用 write-to-temp-then-rename 模式保证原子性：
//  1. 先将目标文件（含新条目）写到临时文件，rename 到目标路径（原子）
//  2. 再更新源文件（删除该条目）
//
// 如果步骤 2 失败，条目同时存在于两个文件中（重复优于丢失）。
func (fm *FileMemory) moveEntryLineUnlocked(dir, fromFile, toFile string, lineIdx int) error {
	fromPath := filepath.Join(dir, fromFile)
	raw, err := os.ReadFile(fromPath)
	if err != nil {
		return fmt.Errorf("读取记忆文件失败: %w", err)
	}
	rawStr := string(raw)
	lines := strings.Split(rawStr, "\n")

	blockStart, blockEnd := findBlockByStartLine(rawStr, lineIdx)
	if blockStart < 0 {
		return fmt.Errorf("记忆条目不存在")
	}

	entryText := extractLineRange(lines, blockStart, blockEnd)
	if strings.TrimSpace(entryText) == "" {
		return fmt.Errorf("记忆条目不存在")
	}

	if err := fileutil.MkdirAll(dir); err != nil {
		return fmt.Errorf("创建记忆目录失败: %w", err)
	}

	// Step 1: 将目标文件内容 + 新条目写到临时文件，然后 rename（原子操作）
	toPath := filepath.Join(dir, toFile)
	existingDest, _ := os.ReadFile(toPath)
	newDest := string(existingDest) + strings.TrimRight(entryText, "\n") + "\n"

	tmpPath := toPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(newDest), 0644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, toPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("原子替换目标文件失败: %w", err)
	}

	// Step 2: 更新源文件（删除该条目的所有行）
	lines = removeLineRange(lines, blockStart, blockEnd)
	if err := atomicWriteFile(fromPath, []byte(strings.Join(lines, "\n")), 0644); err != nil {
		return fmt.Errorf("更新记忆文件失败: %w", err)
	}
	if toFile == memoryArchiveFile { // 修缺陷B：移入归档后裁剪到 ArchiveMax，防无界增长
		fm.capArchiveUnlocked(dir)
	}
	return nil
}

// capArchiveUnlocked 把归档文件裁剪到最近 ArchiveMax 条（修缺陷B 无界增长）。调用方已持写锁。
// 按条目首行（`- [`）切块，保留尾部 N 块（最新），更老的硬删。best-effort：失败不阻断主流程。
func (fm *FileMemory) capArchiveUnlocked(dir string) {
	maxN := fm.config.ArchiveMax
	if maxN <= 0 {
		return
	}
	path := filepath.Join(dir, memoryArchiveFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(string(raw), "\n")
	var starts []int
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "- [") {
			starts = append(starts, i)
		}
	}
	if len(starts) <= maxN {
		return
	}
	keepFrom := starts[len(starts)-maxN] // 保留最近 maxN 条的首行起
	_ = atomicWriteFile(path, []byte(strings.Join(lines[keepFrom:], "\n")), 0644)
}

// UpdateEntry 更新指定 ID 的记忆条目内容
//
// 支持多行条目：保留首行的时间戳和元数据前缀，替换内容部分。
// 新内容可以包含换行（多行记忆）。
func (fm *FileMemory) UpdateEntry(id, content string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	return fm.updateEntryUnlocked(id, content)
}

// updateEntryUnlocked 更新指定 ID 条目内容（调用方已持写锁）。
func (fm *FileMemory) updateEntryUnlocked(id, content string) error {
	dir, filename, lineIdx := fm.resolveEntryLocationUnlocked(id)
	if lineIdx < 0 {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}
	path := filepath.Join(dir, filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取记忆文件失败: %w", err)
	}
	rawStr := string(raw)
	lines := strings.Split(rawStr, "\n")

	blockStart, blockEnd := findBlockByStartLine(rawStr, lineIdx)
	if blockStart < 0 {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}

	// 保留首行的时间戳和类型前缀，替换正文。
	// 修缺陷H：保留条目既有内联 meta（稳定 ID/Pinned/Subject/时序锚）——内容编辑不应让条目丢失稳定标识或常驻位。
	// meta 是单行构造：仅当新正文也是单行时重新挂回；新正文多行则退化为无 meta 的多行条目（与既有多行约定一致）。
	oldFirstLine := strings.TrimSpace(lines[blockStart])
	_, meta := splitEntryMeta(stripEntryPrefix(oldFirstLine))
	newContent := strings.TrimSpace(content)
	if !strings.Contains(newContent, "\n") {
		if tag := meta.serialize(); tag != "" {
			newContent += " " + tag
		}
	}
	newFirstLine := rebuildEntryLine(oldFirstLine, newContent)

	// 用新内容替换整个块（新内容可能也是多行的）。
	// 全切片表达式 lines[:blockStart:blockStart] 强制 append 重新分配，避免覆写后续 lines[blockEnd:]。
	newLines := append(lines[:blockStart:blockStart], newFirstLine)
	newLines = append(newLines, lines[blockEnd:]...)

	return atomicWriteFile(path, []byte(strings.Join(newLines, "\n")), 0644)
}

// DeleteEntry 删除指定 ID 的记忆条目
//
// 支持多行条目：删除条目的所有行。
func (fm *FileMemory) DeleteEntry(id string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	dir, filename, lineIdx := fm.resolveEntryLocationUnlocked(id)
	if lineIdx < 0 {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}
	path := filepath.Join(dir, filename)
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取记忆文件失败: %w", err)
	}
	rawStr := string(raw)
	lines := strings.Split(rawStr, "\n")

	blockStart, blockEnd := findBlockByStartLine(rawStr, lineIdx)
	if blockStart < 0 {
		return fmt.Errorf("记忆条目不存在: %s", id)
	}

	lines = removeLineRange(lines, blockStart, blockEnd)

	return atomicWriteFile(path, []byte(strings.Join(lines, "\n")), 0644)
}

// ArchiveEntry 将活跃记忆降级到归档。
// 解析与移动同持一把写锁：稳定 ID 现扫现定位，杜绝解析后行号漂移的 TOCTOU。
func (fm *FileMemory) ArchiveEntry(id string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	dir, filename, lineIdx := fm.resolveEntryLocationUnlocked(id)
	if filename != memoryActiveFile || lineIdx < 0 {
		return fmt.Errorf("只能归档活跃记忆: %s", id)
	}
	return fm.moveEntryLineUnlocked(dir, memoryActiveFile, memoryArchiveFile, lineIdx)
}

// RestoreEntry 将归档记忆恢复为活跃记忆。
func (fm *FileMemory) RestoreEntry(id string) error {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	dir, filename, lineIdx := fm.resolveEntryLocationUnlocked(id)
	if filename != memoryArchiveFile || lineIdx < 0 {
		return fmt.Errorf("只能恢复归档记忆: %s", id)
	}
	fm.evictIfNeededUnlocked(dir)
	return fm.moveEntryLineUnlocked(dir, memoryArchiveFile, memoryActiveFile, lineIdx)
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

// looksLikeLineID 报告 id 是否为旧式行号 ID（`m-`/`a-`/`r-` 前缀 + 纯数字）。
// 稳定 ID（缺陷H 修复后，形如 `e<hex>`）不命中 → 走内容寻址扫描；二者形态正交、无歧义。
func looksLikeLineID(id string) bool {
	if len(id) < 3 || id[1] != '-' {
		return false
	}
	if id[0] != 'm' && id[0] != 'a' && id[0] != 'r' {
		return false
	}
	for i := 2; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	return true
}

// resolveEntryLocationUnlocked 把一个条目 ID 解析为其**当前**位置 (dir, filename, lineIdx)。调用方已持锁。
//
// 修缺陷H：稳定 ID 不绑行号——按内容寻址扫描定位当前行；行号会随删/改/移行漂移，故每次现扫现定位。
// 旧式行号 ID（`m-N`/`a-N`/`r-N`）保持原 O(1) 行号解析，向后兼容。未找到 → lineIdx=-1。
func (fm *FileMemory) resolveEntryLocationUnlocked(id string) (dir, filename string, lineIdx int) {
	if looksLikeLineID(id) {
		filename, lineIdx = parseEntryFileAndLine(id)
		if lineIdx < 0 {
			return "", "", -1
		}
		return fm.resolveEntryDir(id), filename, lineIdx
	}
	return fm.scanStableIDUnlocked(id)
}

// scanStableIDUnlocked 扫描全部记忆文件（_global + 各角色子目录的活跃/归档），按内联 eid 命中定位条目。
// 调用方已持锁。记忆有界（活跃 ≤ MaxMemory、归档 ≤ ArchiveMax）→ 扫描成本可控；变更操作低频。
func (fm *FileMemory) scanStableIDUnlocked(id string) (string, string, int) {
	dirs := []string{fm.roleDir("")}
	if entries, err := os.ReadDir(fm.dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Name() != "_global" {
				dirs = append(dirs, filepath.Join(fm.dir, e.Name()))
			}
		}
	}
	for _, dir := range dirs {
		for _, fn := range []string{memoryActiveFile, memoryArchiveFile} {
			raw, err := os.ReadFile(filepath.Join(dir, fn))
			if err != nil {
				continue
			}
			for _, b := range splitEntryBlocks(string(raw)) {
				if _, m := splitEntryMeta(stripEntryPrefix(b.text)); m.ID == id {
					return dir, fn, b.startLine
				}
			}
		}
	}
	return "", "", -1
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
	return atomicWriteFile(path, []byte(content), 0644)
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
