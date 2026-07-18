// Package hub 提供 HexClaw 在线技能市场
//
// 从远程 Git 仓库（默认 hexagon-codes/hexclaw-hub）获取技能目录，
// 支持搜索、浏览、安装和卸载技能。
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/toolkit/net/httpx"
	fileutil "github.com/hexagon-codes/toolkit/util/file"
)

// DefaultHubBranch 是技能/MCP 市场未显式配置分支时的默认拉取分支。
// 单一来源：hub.New 与 NewMcpHub 共用，避免两处硬编码漂移（曾出现 hub.go=v0.0.5 / mcp_hub.go=v0.0.2 不一致）。
// 发版时随 config/defaults.go 的 Hub.Branch 一并更新。
const DefaultHubBranch = "v0.0.7"

// HubConfig 技能市场配置
type HubConfig struct {
	Enabled bool   `yaml:"enabled"`
	RepoURL string `yaml:"repo_url"` // 默认: https://github.com/hexagon-codes/hexclaw-hub
	Branch  string `yaml:"branch"`   // 默认: v0.0.7
}

// SkillMeta 技能/MCP Server 元数据
type SkillMeta struct {
	Name         string            `json:"name"`
	DisplayName  string            `json:"display_name"`
	Description  string            `json:"description"`
	Version      string            `json:"version"`
	Author       string            `json:"author"`
	Category     string            `json:"category"`
	Type         string            `json:"type,omitempty"` // "skill" (default) 或 "mcp"
	Tags         []string          `json:"tags"`
	Dependencies []string          `json:"dependencies,omitempty"` // Skill 依赖列表
	URL          string            `json:"url"`                    // 技能文件下载 URL
	Command      string            `json:"command,omitempty"`      // MCP: 启动命令
	Args         []string          `json:"args,omitempty"`         // MCP: 命令参数
	Env          map[string]string `json:"env,omitempty"`          // MCP: stdio 凭证占位（MYSQL_HOST / MDB_MCP_CONNECTION_STRING 等）
	ConfigHint   string            `json:"config_hint,omitempty"`  // MCP: 配置提示
	Source       string            `json:"source,omitempty"`       // MCP: 来源标记
	Downloads    int               `json:"downloads"`
	Rating       float64           `json:"rating"`
}

// Catalog 技能目录
type Catalog struct {
	Version   string      `json:"version"`
	UpdatedAt time.Time   `json:"updated_at"`
	Skills    []SkillMeta `json:"skills"`
}

// hubRefreshTTL 后台刷新节流：内存 catalog 在该时长内视为足够新，不重复打网络。
const hubRefreshTTL = 10 * time.Minute

// hubRetryBackoff 失败退避：上次刷新尝试（无论成败）后该时长内不再触网，
// 防离线时每个市场请求都 re-spawn 一个最长 30s 的失败刷新协程。
const hubRetryBackoff = 60 * time.Second

// Hub 在线技能市场客户端（离线优先：内存 → 磁盘缓存 → 内嵌种子 → 后台网络刷新）。
type Hub struct {
	cfg         HubConfig
	catalog     *Catalog
	mu          sync.RWMutex
	client      *http.Client
	skillsDir   string
	cacheDir    string      // 磁盘缓存目录（空=禁用磁盘缓存层）；最近一次成功拉取落此
	lastSync    time.Time   // 最近一次「网络」刷新成功时间（seed 不计），用于 TTL 节流
	lastAttempt time.Time   // 最近一次刷新尝试时间（含失败），用于失败退避节流
	refreshing  atomic.Bool // 保证同一时刻至多一个后台刷新协程
}

// DefaultCacheDir 返回市场磁盘缓存默认目录（~/.hexclaw/cache）。
// 桌面 Hub 与 CLI/agentic 的 McpHub 共用同一缓存文件，互相暖启。
func DefaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hexclaw", "cache")
}

// SetCacheDir 启用「最近一次成功拉取」磁盘缓存层（跨重启）。dir 为空则禁用。
func (h *Hub) SetCacheDir(dir string) {
	h.mu.Lock()
	h.cacheDir = dir
	h.mu.Unlock()
}

// New 创建技能市场客户端
func New(cfg HubConfig, skillsDir string) *Hub {
	if cfg.RepoURL == "" {
		cfg.RepoURL = "https://github.com/hexagon-codes/hexclaw-hub"
	} else if !strings.HasPrefix(cfg.RepoURL, "http://") && !strings.HasPrefix(cfg.RepoURL, "https://") && !strings.HasPrefix(cfg.RepoURL, "file://") && !strings.HasPrefix(cfg.RepoURL, "/") && !strings.HasPrefix(cfg.RepoURL, "~") && !strings.HasPrefix(cfg.RepoURL, ".") {
		cfg.RepoURL = "https://" + cfg.RepoURL
	}
	if cfg.Branch == "" {
		cfg.Branch = DefaultHubBranch
	}

	return &Hub{
		cfg:       cfg,
		client:    httpx.RawClient(httpx.WithRawTimeout(30 * time.Second)),
		skillsDir: skillsDir,
	}
}

// catalogURL 构造 index.json 的 raw URL
func (h *Hub) catalogURL() string {
	if dir, ok := h.localRepoDir(); ok {
		return filepath.Join(dir, "index.json")
	}
	repoURL := strings.TrimSuffix(h.cfg.RepoURL, ".git")
	repoURL = strings.Replace(repoURL, "github.com", "raw.githubusercontent.com", 1)
	return repoURL + "/" + h.cfg.Branch + "/index.json"
}

// mcpRegistryURL 构造 mcp-registry.json 的 raw URL
func (h *Hub) mcpRegistryURL() string {
	if dir, ok := h.localRepoDir(); ok {
		return filepath.Join(dir, "mcp-registry.json")
	}
	repoURL := strings.TrimSuffix(h.cfg.RepoURL, ".git")
	repoURL = strings.Replace(repoURL, "github.com", "raw.githubusercontent.com", 1)
	return repoURL + "/" + h.cfg.Branch + "/mcp-registry.json"
}

// mcpRegistry MCP Server 注册表
type mcpRegistry struct {
	Servers []SkillMeta `json:"servers"`
}

// Refresh 从远程获取最新技能目录（含 MCP 注册表），成功后写入磁盘缓存。
// 这是离线优先分层里的「网络」层；失败时调用方应回退到 seed()（缓存/内嵌）。
func (h *Hub) Refresh(ctx context.Context) error {
	body, err := h.readCatalog(ctx)
	if err != nil {
		return err
	}

	catalog, err := parseIndexCatalog(body)
	if err != nil {
		return err
	}

	// 加载 MCP 注册表并合并
	if mcpBody, err := h.readURL(ctx, h.mcpRegistryURL()); err == nil {
		mergeMcpRegistry(&catalog, mcpBody)
	}

	h.setCatalog(&catalog, true)
	return nil
}

// EnsureCatalog 保证内存 catalog 非空且尽量新，且「永不阻塞在网络上」：
//  1. seed()：从磁盘缓存→内嵌种子即时填充（离线安全，微秒级）；
//  2. 若 catalog 陈旧（从未网络刷新或超过 TTL），甩一个后台协程做网络刷新，本调用立即返回。
//
// HTTP handler / CLI / agentic 安装技能统一调它，替代旧的「首访同步 Refresh（最长 30s 阻塞、失败即空）」。
func (h *Hub) EnsureCatalog() {
	h.seed()
	h.maybeRefreshAsync()
}

// seed 在 catalog 为空时，按「磁盘缓存（上次成功拉取）→ 内嵌种子（出厂快照）」顺序即时填充。
// 不触网、不设 lastSync —— 故 EnsureCatalog 仍会触发一次后台网络刷新拉取更新条目。
func (h *Hub) seed() {
	h.mu.RLock()
	has := h.catalog != nil
	h.mu.RUnlock()
	if has {
		return
	}

	// 内嵌出厂快照永远可用；磁盘缓存仅在「不比内嵌旧」时才采用——否则二进制升级后离线，
	// 旧缓存（上次拉取的旧版分支内容）会盖过更新的内嵌种子，市场退化成旧版。
	// moving-branch 场景缓存比内嵌新时仍优先缓存（保留新鲜度）。
	chosen := embeddedCatalog()
	if c, ok := h.readCache(); ok && !c.UpdatedAt.Before(chosen.UpdatedAt) {
		chosen = c
	}

	h.mu.Lock()
	if h.catalog == nil {
		h.catalog = chosen
	}
	h.mu.Unlock()
}

// maybeRefreshAsync 在 catalog 陈旧时启动至多一个后台网络刷新协程（自带 ctx，与请求生命周期解耦）。
func (h *Hub) maybeRefreshAsync() {
	h.mu.RLock()
	skip := !shouldRefresh(time.Now(), h.lastSync, h.lastAttempt)
	h.mu.RUnlock()
	if skip {
		return
	}
	if !h.refreshing.CompareAndSwap(false, true) {
		return
	}
	h.mu.Lock()
	h.lastAttempt = time.Now()
	h.mu.Unlock()
	go func() {
		defer h.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		_ = h.Refresh(ctx) // best-effort：失败保留已 seed 的内容
	}()
}

// shouldRefresh 决定是否该触发一次后台网络刷新（纯函数，便于穷举节流真值表）：
//   - 成功拉取后 hubRefreshTTL 内：数据够新 → 跳过；
//   - 任一刷新尝试（含失败）后 hubRetryBackoff 内：退避 → 跳过（防离线每请求重 spawn 失败协程）。
func shouldRefresh(now, lastSync, lastAttempt time.Time) bool {
	if !lastSync.IsZero() && now.Sub(lastSync) < hubRefreshTTL {
		return false
	}
	if !lastAttempt.IsZero() && now.Sub(lastAttempt) < hubRetryBackoff {
		return false
	}
	return true
}

// setCatalog 原子替换内存 catalog；network=true 时记 lastSync 并落磁盘缓存。
func (h *Hub) setCatalog(c *Catalog, network bool) {
	h.mu.Lock()
	h.catalog = c
	if network {
		h.lastSync = time.Now()
	}
	dir := h.cacheDir
	h.mu.Unlock()

	if network && dir != "" {
		h.writeCache(dir, c)
	}
}

func (h *Hub) cacheFile(dir string) string { return filepath.Join(dir, "hub-catalog.json") }

// writeCache 以 temp+rename 原子落盘，避免进程中途退出（如 CLI 后台刷新未完）写出半截损坏缓存。
func (h *Hub) writeCache(dir string, c *Catalog) {
	if c == nil {
		return
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := fileutil.MkdirAll(dir); err != nil {
		return
	}
	// 唯一化 temp：桌面 Hub 与 CLI/agentic McpHub 是不同进程、共用同一缓存文件，
	// 固定 .tmp 名会被跨进程并发写互相截断（feedback_review_concurrency_lessons：tmp 唯一化）。
	tmp := fmt.Sprintf("%s.tmp.%d", h.cacheFile(dir), os.Getpid())
	// 0600：缓存落用户 home(~/.hexclaw/cache)，无需其他用户可读（gosec G306）。
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, h.cacheFile(dir)); err != nil {
		_ = os.Remove(tmp) // rename 失败别留垃圾 temp
	}
}

func (h *Hub) readCache() (*Catalog, bool) {
	h.mu.RLock()
	dir := h.cacheDir
	h.mu.RUnlock()
	if dir == "" {
		return nil, false
	}
	data, err := os.ReadFile(h.cacheFile(dir))
	if err != nil {
		return nil, false
	}
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, false
	}
	return &c, true
}

// embeddedCatalog 解析随二进制出厂的内嵌快照（index.json + mcp-registry.json），合并为统一 catalog。
func embeddedCatalog() *Catalog {
	cat, err := parseIndexCatalog(embeddedCatalogJSON)
	if err != nil {
		return &Catalog{}
	}
	mergeMcpRegistry(&cat, embeddedMcpRegistryJSON)
	return &cat
}

// parseIndexCatalog 解析 index.json（skills），未标 type 的默认 "skill"。
func parseIndexCatalog(body []byte) (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("解析技能目录失败: %w", err)
	}
	for i := range catalog.Skills {
		if catalog.Skills[i].Type == "" {
			catalog.Skills[i].Type = "skill"
		}
	}
	return catalog, nil
}

// mergeMcpRegistry 解析 mcp-registry.json 的 servers 并以 Type="mcp" 并入 catalog。
func mergeMcpRegistry(catalog *Catalog, mcpBody []byte) {
	servers, err := parseMcpServers(mcpBody)
	if err != nil {
		return
	}
	for i := range servers {
		servers[i].Type = "mcp"
	}
	catalog.Skills = append(catalog.Skills, servers...)
}

// parseMcpServers 解析 mcp-registry.json。真实格式是对象 {version,updated_at,servers:[...]}；
// 旧 bug 是把整个文件当裸数组反序列化 → 必失败。这里按 .servers 解析，并兼容极老裸数组格式（容错回退）。
func parseMcpServers(data []byte) ([]SkillMeta, error) {
	var reg mcpRegistry
	if err := json.Unmarshal(data, &reg); err == nil && reg.Servers != nil {
		return reg.Servers, nil
	}
	var bare []SkillMeta
	if err := json.Unmarshal(data, &bare); err != nil {
		return nil, err
	}
	return bare, nil
}

// GetCatalog 获取缓存的技能目录
func (h *Hub) GetCatalog() *Catalog {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.catalog
}

// Search 搜索技能（按名称/描述/标签模糊匹配）
func (h *Hub) Search(query string) []SkillMeta {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.catalog == nil {
		return nil
	}

	query = strings.ToLower(query)
	var results []SkillMeta
	for _, s := range h.catalog.Skills {
		if matchSkill(s, query) {
			results = append(results, s)
		}
	}
	return results
}

// ListByCategory 按分类列出技能
func (h *Hub) ListByCategory(category string) []SkillMeta {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.catalog == nil {
		return nil
	}

	category = strings.ToLower(category)
	var results []SkillMeta
	for _, s := range h.catalog.Skills {
		if strings.ToLower(s.Category) == category {
			results = append(results, s)
		}
	}
	return results
}

// Install 从 Hub 安装技能到本地
func (h *Hub) Install(ctx context.Context, name string) error {
	content, err := h.Content(ctx, name)
	if err != nil {
		return err
	}

	// 路径安全：防止路径穿越
	safeName := filepath.Base(name)
	if safeName != name || strings.ContainsAny(name, `/\..`) {
		return fmt.Errorf("非法技能名称: %s", name)
	}

	// 写入本地目录
	if err := fileutil.MkdirAll(h.skillsDir); err != nil {
		return fmt.Errorf("创建技能目录失败: %w", err)
	}

	path := filepath.Join(h.skillsDir, safeName+".md")
	// 二次验证：确保最终路径在 skillsDir 内
	absPath, _ := filepath.Abs(path)
	absDir, _ := filepath.Abs(h.skillsDir)
	if !strings.HasPrefix(absPath, filepath.Clean(absDir)+string(filepath.Separator)) {
		return fmt.Errorf("路径越界: %s", name)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("保存技能失败: %w", err)
	}

	return nil
}

// Content 获取 Hub 中某技能的 SKILL.md 原文（不落盘，供「安装前预览」）。
// 复用 Install 的来源解析与 readSkillContent 下载路径（同一安全约束与 1 MB 上限），
// 仅省去写盘——因此不引入任何新的 fetch 攻击面。
func (h *Hub) Content(ctx context.Context, name string) ([]byte, error) {
	target, ok := h.findSkill(name)
	if !ok {
		return nil, fmt.Errorf("技能 %s 未找到", name)
	}
	return h.readSkillContent(ctx, h.resolveDownloadURL(target, name))
}

// findSkill 在缓存目录中按精确名查技能（读锁）。
func (h *Hub) findSkill(name string) (SkillMeta, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.catalog == nil {
		return SkillMeta{}, false
	}
	for _, s := range h.catalog.Skills {
		if s.Name == name {
			return s, true
		}
	}
	return SkillMeta{}, false
}

// resolveDownloadURL 计算技能 SKILL.md 的下载地址：
// 优先 meta.URL；其次本地仓库 skills/<name>.md；最后回退默认 raw URL 模式。
func (h *Hub) resolveDownloadURL(target SkillMeta, name string) string {
	if target.URL != "" {
		return target.URL
	}
	if dir, ok := h.localRepoDir(); ok {
		return filepath.Join(dir, "skills", name+".md")
	}
	repoURL := strings.TrimSuffix(h.cfg.RepoURL, ".git")
	repoURL = strings.Replace(repoURL, "github.com", "raw.githubusercontent.com", 1)
	return repoURL + "/" + h.cfg.Branch + "/skills/" + name + ".md"
}

func (h *Hub) localRepoDir() (string, bool) {
	repoURL := strings.TrimSpace(h.cfg.RepoURL)
	if repoURL == "" {
		return "", false
	}
	if strings.HasPrefix(repoURL, "file://") {
		u, err := url.Parse(repoURL)
		if err != nil {
			return "", false
		}
		if u.Path == "" {
			return "", false
		}
		return filepath.Clean(u.Path), true
	}
	if strings.HasPrefix(repoURL, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return filepath.Join(home, repoURL[2:]), true
	}
	if filepath.IsAbs(repoURL) || strings.HasPrefix(repoURL, "./") || strings.HasPrefix(repoURL, "../") {
		return filepath.Clean(repoURL), true
	}
	return "", false
}

func (h *Hub) readCatalog(ctx context.Context) ([]byte, error) {
	return h.readURL(ctx, h.catalogURL())
}

func (h *Hub) readURL(ctx context.Context, url string) ([]byte, error) {
	// 本地文件路径
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		body, err := os.ReadFile(url)
		if err != nil {
			return nil, fmt.Errorf("读取本地文件失败: %w", err)
		}
		return body, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取远程文件失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取远程文件失败: HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}
	return body, nil
}

func (h *Hub) readSkillContent(ctx context.Context, source string) ([]byte, error) {
	if dir, ok := h.localRepoDir(); ok && isLocalSkillSource(source, dir) {
		content, err := os.ReadFile(filepath.Clean(source))
		if err != nil {
			return nil, fmt.Errorf("读取本地技能内容失败: %w", err)
		}
		return content, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("创建下载请求失败: %w", err)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("下载技能失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载技能失败: HTTP %d", resp.StatusCode)
	}

	content, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取技能内容失败: %w", err)
	}
	return content, nil
}

func isLocalSkillSource(source, repoDir string) bool {
	cleanSource := filepath.Clean(source)
	cleanRepo := filepath.Clean(repoDir) + string(filepath.Separator)
	return strings.HasPrefix(cleanSource, cleanRepo)
}

// Uninstall 卸载本地技能
func (h *Hub) Uninstall(name string) error {
	// 路径安全
	safeName := filepath.Base(name)
	if safeName != name || strings.ContainsAny(name, `/\..`) {
		return fmt.Errorf("非法技能名称: %s", name)
	}

	path := filepath.Join(h.skillsDir, safeName+".md")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("技能 %s 未安装", name)
		}
		return fmt.Errorf("删除技能失败: %w", err)
	}
	return nil
}

func matchSkill(s SkillMeta, query string) bool {
	if strings.Contains(strings.ToLower(s.Name), query) {
		return true
	}
	if strings.Contains(strings.ToLower(s.Description), query) {
		return true
	}
	if strings.Contains(strings.ToLower(s.DisplayName), query) {
		return true
	}
	for _, tag := range s.Tags {
		if strings.Contains(strings.ToLower(tag), query) {
			return true
		}
	}
	return false
}
