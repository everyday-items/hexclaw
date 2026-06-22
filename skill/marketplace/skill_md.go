// Package marketplace 提供 Markdown 技能加载和技能市场
//
// 技能格式兼容 OpenClaw Custom Commands，扩展了安全和元数据能力：
//
//	---
//	name: web-search
//	description: 搜索网页并返回结果
//	author: hexclaw-team
//	version: "1.0"
//	triggers:
//	  - 搜索
//	  - search
//	  - 查找
//	tags:
//	  - search
//	  - web
//	---
//
//	# 网络搜索技能
//
//	你是一个网络搜索助手...（完整的 Prompt 指令）
//
// 技能文件存放在 ~/.hexclaw/skills/ 目录下：
//
//	~/.hexclaw/skills/
//	├── web-search/
//	│   └── SKILL.md
//	├── code-review/
//	│   └── SKILL.md
//	└── daily-report.md    # 单文件技能也支持
//
// 按需加载策略：
//   - 启动时只读取 frontmatter（名称+描述，约 24 tokens/skill）
//   - 使用时才读取完整内容（Prompt 指令）
//   - 支持 10,000+ 技能而不影响启动速度
package marketplace

import (
	"bufio"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// SkillMeta 技能元数据（从 YAML frontmatter 解析）
//
// 启动时只加载元数据，不加载完整 Prompt 内容。
// 每个技能约 24 tokens，支持海量技能注册。
//
// v0.4.x Phase 2 扩展字段（when/not_when/preferred_mode）支持条件激活：
//
//	---
//	name: math-tutor
//	when:
//	  - subject=math
//	  - grade=小学
//	not_when:
//	  - mode=react
//	preferred_mode: tot
//	---
type SkillMeta struct {
	Name          string   `yaml:"name"`           // 唯一标识（必需）
	Description   string   `yaml:"description"`    // 描述（供 LLM 理解）
	Author        string   `yaml:"author"`         // 作者
	Version       string   `yaml:"version"`        // 版本
	Triggers      []string `yaml:"triggers"`       // 快速路径触发关键词
	Tags          []string `yaml:"tags"`           // 分类标签
	Signature     string   `yaml:"signature"`      // 签名（安全验证用）
	Icon          string   `yaml:"icon"`           // 可选：emoji（如 "📊"）或内置 lucide 名（如 "Code"），供桌面端渲染图标；缺省时前端按 category/tags/name 派生
	When          []string `yaml:"when"`           // Phase 2: 激活条件 AND
	NotWhen       []string `yaml:"not_when"`       // Phase 2: 排除条件
	PreferredMode string   `yaml:"preferred_mode"` // Phase 2: 推荐 Agent 模式

	// v0.4.0 F5: 工具集依赖（硬依赖：列出 Skill 执行时会调用的工具/Skill 名）。
	// 加载或选择时若环境中缺少其中之一，应跳过此 Skill，避免运行时崩溃。
	// 例：tools: [calculator, web-search]
	Tools []string `yaml:"tools"`
	// v0.4.0 F5: 环境能力依赖（软依赖：抽象的能力标签，如 "knowledge-base" / "fs-write"）。
	// 用于上层做"环境画像"路由判断；不强制为可执行工具名。
	Requires []string `yaml:"requires"`
}

// MarkdownSkill Markdown 格式的技能
//
// 兼容 OpenClaw Custom Commands 格式。
// 实现 skill.Skill 接口，可注册到 Skill 注册中心。
//
// v0.4.0 F10 TOCTOU 防御：首次加载文件时计算 SHA256 并缓存；后续调用 LoadContent
// 会重新读盘并比对 hash —— 若磁盘文件被替换（time-of-check 与 time-of-use 之间），
// 立即返回错误而非把可能被篡改的内容喂给 LLM。
type MarkdownSkill struct {
	Meta     SkillMeta // 元数据
	FilePath string    // 技能文件路径
	Content  string    // 完整 Prompt 内容（懒加载）
	loaded   bool      // 内容是否已加载
	hash     [32]byte  // 首次加载时计算的 SHA256，用于后续 TOCTOU 校验
	mu       sync.RWMutex
}

// Name 返回技能名称
func (s *MarkdownSkill) Name() string {
	return s.Meta.Name
}

// Description 返回技能描述
func (s *MarkdownSkill) Description() string {
	return s.Meta.Description
}

// SkillMeta 实现 skill.MetaProvider，让 select.go 的 SelectByQuery / SelectByContext
// 能利用 frontmatter 的 triggers/tags/when/not_when/preferred_mode。
func (s *MarkdownSkill) SkillMeta() skill.SkillMetaInfo {
	return skill.SkillMetaInfo{
		Name:          s.Meta.Name,
		Description:   s.Meta.Description,
		Triggers:      s.Meta.Triggers,
		Tags:          s.Meta.Tags,
		When:          s.Meta.When,
		NotWhen:       s.Meta.NotWhen,
		PreferredMode: s.Meta.PreferredMode,
		Tools:         s.Meta.Tools,
		Requires:      s.Meta.Requires,
	}
}

// Match 快速路径匹配
//
// 检查消息内容是否包含技能的触发关键词。
// 使用包含匹配，支持关键词出现在消息任意位置（如 "介绍下前女友"）。
func (s *MarkdownSkill) Match(content string) bool {
	if len(s.Meta.Triggers) == 0 {
		return false
	}

	contentLower := strings.ToLower(strings.TrimSpace(content))
	for _, trigger := range s.Meta.Triggers {
		triggerLower := strings.ToLower(trigger)
		if strings.Contains(contentLower, triggerLower) {
			return true
		}
	}
	return false
}

// Execute 执行技能
//
// 返回技能的完整 Prompt 内容作为系统指令。
// 调用方（引擎）应将此内容注入 LLM 上下文。
func (s *MarkdownSkill) Execute(ctx context.Context, args map[string]any) (*SkillResult, error) {
	// 懒加载完整内容
	content, err := s.LoadContent()
	if err != nil {
		return nil, fmt.Errorf("加载技能 %q 内容失败: %w", s.Meta.Name, err)
	}

	query, _ := args["query"].(string)

	return &SkillResult{
		Content: content,
		Metadata: map[string]string{
			"skill_name":   s.Meta.Name,
			"skill_author": s.Meta.Author,
			"query":        query,
		},
	}, nil
}

// SkillResult 技能执行结果
type SkillResult struct {
	Content  string
	Data     any
	Metadata map[string]string
}

// LoadContent 懒加载技能完整内容（v0.4.0 F10 TOCTOU 加固版）。
//
// 首次调用：读盘 → 解析 frontmatter → 缓存 content + 文件 SHA256。
// 后续调用：返回缓存内容（fast path）。
//
// VerifyContent 在每次"使用前 / 加载到 LLM context 前"应被显式调用，它会重读文件
// 比对 hash；若磁盘文件被替换则返回错误，避免 LLM 接受被篡改的 prompt。
func (s *MarkdownSkill) LoadContent() (string, error) {
	s.mu.RLock()
	if s.loaded {
		content := s.Content
		s.mu.RUnlock()
		return content, nil
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	// 双重检查
	if s.loaded {
		return s.Content, nil
	}

	data, err := os.ReadFile(s.FilePath)
	if err != nil {
		return "", fmt.Errorf("读取技能文件失败: %w", err)
	}

	_, content := parseFrontmatter(string(data))
	s.Content = content
	s.hash = sha256.Sum256(data)
	s.loaded = true

	return content, nil
}

// VerifyContent 重读技能文件并校验 SHA256，确认与首次加载时的内容一致。
//
// 用法：在把 Skill 内容塞进 LLM context 之前调用一次：
//
//	if err := s.VerifyContent(); err != nil {
//	    log.Warn("skill tampered or missing", "skill", s.Name(), "err", err)
//	    continue
//	}
//	prompt, _ := s.LoadContent()
//
// 首次加载之前调用直接返回 nil（首次加载时会自动计算 hash）。
func (s *MarkdownSkill) VerifyContent() error {
	s.mu.RLock()
	loaded := s.loaded
	expectedHash := s.hash
	path := s.FilePath
	s.mu.RUnlock()

	if !loaded {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("TOCTOU: skill 文件已不可读: %w", err)
	}
	got := sha256.Sum256(data)
	if got != expectedHash {
		return fmt.Errorf("TOCTOU: skill 文件内容已被篡改（%s）", path)
	}
	return nil
}

// TrustLevel 实现 skill.TrustProvider —— 默认按"是否有签名 + 是否本地文件"判定：
//   - 有 signature 字段视作 TrustSigned（注：当前仅相信字段存在，签名验证由
//     security 包负责，未通过校验的 Skill 应在加载阶段就被拒绝注册）
//   - 否则视作 TrustLocal（用户本地文件、可见但要审批写入）
//
// 来源不明 / 网络下载的 Skill 应在 import 阶段被显式标 TrustUntrusted（通过
// 自定义 wrapper 覆盖此方法），不依赖默认值。
func (s *MarkdownSkill) TrustLevel() skill.TrustLevel {
	if strings.TrimSpace(s.Meta.Signature) != "" {
		return skill.TrustSigned
	}
	return skill.TrustLocal
}

// ============== 技能加载 ==============

// LoadSkillsFromDir 从目录加载所有技能
//
// 扫描目录下的 Markdown 技能文件：
//   - 直接 .md 文件（如 daily-report.md）
//   - 子目录中的 SKILL.md（如 web-search/SKILL.md）
//
// 只读取 frontmatter 元数据，不加载完整内容（按需加载）。
func LoadSkillsFromDir(dir string) ([]*MarkdownSkill, error) {
	// 展开 ~
	if strings.HasPrefix(dir, "~/") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, dir[2:])
	}

	// 确保目录存在
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil // 目录不存在不报错
	}

	var skills []*MarkdownSkill

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取技能目录失败: %w", err)
	}

	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())

		if entry.IsDir() {
			// 子目录：查找 SKILL.md 或 README.md
			skillFile := filepath.Join(path, "SKILL.md")
			if _, err := os.Stat(skillFile); err != nil {
				// 尝试 README.md（兼容一些社区技能）
				skillFile = filepath.Join(path, "README.md")
				if _, err := os.Stat(skillFile); err != nil {
					continue
				}
			}
			skill, err := LoadSkillFromFile(skillFile)
			if err != nil {
				logger.Error("加载技能", "name", entry.Name(), "error", err)
				continue
			}
			// 如果 frontmatter 没有 name，用目录名
			if skill.Meta.Name == "" {
				skill.Meta.Name = entry.Name()
			}
			skills = append(skills, skill)

		} else if strings.HasSuffix(entry.Name(), ".md") {
			// 单文件技能
			skill, err := LoadSkillFromFile(path)
			if err != nil {
				logger.Error("加载技能", "name", entry.Name(), "error", err)
				continue
			}
			// 如果 frontmatter 没有 name，用文件名（去掉 .md）
			if skill.Meta.Name == "" {
				skill.Meta.Name = strings.TrimSuffix(entry.Name(), ".md")
			}
			skills = append(skills, skill)
		}
	}

	return skills, nil
}

// LoadSkillFromFile 从单个文件加载技能
//
// 只读取 frontmatter 元数据，不加载完整内容。
func LoadSkillFromFile(path string) (*MarkdownSkill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取文件失败: %w", err)
	}

	meta, _ := parseFrontmatter(string(data))

	return &MarkdownSkill{
		Meta:     meta,
		FilePath: path,
	}, nil
}

// ============== Frontmatter 解析 ==============

// parseFrontmatter 解析 YAML frontmatter
//
// 格式：
//
//	---
//	key: value
//	---
//	正文内容
//
// 返回解析后的元数据和正文内容。
// 使用简单的行级解析，避免依赖 YAML 库。
func parseFrontmatter(text string) (SkillMeta, string) {
	var meta SkillMeta

	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "---") {
		// 无 frontmatter，整个文件作为内容
		return meta, text
	}

	// 查找结束标记
	endIdx := strings.Index(text[3:], "\n---")
	if endIdx < 0 {
		return meta, text
	}

	frontmatter := text[3 : endIdx+3]
	content := strings.TrimSpace(text[endIdx+3+4:]) // 跳过 "\n---"

	// 简单的行级 YAML 解析
	scanner := bufio.NewScanner(strings.NewReader(frontmatter))
	var currentKey string
	var listValues []string
	inList := false

	flushList := func() {
		if currentKey != "" && len(listValues) > 0 {
			switch currentKey {
			case "triggers":
				meta.Triggers = listValues
			case "tags":
				meta.Tags = listValues
			case "when":
				meta.When = listValues
			case "not_when":
				meta.NotWhen = listValues
			case "tools":
				meta.Tools = listValues
			case "requires":
				meta.Requires = listValues
			}
		}
		listValues = nil
		inList = false
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if trimmed == "" {
			continue
		}

		// 列表项: "  - value"
		if strings.HasPrefix(trimmed, "- ") {
			inList = true
			listValues = append(listValues, strings.TrimSpace(trimmed[2:]))
			continue
		}

		// 新的键值对
		if inList {
			flushList()
		}

		parts := strings.SplitN(trimmed, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		// 去掉引号
		value = strings.Trim(value, `"'`)
		currentKey = key

		switch key {
		case "name":
			meta.Name = value
		case "description":
			meta.Description = value
		case "author":
			meta.Author = value
		case "version":
			meta.Version = value
		case "signature":
			meta.Signature = value
		case "icon":
			meta.Icon = value
		case "preferred_mode":
			meta.PreferredMode = value
		case "triggers", "tags", "when", "not_when", "tools", "requires":
			// 如果值在同一行 [a, b, c]
			if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
				inner := value[1 : len(value)-1]
				for _, item := range strings.Split(inner, ",") {
					item = strings.TrimSpace(item)
					item = strings.Trim(item, `"'`)
					if item != "" {
						listValues = append(listValues, item)
					}
				}
				flushList()
			}
			// 否则等下一行的列表项
		}
	}

	// 刷新最后一个列表
	if inList {
		flushList()
	}

	return meta, content
}
