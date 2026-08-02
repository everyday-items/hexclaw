// Package hub 提供 HexClaw 在线技能市场
//
// 从远程 Git 仓库（默认 hexagon-codes/hexclaw-hub）获取技能目录，
// 支持搜索、浏览、安装和卸载技能。
package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

// MCPArtifact identifies the package release declared by a Hub MCP entry.
// Integrity is registry-native admission metadata (npm SRI or PyPI sha256);
// it does not prove the bytes eventually selected by a package manager and is
// not a statement that the package is trusted or safe.
type MCPArtifact struct {
	Ecosystem      string `json:"ecosystem"`
	Package        string `json:"package"`
	Version        string `json:"version"`
	Integrity      string `json:"integrity"`
	SourceRegistry string `json:"source_registry,omitempty"`
	ResolvedAt     string `json:"resolved_at,omitempty"`
}

// MCPRegistryIdentity records which registry document was merged into a
// catalog snapshot. UpdatedAt remains an opaque upstream identity field: the
// Hub has no contract that makes it comparable with Catalog.UpdatedAt.
type MCPRegistryIdentity struct {
	Version   string `json:"version,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	SHA256    string `json:"sha256"`
}

// SkillMeta 技能/MCP Server 元数据
type SkillMeta struct {
	Name             string            `json:"name"`
	DisplayName      string            `json:"display_name"`
	Description      string            `json:"description"`
	Version          string            `json:"version"`
	Author           string            `json:"author"`
	Category         string            `json:"category"`
	Type             string            `json:"type,omitempty"` // "skill" (default) 或 "mcp"
	Tags             []string          `json:"tags"`
	Dependencies     []string          `json:"dependencies,omitempty"` // Skill 依赖列表
	URL              string            `json:"url"`                    // 技能文件下载 URL
	Command          string            `json:"command,omitempty"`      // MCP: 启动命令
	Args             []string          `json:"args,omitempty"`         // MCP: 命令参数
	Env              map[string]string `json:"env,omitempty"`          // MCP: stdio 凭证占位（MYSQL_HOST / MDB_MCP_CONNECTION_STRING 等）
	ConfigHint       string            `json:"config_hint,omitempty"`  // MCP: 配置提示
	Source           string            `json:"source,omitempty"`       // MCP: 来源标记
	Status           string            `json:"status,omitempty"`       // MCP: pinned 或 quarantined
	QuarantineReason string            `json:"quarantine_reason,omitempty"`
	Artifact         *MCPArtifact      `json:"artifact,omitempty"`
	File             string            `json:"file,omitempty"`
	SHA256           string            `json:"sha256,omitempty"`
	Size             int64             `json:"size,omitempty"`
	Trust            string            `json:"trust,omitempty"`
	MinEngineVersion string            `json:"min_engine_version,omitempty"`
	License          string            `json:"license,omitempty"`
	Requires         []string          `json:"requires,omitempty"`
	Outputs          []string          `json:"outputs,omitempty"`
	SchemaVersion    string            `json:"schema_version,omitempty"`
	Eval             string            `json:"eval,omitempty"`
	Acceptance       string            `json:"acceptance,omitempty"`
	Downloads        int               `json:"downloads"`
	Rating           float64           `json:"rating"`
}

// Catalog 技能目录
type Catalog struct {
	Version     string               `json:"version"`
	UpdatedAt   time.Time            `json:"updated_at"`
	MCPRegistry *MCPRegistryIdentity `json:"mcp_registry,omitempty"`
	Skills      []SkillMeta          `json:"skills"`
}

// hubRefreshTTL 后台刷新节流：内存 catalog 在该时长内视为足够新，不重复打网络。
const hubRefreshTTL = 10 * time.Minute

// hubRetryBackoff 失败退避：上次刷新尝试（无论成败）后该时长内不再触网，
// 防离线时每个市场请求都 re-spawn 一个最长 30s 的失败刷新协程。
const hubRetryBackoff = 60 * time.Second

// Hub 在线技能市场客户端（离线优先：内存 → 磁盘缓存 → 内嵌种子 → 后台网络刷新）。
type Hub struct {
	cfg           HubConfig
	catalog       *Catalog
	mu            sync.RWMutex
	refreshMu     sync.Mutex
	cacheCommitMu sync.Mutex
	client        *http.Client
	skillsDir     string
	cacheDir      string      // 磁盘缓存目录（空=禁用磁盘缓存层）；最近一次成功拉取落此
	lastSync      time.Time   // 最近一次「网络」刷新成功时间（seed 不计），用于 TTL 节流
	lastAttempt   time.Time   // 最近一次刷新尝试时间（含失败），用于失败退避节流
	refreshing    atomic.Bool // 保证同一时刻至多一个后台刷新协程
	cacheWriteOps *hubCacheWriteOps
	// cacheLockRelease is nil in production and permits failure injection in tests.
	cacheLockRelease func(*hubCacheFileLock) error
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
	h.cacheCommitMu.Lock()
	defer h.cacheCommitMu.Unlock()
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
	Version   string      `json:"version"`
	UpdatedAt string      `json:"updated_at"`
	Servers   []SkillMeta `json:"servers"`
}

// RefreshDegradedError reports that one source of the unified Hub snapshot
// failed after another source had succeeded. The partial candidate is never
// published, cached, or counted as a successful refresh.
type RefreshDegradedError struct {
	Source string
	Err    error
}

func (e *RefreshDegradedError) Error() string {
	return fmt.Sprintf("hub refresh degraded at %s: %v", e.Source, e.Err)
}

func (e *RefreshDegradedError) Unwrap() error { return e.Err }

func (e *RefreshDegradedError) Degraded() bool { return true }

// RefreshStaleError reports a candidate whose explicit UpdatedAt predates the
// currently published snapshot. Version strings are intentionally not ordered.
type RefreshStaleError struct {
	CurrentUpdatedAt   time.Time
	CandidateUpdatedAt time.Time
}

func (e *RefreshStaleError) Error() string {
	return fmt.Sprintf("hub refresh candidate is stale: candidate=%s current=%s",
		e.CandidateUpdatedAt.UTC().Format(time.RFC3339Nano),
		e.CurrentUpdatedAt.UTC().Format(time.RFC3339Nano))
}

func (e *RefreshStaleError) Stale() bool { return true }

// RefreshUnorderedError reports snapshots that cannot be safely ordered using
// the current cache contract. Version strings are labels, not sortable data.
type RefreshUnorderedError struct {
	CurrentUpdatedAt   time.Time
	CandidateUpdatedAt time.Time
	Reason             string
}

func (e *RefreshUnorderedError) Error() string {
	return fmt.Sprintf("hub refresh snapshots cannot be ordered: %s; a monotonic updated_at snapshot contract is required", e.Reason)
}

func (e *RefreshUnorderedError) Unordered() bool { return true }

// CacheCommitOutcomeError reports an error after the destination cache was
// atomically replaced and verified. Callers must retain the committed snapshot
// in memory even though durability or lock-release confirmation was degraded.
type CacheCommitOutcomeError struct {
	Stage               string
	Err                 error
	durabilityUncertain bool
}

func (e *CacheCommitOutcomeError) Error() string {
	return fmt.Sprintf("hub cache committed but %s failed: %v", e.Stage, e.Err)
}

func (e *CacheCommitOutcomeError) Unwrap() error { return e.Err }

func (e *CacheCommitOutcomeError) Committed() bool { return true }

func (e *CacheCommitOutcomeError) DurabilityUncertain() bool {
	return e.durabilityUncertain
}

// Refresh 从远程获取最新技能目录（含 MCP 注册表），成功后写入磁盘缓存。
// 这是离线优先分层里的「网络」层；失败时调用方应回退到 seed()（缓存/内嵌）。
func (h *Hub) Refresh(ctx context.Context) error {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()

	body, err := h.readCatalog(ctx)
	if err != nil {
		return err
	}

	catalog, err := parseIndexCatalog(body)
	if err != nil {
		return err
	}

	// A catalog is a single snapshot assembled from both upstream documents.
	// Do not publish a skills-only candidate when the MCP half is unavailable
	// or malformed: readers and the disk cache must retain the last complete
	// snapshot, and lastSync must continue to describe a complete refresh.
	mcpBody, err := h.readURL(ctx, h.mcpRegistryURL())
	if err != nil {
		return &RefreshDegradedError{Source: "mcp-registry", Err: err}
	}
	if err := mergeMcpRegistry(&catalog, mcpBody); err != nil {
		return &RefreshDegradedError{Source: "mcp-registry", Err: err}
	}

	return h.commitRefreshedCatalog(ctx, &catalog)
}

// EnsureCatalog 保证内存 catalog 非空且尽量新，且「永不阻塞在网络上」：
//  1. seed()：从磁盘缓存→内嵌种子即时填充（离线安全，微秒级）；
//  2. 若 catalog 陈旧（从未网络刷新或超过 TTL），甩一个后台协程做网络刷新，本调用立即返回。
//
// HTTP handler / CLI / agentic 安装技能统一调它，替代旧的「首访同步 Refresh（最长 30s 阻塞、失败即空）」。
func (h *Hub) EnsureCatalog() {
	// A configured local repository is deterministic file I/O, not a remote
	// availability dependency. Load it synchronously before the embedded seed;
	// otherwise integrity metadata from the embedded catalog can be paired with
	// different local skill bytes until the async refresh races to completion.
	if _, local := h.localRepoDir(); local {
		h.mu.RLock()
		loaded := h.catalog != nil
		h.mu.RUnlock()
		if !loaded {
			if err := h.Refresh(context.Background()); err == nil {
				return
			}
		}
	}
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

// commitRefreshedCatalog first durably persists an enabled cache and only then
// publishes the same complete snapshot in memory. Shared-cache comparison is
// performed while holding the cross-process cache lock.
func (h *Hub) commitRefreshedCatalog(ctx context.Context, c *Catalog) error {
	if c == nil {
		return fmt.Errorf("hub refresh candidate is nil")
	}
	if err := validateCatalogMCPEntries(c, true); err != nil {
		return &RefreshDegradedError{Source: "mcp-registry", Err: err}
	}
	h.cacheCommitMu.Lock()
	defer h.cacheCommitMu.Unlock()

	h.mu.RLock()
	cacheDir := h.cacheDir
	h.mu.RUnlock()
	if cacheDir != "" {
		if err := h.commitSharedCache(ctx, cacheDir, c); err != nil {
			var stale *RefreshStaleError
			var unordered *RefreshUnorderedError
			if errors.As(err, &stale) || errors.As(err, &unordered) {
				return err
			}
			return &RefreshDegradedError{Source: "cache", Err: err}
		}
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if err := validateCatalogAdvance(h.catalog, c); err != nil {
		return err
	}
	h.catalog = c
	h.lastSync = time.Now()
	return nil
}

func (h *Hub) commitSharedCache(ctx context.Context, dir string, candidate *Catalog) (err error) {
	lock, err := acquireHubCacheFileLock(ctx, dir)
	if err != nil {
		return fmt.Errorf("acquire hub cache lock: %w", err)
	}
	committed := false
	defer func() {
		release := lock.Close
		if h.cacheLockRelease != nil {
			release = func() error { return h.cacheLockRelease(lock) }
		}
		if releaseErr := release(); releaseErr != nil {
			if committed {
				releaseErr = &CacheCommitOutcomeError{Stage: "lock release", Err: releaseErr}
			}
			err = errors.Join(err, releaseErr)
		}
	}()

	current, exists, err := readCatalogCacheFile(filepath.Join(dir, "hub-catalog.json"))
	if err != nil {
		return err
	}
	if exists && validateCatalogMCPEntries(current, true) != nil {
		current, exists = nil, false
	}
	if exists {
		if err := validateCatalogAdvance(current, candidate); err != nil {
			return err
		}
	}
	ops := defaultHubCacheWriteOps()
	if h.cacheWriteOps != nil {
		ops = *h.cacheWriteOps
	}
	prepared, err := prepareHubCache(dir, candidate, ops)
	if err != nil {
		return err
	}
	defer prepared.cleanup()

	err = func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		// seed may have published a snapshot while this refresh waited for the
		// shared lock or prepared its temp file. Revalidate immediately before
		// the atomic replace so disk and memory cannot move backwards.
		if err := validateCatalogAdvance(h.catalog, candidate); err != nil {
			return err
		}
		if err := prepared.replace(ops); err != nil {
			return err
		}
		h.catalog = candidate
		h.lastSync = time.Now()
		committed = true
		return nil
	}()
	if err != nil {
		return err
	}
	if err := ops.syncParent(dir); err != nil {
		if readbackErr := confirmCommittedCatalog(filepath.Join(dir, "hub-catalog.json"), candidate); readbackErr != nil {
			return errors.Join(fmt.Errorf("sync hub cache parent directory: %w", err), readbackErr)
		}
		return &CacheCommitOutcomeError{
			Stage: "parent directory sync", Err: err, durabilityUncertain: true,
		}
	}
	return nil
}

func confirmCommittedCatalog(path string, expected *Catalog) error {
	actual, exists, err := readCatalogCacheFile(path)
	if err != nil {
		return fmt.Errorf("read back committed hub cache: %w", err)
	}
	if !exists || !catalogsEquivalent(actual, expected) {
		return fmt.Errorf("committed hub cache readback digest mismatch")
	}
	return nil
}

func readCatalogCacheFile(path string) (*Catalog, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read current hub cache: %w", err)
	}
	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, false, fmt.Errorf("decode current hub cache: %w", err)
	}
	return &catalog, true, nil
}

func validateCatalogAdvance(current, candidate *Catalog) error {
	if current == nil {
		return nil
	}
	if catalogsEquivalent(current, candidate) {
		return nil
	}
	if current.UpdatedAt.IsZero() || candidate.UpdatedAt.IsZero() {
		return &RefreshUnorderedError{
			CurrentUpdatedAt:   current.UpdatedAt,
			CandidateUpdatedAt: candidate.UpdatedAt,
			Reason:             "one or both snapshots are missing updated_at",
		}
	}
	if candidate.UpdatedAt.Before(current.UpdatedAt) {
		return &RefreshStaleError{
			CurrentUpdatedAt: current.UpdatedAt, CandidateUpdatedAt: candidate.UpdatedAt,
		}
	}
	if candidate.UpdatedAt.Equal(current.UpdatedAt) {
		return &RefreshUnorderedError{
			CurrentUpdatedAt:   current.UpdatedAt,
			CandidateUpdatedAt: candidate.UpdatedAt,
			Reason:             "different snapshots declare the same updated_at",
		}
	}
	return nil
}

func catalogsEquivalent(first, second *Catalog) bool {
	if first == nil || second == nil {
		return first == second
	}
	firstJSON, firstErr := json.Marshal(first)
	secondJSON, secondErr := json.Marshal(second)
	if firstErr != nil || secondErr != nil {
		return false
	}
	return sha256.Sum256(firstJSON) == sha256.Sum256(secondJSON)
}

func (h *Hub) cacheFile(dir string) string { return filepath.Join(dir, "hub-catalog.json") }

type hubCacheWriteOps struct {
	syncTemp   func(*os.File) error
	replace    func(oldPath, newPath string) error
	syncParent func(dir string) error
}

func defaultHubCacheWriteOps() hubCacheWriteOps {
	return hubCacheWriteOps{
		syncTemp:   (*os.File).Sync,
		replace:    replaceHubCacheFile,
		syncParent: syncHubCacheParentDirectory,
	}
}

// writeCache persists one complete snapshot and returns every failure to the
// refresh coordinator. Tests may inject commit operations per Hub instance.
func (h *Hub) writeCache(dir string, c *Catalog) error {
	ops := defaultHubCacheWriteOps()
	if h.cacheWriteOps != nil {
		ops = *h.cacheWriteOps
	}
	return writeCacheWithOps(dir, c, ops)
}

// writeCacheWithOps commits in temp-sync, rename, parent-directory-sync order.
func writeCacheWithOps(dir string, c *Catalog, ops hubCacheWriteOps) error {
	prepared, err := prepareHubCache(dir, c, ops)
	if err != nil {
		return err
	}
	defer prepared.cleanup()
	if err := prepared.replace(ops); err != nil {
		return err
	}
	if err := ops.syncParent(dir); err != nil {
		if readbackErr := confirmCommittedCatalog(filepath.Join(dir, "hub-catalog.json"), c); readbackErr != nil {
			return errors.Join(fmt.Errorf("sync hub cache parent directory: %w", err), readbackErr)
		}
		return &CacheCommitOutcomeError{
			Stage: "parent directory sync", Err: err, durabilityUncertain: true,
		}
	}
	return nil
}

type preparedHubCache struct {
	tmpPath     string
	destination string
	committed   bool
}

func prepareHubCache(dir string, c *Catalog, ops hubCacheWriteOps) (_ *preparedHubCache, retErr error) {
	if c == nil {
		return nil, fmt.Errorf("hub cache catalog is nil")
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal hub cache: %w", err)
	}
	if err := fileutil.MkdirAll(dir); err != nil {
		return nil, fmt.Errorf("create hub cache directory: %w", err)
	}
	// CreateTemp is unique across both goroutines and processes. A PID-only
	// suffix collides when multiple Hub instances refresh in one process.
	tmp, err := os.CreateTemp(dir, ".hub-catalog-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create hub cache temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("chmod hub cache temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return nil, fmt.Errorf("write hub cache temp file: %w", err)
	}
	if err := ops.syncTemp(tmp); err != nil {
		return nil, fmt.Errorf("sync hub cache temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close hub cache temp file: %w", err)
	}
	return &preparedHubCache{
		tmpPath: tmpPath, destination: filepath.Join(dir, "hub-catalog.json"),
	}, nil
}

func (p *preparedHubCache) replace(ops hubCacheWriteOps) error {
	if err := ops.replace(p.tmpPath, p.destination); err != nil {
		return fmt.Errorf("rename hub cache temp file: %w", err)
	}
	p.committed = true
	return nil
}

func (p *preparedHubCache) cleanup() {
	if p != nil && !p.committed {
		_ = os.Remove(p.tmpPath)
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
	if err := validateCatalogMCPEntries(&c, true); err != nil {
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
	if err := mergeMcpRegistry(&cat, embeddedMcpRegistryJSON); err != nil {
		return &Catalog{}
	}
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
func mergeMcpRegistry(catalog *Catalog, mcpBody []byte) error {
	registry, err := parseMCPRegistry(mcpBody)
	if err != nil {
		return err
	}
	servers := make([]SkillMeta, len(registry.Servers))
	for i := range registry.Servers {
		servers[i] = registry.Servers[i]
		servers[i].Type = "mcp"
		if err := validateMCPRegistryEntry(servers[i]); err != nil {
			return fmt.Errorf("MCP registry entry %q: %w", servers[i].Name, err)
		}
	}
	digest := sha256.Sum256(mcpBody)
	catalog.MCPRegistry = &MCPRegistryIdentity{
		Version:   registry.Version,
		UpdatedAt: registry.UpdatedAt,
		SHA256:    fmt.Sprintf("%x", digest),
	}
	catalog.Skills = append(catalog.Skills, servers...)
	return nil
}

func validateMCPRegistryEntry(entry SkillMeta) error {
	if entry.Status == "quarantined" {
		if strings.TrimSpace(entry.QuarantineReason) == "" {
			return fmt.Errorf("quarantined entry is missing quarantine_reason")
		}
		if entry.Artifact != nil {
			return fmt.Errorf("quarantined entry must not retain artifact metadata")
		}
		return nil
	}
	_, err := ValidatePinnedMCPServer(MCPServerMetaFromSkill(entry))
	return err
}

func validateCatalogMCPEntries(catalog *Catalog, requireIdentity bool) error {
	if catalog == nil {
		return fmt.Errorf("catalog is nil")
	}
	hasMCP := false
	for _, entry := range catalog.Skills {
		if !strings.EqualFold(entry.Type, "mcp") {
			continue
		}
		hasMCP = true
		if err := validateMCPRegistryEntry(entry); err != nil {
			return fmt.Errorf("MCP catalog entry %q: %w", entry.Name, err)
		}
	}
	if !hasMCP || !requireIdentity {
		return nil
	}
	if catalog.MCPRegistry == nil {
		return fmt.Errorf("MCP catalog is missing registry identity")
	}
	digest, err := hex.DecodeString(catalog.MCPRegistry.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("MCP registry identity has invalid sha256")
	}
	return nil
}

// parseMcpServers 解析 mcp-registry.json。真实格式是对象 {version,updated_at,servers:[...]}；
// 旧 bug 是把整个文件当裸数组反序列化 → 必失败。这里按 .servers 解析，并兼容极老裸数组格式（容错回退）。
func parseMcpServers(data []byte) ([]SkillMeta, error) {
	registry, err := parseMCPRegistry(data)
	return registry.Servers, err
}

func parseMCPRegistry(data []byte) (mcpRegistry, error) {
	var reg mcpRegistry
	if err := json.Unmarshal(data, &reg); err == nil && reg.Servers != nil {
		return reg, nil
	}
	var bare []SkillMeta
	if err := json.Unmarshal(data, &bare); err != nil {
		return mcpRegistry{}, err
	}
	return mcpRegistry{Servers: bare}, nil
}

// GetCatalog 获取缓存的技能目录
func (h *Hub) GetCatalog() *Catalog {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.catalog == nil {
		return nil
	}
	snapshot := *h.catalog
	if h.catalog.MCPRegistry != nil {
		identity := *h.catalog.MCPRegistry
		snapshot.MCPRegistry = &identity
	}
	snapshot.Skills = make([]SkillMeta, len(h.catalog.Skills))
	for i := range h.catalog.Skills {
		snapshot.Skills[i] = cloneSkillMeta(h.catalog.Skills[i])
	}
	return &snapshot
}

func cloneSkillMeta(meta SkillMeta) SkillMeta {
	meta.Tags = slices.Clone(meta.Tags)
	meta.Dependencies = slices.Clone(meta.Dependencies)
	meta.Args = slices.Clone(meta.Args)
	meta.Env = maps.Clone(meta.Env)
	meta.Requires = slices.Clone(meta.Requires)
	meta.Outputs = slices.Clone(meta.Outputs)
	if meta.Artifact != nil {
		artifact := *meta.Artifact
		meta.Artifact = &artifact
	}
	return meta
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
	tmp, err := os.CreateTemp(absDir, ".hexclaw-skill-install-*.tmp")
	if err != nil {
		return fmt.Errorf("创建技能临时文件失败: %w", err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("设置技能文件权限失败: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("写入技能临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("同步技能临时文件失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭技能临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("原子安装技能失败: %w", err)
	}
	committed = true

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
	source, err := h.resolveDownloadURL(target, name)
	if err != nil {
		return nil, err
	}
	content, err := h.readSkillContent(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := verifySkillContent(target, content); err != nil {
		return nil, fmt.Errorf("技能 %s 完整性校验失败: %w", name, err)
	}
	return content, nil
}

func verifySkillContent(meta SkillMeta, content []byte) error {
	if meta.Size <= 0 || strings.TrimSpace(meta.SHA256) == "" {
		return fmt.Errorf("catalog 缺少 size/sha256")
	}
	if int64(len(content)) != meta.Size {
		return fmt.Errorf("size 不匹配: catalog=%d actual=%d", meta.Size, len(content))
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	if !strings.EqualFold(digest, meta.SHA256) {
		return fmt.Errorf("sha256 不匹配")
	}
	return nil
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
func (h *Hub) resolveDownloadURL(target SkillMeta, name string) (string, error) {
	if dir, ok := h.localRepoDir(); ok {
		expected := filepath.Join(dir, "skills", name+".md")
		if target.URL == "" {
			return expected, nil
		}
		if !isLocalSkillSource(target.URL, dir) {
			return "", fmt.Errorf("技能 %s 的下载路径不在已配置 Hub 内", name)
		}
		return filepath.Clean(target.URL), nil
	}
	repoURL := strings.TrimSuffix(h.cfg.RepoURL, ".git")
	repoURL = strings.Replace(repoURL, "github.com", "raw.githubusercontent.com", 1)
	expected := repoURL + "/" + h.cfg.Branch + "/skills/" + name + ".md"
	if target.URL == "" {
		return expected, nil
	}
	if !sameHTTPOrigin(target.URL, expected) {
		return "", fmt.Errorf("技能 %s 的下载 URL 跨越已配置 Hub origin", name)
	}
	return target.URL, nil
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

func (h *Hub) readURL(ctx context.Context, source string) ([]byte, error) {
	// 本地文件路径
	if !strings.HasPrefix(source, "http://") && !strings.HasPrefix(source, "https://") {
		body, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("读取本地文件失败: %w", err)
		}
		return body, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}

	client, err := h.sameOriginRedirectClient(source)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
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

	client, err := h.sameOriginRedirectClient(source)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
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

func (h *Hub) sameOriginRedirectClient(source string) (*http.Client, error) {
	initial, err := url.Parse(source)
	if err != nil || initial.Scheme == "" || initial.Host == "" || initial.User != nil {
		return nil, fmt.Errorf("Hub URL 非法")
	}
	if initial.Scheme != "http" && initial.Scheme != "https" {
		return nil, fmt.Errorf("Hub URL scheme 非法")
	}
	if h.client == nil {
		return nil, fmt.Errorf("Hub HTTP client 未配置")
	}
	client := *h.client
	previousCheck := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameParsedHTTPOrigin(initial, req.URL) {
			return fmt.Errorf("Hub redirect 跨 origin")
		}
		if previousCheck != nil {
			return previousCheck(req, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("Hub redirect 次数过多")
		}
		return nil
	}
	return &client, nil
}

func sameHTTPOrigin(first, second string) bool {
	a, errA := url.Parse(first)
	b, errB := url.Parse(second)
	return errA == nil && errB == nil && a.User == nil && b.User == nil && sameParsedHTTPOrigin(a, b)
}

func sameParsedHTTPOrigin(first, second *url.URL) bool {
	if first == nil || second == nil {
		return false
	}
	return strings.EqualFold(first.Scheme, second.Scheme) &&
		strings.EqualFold(first.Hostname(), second.Hostname()) &&
		effectiveHTTPPort(first) == effectiveHTTPPort(second)
}

func effectiveHTTPPort(parsed *url.URL) string {
	if port := parsed.Port(); port != "" {
		return port
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(parsed.Scheme, "http") {
		return "80"
	}
	return ""
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
