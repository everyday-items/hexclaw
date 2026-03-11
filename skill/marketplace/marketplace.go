// marketplace.go 技能市场管理器
//
// 提供技能的安装、更新、删除和列表能力。
// 支持本地技能目录 + 远程技能注册表（Git URL）。
package marketplace

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Marketplace 技能市场管理器
//
// 管理本地已安装的技能，支持从远程安装新技能。
// 技能安装到 ~/.hexclaw/skills/ 目录。
type Marketplace struct {
	mu       sync.RWMutex
	skillDir string                    // 技能安装目录
	skills   map[string]*MarkdownSkill // name -> skill
}

// NewMarketplace 创建技能市场管理器
//
// skillDir 为技能安装目录，默认 ~/.hexclaw/skills/
func NewMarketplace(skillDir string) *Marketplace {
	if skillDir == "" {
		home, _ := os.UserHomeDir()
		skillDir = filepath.Join(home, ".hexclaw", "skills")
	}

	// 展开 ~
	if strings.HasPrefix(skillDir, "~/") {
		home, _ := os.UserHomeDir()
		skillDir = filepath.Join(home, skillDir[2:])
	}

	return &Marketplace{
		skillDir: skillDir,
		skills:   make(map[string]*MarkdownSkill),
	}
}

// Init 初始化技能市场
//
// 扫描本地技能目录，加载所有已安装技能的元数据。
// 只读取 frontmatter（按需加载策略），不加载完整内容。
func (m *Marketplace) Init() error {
	// 确保目录存在
	if err := os.MkdirAll(m.skillDir, 0755); err != nil {
		return fmt.Errorf("创建技能目录失败: %w", err)
	}

	skills, err := LoadSkillsFromDir(m.skillDir)
	if err != nil {
		return fmt.Errorf("扫描技能目录失败: %w", err)
	}

	m.mu.Lock()
	for _, skill := range skills {
		m.skills[skill.Meta.Name] = skill
	}
	m.mu.Unlock()

	log.Printf("技能市场已初始化: %d 个技能 (目录: %s)", len(skills), m.skillDir)
	return nil
}

// List 列出所有已安装技能
func (m *Marketplace) List() []*MarkdownSkill {
	m.mu.RLock()
	defer m.mu.RUnlock()

	skills := make([]*MarkdownSkill, 0, len(m.skills))
	for _, s := range m.skills {
		skills = append(skills, s)
	}
	return skills
}

// Get 获取指定技能
func (m *Marketplace) Get(name string) (*MarkdownSkill, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.skills[name]
	return s, ok
}

// Install 安装技能
//
// source 支持：
//   - 本地文件路径（.md 文件）
//   - 本地目录路径（包含 SKILL.md）
//
// TODO: 后续支持 Git URL 远程安装
func (m *Marketplace) Install(source string) (*MarkdownSkill, error) {
	// 展开 ~
	if strings.HasPrefix(source, "~/") {
		home, _ := os.UserHomeDir()
		source = filepath.Join(home, source[2:])
	}

	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("源不存在: %w", err)
	}

	var skill *MarkdownSkill

	if info.IsDir() {
		// 目录安装：复制整个目录
		skillFile := filepath.Join(source, "SKILL.md")
		if _, err := os.Stat(skillFile); err != nil {
			return nil, fmt.Errorf("目录中未找到 SKILL.md")
		}
		skill, err = LoadSkillFromFile(skillFile)
		if err != nil {
			return nil, fmt.Errorf("加载技能失败: %w", err)
		}
		if skill.Meta.Name == "" {
			skill.Meta.Name = info.Name()
		}

		// 复制到技能目录
		destDir := filepath.Join(m.skillDir, skill.Meta.Name)
		if err := copyDir(source, destDir); err != nil {
			return nil, fmt.Errorf("复制技能目录失败: %w", err)
		}
		skill.FilePath = filepath.Join(destDir, "SKILL.md")

	} else if strings.HasSuffix(source, ".md") {
		// 单文件安装
		skill, err = LoadSkillFromFile(source)
		if err != nil {
			return nil, fmt.Errorf("加载技能失败: %w", err)
		}
		if skill.Meta.Name == "" {
			skill.Meta.Name = strings.TrimSuffix(info.Name(), ".md")
		}

		// 复制到技能目录
		destPath := filepath.Join(m.skillDir, skill.Meta.Name+".md")
		data, err := os.ReadFile(source)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(destPath, data, 0644); err != nil {
			return nil, fmt.Errorf("写入技能文件失败: %w", err)
		}
		skill.FilePath = destPath

	} else {
		return nil, fmt.Errorf("不支持的技能源: %s（需要 .md 文件或包含 SKILL.md 的目录）", source)
	}

	// 注册技能
	m.mu.Lock()
	m.skills[skill.Meta.Name] = skill
	m.mu.Unlock()

	log.Printf("技能已安装: %s (v%s by %s)", skill.Meta.Name, skill.Meta.Version, skill.Meta.Author)
	return skill, nil
}

// Uninstall 删除已安装的技能
func (m *Marketplace) Uninstall(name string) error {
	m.mu.Lock()
	skill, ok := m.skills[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("技能 %q 未安装", name)
	}
	delete(m.skills, name)
	m.mu.Unlock()

	// 删除文件/目录
	dir := filepath.Dir(skill.FilePath)
	base := filepath.Base(dir)

	// 如果是子目录（名称等于技能名），删除整个目录
	if base == name {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("删除技能目录失败: %w", err)
		}
	} else {
		// 单文件技能
		if err := os.Remove(skill.FilePath); err != nil {
			return fmt.Errorf("删除技能文件失败: %w", err)
		}
	}

	log.Printf("技能已删除: %s", name)
	return nil
}

// Dir 返回技能安装目录
func (m *Marketplace) Dir() string {
	return m.skillDir
}

// ============== 内部工具 ==============

// copyDir 递归复制目录
func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			data, err := os.ReadFile(srcPath)
			if err != nil {
				return err
			}
			if err := os.WriteFile(dstPath, data, 0644); err != nil {
				return err
			}
		}
	}

	return nil
}
