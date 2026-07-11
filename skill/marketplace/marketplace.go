// marketplace.go 技能市场管理器
//
// 提供技能的安装、更新、删除和列表能力。
// 支持本地技能目录 + 远程技能注册表（Git URL）。
package marketplace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/toolkit/util/logger"

	fileutil "github.com/hexagon-codes/toolkit/util/file"
)

// ErrSkillNotInstalled is returned when operating on a skill that is not installed.
var ErrSkillNotInstalled = errors.New("skill not installed")

const (
	seedTempPrefix = ".hexclaw-seed-"
	seedTempSuffix = ".tmp"
	seedTempMaxAge = 24 * time.Hour
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
	if err := fileutil.MkdirAll(m.skillDir); err != nil {
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

	logger.Info("count", "count", len(skills), "dir", m.skillDir)
	return nil
}

// SeedFromFS 场景包出厂 skill 的**首启幂等 seed**：把 fsys 下 subdir/*.md 写入 skillDir，
// **已存在的文件不覆盖**（保留用户/更高版本的本地修改，避免每次启动回滚）。返回新写入数量。
//
// batteries-included 的落地：场景包 skill 随二进制 go:embed 出厂，首次运行注入 ~/.hexclaw/skills/，
// 零下载、离线可用（产品决策：本地部署·零云端依赖）。须在 Init() 之前调用，本次启动才会被扫描注册。
func (m *Marketplace) SeedFromFS(fsys fs.FS, subdir string) (int, error) {
	if err := fileutil.MkdirAll(m.skillDir); err != nil {
		return 0, fmt.Errorf("创建技能目录失败: %w", err)
	}
	if err := cleanupStaleSeedTemps(m.skillDir, time.Now()); err != nil {
		return 0, fmt.Errorf("清理 seed 临时文件失败: %w", err)
	}
	entries, err := fs.ReadDir(fsys, subdir)
	if err != nil {
		return 0, fmt.Errorf("读取内嵌 seed 目录失败: %w", err)
	}
	seeded := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, rerr := fs.ReadFile(fsys, path.Join(subdir, e.Name()))
		if rerr != nil {
			return seeded, fmt.Errorf("读取内嵌 skill %q 失败: %w", e.Name(), rerr)
		}
		dest := filepath.Join(m.skillDir, e.Name())
		published, werr := publishSeedNoReplace(dest, data)
		if werr != nil {
			return seeded, fmt.Errorf("写入 seed skill %q 失败: %w", e.Name(), werr)
		}
		if published {
			seeded++
		}
	}
	return seeded, nil
}

func publishSeedNoReplace(dest string, data []byte) (bool, error) {
	tmp, err := os.CreateTemp(filepath.Dir(dest), seedTempPrefix+filepath.Base(dest)+"-*"+seedTempSuffix)
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return false, err
	}
	if _, err := tmp.Write(data); err != nil {
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Link(tmpPath, dest); err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func cleanupStaleSeedTemps(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, seedTempPrefix) || !strings.HasSuffix(name, seedTempSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if now.Sub(info.ModTime()) < seedTempMaxAge {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
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

		safeName := filepath.Base(skill.Meta.Name)
		if safeName != skill.Meta.Name || strings.Contains(skill.Meta.Name, "..") {
			return nil, fmt.Errorf("非法技能名称: %s", skill.Meta.Name)
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

		safeName := filepath.Base(skill.Meta.Name)
		if safeName != skill.Meta.Name || strings.Contains(skill.Meta.Name, "..") {
			return nil, fmt.Errorf("非法技能名称: %s", skill.Meta.Name)
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

	logger.Info("name", "name", skill.Meta.Name, "version", skill.Meta.Version, "author", skill.Meta.Author)
	return skill, nil
}

// Uninstall 删除已安装的技能
func (m *Marketplace) Uninstall(name string) error {
	// 校验技能名称，防止路径穿越
	safeName := filepath.Base(name)
	if safeName != name || strings.Contains(name, "..") {
		return fmt.Errorf("非法技能名称: %s", name)
	}

	m.mu.Lock()
	skill, ok := m.skills[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("技能 %q: %w", name, ErrSkillNotInstalled)
	}
	delete(m.skills, name)
	m.mu.Unlock()

	// 删除文件/目录 — 验证路径在 skillDir 内
	dir := filepath.Dir(skill.FilePath)
	absDir, _ := filepath.Abs(dir)
	absSkillDir, _ := filepath.Abs(m.skillDir)
	if !strings.HasPrefix(absDir, filepath.Clean(absSkillDir)+string(filepath.Separator)) && absDir != filepath.Clean(absSkillDir) {
		return fmt.Errorf("路径逃逸: %s 不在 %s 内", absDir, absSkillDir)
	}

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

	logger.Info("name", "name", name)
	return nil
}

// Dir 返回技能安装目录
func (m *Marketplace) Dir() string {
	return m.skillDir
}

// disabledPath 返回禁用列表文件路径
func (m *Marketplace) disabledPath() string {
	return filepath.Join(m.skillDir, "disabled.json")
}

// IsEnabled 检查技能是否启用（默认启用）
func (m *Marketplace) IsEnabled(name string) bool {
	disabled := m.getDisabled()
	return !disabled[name]
}

// SetEnabled 设置技能启用状态
func (m *Marketplace) SetEnabled(name string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	disabled := m.getDisabledLocked()
	if enabled {
		delete(disabled, name)
	} else {
		disabled[name] = true
	}
	return m.saveDisabledLocked(disabled)
}

func (m *Marketplace) getDisabled() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getDisabledLocked()
}

func (m *Marketplace) getDisabledLocked() map[string]bool {
	data, err := os.ReadFile(m.disabledPath())
	if err != nil {
		return make(map[string]bool)
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		return make(map[string]bool)
	}
	out := make(map[string]bool)
	for _, n := range list {
		out[n] = true
	}
	return out
}

func (m *Marketplace) saveDisabledLocked(disabled map[string]bool) error {
	var list []string
	for n := range disabled {
		list = append(list, n)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.disabledPath(), data, 0644)
}

// ============== 内部工具 ==============

// copyDir 递归复制目录
func copyDir(src, dst string) error {
	if err := fileutil.MkdirAll(dst); err != nil {
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
