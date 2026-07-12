// marketplace.go 技能市场管理器
//
// 提供技能的安装、更新、删除和列表能力。
// 支持本地技能目录 + 远程技能注册表（Git URL）。
package marketplace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
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
	// manifest 记录每个 seed 文件「上次由本程序写入的内容哈希」，用于版本感知 re-seed 时
	// 区分「用户未改动（可安全升级覆盖）」与「用户已自定义（保护不覆盖）」（BUG-20260712）。
	manifest := loadSeedManifest(m.skillDir)
	manifestDirty := false
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
		published, recordHash, werr := publishSeedVersionAware(dest, data, manifest[e.Name()])
		if werr != nil {
			return seeded, fmt.Errorf("写入 seed skill %q 失败: %w", e.Name(), werr)
		}
		if published {
			seeded++
		}
		if recordHash != "" && manifest[e.Name()] != recordHash {
			manifest[e.Name()] = recordHash
			manifestDirty = true
		}
	}
	if manifestDirty {
		// manifest 只用于「用户是否改过 seed」的判定，写失败不影响本次 seed 正确性 → best-effort。
		if err := saveSeedManifest(m.skillDir, manifest); err != nil {
			logger.Warn("写入 seed manifest 失败（不影响本次 seed）", "error", err)
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

// publishSeedVersionAware 版本感知发布单个 seed 文件（BUG-20260712）。返回：
//   - published：本次是否新写入/升级覆盖了磁盘（用于 seeded 计数）
//   - recordHash：应写入 manifest 的内容哈希（磁盘现内容 == bundled 时才非空）
//
// 语义：①dest 不存在 → no-replace 原子写（保持并发单赢家）；②已存在且 bundled 版本更高
// 且磁盘文件未被用户改（哈希 == 上次 seed 或无 manifest 的 legacy）→ 原子升级覆盖；
// ③已存在但用户改过 → 保留用户版本不覆盖；④bundled 未升版 → 不动磁盘。
func publishSeedVersionAware(dest string, data []byte, prevHash string) (bool, string, error) {
	// 新文件：走 no-replace 原子发布（dest 缺失时唯一赢家写入，多进程并发只有一个成功）。
	published, err := publishSeedNoReplace(dest, data)
	if err != nil {
		return false, "", err
	}
	if published {
		return true, hashSeedContent(data), nil
	}

	// dest 已存在 → 版本感知。link 失败即 IsExist，说明磁盘上是一份完整文件（写者先写满 temp 再 link）。
	existing, rerr := os.ReadFile(dest)
	if rerr != nil {
		return false, "", rerr
	}
	if compareSkillVersions(skillSeedVersion(data), skillSeedVersion(existing)) <= 0 {
		// bundled 未比磁盘新：不动磁盘（含用户改动/更高本地版本）。内容恰与 bundled 一致时
		// 回填 hash，让未来的编辑判定有基线。
		if bytes.Equal(existing, data) {
			return false, hashSeedContent(data), nil
		}
		return false, "", nil
	}

	// bundled 更新：仅当磁盘文件与「上次 seed 内容」一致（未被用户改）才升级覆盖。
	// prevHash == "" 是 legacy（本特性上线前已 no-replace seed，无 manifest）——视作未改，放行升级，
	// 让 skill 更新能到达老安装（信条：更新要流达用户；用户改动由 manifest 上线后开始受保护）。
	if prevHash != "" && hashSeedContent(existing) != prevHash {
		logger.Info("bundled skill 有新版但本地已被用户修改，保留用户版本不覆盖",
			"skill", filepath.Base(dest), "bundled", skillSeedVersion(data), "local", skillSeedVersion(existing))
		return false, "", nil
	}
	if err := overwriteSeedAtomic(dest, data); err != nil {
		return false, "", err
	}
	return true, hashSeedContent(data), nil
}

// overwriteSeedAtomic 原子覆盖写（temp + rename），用于版本升级覆盖已存在文件。
func overwriteSeedAtomic(dest string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(dest), seedTempPrefix+filepath.Base(dest)+"-*"+seedTempSuffix)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return err
	}
	cleanup = false // rename 已消费 temp，无泄漏
	return nil
}

// skillSeedVersion 从 skill markdown 原始字节取 frontmatter version（无则空串，视为最低版本）。
func skillSeedVersion(data []byte) string {
	meta, _ := parseFrontmatter(string(data))
	return strings.TrimSpace(meta.Version)
}

// compareSkillVersions 比较点分版本号（如 1.2.0），返回 -1/0/1。数字段按整数比、缺段补 0、
// 非数字段回退字典序；空串视为最低。
func compareSkillVersions(a, b string) int {
	a = strings.TrimPrefix(strings.TrimSpace(a), "v")
	b = strings.TrimPrefix(strings.TrimSpace(b), "v")
	if a == b {
		return 0
	}
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		ai, aerr := strconv.Atoi(av)
		bi, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
			continue
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func hashSeedContent(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

const seedManifestName = ".hexclaw-seed-manifest.json"

func seedManifestPath(skillDir string) string {
	return filepath.Join(skillDir, seedManifestName)
}

// loadSeedManifest 读取 seed 内容哈希清单（filename → sha256hex）。缺失/损坏均返回空 map（best-effort）。
func loadSeedManifest(skillDir string) map[string]string {
	out := make(map[string]string)
	data, err := os.ReadFile(seedManifestPath(skillDir))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	if out == nil {
		out = make(map[string]string)
	}
	return out
}

// saveSeedManifest 原子写清单（temp + rename）。
func saveSeedManifest(skillDir string, manifest map[string]string) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(skillDir, seedTempPrefix+seedManifestName+"-*"+seedTempSuffix)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, seedManifestPath(skillDir)); err != nil {
		return err
	}
	cleanup = false
	return nil
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
