package builtin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/os/sandbox"
)

// CodeExecSkill 代码执行工具
//
// 在 HexClaw Sandbox（平台原生沙箱封装）内执行 Python/JavaScript/Go 代码。
// code_exec 是代码执行工具名；Sandbox 是其背后的执行边界和权限策略。
type CodeExecSkill struct {
	policyUpdateMu    sync.Mutex
	mu                sync.RWMutex
	policy            *codeExecPolicyState
	fileAccessRuntime *FileAccessBroker
	initializationErr error
	sandboxFactory    func(sandbox.Config) (sandbox.Sandbox, error)
	goHelperFactory   func(sandbox.Config) (sandbox.Sandbox, error)
	projectStager     codeExecProjectStager
	// goBuildCacheBase 保存由宿主进程生成的可信标准库缓存种子；每次执行只使用其私有副本。
	goBuildCacheBase    string
	goBuildCacheCleaner func(codeExecRun) error
	// scratchBase is instance-scoped so tests can keep project staging inside
	// testing.T's lifetime. Empty preserves the production /tmp default.
	scratchBase string
}

// codeExecPolicyState 是一次发布后只读的完整策略代际。
type codeExecPolicyState struct {
	cfg        sandbox.Config
	authorizer *FileAccessBroker
}

// SandboxPolicy 是 CodeExec 一次原子发布的完整运行策略。
type SandboxPolicy struct {
	NetworkEnabled bool
	ReadablePaths  []string
}

var errCodeExecHostNetworkUnsupported = errors.New("code execution host network is unsupported because destination filtering is unavailable")

// SandboxPolicyCandidate 表示已经完成构建、能力验证和关闭的待发布策略。
type SandboxPolicyCandidate struct {
	once   sync.Once
	finish func(commit bool)
}

// Commit 原子发布候选；重复 Commit 或后续 Discard 不产生效果。
func (c *SandboxPolicyCandidate) Commit() {
	if c == nil || c.finish == nil {
		return
	}
	c.once.Do(func() { c.finish(true) })
}

// Discard 放弃候选并释放写事务；重复调用不产生效果。
func (c *SandboxPolicyCandidate) Discard() {
	if c == nil || c.finish == nil {
		return
	}
	c.once.Do(func() { c.finish(false) })
}

// SetFileAccess 注入集中文件访问裁决器（FileAccessBroker）。
//
// code_exec 的 mode=file 入口文件、mode=project 项目根（含要加入沙箱只读放行的父目录）
// 在读取/授予前必须过 broker 的 allow-list 授权；未授权路径一律拒绝执行（fail-closed）。
// 若 broker 允许集为空（用户未授权任何目录），project_root=$HOME 之类必被拒。
func (s *CodeExecSkill) SetFileAccess(b *FileAccessBroker) {
	s.policyUpdateMu.Lock()
	defer s.policyUpdateMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	current := cloneCodeExecPolicyState(s.policy)
	current.authorizer = cloneCodeExecFileAccessBroker(b)
	s.fileAccessRuntime = b
	s.policy = current
}

type codeExecRequest struct {
	Mode        string
	Language    string
	Code        string
	EntryPoint  string
	ProjectRoot string
	Command     []string
	CommandText string
	Files       []codeExecInputFile
	Timeout     int
	Artifacts   bool
}

type codeExecInputFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type codeExecProjectStager func(
	ctx context.Context,
	hostProjectRoot string,
	stageRoot string,
	plan codeExecExecutionPlan,
	broker *FileAccessBroker,
	cfg sandbox.Config,
) (string, string, bool, error)

type codeExecRun struct {
	ID            string
	Base          string
	Root          string
	Workspace     string
	Scratch       string
	ArtifactDir   string
	LogDir        string
	CacheDir      string
	ProjectRoot   string
	WorkingDir    string
	ManifestPath  string
	Plan          codeExecExecutionPlan
	GoWorkPath    string
	GoVendored    bool
	StagedProject bool
	Config        sandbox.Config
	Budget        codeExecApplicationBudget
	OwnedRoots    []codeExecOwnedRunRoot
}

const (
	codeExecDefaultTimeoutSeconds    = 60
	codeExecDefaultMaxWorkspaceBytes = 1024 * 1024 * 1024
	codeExecDefaultMaxArtifactBytes  = 50 * 1024 * 1024
)

// codeExecApplicationBudget 是 HexClaw 在宿主侧执行的暂存与产物预算，
// 不代表 Toolkit Sandbox 已提供对应的操作系统实时硬配额。
type codeExecApplicationBudget struct {
	MaxWorkspaceBytes int64
	MaxArtifactBytes  int64
}

func codeExecApplicationBudgetFor(cfg sandbox.Config) codeExecApplicationBudget {
	budget := codeExecApplicationBudget{
		MaxWorkspaceBytes: cfg.MaxWorkspaceBytes,
		MaxArtifactBytes:  cfg.MaxArtifactBytes,
	}
	if budget.MaxWorkspaceBytes <= 0 {
		budget.MaxWorkspaceBytes = codeExecDefaultMaxWorkspaceBytes
	}
	if budget.MaxArtifactBytes <= 0 {
		budget.MaxArtifactBytes = codeExecDefaultMaxArtifactBytes
	}
	return budget
}

func (run codeExecRun) applicationBudget() codeExecApplicationBudget {
	budget := run.Budget
	if budget.MaxWorkspaceBytes <= 0 || budget.MaxArtifactBytes <= 0 {
		defaults := codeExecApplicationBudgetFor(run.Config)
		if budget.MaxWorkspaceBytes <= 0 {
			budget.MaxWorkspaceBytes = defaults.MaxWorkspaceBytes
		}
		if budget.MaxArtifactBytes <= 0 {
			budget.MaxArtifactBytes = defaults.MaxArtifactBytes
		}
	}
	return budget
}

type codeExecOwnedRunRoot struct {
	Path   string
	Parent string
	Owner  string
}

const codeExecRunOwnerMarkerName = ".hexclaw-run-owner"

type codeExecExecutionPlan struct {
	Command            []string
	Environment        map[string]string
	GoRuntime          bool
	GoTest             bool
	GoCommand          *codeExecGoCommand
	Toolchain          *codeExecGoToolchainDescriptor
	Helper             codeExecGoHelper
	stageDefaultOutput bool
	stageDefaultStderr bool
}

func newCodeExecExecutionPlan(
	ctx context.Context,
	cfg sandbox.Config,
	goRuntime bool,
	helper codeExecGoHelper,
) (codeExecExecutionPlan, error) {
	plan := codeExecExecutionPlan{
		GoRuntime:          goRuntime,
		Helper:             helper,
		stageDefaultOutput: cfg.MaxOutputBytes <= 0,
		stageDefaultStderr: cfg.MaxStderrBytes <= 0,
	}
	if !goRuntime {
		return plan, nil
	}
	toolchain, available, err := resolveCodeExecGoToolchainDescriptor(ctx, cfg, helper)
	if err != nil {
		return codeExecExecutionPlan{}, err
	}
	if !available {
		return plan, nil
	}
	plan.Toolchain = toolchain
	return plan, nil
}

func bindCodeExecExecutionPlanCommand(
	plan codeExecExecutionPlan,
	command []string,
) (codeExecExecutionPlan, error) {
	plan.Command = append([]string(nil), command...)
	if !plan.GoRuntime {
		return plan, nil
	}
	parsed, err := parseCodeExecGoUserCommand(plan.Command)
	if err != nil {
		return codeExecExecutionPlan{}, err
	}
	goIndex, err := codeExecDirectGoCommandIndex(plan.Command)
	if err != nil {
		return codeExecExecutionPlan{}, err
	}
	if plan.Toolchain == nil {
		return codeExecExecutionPlan{}, errors.New("go execution requires a bound toolchain descriptor")
	}
	if strings.TrimSpace(plan.Toolchain.Binary) == "" || strings.TrimSpace(plan.Toolchain.GOROOT) == "" {
		return codeExecExecutionPlan{}, errors.New("go execution plan requires a complete toolchain descriptor")
	}
	plan.Command, plan.Environment = normalizeCodeExecDirectGoCommand(
		plan.Command,
		goIndex,
		plan.Toolchain.Binary,
	)
	plan.GoCommand = &parsed
	plan.GoTest = parsed.Kind == codeExecGoCommandTest
	return plan, nil
}

func normalizeCodeExecDirectGoCommand(command []string, goIndex int, goBinary string) ([]string, map[string]string) {
	environment := make(map[string]string, goIndex)
	for index := 1; index < goIndex; index++ {
		key, value, _ := strings.Cut(command[index], "=")
		environment[key] = value
	}
	if strings.TrimSpace(goBinary) == "" {
		goBinary = command[goIndex]
	}
	normalized := make([]string, 1, len(command)-goIndex)
	normalized[0] = goBinary
	normalized = append(normalized, command[goIndex+1:]...)
	return normalized, environment
}

func codeExecDirectGoCommandIndex(command []string) (int, error) {
	if len(command) == 0 {
		return -1, errors.New("go execution requires structured direct go argv")
	}
	first := strings.TrimSpace(command[0])
	if codeExecLiteralCommand(first, "go", "go.exe") {
		return 0, nil
	}
	return -1, errors.New("go execution accepts only structured direct go argv")
}

func codeExecLiteralCommand(value string, allowed ...string) bool {
	if value == "" || filepath.IsAbs(value) || filepath.Base(value) != value {
		return false
	}
	for _, candidate := range allowed {
		if strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}

func codeExecControlledEnvironmentKey(key string) bool {
	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" || strings.HasPrefix(key, "GO") || strings.HasPrefix(key, "CGO_") ||
		strings.HasPrefix(key, "HEXCLAW_") || strings.HasPrefix(key, "DYLD_") ||
		strings.HasPrefix(key, "LD_") {
		return true
	}
	switch key {
	case "HOME", "PATH", "SHELL", "COMSPEC", "TMPDIR", "TMP", "TEMP",
		"XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA", "PIP_CACHE_DIR",
		"PYTHONPYCACHEPREFIX", "NPM_CONFIG_CACHE", "CC", "CXX", "FC", "AR", "PKG_CONFIG":
		return true
	default:
		return false
	}
}

const (
	codeExecGoBuildCacheSeedVersion   = "v2"
	codeExecGoBuildCacheManifestName  = ".manifest.json"
	codeExecGoHelperHardTimeout       = 60 * time.Second
	codeExecGoStageHardTimeout        = 2 * time.Minute
	codeExecGoHelperMaxOutputBytes    = 4 * 1024 * 1024
	codeExecGoHelperMaxStderrBytes    = 256 * 1024
	codeExecGoHelperMaxWorkspaceBytes = 256 * 1024 * 1024
	codeExecGoHelperMaxMemoryBytes    = 1024 * 1024 * 1024
	codeExecGoVendorMaxFiles          = 100_000
	codeExecGoVendorMaxDirectories    = 20_000
	codeExecGoVendorMaxEntries        = 120_000
)

type codeExecGoHelper struct {
	Factory func(sandbox.Config) (sandbox.Sandbox, error)
}

type codeExecGoToolchainDescriptor struct {
	Binary         string `json:"binary"`
	BinarySHA256   string `json:"binary_sha256"`
	CompileVersion string `json:"compile_version"`
	GOROOT         string `json:"goroot"`
	GOOS           string `json:"goos"`
	GOARCH         string `json:"goarch"`
	GOVERSION      string `json:"goversion"`
	CGOEnabled     string `json:"cgo_enabled"`
	GOEXPERIMENT   string `json:"goexperiment"`
	GOAMD64        string `json:"goamd64"`
	GOARM64        string `json:"goarm64"`
	Identity       string `json:"identity"`
	binaryIdentity codeExecRegularFileIdentity
}

type codeExecRegularFileIdentity struct {
	Info   os.FileInfo
	SHA256 string
}

var errCodeExecFileIdentityUnavailable = errors.New("opened file identity is unavailable")

var errCodeExecSandboxClose = errors.New("code execution sandbox close failed")

type codeExecPlatformIdentity struct {
	Volume           uint64
	FileIDHigh       uint64
	FileIDLow        uint64
	Links            uint64
	ChangeTimeSec    int64
	ChangeTimeNsec   int64
	ChangeTimeKnown  bool
	NoFollowVerified bool
}

type codeExecOpenedFileSnapshot struct {
	Info     os.FileInfo
	Platform codeExecPlatformIdentity
}

func snapshotCodeExecOpenedFile(file *os.File) (codeExecOpenedFileSnapshot, error) {
	if file == nil {
		return codeExecOpenedFileSnapshot{}, errCodeExecFileIdentityUnavailable
	}
	info, err := file.Stat()
	if err != nil {
		return codeExecOpenedFileSnapshot{}, err
	}
	identity, err := codeExecPlatformFileIdentity(file, info)
	if err != nil || identity.Links == 0 || !identity.ChangeTimeKnown || !identity.NoFollowVerified {
		return codeExecOpenedFileSnapshot{}, errCodeExecFileIdentityUnavailable
	}
	return codeExecOpenedFileSnapshot{Info: info, Platform: identity}, nil
}

func sameCodeExecOpenedFileSnapshot(before, current codeExecOpenedFileSnapshot) bool {
	return sameCodeExecFileSnapshot(before.Info, current.Info) && before.Platform == current.Platform
}

func sameCodeExecOpenedFileObject(before, current codeExecOpenedFileSnapshot) bool {
	return before.Info != nil && current.Info != nil && os.SameFile(before.Info, current.Info) &&
		before.Platform.Volume == current.Platform.Volume &&
		before.Platform.FileIDHigh == current.Platform.FileIDHigh &&
		before.Platform.FileIDLow == current.Platform.FileIDLow
}

func codeExecPathMatchesOpenedSnapshot(pathInfo os.FileInfo, opened codeExecOpenedFileSnapshot) bool {
	if !sameCodeExecFileSnapshot(pathInfo, opened.Info) {
		return false
	}
	pathIdentity, available := codeExecPlatformPathIdentity(pathInfo)
	return !available || pathIdentity == opened.Platform
}

type codeExecGoEnvironment struct {
	GOROOT       string `json:"GOROOT"`
	GOOS         string `json:"GOOS"`
	GOARCH       string `json:"GOARCH"`
	GOVERSION    string `json:"GOVERSION"`
	CGOEnabled   string `json:"CGO_ENABLED"`
	GOEXPERIMENT string `json:"GOEXPERIMENT"`
	GOAMD64      string `json:"GOAMD64"`
	GOARM64      string `json:"GOARM64"`
}

type codeExecGoBuildCacheManifest struct {
	Version           string                             `json:"version"`
	ToolchainIdentity string                             `json:"toolchain_identity"`
	Files             []codeExecGoBuildCacheManifestFile `json:"files"`
}

type codeExecGoBuildCacheManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   uint32 `json:"mode"`
	Owner  string `json:"owner"`
}

// Run 通过平台沙箱执行受信任的 Go 辅助命令；仅收紧调用方显式请求的 OS 硬配额，
// 宿主侧暂存预算由 codeExecApplicationBudget 独立执行。
func (h codeExecGoHelper) Run(
	ctx context.Context,
	requested sandbox.Config,
	workspace string,
	workingDir string,
	readablePaths []string,
	goBinary string,
	args []string,
	exports map[string]string,
) (result *sandbox.ExecResult, returnErr error) {
	return h.runWithHardTimeout(
		ctx,
		requested,
		workspace,
		workingDir,
		readablePaths,
		goBinary,
		args,
		exports,
		codeExecGoHelperHardTimeout,
	)
}

func (h codeExecGoHelper) runWithHardTimeout(
	ctx context.Context,
	requested sandbox.Config,
	workspace string,
	workingDir string,
	readablePaths []string,
	goBinary string,
	args []string,
	exports map[string]string,
	hardTimeout time.Duration,
) (result *sandbox.ExecResult, returnErr error) {
	if ctx == nil {
		return nil, errors.New("go helper context must not be nil")
	}
	if hardTimeout <= 0 {
		return nil, errors.New("go helper timeout must be positive")
	}
	if strings.TrimSpace(goBinary) == "" {
		return nil, errors.New("go helper binary is required")
	}
	canonicalWorkspace, err := resolveCodeExecBoundaryPath(workspace)
	if err != nil {
		return nil, errors.New("go helper workspace boundary is invalid")
	}
	workspace = canonicalWorkspace
	if strings.TrimSpace(workingDir) == "" {
		workingDir = workspace
	} else {
		workingDir, err = resolveCodeExecBoundaryPath(workingDir)
		if err != nil {
			return nil, errors.New("go helper working directory boundary is invalid")
		}
	}
	goBinary, err = codeExecCanonicalRuntimeExecutable(goBinary)
	if err != nil {
		return nil, errors.New("go helper binary binding is invalid")
	}

	cfg, err := codeExecGoHelperConfigWithLimit(
		requested,
		workspace,
		workingDir,
		readablePaths,
		goBinary,
		hardTimeout,
	)
	if err != nil {
		return nil, errors.New("go helper sandbox boundary is invalid")
	}
	factory := h.Factory
	if factory == nil {
		factory = sandbox.New
	}
	sb, err := factory(cfg)
	if err != nil {
		return nil, fmt.Errorf("create Go helper sandbox: %w", err)
	}
	if sb == nil {
		return nil, errors.New("create Go helper sandbox: factory returned nil sandbox")
	}
	defer func() {
		returnErr = joinCodeExecSandboxClose(ctx, returnErr, sb, "close Go helper sandbox")
	}()

	timeout := hardTimeout
	if requested.Timeout > 0 {
		requestedTimeout := time.Duration(requested.Timeout) * time.Second
		if requestedTimeout < timeout {
			timeout = requestedTimeout
		}
	}
	helperCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := append([]string{goBinary}, args...)
	return runStructuredSandboxCommand(helperCtx, sb, workingDir, command, exports)
}

func codeExecGoHelperConfig(
	requested sandbox.Config,
	workspace string,
	workingDir string,
	readablePaths []string,
	goBinary string,
) (sandbox.Config, error) {
	return codeExecGoHelperConfigWithLimit(
		requested,
		workspace,
		workingDir,
		readablePaths,
		goBinary,
		codeExecGoHelperHardTimeout,
	)
}

func codeExecGoHelperConfigWithLimit(
	requested sandbox.Config,
	workspace string,
	workingDir string,
	readablePaths []string,
	goBinary string,
	hardTimeout time.Duration,
) (sandbox.Config, error) {
	if hardTimeout <= 0 {
		return sandbox.Config{}, errors.New("go helper timeout must be positive")
	}
	cfg := ensureCodeExecConfigDefaults(requested)
	cfg.Workspace = workspace
	cfg.Network = sandbox.NetworkDisabled
	cfg.Timeout = int((hardTimeout + time.Second - 1) / time.Second)
	if requested.Timeout > 0 && requested.Timeout < cfg.Timeout {
		cfg.Timeout = requested.Timeout
	}
	cfg.MaxOutputBytes = minPositiveInt64(cfg.MaxOutputBytes, codeExecGoHelperMaxOutputBytes)
	cfg.MaxStderrBytes = minPositiveInt64(cfg.MaxStderrBytes, codeExecGoHelperMaxStderrBytes)
	cfg.MaxWorkspaceBytes = capPositiveInt64(cfg.MaxWorkspaceBytes, codeExecGoHelperMaxWorkspaceBytes)
	cfg.MaxArtifactBytes = capPositiveInt64(cfg.MaxArtifactBytes, codeExecGoHelperMaxWorkspaceBytes)
	cfg.MaxMemoryBytes = capPositiveInt64(cfg.MaxMemoryBytes, codeExecGoHelperMaxMemoryBytes)
	// 可信 Go 工具链必须能够派生 compiler/linker 子进程；Darwin 原生边界无法同时
	// 证明任意后代收容，因此构建阶段不请求进程数配额或进程收容能力。
	cfg.MaxProcesses = 0

	paths := append([]string(nil), readablePaths...)
	if strings.TrimSpace(workingDir) != "" {
		paths = append(paths, workingDir)
	}
	paths = append(paths, filepath.Dir(goBinary))
	canonicalReadable, err := canonicalCodeExecPaths(paths)
	if err != nil {
		return sandbox.Config{}, err
	}
	cfg.ReadablePaths = canonicalReadable
	denied := make([]string, 0, len(cfg.DeniedPaths))
	for _, path := range cfg.DeniedPaths {
		// 受信任辅助命令的专属工作区可能位于可信缓存下；不得让父级拒绝规则
		// 覆盖其自身工作区，否则平台沙箱会在启动后立即拒绝 Go 写入。
		if pathWithinResolved(path, workspace) {
			continue
		}
		denied = append(denied, path)
	}
	canonicalDenied, err := canonicalCodeExecPaths(denied)
	if err != nil {
		return sandbox.Config{}, err
	}
	cfg.DeniedPaths = canonicalDenied
	return withCodeExecTrustedBuildCapabilities(cfg), nil
}

// withCodeExecTrustedBuildCapabilities 只为固定工具链构建阶段派生能力合同。
// 不可信产物必须继续通过 withCodeExecRequiredCapabilities 进入严格运行边界。
func withCodeExecTrustedBuildCapabilities(cfg sandbox.Config) sandbox.Config {
	// TrustedBuild 需要派生编译器和链接器子进程，不能声明当前边界无法真实落实的进程数配额。
	cfg.ExecutionProfile = sandbox.ExecutionProfileTrustedBuild
	cfg.MaxProcesses = 0
	required := sandbox.TrustedBuildIsolationCapabilities
	if cfg.MaxMemoryBytes > 0 {
		required |= sandbox.CapabilityMemory
	}
	if cfg.MaxWorkspaceBytes > 0 || cfg.MaxArtifactBytes > 0 {
		required |= sandbox.CapabilityStorage
	}
	cfg.RequiredCapabilities = required
	return cfg
}

func minPositiveInt64(value, limit int64) int64 {
	if value <= 0 || value > limit {
		return limit
	}
	return value
}

func codeExecGoHelperApplicationWorkspaceBudget(cfg sandbox.Config) int64 {
	return minPositiveInt64(
		codeExecApplicationBudgetFor(cfg).MaxWorkspaceBytes,
		codeExecGoHelperMaxWorkspaceBytes,
	)
}

func capPositiveInt64(value, limit int64) int64 {
	if value <= 0 {
		return 0
	}
	if value > limit {
		return limit
	}
	return value
}

func inspectCodeExecGoToolchainDescriptor(
	ctx context.Context,
	cfg sandbox.Config,
	goBinary string,
	helper codeExecGoHelper,
) (codeExecGoToolchainDescriptor, error) {
	if ctx == nil {
		return codeExecGoToolchainDescriptor{}, errors.New("go toolchain context must not be nil")
	}
	descriptorCtx, cancel := context.WithTimeout(ctx, codeExecGoHelperHardTimeout)
	defer cancel()
	goBinary, inspectErr := codeExecCanonicalRuntimeExecutable(goBinary)
	if inspectErr != nil {
		return codeExecGoToolchainDescriptor{}, errors.New("inspect Go binary: invalid executable binding")
	}
	binaryIdentity, inspectErr := inspectCodeExecRegularFileNoFollow(goBinary, true)
	if inspectErr != nil {
		return codeExecGoToolchainDescriptor{}, fmt.Errorf("inspect Go binary: %w", inspectErr)
	}
	cfg.Workspace, inspectErr = resolveCodeExecBoundaryPath(cfg.Workspace)
	if inspectErr != nil {
		return codeExecGoToolchainDescriptor{}, errors.New("create Go toolchain workspace: invalid boundary")
	}
	workspace, inspectErr := os.MkdirTemp(cfg.Workspace, ".go-toolchain-*")
	if inspectErr != nil {
		return codeExecGoToolchainDescriptor{}, fmt.Errorf("create Go toolchain workspace: %w", inspectErr)
	}
	workspace, inspectErr = resolveCodeExecBoundaryPath(workspace)
	if inspectErr != nil {
		return codeExecGoToolchainDescriptor{}, errors.New("create Go toolchain workspace: invalid boundary")
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	home := filepath.Join(workspace, "home")
	tempDir := filepath.Join(workspace, "tmp")
	exports := map[string]string{
		"APPDATA":         filepath.Join(home, "AppData", "Roaming"),
		"GOCACHE":         filepath.Join(workspace, "go-build"),
		"GOENV":           "off",
		"GOFLAGS":         "",
		"GONOPROXY":       "",
		"GONOSUMDB":       "",
		"GOPROXY":         "off",
		"GOSUMDB":         "off",
		"GOTOOLCHAIN":     "local",
		"GOWORK":          "off",
		"HOME":            home,
		"LOCALAPPDATA":    filepath.Join(home, "AppData", "Local"),
		"TMP":             tempDir,
		"TEMP":            tempDir,
		"TMPDIR":          tempDir,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
	}
	for _, path := range []string{home, tempDir, exports["GOCACHE"]} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return codeExecGoToolchainDescriptor{}, fmt.Errorf("create Go toolchain directory: %w", err)
		}
	}
	if err := ensureCodeExecGoTelemetryOff(exports); err != nil {
		return codeExecGoToolchainDescriptor{}, fmt.Errorf("prepare Go toolchain identity: %w", err)
	}
	initialReadable := []string{filepath.Dir(filepath.Dir(goBinary))}
	envResult, inspectErr := helper.Run(
		descriptorCtx,
		cfg,
		workspace,
		workspace,
		initialReadable,
		goBinary,
		[]string{"env", "-json", "GOOS", "GOARCH", "GOVERSION", "GOROOT", "CGO_ENABLED", "GOEXPERIMENT", "GOAMD64", "GOARM64"},
		exports,
	)
	if inspectErr != nil && !codeExecGoHelperResultSucceeded(envResult, inspectErr) {
		return codeExecGoToolchainDescriptor{}, codeExecGoHelperFailure("inspect Go environment", envResult, inspectErr)
	}
	if !codeExecGoHelperResultSucceeded(envResult, inspectErr) {
		return codeExecGoToolchainDescriptor{}, codeExecGoHelperResultError("inspect Go environment", envResult)
	}
	var environment codeExecGoEnvironment
	if err := json.Unmarshal([]byte(envResult.Stdout), &environment); err != nil {
		return codeExecGoToolchainDescriptor{}, fmt.Errorf("decode Go environment: %w", err)
	}
	if strings.TrimSpace(environment.GOROOT) == "" || strings.TrimSpace(environment.GOOS) == "" ||
		strings.TrimSpace(environment.GOARCH) == "" || strings.TrimSpace(environment.GOVERSION) == "" {
		return codeExecGoToolchainDescriptor{}, errors.New("go environment descriptor is incomplete")
	}

	compileResult, inspectErr := helper.Run(
		descriptorCtx,
		cfg,
		workspace,
		workspace,
		[]string{environment.GOROOT},
		goBinary,
		[]string{"tool", "compile", "-V=full"},
		exports,
	)
	if inspectErr != nil && !codeExecGoHelperResultSucceeded(compileResult, inspectErr) {
		return codeExecGoToolchainDescriptor{}, codeExecGoHelperFailure("inspect Go compiler identity", compileResult, inspectErr)
	}
	if !codeExecGoHelperResultSucceeded(compileResult, inspectErr) {
		return codeExecGoToolchainDescriptor{}, codeExecGoHelperResultError("inspect Go compiler identity", compileResult)
	}
	compileVersion := strings.TrimSpace(compileResult.Stdout)
	if compileVersion == "" {
		return codeExecGoToolchainDescriptor{}, errors.New("go compiler identity is empty")
	}

	goRoot, inspectErr := canonicalCodeExecPath(environment.GOROOT)
	if inspectErr != nil {
		return codeExecGoToolchainDescriptor{}, errors.New("go environment descriptor has invalid GOROOT")
	}
	descriptor := codeExecGoToolchainDescriptor{
		Binary:         goBinary,
		BinarySHA256:   binaryIdentity.SHA256,
		CompileVersion: compileVersion,
		GOROOT:         goRoot,
		GOOS:           environment.GOOS,
		GOARCH:         environment.GOARCH,
		GOVERSION:      environment.GOVERSION,
		CGOEnabled:     environment.CGOEnabled,
		GOEXPERIMENT:   environment.GOEXPERIMENT,
		GOAMD64:        environment.GOAMD64,
		GOARM64:        environment.GOARM64,
	}
	identityPayload, inspectErr := json.Marshal(descriptor)
	if inspectErr != nil {
		return codeExecGoToolchainDescriptor{}, fmt.Errorf("encode Go toolchain identity: %w", inspectErr)
	}
	identity := sha256.Sum256(append([]byte(codeExecGoBuildCacheSeedVersion+"\x00"), identityPayload...))
	descriptor.Identity = hex.EncodeToString(identity[:])
	descriptor.binaryIdentity = binaryIdentity
	if err := verifyCodeExecGoToolchainDescriptor(descriptor); err != nil {
		return codeExecGoToolchainDescriptor{}, err
	}
	return descriptor, nil
}

func codeExecGoHelperResultError(operation string, result *sandbox.ExecResult) error {
	if result == nil {
		return fmt.Errorf("%s: Go helper returned no result", operation)
	}
	return fmt.Errorf("%s: exit status %d", operation, result.ExitCode)
}

func codeExecGoHelperFailure(operation string, result *sandbox.ExecResult, cause error) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, cause)
	}
	if result != nil && result.ExitCode != 0 {
		return codeExecGoHelperResultError(operation, result)
	}
	if errors.Is(cause, sandbox.ErrStorageLimitExceeded) {
		return fmt.Errorf("%s: storage limit exceeded", operation)
	}
	if result == nil && cause == nil {
		return codeExecGoHelperResultError(operation, nil)
	}
	return fmt.Errorf("%s: sandbox execution failed", operation)
}

// codeExecGoHelperResultSucceeded 以已完成的进程结果为主，仅忽略不会改变退出状态或
// 生命周期完整性的封装层附加错误；取消、超时、资源违规和 Close 失败不能被 ExitCode=0 掩盖。
func codeExecGoHelperResultSucceeded(result *sandbox.ExecResult, cause error) bool {
	if result == nil || result.ExitCode != 0 {
		return false
	}
	return !errors.Is(cause, context.Canceled) &&
		!errors.Is(cause, context.DeadlineExceeded) &&
		!errors.Is(cause, sandbox.ErrStorageLimitExceeded) &&
		!errors.Is(cause, errCodeExecSandboxClose)
}

func hashCodeExecRegularFileNoFollow(path string) (string, error) {
	identity, err := inspectCodeExecRegularFileNoFollow(path, false)
	if err != nil {
		return "", err
	}
	return identity.SHA256, nil
}

func inspectCodeExecRegularFileNoFollow(path string, requireExecutable bool) (_ codeExecRegularFileIdentity, returnErr error) {
	path = filepath.Clean(path)
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return codeExecRegularFileIdentity{}, err
	}
	defer joinCodeExecResourceClose(&returnErr, root, "close file inspection root")
	name := filepath.Base(path)
	before, err := root.Lstat(name)
	if err != nil {
		return codeExecRegularFileIdentity{}, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return codeExecRegularFileIdentity{}, errors.New("file is not regular")
	}
	if requireExecutable && runtime.GOOS != "windows" && before.Mode().Perm()&0111 == 0 {
		return codeExecRegularFileIdentity{}, errors.New("file is not executable")
	}
	file, err := openCodeExecRegularFileNoFollow(root, name)
	if err != nil {
		return codeExecRegularFileIdentity{}, err
	}
	defer joinCodeExecResourceClose(&returnErr, file, "close inspected file")
	opened, err := snapshotCodeExecOpenedFile(file)
	if err != nil || !opened.Info.Mode().IsRegular() || !codeExecPathMatchesOpenedSnapshot(before, opened) ||
		opened.Platform.Links != 1 {
		return codeExecRegularFileIdentity{}, errors.New("file changed while opening")
	}
	hash := sha256.New()
	if _, copyErr := io.Copy(hash, file); copyErr != nil {
		return codeExecRegularFileIdentity{}, copyErr
	}
	after, err := snapshotCodeExecOpenedFile(file)
	postPath, pathErr := root.Lstat(name)
	if err != nil || pathErr != nil || !sameCodeExecOpenedFileSnapshot(opened, after) ||
		!codeExecPathMatchesOpenedSnapshot(postPath, after) || postPath.Mode()&os.ModeSymlink != 0 ||
		after.Platform.Links != 1 {
		return codeExecRegularFileIdentity{}, errors.New("file changed while hashing")
	}
	return codeExecRegularFileIdentity{Info: postPath, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

// joinCodeExecResourceClose 将资源关闭失败合并进当前返回错误，避免清理失败被成功路径掩盖。
func joinCodeExecResourceClose(returnErr *error, resource io.Closer, operation string) {
	if returnErr == nil || resource == nil {
		return
	}
	if closeErr := resource.Close(); closeErr != nil {
		*returnErr = errors.Join(*returnErr, fmt.Errorf("%s: %w", operation, closeErr))
	}
}

func verifyCodeExecGoToolchainDescriptor(descriptor codeExecGoToolchainDescriptor) error {
	if !filepath.IsAbs(descriptor.Binary) || strings.TrimSpace(descriptor.GOROOT) == "" ||
		!codeExecCanonicalSHA256(descriptor.BinarySHA256) || !codeExecCanonicalSHA256(descriptor.Identity) {
		return errors.New("go toolchain descriptor is incomplete")
	}
	canonicalBinary, err := codeExecCanonicalRuntimeExecutable(descriptor.Binary)
	if err != nil || canonicalBinary != descriptor.Binary {
		return errors.New("go toolchain binary binding changed")
	}
	canonicalGOROOT, err := canonicalCodeExecPath(descriptor.GOROOT)
	if err != nil || canonicalGOROOT != descriptor.GOROOT {
		return errors.New("go toolchain descriptor is incomplete")
	}
	current, err := inspectCodeExecRegularFileNoFollow(descriptor.Binary, true)
	if err != nil {
		return errors.New("go toolchain binary binding changed")
	}
	if current.SHA256 != descriptor.BinarySHA256 || descriptor.binaryIdentity.Info == nil ||
		!os.SameFile(descriptor.binaryIdentity.Info, current.Info) {
		return errors.New("go toolchain binary binding changed")
	}
	return nil
}

func codeExecCanonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

const (
	codeExecGoSeedLeaseHeartbeat = 5 * time.Second
	codeExecGoSeedLeaseStale     = 2 * time.Minute
	codeExecGoSeedLeasePoll      = 20 * time.Millisecond
)

type codeExecGoSeedLeaseOwner struct {
	PID       int    `json:"pid"`
	Nonce     string `json:"nonce"`
	StartedAt string `json:"started_at"`
}

// acquireCodeExecGoBuildCacheSeedLock 使用带心跳的文件租约串行化跨进程种子首建。
func acquireCodeExecGoBuildCacheSeedLock(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("wait for trusted Go build cache seed: %w", err)
	}
	lockPath := filepath.Clean(key) + ".lock"
	for {
		if mkdirErr := os.Mkdir(lockPath, 0700); mkdirErr == nil {
			nonce := newCodeExecRunID()
			owner := codeExecGoSeedLeaseOwner{
				PID:       os.Getpid(),
				Nonce:     nonce,
				StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
			}
			ownerData, marshalErr := json.Marshal(owner)
			if marshalErr != nil {
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("create trusted Go build cache lease: %w", marshalErr)
			}
			if writeErr := writeCodeExecSecureFile(lockPath, "owner.json", ownerData, 0600); writeErr != nil {
				_ = os.RemoveAll(lockPath)
				return nil, fmt.Errorf("create trusted Go build cache lease: %w", writeErr)
			}
			if syncErr := syncCodeExecDirectory(filepath.Dir(lockPath)); syncErr != nil {
				_ = os.RemoveAll(lockPath)
				return nil, fmt.Errorf("sync trusted Go build cache lease: %w", syncErr)
			}
			_ = os.Chtimes(lockPath, time.Now(), time.Now())
			stop := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				ticker := time.NewTicker(codeExecGoSeedLeaseHeartbeat)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						now := time.Now()
						if heartbeatErr := os.Chtimes(lockPath, now, now); heartbeatErr != nil {
							return
						}
					case <-stop:
						return
					}
				}
			}()
			var once sync.Once
			return func() {
				once.Do(func() {
					close(stop)
					<-done
					data, readErr := readCodeExecSecureFile(lockPath, "owner.json", 4096)
					var current codeExecGoSeedLeaseOwner
					if readErr != nil || json.Unmarshal(data, &current) != nil || current.Nonce != nonce {
						return
					}
					released := lockPath + ".released-" + nonce
					if releaseErr := os.Rename(lockPath, released); releaseErr == nil {
						_ = syncCodeExecDirectory(filepath.Dir(lockPath))
						_ = os.RemoveAll(released)
						_ = syncCodeExecDirectory(filepath.Dir(lockPath))
					}
				})
			}, nil
		} else if !os.IsExist(mkdirErr) {
			return nil, fmt.Errorf("create trusted Go build cache lease: %w", mkdirErr)
		}

		info, err := os.Lstat(lockPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("inspect trusted Go build cache lease: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("trusted Go build cache lease is non-regular")
		}
		if time.Since(info.ModTime()) > codeExecGoSeedLeaseStale {
			stale := lockPath + ".stale-" + newCodeExecRunID()
			if err := os.Rename(lockPath, stale); err == nil {
				_ = syncCodeExecDirectory(filepath.Dir(lockPath))
				_ = os.RemoveAll(stale)
				_ = syncCodeExecDirectory(filepath.Dir(lockPath))
				continue
			} else if os.IsNotExist(err) {
				continue
			}
		}

		timer := time.NewTimer(codeExecGoSeedLeasePoll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, fmt.Errorf("wait for trusted Go build cache seed: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func codeExecEnvironmentAssignment(arg string) bool {
	name, _, ok := strings.Cut(arg, "=")
	if !ok || name == "" {
		return false
	}
	for index, r := range name {
		valid := r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || index > 0 && r >= '0' && r <= '9'
		if !valid {
			return false
		}
	}
	return true
}

func prepareCodeExecGoBuildCache(
	ctx context.Context,
	run codeExecRun,
	cacheBase string,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("prepare trusted Go build cache: %w", err)
	}
	seedCtx, cancel := codeExecGoBuildCacheContext(ctx, run.Config.Timeout)
	defer cancel()
	seed, available, err := ensureCodeExecGoBuildCacheSeed(seedCtx, run, cacheBase)
	if err != nil {
		return false, err
	}
	if !available {
		// Go 不存在时保留原有执行链，由沙箱结果统一报告 runtime_missing。
		return false, nil
	}
	measureWorkspace := codeExecDirSizeContext
	if run.GoVendored {
		measureWorkspace = codeExecGoVendorDirSizeContext
	}
	used, err := measureWorkspace(seedCtx, run.Scratch)
	if err != nil {
		return false, fmt.Errorf("measure sandbox workspace before Go cache seed: %w", err)
	}
	applicationBudget := run.applicationBudget()
	if used >= applicationBudget.MaxWorkspaceBytes {
		return false, errors.New("trusted Go build cache seed exceeds application workspace budget")
	}
	remaining := applicationBudget.MaxWorkspaceBytes - used
	destination := filepath.Join(run.CacheDir, "go-build")
	if err := copyCodeExecGoBuildCacheSeed(seedCtx, seed, destination, remaining); err != nil {
		return false, fmt.Errorf("prepare trusted Go build cache: %w", err)
	}
	return true, nil
}

func codeExecGoBuildCacheContext(
	parent context.Context,
	timeoutSeconds int,
) (context.Context, context.CancelFunc) {
	if timeoutSeconds <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(timeoutSeconds)*time.Second)
}

func ensureCodeExecGoBuildCacheSeed(
	ctx context.Context,
	run codeExecRun,
	cacheBase string,
) (string, bool, error) {
	trustedCacheBase, cacheBaseErr := ensureCodeExecTrustedDirectory(cacheBase)
	if cacheBaseErr != nil {
		return "", false, fmt.Errorf("create trusted Go build cache base: %w", cacheBaseErr)
	}
	cacheBase = trustedCacheBase
	if pathWithinResolved(run.Workspace, cacheBase) {
		return "", false, errors.New("trusted Go build cache base must be outside the run workspace")
	}

	toolchain := run.Plan.Toolchain
	if toolchain == nil {
		return "", false, nil
	}
	if err := verifyCodeExecGoToolchainDescriptor(*toolchain); err != nil {
		return "", false, err
	}
	goBinary := toolchain.Binary
	identity := toolchain.Identity
	seedRoot := filepath.Join(cacheBase, identity)
	release, leaseErr := acquireCodeExecGoBuildCacheSeedLock(ctx, seedRoot)
	if leaseErr != nil {
		return "", false, leaseErr
	}
	defer release()

	if seed, valid, validationErr := codeExecValidGoBuildCacheSeed(seedRoot, identity); valid && validationErr == nil {
		return seed, true, nil
	}
	if err := quarantineCodeExecGoBuildCacheGeneration(cacheBase, seedRoot, identity); err != nil {
		return "", false, err
	}
	if err := cleanupCodeExecStaleGoBuildCacheGenerations(cacheBase, identity); err != nil {
		return "", false, err
	}

	buildRoot, buildErr := os.MkdirTemp(cacheBase, ".building-"+identity[:12]+"-*")
	if buildErr != nil {
		return "", false, fmt.Errorf("create trusted Go build cache staging directory: %w", buildErr)
	}
	cleanupBuildRoot := true
	defer func() {
		if cleanupBuildRoot {
			_ = os.RemoveAll(buildRoot)
		}
	}()
	seedCache := filepath.Join(buildRoot, "go-build")
	home := filepath.Join(buildRoot, "home")
	tempDir := filepath.Join(buildRoot, "tmp")
	for _, dir := range []string{seedCache, home, tempDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", false, fmt.Errorf("create trusted Go build cache directory: %w", err)
		}
	}
	exports := map[string]string{
		"APPDATA":         filepath.Join(home, "AppData", "Roaming"),
		"GOCACHE":         seedCache,
		"GOENV":           "off",
		"GOFLAGS":         "",
		"GONOPROXY":       "",
		"GONOSUMDB":       "",
		"GOPROXY":         "off",
		"GOSUMDB":         "off",
		"GOTOOLCHAIN":     "local",
		"GOWORK":          "off",
		"HOME":            home,
		"LOCALAPPDATA":    filepath.Join(home, "AppData", "Local"),
		"TMP":             tempDir,
		"TEMP":            tempDir,
		"TMPDIR":          tempDir,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
	}
	if err := ensureCodeExecGoTelemetryOff(exports); err != nil {
		return "", false, fmt.Errorf("prepare trusted Go build cache: %w", err)
	}
	result, buildErr := run.Plan.Helper.runWithHardTimeout(
		ctx,
		run.Config,
		buildRoot,
		buildRoot,
		[]string{toolchain.GOROOT},
		goBinary,
		[]string{"list", "-deps", "-export", "testing"},
		exports,
		codeExecGoStageHardTimeout,
	)
	if buildErr != nil && !codeExecGoHelperResultSucceeded(result, buildErr) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", false, fmt.Errorf("prepare trusted Go build cache: %w", ctxErr)
		}
		return "", false, codeExecGoHelperFailure("prepare trusted Go build cache", result, buildErr)
	}
	if !codeExecGoHelperResultSucceeded(result, buildErr) {
		return "", false, codeExecGoHelperFailure("prepare trusted Go build cache", result, nil)
	}
	if err := writeCodeExecSecureFile(buildRoot, ".complete", []byte(identity), 0600); err != nil {
		return "", false, fmt.Errorf("write trusted Go build cache marker: %w", err)
	}
	if err := writeCodeExecGoBuildCacheManifest(buildRoot, identity); err != nil {
		return "", false, err
	}
	if err := syncCodeExecGoBuildCacheTree(buildRoot); err != nil {
		return "", false, fmt.Errorf("sync trusted Go build cache seed: %w", err)
	}
	if err := syncCodeExecDirectory(cacheBase); err != nil {
		return "", false, fmt.Errorf("sync trusted Go build cache parent: %w", err)
	}
	if err := os.Rename(buildRoot, seedRoot); err != nil {
		if seed, valid, validationErr := codeExecValidGoBuildCacheSeed(seedRoot, identity); validationErr != nil {
			return "", false, validationErr
		} else if valid {
			return seed, true, nil
		}
		return "", false, fmt.Errorf("publish trusted Go build cache seed: %w", err)
	}
	cleanupBuildRoot = false
	if err := syncCodeExecDirectory(cacheBase); err != nil {
		return "", false, fmt.Errorf("sync published Go build cache parent: %w", err)
	}
	seed, valid, validationErr := codeExecValidGoBuildCacheSeed(seedRoot, identity)
	if validationErr != nil || !valid {
		if validationErr == nil {
			validationErr = errors.New("published trusted Go build cache seed is incomplete")
		}
		return "", false, validationErr
	}
	return seed, true, nil
}

func quarantineCodeExecGoBuildCacheGeneration(cacheBase, seedRoot, identity string) error {
	cacheBase = filepath.Clean(cacheBase)
	seedRoot = filepath.Clean(seedRoot)
	if filepath.Dir(seedRoot) != cacheBase || filepath.Base(seedRoot) != identity {
		return errors.New("quarantine trusted Go build cache seed: invalid generation boundary")
	}
	if _, err := os.Lstat(seedRoot); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect trusted Go build cache seed: %w", err)
	}
	quarantine := filepath.Join(cacheBase, ".quarantine-"+identity[:12]+"-"+newCodeExecRunID())
	if err := os.Rename(seedRoot, quarantine); err != nil {
		return fmt.Errorf("quarantine trusted Go build cache seed: %w", err)
	}
	if err := syncCodeExecDirectory(cacheBase); err != nil {
		return fmt.Errorf("sync quarantined Go build cache parent: %w", err)
	}
	if err := os.RemoveAll(quarantine); err != nil {
		return fmt.Errorf("remove quarantined Go build cache seed: %w", err)
	}
	return syncCodeExecDirectory(cacheBase)
}

func cleanupCodeExecStaleGoBuildCacheGenerations(cacheBase, identity string) error {
	entries, err := os.ReadDir(cacheBase)
	if err != nil {
		return fmt.Errorf("inspect trusted Go build cache generations: %w", err)
	}
	prefixes := []string{
		".building-" + identity[:12] + "-",
		".quarantine-" + identity[:12] + "-",
		".removing-" + identity[:12] + "-",
	}
	for _, entry := range entries {
		matched := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry.Name(), prefix) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		path := filepath.Join(cacheBase, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("inspect stale Go build cache generation: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove stale Go build cache link: %w", err)
			}
			continue
		}
		if !info.IsDir() {
			return errors.New("stale Go build cache generation is non-regular")
		}
		removing := filepath.Join(cacheBase, ".removing-"+identity[:12]+"-"+newCodeExecRunID())
		if err := os.Rename(path, removing); err != nil {
			return fmt.Errorf("quarantine stale Go build cache generation: %w", err)
		}
		if err := os.RemoveAll(removing); err != nil {
			return fmt.Errorf("remove stale Go build cache generation: %w", err)
		}
	}
	return syncCodeExecDirectory(cacheBase)
}

func resolveCodeExecGoToolchainDescriptor(
	ctx context.Context,
	cfg sandbox.Config,
	helper codeExecGoHelper,
) (*codeExecGoToolchainDescriptor, bool, error) {
	goBinary, err := codeExecFindRuntimeExecutable("go")
	if err != nil {
		return nil, false, nil
	}
	descriptor, err := inspectCodeExecGoToolchainDescriptor(ctx, cfg, goBinary, helper)
	if err != nil {
		return nil, false, err
	}
	return &descriptor, true, nil
}

func codeExecFindRuntimeExecutable(name string) (string, error) {
	fileNames := []string{name}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		fileNames = []string{name + ".exe", name}
	}
	for _, directory := range filepath.SplitList(codeExecRuntimePath()) {
		if strings.TrimSpace(directory) == "" {
			continue
		}
		for _, fileName := range fileNames {
			candidate := filepath.Join(directory, fileName)
			canonical, err := codeExecCanonicalRuntimeExecutable(candidate)
			if err == nil {
				return canonical, nil
			}
		}
	}
	return "", fmt.Errorf("runtime executable %s was not found", name)
}

func codeExecCanonicalRuntimeExecutable(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	canonical = filepath.Clean(canonical)
	if _, err := inspectCodeExecRegularFileNoFollow(canonical, true); err != nil {
		return "", err
	}
	return canonical, nil
}

func writeCodeExecSecureFile(rootPath, name string, data []byte, mode os.FileMode) (returnErr error) {
	root, _, err := openCodeExecRootNoFollow(rootPath)
	if err != nil {
		return err
	}
	defer joinCodeExecResourceClose(&returnErr, root, "close secure file root")
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	opened, statErr := snapshotCodeExecOpenedFile(file)
	if statErr != nil || !opened.Info.Mode().IsRegular() || opened.Platform.Links != 1 {
		_ = file.Close()
		return errors.New("secure file changed while opening")
	}
	written, err := file.Write(data)
	if err != nil {
		_ = file.Close()
		return err
	}
	if written != len(data) {
		_ = file.Close()
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	after, statErr := snapshotCodeExecOpenedFile(file)
	postPath, pathErr := root.Lstat(name)
	closeErr := file.Close()
	if statErr != nil || pathErr != nil || !sameCodeExecOpenedFileObject(opened, after) ||
		!codeExecPathMatchesOpenedSnapshot(postPath, after) || postPath.Mode()&os.ModeSymlink != 0 ||
		!postPath.Mode().IsRegular() || after.Platform.Links != 1 || after.Info.Size() != int64(len(data)) {
		return errors.New("secure file changed while writing")
	}
	return closeErr
}

func readCodeExecSecureFile(rootPath, name string, maxBytes int64) (_ []byte, returnErr error) {
	root, _, err := openCodeExecRootNoFollow(rootPath)
	if err != nil {
		return nil, err
	}
	defer joinCodeExecResourceClose(&returnErr, root, "close secure file root")
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, errors.New("secure file is non-regular")
	}
	if before.Size() < 0 || maxBytes < 0 || before.Size() > maxBytes {
		return nil, errors.New("secure file exceeds size limit")
	}
	file, err := openCodeExecRegularFileNoFollow(root, name)
	if err != nil {
		return nil, err
	}
	opened, statErr := snapshotCodeExecOpenedFile(file)
	if statErr != nil || !opened.Info.Mode().IsRegular() || !codeExecPathMatchesOpenedSnapshot(before, opened) ||
		opened.Platform.Links != 1 {
		_ = file.Close()
		return nil, errors.New("secure file changed while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	after, afterErr := snapshotCodeExecOpenedFile(file)
	postPath, pathErr := root.Lstat(name)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if len(data) > int(maxBytes) {
		return nil, errors.New("secure file exceeds size limit")
	}
	if afterErr != nil || pathErr != nil || !sameCodeExecOpenedFileSnapshot(opened, after) ||
		!codeExecPathMatchesOpenedSnapshot(postPath, after) || postPath.Mode()&os.ModeSymlink != 0 ||
		!postPath.Mode().IsRegular() || after.Platform.Links != 1 {
		return nil, errors.New("secure file changed while reading")
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return data, nil
}

func syncCodeExecGoBuildCacheTree(rootPath string) (returnErr error) {
	root, _, err := openCodeExecRootNoFollow(rootPath)
	if err != nil {
		return err
	}
	defer joinCodeExecResourceClose(&returnErr, root, "close trusted Go build cache root")
	if err := syncCodeExecRootTree(root); err != nil {
		return err
	}
	return syncCodeExecRootDirectory(root)
}

func syncCodeExecRootTree(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		before, entryErr := root.Lstat(name)
		if entryErr != nil {
			return entryErr
		}
		if before.Mode()&os.ModeSymlink != 0 {
			return errors.New("trusted Go build cache seed contains a non-regular entry")
		}
		if before.IsDir() {
			child, childErr := root.OpenRoot(name)
			if childErr != nil {
				return childErr
			}
			opened, statErr := child.Stat(".")
			if statErr != nil || !os.SameFile(before, opened) {
				_ = child.Close()
				return errors.New("trusted Go build cache directory changed while syncing")
			}
			if err := syncCodeExecRootTree(child); err != nil {
				_ = child.Close()
				return err
			}
			if err := syncCodeExecRootDirectory(child); err != nil {
				_ = child.Close()
				return err
			}
			if err := child.Close(); err != nil {
				return err
			}
			postPath, err := root.Lstat(name)
			if err != nil || !os.SameFile(opened, postPath) || postPath.Mode()&os.ModeSymlink != 0 {
				return errors.New("trusted Go build cache directory changed while syncing")
			}
			continue
		}
		if !before.Mode().IsRegular() || codeExecFileLinkCount(before) != 1 {
			return errors.New("trusted Go build cache seed contains a non-regular entry")
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		opened, statErr := file.Stat()
		if statErr != nil || !os.SameFile(before, opened) || codeExecFileLinkCount(opened) != 1 {
			_ = file.Close()
			return errors.New("trusted Go build cache file changed while syncing")
		}
		syncErr := file.Sync()
		after, afterErr := file.Stat()
		postPath, pathErr := root.Lstat(name)
		closeErr := file.Close()
		if syncErr != nil {
			return syncErr
		}
		if afterErr != nil || pathErr != nil || !os.SameFile(opened, after) || !os.SameFile(after, postPath) ||
			postPath.Mode()&os.ModeSymlink != 0 || codeExecFileLinkCount(after) != 1 ||
			codeExecFileLinkCount(postPath) != 1 {
			return errors.New("trusted Go build cache file changed while syncing")
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func syncCodeExecRootDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil && runtime.GOOS != "windows" {
		return err
	}
	return closeErr
}

func syncCodeExecDirectory(path string) (returnErr error) {
	root, _, err := openCodeExecRootNoFollow(path)
	if err != nil {
		return err
	}
	defer joinCodeExecResourceClose(&returnErr, root, "close synchronized directory")
	return syncCodeExecRootDirectory(root)
}

func writeCodeExecGoBuildCacheManifest(seedRoot, identity string) error {
	files, err := collectCodeExecGoBuildCacheManifestFiles(seedRoot)
	if err != nil {
		return err
	}
	manifest := codeExecGoBuildCacheManifest{
		Version:           codeExecGoBuildCacheSeedVersion,
		ToolchainIdentity: identity,
		Files:             files,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("encode trusted Go build cache manifest: %w", err)
	}
	if err := writeCodeExecSecureFile(seedRoot, codeExecGoBuildCacheManifestName, data, 0600); err != nil {
		return fmt.Errorf("write trusted Go build cache manifest: %w", err)
	}
	return nil
}

func readCodeExecGoBuildCacheManifest(seedRoot string) (codeExecGoBuildCacheManifest, error) {
	const maxManifestBytes = 16 * 1024 * 1024
	data, err := readCodeExecSecureFile(seedRoot, codeExecGoBuildCacheManifestName, maxManifestBytes)
	if err != nil {
		return codeExecGoBuildCacheManifest{}, err
	}
	var manifest codeExecGoBuildCacheManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return codeExecGoBuildCacheManifest{}, fmt.Errorf("decode trusted Go build cache manifest: %w", err)
	}
	return manifest, nil
}

func collectCodeExecGoBuildCacheManifestFiles(seedRoot string) (_ []codeExecGoBuildCacheManifestFile, returnErr error) {
	seed, _, err := openCodeExecRootNoFollow(seedRoot)
	if err != nil {
		return nil, fmt.Errorf("open trusted Go build cache seed: %w", err)
	}
	defer joinCodeExecResourceClose(&returnErr, seed, "close trusted Go build cache seed")
	cacheInfo, err := seed.Lstat("go-build")
	if err != nil {
		return nil, fmt.Errorf("inspect trusted Go build cache directory: %w", err)
	}
	if cacheInfo.Mode()&os.ModeSymlink != 0 || !cacheInfo.IsDir() {
		return nil, errors.New("trusted Go build cache directory is non-regular")
	}
	cache, err := seed.OpenRoot("go-build")
	if err != nil {
		return nil, fmt.Errorf("open trusted Go build cache directory: %w", err)
	}
	defer joinCodeExecResourceClose(&returnErr, cache, "close trusted Go build cache directory")
	opened, err := cache.Stat(".")
	if err != nil || !os.SameFile(cacheInfo, opened) {
		return nil, errors.New("trusted Go build cache directory changed while opening")
	}
	var files []codeExecGoBuildCacheManifestFile
	if err := walkCodeExecGoBuildCacheRoot(cache, "", &files); err != nil {
		return nil, err
	}
	return files, nil
}

func walkCodeExecGoBuildCacheRoot(
	root *os.Root,
	prefix string,
	files *[]codeExecGoBuildCacheManifestFile,
) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("read trusted Go build cache directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		info, entryErr := root.Lstat(name)
		if entryErr != nil {
			return fmt.Errorf("inspect trusted Go build cache entry: %w", entryErr)
		}
		relative := filepath.Join(prefix, name)
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("trusted Go build cache seed contains a non-regular entry")
		}
		if info.IsDir() {
			directory, openErr := root.OpenRoot(name)
			if openErr != nil {
				return fmt.Errorf("open trusted Go build cache directory: %w", openErr)
			}
			opened, statErr := directory.Stat(".")
			if statErr != nil || !os.SameFile(info, opened) {
				_ = directory.Close()
				return errors.New("trusted Go build cache directory changed while opening")
			}
			if err := walkCodeExecGoBuildCacheRoot(directory, relative, files); err != nil {
				_ = directory.Close()
				return err
			}
			if err := directory.Close(); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() || codeExecFileLinkCount(info) != 1 {
			return errors.New("trusted Go build cache seed contains a non-regular entry")
		}
		file, err := root.Open(name)
		if err != nil {
			return fmt.Errorf("open trusted Go build cache file: %w", err)
		}
		opened, statErr := file.Stat()
		if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) ||
			codeExecFileLinkCount(opened) != 1 {
			_ = file.Close()
			return errors.New("trusted Go build cache file changed while opening")
		}
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			_ = file.Close()
			return fmt.Errorf("hash trusted Go build cache file: %w", err)
		}
		after, statErr := file.Stat()
		closeErr := file.Close()
		postPathInfo, pathErr := root.Lstat(name)
		if statErr != nil || pathErr != nil || !os.SameFile(opened, after) || !os.SameFile(after, postPathInfo) ||
			postPathInfo.Mode()&os.ModeSymlink != 0 || opened.Size() != after.Size() || opened.Mode() != after.Mode() ||
			!opened.ModTime().Equal(after.ModTime()) || codeExecFileLinkCount(after) != 1 ||
			codeExecFileLinkCount(postPathInfo) != 1 {
			return errors.New("trusted Go build cache file changed while hashing")
		}
		if closeErr != nil {
			return closeErr
		}
		*files = append(*files, codeExecGoBuildCacheManifestFile{
			Path:   filepath.ToSlash(relative),
			SHA256: hex.EncodeToString(hash.Sum(nil)),
			Size:   opened.Size(),
			Mode:   uint32(opened.Mode()),
			Owner:  codeExecFileOwnerIdentity(opened),
		})
	}
	return nil
}

func openCodeExecRootNoFollow(path string) (*os.Root, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, nil, errors.New("path is not a regular directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, nil, errors.New("directory changed while opening")
	}
	return root, opened, nil
}

func codeExecFileOwnerIdentity(info os.FileInfo) string {
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		uid, uidOK := codeExecReflectUnsigned(value.FieldByName("Uid"))
		gid, gidOK := codeExecReflectUnsigned(value.FieldByName("Gid"))
		if uidOK && gidOK {
			return fmt.Sprintf("uid:%d:gid:%d", uid, gid)
		}
	}
	if current, err := user.Current(); err == nil && strings.TrimSpace(current.Uid) != "" {
		return "user:" + current.Uid
	}
	return "platform:" + runtime.GOOS
}

func codeExecFileLinkCount(info os.FileInfo) uint64 {
	identity, available := codeExecPlatformPathIdentity(info)
	if !available {
		return 0
	}
	return identity.Links
}

func codeExecReflectUnsigned(value reflect.Value) (uint64, bool) {
	if !value.IsValid() {
		return 0, false
	}
	switch value.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		integer := value.Int()
		if integer >= 0 {
			return uint64(integer), true
		}
	}
	return 0, false
}

func codeExecValidGoBuildCacheSeed(
	seedRoot string,
	identity string,
) (string, bool, error) {
	rootInfo, err := os.Lstat(seedRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect trusted Go build cache seed: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("trusted Go build cache seed is non-regular")
	}
	marker, err := readCodeExecSecureFile(seedRoot, ".complete", 4096)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read trusted Go build cache marker: %w", err)
	}
	if string(marker) != identity {
		return "", false, errors.New("trusted Go build cache marker does not match the Go runtime")
	}
	manifest, err := readCodeExecGoBuildCacheManifest(seedRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("verify trusted Go build cache manifest: %w", err)
	}
	if manifest.Version != codeExecGoBuildCacheSeedVersion || manifest.ToolchainIdentity != identity {
		return "", false, errors.New("trusted Go build cache manifest identity does not match the Go runtime")
	}
	files, err := collectCodeExecGoBuildCacheManifestFiles(seedRoot)
	if err != nil {
		return "", false, err
	}
	if !slices.Equal(manifest.Files, files) {
		return "", false, errors.New("trusted Go build cache manifest integrity check failed")
	}
	cache := filepath.Join(seedRoot, "go-build")
	cacheInfo, err := os.Lstat(cache)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("inspect trusted Go build cache directory: %w", err)
	}
	if !cacheInfo.IsDir() || cacheInfo.Mode()&os.ModeSymlink != 0 {
		return "", false, errors.New("trusted Go build cache directory is non-regular")
	}
	return cache, true, nil
}

func copyCodeExecGoBuildCacheSeed(
	ctx context.Context,
	sourceRoot string,
	destinationRoot string,
	maxBytes int64,
) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("copy trusted Go build cache seed: %w", err)
	}
	seedRoot := filepath.Dir(filepath.Clean(sourceRoot))
	if filepath.Base(filepath.Clean(sourceRoot)) != "go-build" {
		return errors.New("trusted Go build cache source must be the manifest go-build directory")
	}
	seedBefore, seedErr := os.Lstat(seedRoot)
	if seedErr != nil || seedBefore.Mode()&os.ModeSymlink != 0 || !seedBefore.IsDir() {
		return errors.New("trusted Go build cache seed is non-regular")
	}
	manifest, seedErr := readCodeExecGoBuildCacheManifest(seedRoot)
	if seedErr != nil {
		return fmt.Errorf("verify trusted Go build cache manifest before copy: %w", seedErr)
	}
	if manifest.Version != codeExecGoBuildCacheSeedVersion || strings.TrimSpace(manifest.ToolchainIdentity) == "" {
		return errors.New("trusted Go build cache manifest integrity check failed")
	}
	marker, seedErr := readCodeExecSecureFile(seedRoot, ".complete", 4096)
	if seedErr != nil || string(marker) != manifest.ToolchainIdentity {
		return errors.New("trusted Go build cache manifest integrity check failed")
	}
	seedAfterMetadata, seedErr := os.Lstat(seedRoot)
	if seedErr != nil || !os.SameFile(seedBefore, seedAfterMetadata) || seedAfterMetadata.Mode()&os.ModeSymlink != 0 {
		return errors.New("trusted Go build cache seed changed while reading metadata")
	}
	expected := make(map[string]codeExecGoBuildCacheManifestFile, len(manifest.Files))
	for _, entry := range manifest.Files {
		clean, cleanErr := cleanRunRelativePath(filepath.FromSlash(entry.Path))
		if cleanErr != nil || filepath.ToSlash(clean) != entry.Path || entry.Size < 0 || len(entry.SHA256) != 64 ||
			entry.Mode == 0 || strings.TrimSpace(entry.Owner) == "" {
			return errors.New("trusted Go build cache manifest integrity check failed")
		}
		if _, duplicate := expected[entry.Path]; duplicate {
			return errors.New("trusted Go build cache manifest integrity check failed")
		}
		expected[entry.Path] = entry
	}
	if _, destinationErr := os.Lstat(destinationRoot); destinationErr == nil {
		return errors.New("run-local Go build cache already exists")
	} else if !os.IsNotExist(destinationErr) {
		return fmt.Errorf("inspect run-local Go build cache: %w", destinationErr)
	}
	if mkdirErr := os.MkdirAll(destinationRoot, 0700); mkdirErr != nil {
		return fmt.Errorf("create run-local Go build cache: %w", mkdirErr)
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destinationRoot)
		}
	}()
	source, sourceInfo, sourceErr := openCodeExecRootNoFollow(sourceRoot)
	if sourceErr != nil {
		return fmt.Errorf("open trusted Go build cache seed: %w", sourceErr)
	}
	defer joinCodeExecResourceClose(&returnErr, source, "close trusted Go build cache seed")
	destination, _, destinationErr := openCodeExecRootNoFollow(destinationRoot)
	if destinationErr != nil {
		return fmt.Errorf("open run-local Go build cache: %w", destinationErr)
	}
	defer joinCodeExecResourceClose(&returnErr, destination, "close run-local Go build cache")
	copied := int64(0)
	seen := make(map[string]bool, len(expected))
	if copyErr := copyCodeExecGoBuildCacheRoot(ctx, source, destination, "", expected, seen, &copied, maxBytes); copyErr != nil {
		return copyErr
	}
	if len(seen) != len(expected) {
		return errors.New("trusted Go build cache manifest integrity check failed")
	}
	seedAfterCopy, seedPathErr := os.Lstat(seedRoot)
	sourceAfterCopy, sourcePathErr := os.Lstat(sourceRoot)
	if seedPathErr != nil || sourcePathErr != nil || !os.SameFile(seedBefore, seedAfterCopy) ||
		!os.SameFile(sourceInfo, sourceAfterCopy) || seedAfterCopy.Mode()&os.ModeSymlink != 0 ||
		sourceAfterCopy.Mode()&os.ModeSymlink != 0 {
		return errors.New("trusted Go build cache seed changed while copying")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	complete = true
	return nil
}

func copyCodeExecGoBuildCacheRoot(
	ctx context.Context,
	source *os.Root,
	destination *os.Root,
	prefix string,
	expected map[string]codeExecGoBuildCacheManifestFile,
	seen map[string]bool,
	copied *int64,
	maxBytes int64,
) error {
	entries, readErr := fs.ReadDir(source.FS(), ".")
	if readErr != nil {
		return fmt.Errorf("read trusted Go build cache seed: %w", readErr)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		info, entryErr := source.Lstat(name)
		if entryErr != nil {
			return fmt.Errorf("inspect trusted Go build cache entry: %w", entryErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("trusted Go build cache seed contains a non-regular entry")
		}
		relative := filepath.Join(prefix, name)
		if info.IsDir() {
			sourceDirectory, sourceOpenErr := source.OpenRoot(name)
			if sourceOpenErr != nil {
				return fmt.Errorf("open trusted Go build cache directory: %w", sourceOpenErr)
			}
			opened, statErr := sourceDirectory.Stat(".")
			if statErr != nil || !os.SameFile(info, opened) {
				_ = sourceDirectory.Close()
				return errors.New("trusted Go build cache directory changed while opening")
			}
			if mkdirErr := destination.Mkdir(name, 0700); mkdirErr != nil && !os.IsExist(mkdirErr) {
				_ = sourceDirectory.Close()
				return fmt.Errorf("create run-local Go build cache directory: %w", mkdirErr)
			}
			destinationDirectory, destinationOpenErr := destination.OpenRoot(name)
			if destinationOpenErr != nil {
				_ = sourceDirectory.Close()
				return fmt.Errorf("open run-local Go build cache directory: %w", destinationOpenErr)
			}
			copyErr := copyCodeExecGoBuildCacheRoot(
				ctx, sourceDirectory, destinationDirectory, relative, expected, seen, copied, maxBytes,
			)
			sourceCloseErr := sourceDirectory.Close()
			destinationCloseErr := destinationDirectory.Close()
			if copyErr != nil {
				return copyErr
			}
			if sourceCloseErr != nil {
				return sourceCloseErr
			}
			if destinationCloseErr != nil {
				return destinationCloseErr
			}
			continue
		}
		if !info.Mode().IsRegular() || codeExecFileLinkCount(info) != 1 {
			return errors.New("trusted Go build cache seed contains a non-regular entry")
		}
		manifestPath := filepath.ToSlash(relative)
		expectedFile, ok := expected[manifestPath]
		if !ok || seen[manifestPath] || expectedFile.Size != info.Size() ||
			expectedFile.Mode != uint32(info.Mode()) || expectedFile.Owner != codeExecFileOwnerIdentity(info) {
			return errors.New("trusted Go build cache manifest integrity check failed")
		}
		if maxBytes > 0 && (*copied > maxBytes || expectedFile.Size > maxBytes-*copied) {
			return errors.New("trusted Go build cache seed exceeds sandbox workspace limit")
		}
		input, err := source.Open(name)
		if err != nil {
			return fmt.Errorf("open trusted Go build cache file: %w", err)
		}
		opened, statErr := input.Stat()
		if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) ||
			codeExecFileLinkCount(opened) != 1 {
			_ = input.Close()
			return errors.New("trusted Go build cache file changed while opening")
		}
		output, err := destination.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			_ = input.Close()
			return fmt.Errorf("create run-local Go build cache file: %w", err)
		}
		written, digest, copyErr := copyCodeExecManifestFile(
			ctx, output, input, expectedFile.Size, copied, maxBytes,
		)
		// run-local 副本仅服务当前执行，崩溃后会按 owner 目录整体丢弃；持久 seed
		// 已在发布阶段完成全树同步，此处逐文件 fsync 只会放大 I/O 且不增加信任强度。
		closeOutputErr := output.Close()
		after, afterErr := input.Stat()
		closeInputErr := input.Close()
		postPathInfo, pathErr := source.Lstat(name)
		if copyErr != nil {
			return copyErr
		}
		if closeOutputErr != nil {
			return closeOutputErr
		}
		if afterErr != nil || pathErr != nil || !os.SameFile(opened, after) || !os.SameFile(after, postPathInfo) ||
			postPathInfo.Mode()&os.ModeSymlink != 0 || opened.Size() != after.Size() || opened.Mode() != after.Mode() ||
			!opened.ModTime().Equal(after.ModTime()) || codeExecFileLinkCount(after) != 1 ||
			codeExecFileLinkCount(postPathInfo) != 1 {
			return errors.New("trusted Go build cache file changed while copying")
		}
		if closeInputErr != nil {
			return closeInputErr
		}
		if written != expectedFile.Size || digest != expectedFile.SHA256 {
			return errors.New("trusted Go build cache manifest integrity check failed")
		}
		*copied += written
		seen[manifestPath] = true
	}
	return nil
}

func copyCodeExecManifestFile(
	ctx context.Context,
	destination io.Writer,
	source io.Reader,
	expectedBytes int64,
	alreadyCopied *int64,
	maxBytes int64,
) (int64, string, error) {
	hash := sha256.New()
	buffer := make([]byte, 128*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, "", err
		}
		read, readErr := source.Read(buffer)
		if read > 0 {
			chunk := int64(read)
			if chunk > expectedBytes-total {
				return total, "", errors.New("trusted Go build cache manifest integrity check failed")
			}
			if maxBytes > 0 && (*alreadyCopied > maxBytes || total > maxBytes-*alreadyCopied ||
				chunk > maxBytes-*alreadyCopied-total) {
				return total, "", errors.New("trusted Go build cache seed exceeds sandbox workspace limit")
			}
			if err := ctx.Err(); err != nil {
				return total, "", err
			}
			written, writeErr := destination.Write(buffer[:read])
			if writeErr != nil {
				return total, "", writeErr
			}
			if written != read {
				return total, "", io.ErrShortWrite
			}
			_, _ = hash.Write(buffer[:read])
			total += chunk
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, hex.EncodeToString(hash.Sum(nil)), nil
			}
			return total, "", readErr
		}
	}
}

func codeExecGoBuildCacheBasePath(configured string) (string, error) {
	base := strings.TrimSpace(configured)
	if base == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("locate HexClaw cache directory: %w", err)
		}
		base = filepath.Join(userCache, "hexclaw", "code-exec", "go-build")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("trusted Go build cache base must be absolute")
	}
	canonical, err := resolveCodeExecBoundaryPath(base)
	if err != nil {
		return "", fmt.Errorf("resolve trusted Go build cache boundary: %w", err)
	}
	return canonical, nil
}

func codeExecRequestMayUseGo(req codeExecRequest) (bool, error) {
	usesGo, err := codeExecGoExecutionIntent(req)
	if err != nil || usesGo {
		return usesGo, err
	}
	switch req.Mode {
	case "project":
		projectRoot, err := resolveProjectRoot(req.ProjectRoot)
		if err != nil {
			return false, err
		}
		if err := validateCodeExecProjectRuntimeMetadata(req, projectRoot); err != nil {
			return false, err
		}
		return codeExecMayNeedGoRuntime(req, projectRoot), nil
	case "module":
		return codeExecMayNeedGoRuntime(req, ""), nil
	default:
		return false, nil
	}
}

// protectCodeExecTrustedGoCache 在任何项目暂存前建立可信缓存的隔离边界。
func protectCodeExecTrustedGoCache(cfg sandbox.Config, cacheBase string) (sandbox.Config, error) {
	workspace, err := resolveCodeExecBoundaryPath(cfg.Workspace)
	if err != nil {
		return sandbox.Config{}, fmt.Errorf("resolve sandbox workspace boundary: %w", err)
	}
	cache, err := resolveCodeExecBoundaryPath(cacheBase)
	if err != nil {
		return sandbox.Config{}, fmt.Errorf("resolve trusted Go build cache boundary: %w", err)
	}
	if isPathInside(workspace, cache) || isPathInside(cache, workspace) {
		return sandbox.Config{}, errors.New("trusted Go build cache and sandbox workspace must not overlap")
	}
	cfg.DeniedPaths = compactCleanPaths(append(
		append([]string(nil), cfg.DeniedPaths...),
		cache,
	))
	return cfg, nil
}

// canonicalCodeExecPath 从已存在的最长父目录开始解析符号链接，使已存在与待创建路径
// 共享同一规范形式，避免 macOS 的 /var 与 /private/var 等别名进入安全边界。
func canonicalCodeExecPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	current := abs
	var suffix []string
	for {
		if _, statErr := os.Lstat(current); statErr == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil {
				return "", resolveErr
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("boundary path has no existing ancestor")
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// resolveCodeExecBoundaryPath 在统一规范化后拒绝文件系统根目录作为可写安全边界。
func resolveCodeExecBoundaryPath(path string) (string, error) {
	canonical, err := canonicalCodeExecPath(path)
	if err != nil {
		return "", err
	}
	if canonical == filepath.VolumeName(canonical)+string(filepath.Separator) {
		return "", errors.New("boundary path must not be a filesystem root")
	}
	return canonical, nil
}

// ensureCodeExecTrustedDirectory 逐级创建可信目录，并拒绝任何符号链接或非目录组件。
func ensureCodeExecTrustedDirectory(path string) (string, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", errors.New("trusted directory must be absolute")
	}
	volume := filepath.VolumeName(clean)
	rootPath := volume + string(filepath.Separator)
	relative, err := filepath.Rel(rootPath, clean)
	if err != nil {
		return "", fmt.Errorf("resolve trusted directory: %w", err)
	}
	if relative == "." || relative == "" || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("trusted directory must be below a filesystem root")
	}

	current, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", fmt.Errorf("open trusted directory root: %w", err)
	}
	defer func() { _ = current.Close() }()
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		info, inspectErr := current.Lstat(component)
		if os.IsNotExist(inspectErr) {
			if mkdirErr := current.Mkdir(component, 0700); mkdirErr != nil && !os.IsExist(mkdirErr) {
				return "", fmt.Errorf("create trusted directory component %q: %w", component, mkdirErr)
			}
			info, inspectErr = current.Lstat(component)
		}
		if inspectErr != nil {
			return "", fmt.Errorf("inspect trusted directory component %q: %w", component, inspectErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("trusted directory contains symlink component %q", component)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("trusted directory contains non-directory component %q", component)
		}

		next, openErr := current.OpenRoot(component)
		if openErr != nil {
			return "", fmt.Errorf("open trusted directory component %q: %w", component, openErr)
		}
		nextInfo, statErr := next.Stat(".")
		if statErr != nil || !os.SameFile(info, nextInfo) {
			_ = next.Close()
			if statErr != nil {
				return "", fmt.Errorf("verify trusted directory component %q: %w", component, statErr)
			}
			return "", fmt.Errorf("trusted directory component %q changed during creation", component)
		}
		if closeErr := current.Close(); closeErr != nil {
			_ = next.Close()
			return "", fmt.Errorf("close trusted directory component %q: %w", component, closeErr)
		}
		current = next
	}
	if err := current.Chmod(".", 0700); err != nil {
		return "", fmt.Errorf("protect trusted directory: %w", err)
	}
	return clean, nil
}

// cleanupCodeExecGoBuildCache 仅删除当前运行私有的 Go 构建缓存。
func cleanupCodeExecGoBuildCache(run codeExecRun) (returnErr error) {
	workspace, err := filepath.Abs(run.Workspace)
	if err != nil {
		return fmt.Errorf("resolve run workspace: %w", err)
	}
	cacheDir, err := filepath.Abs(run.CacheDir)
	if err != nil {
		return fmt.Errorf("resolve run cache directory: %w", err)
	}
	relative, err := filepath.Rel(workspace, cacheDir)
	if err != nil || relative != "cache" {
		if err != nil {
			return fmt.Errorf("validate run cache directory: %w", err)
		}
		return errors.New("run cache directory must be the workspace cache directory")
	}

	workspaceRoot, err := os.OpenRoot(workspace)
	if err != nil {
		return fmt.Errorf("open run workspace: %w", err)
	}
	defer joinCodeExecResourceClose(&returnErr, workspaceRoot, "close run workspace")
	cacheInfo, err := workspaceRoot.Lstat("cache")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect run cache directory: %w", err)
	}
	if cacheInfo.Mode()&os.ModeSymlink != 0 || !cacheInfo.IsDir() {
		return errors.New("run cache directory is not a regular directory")
	}
	cacheRoot, err := workspaceRoot.OpenRoot("cache")
	if err != nil {
		return fmt.Errorf("open run cache directory: %w", err)
	}
	defer joinCodeExecResourceClose(&returnErr, cacheRoot, "close run cache directory")
	openedCacheInfo, err := cacheRoot.Stat(".")
	if err != nil {
		return fmt.Errorf("verify run cache directory: %w", err)
	}
	if !os.SameFile(cacheInfo, openedCacheInfo) {
		return errors.New("run cache directory changed during cleanup")
	}

	goBuildInfo, err := cacheRoot.Lstat("go-build")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect run-local Go build cache: %w", err)
	}
	if goBuildInfo.Mode()&os.ModeSymlink != 0 || !goBuildInfo.IsDir() {
		if removeErr := cacheRoot.Remove("go-build"); removeErr != nil {
			return fmt.Errorf("remove invalid run-local Go build cache: %w", removeErr)
		}
		return errors.New("run-local Go build cache was not a regular directory")
	}
	if err := cacheRoot.RemoveAll("go-build"); err != nil {
		return fmt.Errorf("remove run-local Go build cache: %w", err)
	}
	if _, err := cacheRoot.Lstat("go-build"); err == nil {
		return errors.New("run-local Go build cache still exists after cleanup")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("verify run-local Go build cache cleanup: %w", err)
	}
	return nil
}

type codeExecArtifact struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	MIME   string `json:"mime,omitempty"`
}

type codeExecReport struct {
	RunID             string   `json:"run_id"`
	Mode              string   `json:"mode"`
	Language          string   `json:"language,omitempty"`
	Command           []string `json:"command,omitempty"`
	EntryPoint        string   `json:"entrypoint,omitempty"`
	Status            string   `json:"status"`
	ExitCode          int      `json:"exit_code"`
	Timeout           bool     `json:"timeout"`
	RuntimeMissing    bool     `json:"runtime_missing"`
	DependencyMissing []string `json:"dependency_missing,omitempty"`
	Error             string   `json:"error,omitempty"`
	StdoutBytes       int64    `json:"stdout_bytes"`
	StderrBytes       int64    `json:"stderr_bytes"`
	StdoutTruncated   bool     `json:"stdout_truncated"`
	StderrTruncated   bool     `json:"stderr_truncated"`
	Truncated         bool     `json:"truncated"`
	MaxStdoutBytes    int64    `json:"max_stdout_bytes"`
	MaxStderrBytes    int64    `json:"max_stderr_bytes"`
	MaxWorkspaceBytes int64    `json:"max_workspace_bytes"`
	MaxArtifactBytes  int64    `json:"max_artifact_bytes"`
	MaxProcesses      int      `json:"max_processes"`
	MaxMemoryBytes    int64    `json:"max_memory_bytes"`
	WorkspaceBytes    int64    `json:"workspace_bytes"`
	WorkspaceLimited  bool     `json:"workspace_limited"`
	// FilesystemIsolation 如实反映本次执行的文件系统隔离强度（enforced/unsupported），
	// 来自沙箱后端上报的 ExecResult.Limits.Filesystem，而非硬编码常量。
	FilesystemIsolation string             `json:"filesystem_isolation,omitempty"`
	FilesystemDegraded  bool               `json:"filesystem_degraded,omitempty"`
	Paths               map[string]string  `json:"paths"`
	Artifacts           []codeExecArtifact `json:"artifacts"`
	Capabilities        map[string]any     `json:"capabilities"`
}

// NewCodeExecSkill 创建代码执行 Skill
//
// validatedSandbox 的所有权随调用转移；它仅用于证明初始配置可构造，构造函数会立即关闭，
// CodeExecSkill 不保留长期占位 Sandbox。每次执行及策略校验都拥有并关闭自己的临时实例。
func NewCodeExecSkill(validatedSandbox sandbox.Sandbox, cfg sandbox.Config) *CodeExecSkill {
	cfg = withCodeExecRequiredCapabilities(cfg)
	// 构造时即使用与执行边界相同的规范路径，保证配置快照、生命周期归属和报告一致。
	if canonicalWorkspace, err := resolveCodeExecBoundaryPath(cfg.Workspace); err == nil {
		cfg.Workspace = canonicalWorkspace
	}
	initializationErr := joinCodeExecSandboxClose(context.Background(), nil, validatedSandbox, "close initial sandbox")
	return &CodeExecSkill{
		policy:            &codeExecPolicyState{cfg: cloneCodeExecSandboxConfig(cfg)},
		initializationErr: initializationErr,
		sandboxFactory:    sandbox.New,
		goHelperFactory:   sandbox.New,
	}
}

// PrepareSandboxPolicy 构建并验证完整策略候选，但在 Commit 前不改变运行态。
// 返回候选后调用方必须且只能选择 Commit 或 Discard；两者均可安全重复调用。
func (s *CodeExecSkill) PrepareSandboxPolicy(
	ctx context.Context,
	policy SandboxPolicy,
) (candidate *SandboxPolicyCandidate, returnErr error) {
	if ctx == nil {
		return nil, errors.New("sandbox policy context must not be nil")
	}
	if policy.NetworkEnabled {
		return nil, errCodeExecHostNetworkUnsupported
	}
	s.policyUpdateMu.Lock()
	releaseWriter := true
	defer func() {
		if releaseWriter {
			s.policyUpdateMu.Unlock()
		}
	}()

	s.mu.RLock()
	initializationErr := s.initializationErr
	current := cloneCodeExecPolicyState(s.policy)
	factory := s.sandboxFactory
	hasAuthorizer := current.authorizer != nil || s.fileAccessRuntime != nil
	s.mu.RUnlock()
	if initializationErr != nil {
		return nil, fmt.Errorf("code execution sandbox initialization failed: %w", initializationErr)
	}
	if factory == nil {
		factory = sandbox.New
	}

	nextCfg := cloneCodeExecSandboxConfig(current.cfg)
	nextCfg.Network = sandbox.NetworkDisabled
	nextCfg.ReadablePaths = append([]string(nil), policy.ReadablePaths...)
	nextCfg = withCodeExecRequiredCapabilities(nextCfg)
	if err := validateCodeExecSandboxPolicyCandidate(ctx, nextCfg, factory); err != nil {
		return nil, fmt.Errorf("prepare sandbox policy update: %w", err)
	}

	preparedFileAccess := prepareFileAccessPolicy(policy.ReadablePaths)
	nextState := &codeExecPolicyState{cfg: nextCfg}
	if hasAuthorizer {
		nextState.authorizer = newFileAccessBrokerFromPolicy(preparedFileAccess)
	}
	candidate = &SandboxPolicyCandidate{finish: func(commit bool) {
		defer s.policyUpdateMu.Unlock()
		if !commit {
			return
		}
		s.mu.Lock()
		if s.fileAccessRuntime != nil {
			s.fileAccessRuntime.commitFileAccessPolicy(preparedFileAccess)
		}
		s.policy = nextState
		s.mu.Unlock()
	}}
	releaseWriter = false
	return candidate, nil
}

// SandboxPolicy 返回当前完整策略代际的防御性副本。
func (s *CodeExecSkill) SandboxPolicy() SandboxPolicy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := cloneCodeExecPolicyState(s.policy)
	return SandboxPolicy{
		NetworkEnabled: state.cfg.Network == sandbox.NetworkHost,
		ReadablePaths:  append([]string(nil), state.cfg.ReadablePaths...),
	}
}

func (s *CodeExecSkill) Name() string { return "code_exec" }
func (s *CodeExecSkill) Description() string {
	return "Execute code, files, or project commands inside the HexClaw Sandbox and return output, run metadata, and artifacts."
}
func (s *CodeExecSkill) Match(_ string) bool { return false } // LLM-only, no keyword trigger

func (s *CodeExecSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("code_exec", "Execute code, existing files, or project commands inside the HexClaw Sandbox. For quick Python/shell calculations use mode=snippet and provide both language and the complete code in the same tool call; never call snippet with only mode/language/timeout. Prefer mode=project with command for repository tests/builds, mode=file with entrypoint for existing scripts, mode=module with files for multi-file snippets. Each run has an isolated run_id workspace, bounded output, resource-limit metadata, runtime diagnostics, and artifact manifest.", &llm.Schema{
		Type: "object",
		Properties: map[string]*llm.Schema{
			"mode": {
				Type:        "string",
				Description: "Execution mode: snippet, file, module, or project. Defaults from provided fields.",
				Enum:        []any{"snippet", "file", "module", "project"},
			},
			"language": {
				Type:        "string",
				Description: "Runtime language for snippet, module, file, and project execution: python, python3, javascript, js, node, go, golang. Required when mode=snippet; Go mode accepts only structured direct go argv.",
				Enum:        []any{"python", "python3", "javascript", "js", "node", "go", "golang"},
			},
			"code": {
				Type:        "string",
				Description: "Complete source code to execute. Required when mode=snippet; include the full Python/JS/Go script here, not in natural language.",
			},
			"entrypoint": {
				Type:        "string",
				Description: "Existing script path or file path to run for mode=file. Absolute paths are copied into the run workspace before execution.",
			},
			"project_root": {
				Type:        "string",
				Description: "Project directory for mode=project. If omitted, HexClaw uses the nearest local project root.",
			},
			"command": {
				Type:        "array",
				Description: "Structured command argv for mode=project/file/module, for example ['go','test','./...'] or ['node','scripts/check.js']; Go commands cannot use shell, task-runner, or project-script wrappers.",
				Items:       &llm.Schema{Type: "string"},
			},
			"files": {
				Type:        "array",
				Description: "Files to materialize in the per-run workspace for mode=module. Each item has path and content.",
				Items: &llm.Schema{
					Type: "object",
					Properties: map[string]*llm.Schema{
						"path":    {Type: "string", Description: "Relative file path inside the run workspace."},
						"content": {Type: "string", Description: "File content."},
					},
					Required: []string{"path", "content"},
				},
			},
			"artifacts": {
				Type:        "boolean",
				Description: "Whether to scan artifacts/ and include artifact manifest entries. Defaults false and requires enforced process containment.",
			},
			"timeout": {
				Type:        "integer",
				Description: "Optional per-run timeout in seconds. Defaults to sandbox config timeout.",
			},
		},
	})
}

func (s *CodeExecSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	if ctx == nil {
		return nil, errors.New("code execution context must not be nil")
	}
	if err := s.initializationError(); err != nil {
		return nil, fmt.Errorf("code execution sandbox initialization failed: %w", err)
	}
	req, err := parseCodeExecRequest(args)
	if err != nil {
		return nil, err
	}

	cfg, broker, factory, goHelperFactory, scratchBase, projectStager, goBuildCacheBase, goBuildCacheCleaner := s.snapshot()
	if cfg.Network == sandbox.NetworkHost {
		return nil, errCodeExecHostNetworkUnsupported
	}
	if strings.TrimSpace(cfg.Workspace) == "" {
		return nil, errors.New("sandbox workspace is required")
	}
	cfg.Workspace, err = resolveCodeExecBoundaryPath(cfg.Workspace)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox workspace boundary: %w", err)
	}
	// P0 收口：mode=file/project 触达宿主机的路径在读取/授予前必须过集中裁决（fail-closed）。
	if authorizationErr := authorizeCodeExecHostPaths(broker, cfg.Workspace, req); authorizationErr != nil {
		return nil, authorizationErr
	}
	// 工具链识别、项目暂存、依赖闭包、构建和最终执行共享同一个请求截止时间，
	// 多包测试不得因阶段切换而重复获得完整超时预算。
	executionCtx, cancelExecution := context.WithTimeout(
		ctx,
		codeExecRequestTimeout(cfg.Timeout, req.Timeout),
	)
	defer cancelExecution()
	ctx = executionCtx
	goRuntime, err := codeExecRequestMayUseGo(req)
	if err != nil {
		return nil, err
	}
	if goRuntime {
		goBuildCacheBase, err = codeExecGoBuildCacheBasePath(goBuildCacheBase)
		if err != nil {
			return nil, err
		}
		cfg, err = protectCodeExecTrustedGoCache(cfg, goBuildCacheBase)
		if err != nil {
			return nil, err
		}
	}
	if mkdirErr := os.MkdirAll(cfg.Workspace, 0755); mkdirErr != nil {
		return nil, fmt.Errorf("create sandbox base workspace: %w", mkdirErr)
	}
	plan, err := newCodeExecExecutionPlan(
		ctx,
		cfg,
		goRuntime,
		codeExecGoHelper{Factory: goHelperFactory},
	)
	if err != nil {
		return nil, err
	}
	run, err := prepareCodeExecRun(ctx, cfg, req, broker, scratchBase, projectStager, plan)
	if err != nil {
		return nil, err
	}
	var cleanupOnce sync.Once
	var cleanupErr error
	goCacheRun := run
	cleanupRunGoCache := func() error {
		if !goCacheRun.Plan.GoRuntime {
			return nil
		}
		cleanupOnce.Do(func() {
			cleanupErr = goBuildCacheCleaner(goCacheRun)
		})
		return cleanupErr
	}
	// GOCACHE 是否需要清理由运行事实决定，并在创建运行目录后立即注册。
	defer func() { _ = cleanupRunGoCache() }()
	withGoCacheCleanup := func(runErr error) error {
		if cleanErr := cleanupRunGoCache(); cleanErr != nil {
			return errors.Join(runErr, fmt.Errorf("remove run-local Go build cache: %w", cleanErr))
		}
		return runErr
	}
	command, commandConfig, err := prepareCodeExecCommandWithCapabilities(ctx, req, run, runtime.GOOS)
	if err != nil {
		return nil, withGoCacheCleanup(err)
	}
	run.Config = commandConfig
	plan, err = bindCodeExecExecutionPlanCommand(run.Plan, command)
	if err != nil {
		return nil, withGoCacheCleanup(err)
	}
	run.Plan = plan
	command = plan.Command
	if plan.GoRuntime {
		if err := prepareCodeExecRunGoDependencyClosure(ctx, &run); err != nil {
			return nil, withGoCacheCleanup(err)
		}
	}
	usesGoTest := plan.GoTest
	if usesGoTest {
		if _, err := prepareCodeExecGoBuildCache(ctx, run, goBuildCacheBase); err != nil {
			return nil, withGoCacheCleanup(err)
		}
	}
	run.Config = withCodeExecRequiredCapabilities(run.Config)
	var result *sandbox.ExecResult
	var execErr error
	var missingDeps []string
	if plan.GoRuntime {
		if plan.GoCommand == nil {
			return nil, withGoCacheCleanup(errors.New("go execution plan is missing the parsed command"))
		}
		var executionRun codeExecRun
		result, executionRun, execErr = executeCodeExecGoTwoPhase(
			ctx,
			run,
			*plan.GoCommand,
			goHelperFactory,
			factory,
		)
		if strings.TrimSpace(executionRun.Workspace) != "" {
			run = executionRun
		}
	} else {
		sb, createErr := factory(run.Config)
		if createErr != nil {
			createErr = fmt.Errorf("create sandbox run %s failed: %w", run.ID, createErr)
			return nil, withGoCacheCleanup(createErr)
		}
		if sb == nil {
			createErr = fmt.Errorf("create sandbox run %s failed: factory returned nil sandbox", run.ID)
			return nil, withGoCacheCleanup(createErr)
		}
		result, execErr, missingDeps = runCodeExecWithOwnedSandbox(ctx, sb, run, req, command)
	}
	if cleanErr := cleanupRunGoCache(); cleanErr != nil {
		execErr = errors.Join(execErr, fmt.Errorf("remove run-local Go build cache: %w", cleanErr))
	}

	report := buildCodeExecReport(req, run, command, result, execErr, missingDeps)
	finalizeErr := finalizeCodeExecReport(ctx, run, req.Artifacts, &report)
	if finalizeErr != nil {
		appendCodeExecReportError(&report, fmt.Errorf("finalize code execution report: %w", finalizeErr))
	}
	report.Capabilities["artifact_manifest"] = req.Artifacts && finalizeErr == nil
	if err := writeCodeExecManifest(run, report); err != nil {
		report.Capabilities["artifact_manifest"] = false
		appendCodeExecReportError(&report, fmt.Errorf("write code execution manifest: %w", err))
	}

	content := formatCodeExecOutput(result, report)
	return &skill.Result{
		Content: content,
		Data:    report,
		Metadata: map[string]string{
			"run_id": report.RunID,
			"status": report.Status,
			"mode":   report.Mode,
		},
	}, nil
}

func (s *CodeExecSkill) snapshot() (
	sandbox.Config,
	*FileAccessBroker,
	func(sandbox.Config) (sandbox.Sandbox, error),
	func(sandbox.Config) (sandbox.Sandbox, error),
	string,
	codeExecProjectStager,
	string,
	func(codeExecRun) error,
) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state := cloneCodeExecPolicyState(s.policy)
	cfg := state.cfg
	broker := state.authorizer
	factory := s.sandboxFactory
	if factory == nil {
		factory = sandbox.New
	}
	goHelperFactory := s.goHelperFactory
	if goHelperFactory == nil {
		goHelperFactory = sandbox.New
	}
	projectStager := s.projectStager
	if projectStager == nil {
		projectStager = stageCodeExecProject
	}
	goBuildCacheCleaner := s.goBuildCacheCleaner
	if goBuildCacheCleaner == nil {
		goBuildCacheCleaner = cleanupCodeExecGoBuildCache
	}
	return cfg, broker, factory, goHelperFactory, s.scratchBase, projectStager, s.goBuildCacheBase, goBuildCacheCleaner
}

func (s *CodeExecSkill) initializationError() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.initializationErr
}

func validateCodeExecSandboxPolicyCandidate(
	ctx context.Context,
	cfg sandbox.Config,
	factory func(sandbox.Config) (sandbox.Sandbox, error),
) error {
	candidate, err := factory(cfg)
	if err != nil {
		return fmt.Errorf("create sandbox policy candidate: %w", err)
	}
	if candidate == nil {
		return errors.New("create sandbox policy candidate: factory returned nil sandbox")
	}
	available, inspectErr := sandbox.AvailableCapabilities(ctx, candidate)
	if inspectErr != nil {
		inspectErr = fmt.Errorf("inspect sandbox policy capabilities: %w", inspectErr)
	} else if missing := cfg.RequiredCapabilities.Missing(available); missing != 0 {
		inspectErr = fmt.Errorf("%w: %s", sandbox.ErrRequiredCapabilitiesUnavailable, missing)
	}
	return joinCodeExecSandboxClose(ctx, inspectErr, candidate, "close sandbox policy candidate")
}

func cloneCodeExecPolicyState(state *codeExecPolicyState) *codeExecPolicyState {
	if state == nil {
		return &codeExecPolicyState{}
	}
	return &codeExecPolicyState{
		cfg:        cloneCodeExecSandboxConfig(state.cfg),
		authorizer: state.authorizer,
	}
}

func cloneCodeExecSandboxConfig(cfg sandbox.Config) sandbox.Config {
	cfg.ReadablePaths = append([]string(nil), cfg.ReadablePaths...)
	cfg.DeniedPaths = append([]string(nil), cfg.DeniedPaths...)
	return cfg
}

func cloneCodeExecFileAccessBroker(broker *FileAccessBroker) *FileAccessBroker {
	if broker == nil {
		return nil
	}
	return NewFileAccessBroker(broker.AllowedDirectories())
}

//nolint:staticcheck // 保持既有 result、error、missingDeps 内部合同，避免错误重排破坏现有调用语义。
func runCodeExecWithOwnedSandbox(
	ctx context.Context,
	sb sandbox.Sandbox,
	run codeExecRun,
	req codeExecRequest,
	command []string,
) (result *sandbox.ExecResult, execErr error, missingDeps []string) {
	defer func() {
		execErr = joinCodeExecSandboxClose(ctx, execErr, sb, "close code execution sandbox")
	}()

	result, execErr = runCodeExecPlannedCommand(ctx, sb, run, command)
	missingDeps = detectMissingPackages(req.Language, execText(result, execErr))
	if run.Config.Network != sandbox.NetworkHost || result == nil || result.ExitCode == 0 || len(missingDeps) == 0 {
		return result, execErr, missingDeps
	}
	installCommand := buildInstallCommand(req.Language, missingDeps)
	if len(installCommand) == 0 {
		return result, execErr, missingDeps
	}
	if capabilityErr := validateCodeExecDynamicCommandCapabilities(runtime.GOOS, installCommand); capabilityErr != nil {
		return result, errors.Join(execErr, capabilityErr), missingDeps
	}
	installResult, installErr := runSandboxCommand(ctx, sb, run, installCommand)
	if installErr == nil && installResult != nil && installResult.ExitCode == 0 {
		result, execErr = runCodeExecPlannedCommand(ctx, sb, run, command)
	}
	return result, execErr, missingDeps
}

const (
	codeExecWindowsSandboxCloseAttempts   = 3
	codeExecWindowsSandboxCloseRetryDelay = 10 * time.Millisecond
	codeExecSandboxCloseConvergenceLimit  = time.Minute
)

func joinCodeExecSandboxClose(
	ctx context.Context,
	runErr error,
	sb sandbox.Sandbox,
	operation string,
) error {
	if sb == nil {
		return runErr
	}
	// 请求取消不能放弃资源所有权；独立硬截止时间只约束关闭收敛，避免无限重试。
	closeBase := context.Background()
	if ctx != nil {
		closeBase = context.WithoutCancel(ctx)
	}
	closeCtx, cancel := context.WithTimeout(closeBase, codeExecSandboxCloseConvergenceLimit)
	defer cancel()
	return joinCodeExecSandboxCloseForPlatform(closeCtx, runErr, sb, operation, runtime.GOOS)
}

func joinCodeExecSandboxCloseForPlatform(
	ctx context.Context,
	runErr error,
	sb sandbox.Sandbox,
	operation string,
	goos string,
) error {
	if sb == nil {
		return runErr
	}
	if closeErr := closeCodeExecSandboxForPlatform(ctx, sb, goos); closeErr != nil {
		return errors.Join(
			runErr,
			fmt.Errorf("%w: %s: %w", errCodeExecSandboxClose, operation, closeErr),
		)
	}
	return runErr
}

func closeCodeExecSandboxForPlatform(ctx context.Context, sb sandbox.Sandbox, goos string) error {
	attempts := 1
	retryDelay := time.Duration(0)
	if strings.EqualFold(strings.TrimSpace(goos), "windows") {
		attempts = codeExecWindowsSandboxCloseAttempts
		retryDelay = codeExecWindowsSandboxCloseRetryDelay
	}
	return closeCodeExecSandboxWithPolicy(ctx, sb, attempts, retryDelay)
}

// closeCodeExecSandboxWithPolicy 以固定次数收敛可重试关闭；仅重试间隔受上下文取消，
// 单次 Close 的上界由 Toolkit 后端合同负责。
func closeCodeExecSandboxWithPolicy(
	ctx context.Context,
	sb sandbox.Sandbox,
	maxAttempts int,
	retryDelay time.Duration,
) error {
	if sb == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("sandbox close context must not be nil")
	}
	if maxAttempts <= 0 {
		return errors.New("sandbox close attempts must be positive")
	}
	if retryDelay < 0 {
		return errors.New("sandbox close retry delay must not be negative")
	}
	var accumulated error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		closeErr := sb.Close()
		if closeErr == nil {
			return nil
		}
		accumulated = errors.Join(accumulated, closeErr)
		if attempt == maxAttempts {
			break
		}
		if retryDelay == 0 {
			select {
			case <-ctx.Done():
				return errors.Join(accumulated, ctx.Err())
			default:
			}
			continue
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(accumulated, ctx.Err())
		}
	}
	return accumulated
}

func parseCodeExecRequest(args map[string]any) (codeExecRequest, error) {
	req := codeExecRequest{
		Mode:     strings.ToLower(strings.TrimSpace(stringArg(args, "mode"))),
		Language: normalizeCodeExecLanguage(stringArg(args, "language")),
		Code:     stringArg(args, "code"),
		EntryPoint: strings.TrimSpace(firstNonEmpty(
			stringArg(args, "entrypoint"),
			stringArg(args, "file_path"),
			stringArg(args, "script_path"),
			stringArg(args, "script"),
			stringArg(args, "filename"),
			stringArg(args, "path"),
			stringArg(args, "file"),
		)),
		ProjectRoot: strings.TrimSpace(firstNonEmpty(
			stringArg(args, "project_root"),
			stringArg(args, "working_directory"),
			stringArg(args, "workdir"),
			stringArg(args, "directory"),
			stringArg(args, "root"),
			stringArg(args, "cwd"),
			stringArg(args, "repo"),
		)),
		Timeout:   intArgDefault(args, "timeout", 0),
		Artifacts: boolArgDefault(args, "artifacts", false),
	}
	req.Command, req.CommandText = firstCommandArg(args, "command", "cmd", "argv", "args")
	req.Files = filesArg(args, "files")
	if req.EntryPoint == "" && req.Code != "" && looksLikeExistingPath(req.Code) {
		req.EntryPoint = strings.TrimSpace(req.Code)
		req.Code = ""
	}
	if req.Mode == "" {
		switch {
		case len(req.Command) > 0 || req.CommandText != "":
			req.Mode = "project"
		case req.EntryPoint != "":
			req.Mode = "file"
		case len(req.Files) > 0:
			req.Mode = "module"
		default:
			req.Mode = "snippet"
		}
	}
	if req.Language == "" && strings.TrimSpace(req.Code) != "" {
		req.Language = inferLanguageFromCode(req.Code)
	}
	if req.Mode == "snippet" && strings.TrimSpace(req.Code) == "" {
		if language, code, ok := snippetFromCommand(req.Command, req.CommandText); ok {
			if req.Language == "" {
				req.Language = language
			}
			req.Code = code
			req.Command = nil
			req.CommandText = ""
		}
	}
	if req.CommandText != "" {
		return req, errors.New("structured command argv is required; command strings are not supported")
	}

	switch req.Mode {
	case "snippet":
		if req.Language == "" || req.Code == "" {
			return req, fmt.Errorf("language and code are required for mode=snippet")
		}
	case "file":
		// Prefer entrypoint/command, but allow inference from the base workspace
		// for weaker tool-use models that only emit mode=file.
	case "module":
		if len(req.Files) == 0 && req.Code == "" {
			return req, fmt.Errorf("files or code are required for mode=module")
		}
	case "project":
		if strings.TrimSpace(req.Code) != "" {
			return req, errors.New("code is not supported for mode=project; provide structured command argv")
		}
		// Prefer command, but allow project-level default runner inference.
	default:
		return req, fmt.Errorf("unsupported code_exec mode: %s", req.Mode)
	}
	return req, nil
}

func snippetFromCommand(command []string, commandText string) (string, string, bool) {
	if len(command) >= 3 && isPythonCommand(command[0]) && command[1] == "-c" && strings.TrimSpace(command[2]) != "" {
		return "python3", command[2], true
	}
	if parsed := jsonStringCommandArg(commandText); len(parsed) > 0 {
		return snippetFromCommand(parsed, "")
	}
	return "", "", false
}

func isPythonCommand(name string) bool {
	base := filepath.Base(strings.TrimSpace(name))
	return base == "python" || base == "python3"
}

func prepareCodeExecRun(
	ctx context.Context,
	cfg sandbox.Config,
	req codeExecRequest,
	broker *FileAccessBroker,
	scratchBase string,
	projectStager codeExecProjectStager,
	plan codeExecExecutionPlan,
) (run codeExecRun, returnErr error) {
	if cfg.Workspace == "" {
		return codeExecRun{}, fmt.Errorf("sandbox workspace is required")
	}
	base, boundaryErr := resolveCodeExecBoundaryPath(cfg.Workspace)
	if boundaryErr != nil {
		return codeExecRun{}, fmt.Errorf("resolve sandbox workspace boundary: %w", boundaryErr)
	}
	cfg.Workspace = base
	hostProjectRoot := ""
	if req.Mode == "project" {
		resolvedProjectRoot, projectErr := resolveProjectRoot(req.ProjectRoot)
		if projectErr != nil {
			return codeExecRun{}, projectErr
		}
		hostProjectRoot = resolvedProjectRoot
		// 元数据校验必须先于任何运行目录创建，拒绝请求时不得留下暂存痕迹。
		if validationErr := validateCodeExecProjectRuntimeMetadata(req, hostProjectRoot); validationErr != nil {
			return codeExecRun{}, validationErr
		}
	}
	cfg = ensureCodeExecConfigDefaults(cfg)
	applicationBudget := codeExecApplicationBudgetFor(cfg)
	if mkdirErr := os.MkdirAll(base, 0755); mkdirErr != nil {
		return codeExecRun{}, fmt.Errorf("create sandbox base workspace: %w", mkdirErr)
	}
	runID := newCodeExecRunID()
	root := filepath.Join(base, "runs", runID)
	workspace := filepath.Join(root, "work")
	scratch := workspace

	if req.Mode == "project" {
		if strings.TrimSpace(scratchBase) == "" {
			scratchBase = codeExecScratchBase()
		}
		canonicalScratchBase, scratchErr := resolveCodeExecBoundaryPath(scratchBase)
		if scratchErr != nil {
			return codeExecRun{}, fmt.Errorf("resolve sandbox scratch boundary: %w", scratchErr)
		}
		scratch = filepath.Join(canonicalScratchBase, "hexclaw-sandbox-runs", runID)
		workspace = scratch
	}

	run = codeExecRun{
		ID:           runID,
		Base:         base,
		Root:         root,
		Workspace:    workspace,
		Scratch:      scratch,
		ArtifactDir:  filepath.Join(scratch, "artifacts"),
		LogDir:       filepath.Join(root, "logs"),
		CacheDir:     filepath.Join(scratch, "cache"),
		ManifestPath: filepath.Join(root, "manifest.json"),
		Plan:         plan,
		Config:       cfg,
		Budget:       applicationBudget,
	}
	ownedRoots := []codeExecOwnedRunRoot{{Path: run.Root, Parent: filepath.Dir(run.Root), Owner: runID}}
	if run.Scratch != run.Root && !isPathInside(run.Root, run.Scratch) {
		ownedRoots = append(ownedRoots, codeExecOwnedRunRoot{
			Path:   run.Scratch,
			Parent: filepath.Dir(run.Scratch),
			Owner:  runID,
		})
	}
	ownedForCleanup := make([]codeExecOwnedRunRoot, 0, len(ownedRoots))
	defer func() {
		if returnErr == nil {
			return
		}
		if cleanupErr := cleanupCodeExecOwnedRunRoots(ownedForCleanup); cleanupErr != nil {
			returnErr = errors.Join(returnErr, cleanupErr)
		}
	}()
	for _, owned := range ownedRoots {
		if createErr := createCodeExecOwnedRunRoot(owned); createErr != nil {
			return codeExecRun{}, createErr
		}
		run.OwnedRoots = append(run.OwnedRoots, owned)
		ownedForCleanup = append(ownedForCleanup, owned)
	}
	for _, dir := range []string{run.Scratch, run.ArtifactDir, run.LogDir, run.CacheDir} {
		if mkdirErr := os.MkdirAll(dir, 0755); mkdirErr != nil {
			return codeExecRun{}, fmt.Errorf("create run directory: %w", mkdirErr)
		}
	}
	if req.Mode == "project" {
		stagedRoot := filepath.Join(run.Scratch, "project-workspace")
		stagedProjectRoot, stagedGoWork, vendored, stageErr := projectStager(
			ctx,
			hostProjectRoot,
			stagedRoot,
			run.Plan,
			broker,
			cfg,
		)
		if stageErr != nil {
			return codeExecRun{}, stageErr
		}
		run.ProjectRoot = stagedProjectRoot
		run.GoWorkPath = stagedGoWork
		run.GoVendored = vendored
		run.StagedProject = true
	}
	if run.Plan.GoRuntime {
		// Go 最终进程始终离线；依赖只能由受信任辅助过程投影到本次运行目录。
		run.Config.Network = sandbox.NetworkDisabled
	}

	run.Config.Workspace = run.Workspace
	if req.Timeout > 0 && (run.Config.Timeout <= 0 || req.Timeout < run.Config.Timeout) {
		run.Config.Timeout = req.Timeout
	}
	// 已暂存项目不得继续读取宿主原树；与项目无关的连接器授权仍按用户显式合同保留。
	run.Config.ReadablePaths = run.Config.ReadablePaths[:0]
	for _, readable := range cfg.ReadablePaths {
		if hostProjectRoot != "" &&
			(pathWithinResolved(readable, hostProjectRoot) || pathWithinResolved(hostProjectRoot, readable)) {
			continue
		}
		run.Config.ReadablePaths = append(run.Config.ReadablePaths, readable)
	}
	finalDenied := append([]string(nil), run.Config.DeniedPaths...)
	if hostProjectRoot != "" {
		finalDenied = append(finalDenied, hostProjectRoot)
	}
	if req.Mode == "file" && strings.TrimSpace(req.EntryPoint) != "" {
		if entryPath, pathErr := canonicalCodeExecPath(req.EntryPoint); pathErr == nil {
			finalDenied = append(finalDenied, entryPath)
		}
	}
	canonicalDenied, err := canonicalCodeExecPaths(finalDenied)
	if err != nil {
		return codeExecRun{}, fmt.Errorf("resolve final sandbox denied paths: %w", err)
	}
	run.Config.DeniedPaths = canonicalDenied
	if run.Plan.GoRuntime {
		if run.Plan.Toolchain != nil {
			run.Config.ReadablePaths = append(run.Config.ReadablePaths, run.Plan.Toolchain.GOROOT)
		}
	}
	run.Config.ReadablePaths = compactCleanPaths(run.Config.ReadablePaths)
	return run, nil
}

func createCodeExecOwnedRunRoot(owned codeExecOwnedRunRoot) error {
	path := filepath.Clean(owned.Path)
	parent := filepath.Clean(owned.Parent)
	if strings.TrimSpace(owned.Owner) == "" || filepath.Dir(path) != parent {
		return errors.New("create owned run directory: invalid ownership boundary")
	}
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("create owned run parent: %w", err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return fmt.Errorf("create owned run directory: %w", err)
	}
	if err := writeCodeExecSecureFile(path, codeExecRunOwnerMarkerName, []byte(owned.Owner), 0600); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write owned run marker: %w", err)
	}
	return nil
}

func cleanupCodeExecOwnedRunRoots(roots []codeExecOwnedRunRoot) error {
	var cleanupErr error
	for index := len(roots) - 1; index >= 0; index-- {
		owned := roots[index]
		path := filepath.Clean(owned.Path)
		parent := filepath.Clean(owned.Parent)
		if filepath.Dir(path) != parent || strings.TrimSpace(owned.Owner) == "" {
			cleanupErr = errors.Join(cleanupErr, errors.New("remove owned run directory: invalid ownership boundary"))
			continue
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			cleanupErr = errors.Join(cleanupErr, errors.New("remove owned run directory: ownership check failed"))
			continue
		}
		marker, err := readCodeExecSecureFile(path, codeExecRunOwnerMarkerName, 4096)
		if err != nil || string(marker) != owned.Owner {
			cleanupErr = errors.Join(cleanupErr, errors.New("remove owned run directory: ownership check failed"))
			continue
		}
		quarantine := filepath.Join(parent, ".cleanup-"+owned.Owner+"-"+newCodeExecRunID())
		if err := os.Rename(path, quarantine); err != nil {
			cleanupErr = errors.Join(cleanupErr, errors.New("remove owned run directory: quarantine failed"))
			continue
		}
		if err := os.RemoveAll(quarantine); err != nil {
			cleanupErr = errors.Join(cleanupErr, errors.New("remove owned run directory: cleanup failed"))
		}
	}
	return cleanupErr
}

type codeExecGoModuleRef struct {
	Path     string
	Version  string
	Indirect bool
}

type codeExecGoReplace struct {
	Old codeExecGoModuleRef
	New codeExecGoModuleRef
}

type codeExecGoWorkUse struct {
	DiskPath string
}

type codeExecGoWorkEdit struct {
	Use     []codeExecGoWorkUse
	Replace []codeExecGoReplace
}

type codeExecGoModEdit struct {
	Module  codeExecGoModuleRef
	Require []codeExecGoModuleRef
	Replace []codeExecGoReplace
}

type codeExecStageModule struct {
	Source          string
	Dest            string
	Mod             codeExecGoModEdit
	WorkspaceMember bool
}

const codeExecTraversalReadBatch = 128

type codeExecTraversalLimits struct {
	MaxFiles       int
	MaxDirectories int
	MaxEntries     int
	MaxDepth       int
	MaxPathBytes   int
	MaxTotalBytes  int64
}

type codeExecTraversalBudget struct {
	Limits       codeExecTraversalLimits
	Files        int
	Directories  int
	Entries      int
	TotalBytes   int64
	ReadObserver func()
}

func defaultCodeExecTraversalLimits(maxTotalBytes int64) codeExecTraversalLimits {
	return codeExecTraversalLimits{
		MaxFiles:       10_000,
		MaxDirectories: 2_048,
		MaxEntries:     12_000,
		MaxDepth:       64,
		MaxPathBytes:   4_096,
		MaxTotalBytes:  maxTotalBytes,
	}
}

func newCodeExecTraversalBudget(maxTotalBytes int64, observer func()) *codeExecTraversalBudget {
	return &codeExecTraversalBudget{
		Limits:       defaultCodeExecTraversalLimits(maxTotalBytes),
		ReadObserver: observer,
	}
}

func (b *codeExecTraversalBudget) observeRead() {
	if b != nil && b.ReadObserver != nil {
		b.ReadObserver()
	}
}

func (b *codeExecTraversalBudget) addEntries(count int) error {
	if b == nil || count < 0 {
		return errors.New("invalid traversal entry count")
	}
	if b.Limits.MaxEntries > 0 && count > b.Limits.MaxEntries-b.Entries {
		return errors.New("traversal entry limit exceeded")
	}
	b.Entries += count
	return nil
}

func (b *codeExecTraversalBudget) addDirectory(path string, depth int) error {
	if err := b.validatePath(path, depth); err != nil {
		return err
	}
	if b.Limits.MaxDirectories > 0 && b.Directories >= b.Limits.MaxDirectories {
		return errors.New("traversal directory limit exceeded")
	}
	b.Directories++
	return nil
}

func (b *codeExecTraversalBudget) addFile(path string, depth int, size int64) error {
	if err := b.validatePath(path, depth); err != nil {
		return err
	}
	if size < 0 {
		return errors.New("traversal encountered a negative file size")
	}
	if b.Limits.MaxFiles > 0 && b.Files >= b.Limits.MaxFiles {
		return errors.New("traversal file limit exceeded")
	}
	if b.Limits.MaxTotalBytes > 0 && size > b.Limits.MaxTotalBytes-b.TotalBytes {
		return errors.New("traversal total byte limit exceeded")
	}
	b.Files++
	b.TotalBytes += size
	return nil
}

func (b *codeExecTraversalBudget) validatePath(path string, depth int) error {
	if b == nil {
		return errors.New("traversal budget is required")
	}
	if depth < 0 || b.Limits.MaxDepth > 0 && depth > b.Limits.MaxDepth {
		return errors.New("traversal depth limit exceeded")
	}
	if b.Limits.MaxPathBytes > 0 && len(path) > b.Limits.MaxPathBytes {
		return errors.New("traversal path length limit exceeded")
	}
	return nil
}

type codeExecStageCopyBudget struct {
	Used      int64
	Max       int64
	Traversal *codeExecTraversalBudget
}

func (b *codeExecStageCopyBudget) traversalBudget() *codeExecTraversalBudget {
	if b.Traversal == nil {
		b.Traversal = newCodeExecTraversalBudget(b.Max, nil)
	}
	return b.Traversal
}

func stageCodeExecProject(
	ctx context.Context,
	hostProjectRoot string,
	stageRoot string,
	plan codeExecExecutionPlan,
	broker *FileAccessBroker,
	cfg sandbox.Config,
) (_ string, _ string, _ bool, returnErr error) {
	if ctx == nil {
		return "", "", false, errors.New("project dependency closure: context is required")
	}
	applicationBudget := codeExecApplicationBudgetFor(cfg)
	resolvedProjectRoot, rootErr := resolveCodeExecBoundaryPath(hostProjectRoot)
	if rootErr != nil {
		return "", "", false, errors.New("project dependency closure: project root boundary is invalid")
	}
	hostProjectRoot = resolvedProjectRoot
	resolvedStageRoot, stageRootErr := resolveCodeExecBoundaryPath(stageRoot)
	if stageRootErr != nil {
		return "", "", false, errors.New("project dependency closure: staged root boundary is invalid")
	}
	stageRoot = resolvedStageRoot
	if codeExecStagePathDenied(hostProjectRoot, cfg.DeniedPaths) {
		return "", "", false, fmt.Errorf("project dependency closure: project root is denied")
	}
	if err := os.MkdirAll(stageRoot, 0755); err != nil {
		return "", "", false, fmt.Errorf("project dependency closure: create staged root: %w", err)
	}
	stageTemp := filepath.Join(stageRoot, ".go-tmp")
	if err := os.MkdirAll(stageTemp, 0755); err != nil {
		return "", "", false, fmt.Errorf("project dependency closure: create staging temp: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(stageTemp); cleanupErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("remove Go staging temp directory: %w", cleanupErr))
		}
	}()

	if !plan.GoRuntime {
		dest := filepath.Join(stageRoot, "project")
		budget := &codeExecStageCopyBudget{Max: applicationBudget.MaxWorkspaceBytes}
		if err := copyCodeExecStageTree(ctx, hostProjectRoot, dest, cfg.DeniedPaths, budget); err != nil {
			return "", "", false, err
		}
		return dest, "", false, nil
	}
	stageCtx, cancel := context.WithTimeout(ctx, codeExecGoStageHardTimeout)
	defer cancel()

	hostGoWork, discoveryErr := discoverCodeExecHostGoWork(hostProjectRoot)
	if discoveryErr != nil {
		return "", "", false, discoveryErr
	}
	if hostGoWork != "" && broker != nil {
		if err := authorizeCodeExecPath(broker, cfg.Workspace, hostGoWork); err != nil {
			// 未授权的 go.work 不是项目输入，无论来自环境还是祖先目录都按独立模块执行。
			hostGoWork = ""
		}
	}
	hostModCache := hostGoModCachePath()
	goStage, runnerErr := newCodeExecGoStageRunner(stageCtx, cfg, stageRoot, hostModCache, stageTemp, plan)
	if runnerErr != nil {
		return "", "", false, runnerErr
	}
	var workEdit codeExecGoWorkEdit
	if hostGoWork != "" {
		var readErr error
		workEdit, readErr = readCodeExecGoWorkEdit(hostGoWork, goStage)
		if readErr != nil {
			return "", "", false, readErr
		}
		member, memberErr := codeExecGoWorkContainsProject(hostGoWork, workEdit, hostProjectRoot)
		if memberErr != nil {
			return "", "", false, memberErr
		}
		if !member {
			hostGoWork = ""
			workEdit = codeExecGoWorkEdit{}
		}
	}

	var sources []string
	workspaceMembers := map[string]bool{}
	sourceIndex := map[string]int{}
	addSource := func(raw string) (string, error) {
		source, err := resolveCodeExecStageSource("", raw)
		if err != nil {
			return "", err
		}
		if codeExecStagePathDenied(source, cfg.DeniedPaths) {
			return "", fmt.Errorf("project dependency closure: local module is denied: %s", filepath.Base(source))
		}
		if broker != nil {
			if err := authorizeCodeExecPath(broker, cfg.Workspace, source); err != nil {
				return "", errors.New("project dependency closure: local module authorization denied")
			}
		}
		if _, ok := sourceIndex[source]; !ok {
			sourceIndex[source] = len(sources)
			sources = append(sources, source)
		}
		return source, nil
	}

	workDir := ""
	if hostGoWork != "" {
		workDir = filepath.Dir(hostGoWork)
		for _, use := range workEdit.Use {
			source, err := resolveCodeExecStageSource(workDir, use.DiskPath)
			if err != nil {
				return "", "", false, err
			}
			source, err = addSource(source)
			if err != nil {
				return "", "", false, err
			}
			workspaceMembers[source] = true
		}
		for _, replace := range workEdit.Replace {
			if source, local, err := codeExecLocalReplacementSource(workDir, replace); err != nil {
				return "", "", false, err
			} else if local {
				if _, err := addSource(source); err != nil {
					return "", "", false, err
				}
			}
		}
	}

	commandSource := ""
	for _, source := range sources {
		if pathWithinResolved(source, hostProjectRoot) &&
			(commandSource == "" || len(source) > len(commandSource)) {
			commandSource = source
		}
	}
	if commandSource == "" {
		var addErr error
		commandSource, addErr = addSource(hostProjectRoot)
		if addErr != nil {
			return "", "", false, addErr
		}
	}

	moduleEdits := map[string]codeExecGoModEdit{}
	for i := 0; i < len(sources); i++ {
		source := sources[i]
		modFile := filepath.Join(source, "go.mod")
		if !fileExists(modFile) {
			continue
		}
		modEdit, err := readCodeExecGoModEdit(modFile, goStage)
		if err != nil {
			return "", "", false, err
		}
		moduleEdits[source] = modEdit
		for _, replace := range modEdit.Replace {
			dependency, local, err := codeExecLocalReplacementSource(source, replace)
			if err != nil {
				return "", "", false, err
			}
			if local {
				if _, err := addSource(dependency); err != nil {
					return "", "", false, err
				}
			}
		}
	}
	selectedSources, selectionErr := selectCodeExecGoStageSources(sources, commandSource, moduleEdits, workEdit, workDir)
	if selectionErr != nil {
		return "", "", false, selectionErr
	}
	sources = selectedSources
	destBySource := make(map[string]string, len(sources))
	modules := make([]codeExecStageModule, 0, len(sources))
	for _, source := range sources {
		dest := filepath.Join(stageRoot, "modules", codeExecStagedModuleDirName(source))
		if source == commandSource {
			dest = filepath.Join(stageRoot, "project")
		}
		destBySource[source] = dest
		modules = append(modules, codeExecStageModule{
			Source:          source,
			Dest:            dest,
			Mod:             moduleEdits[source],
			WorkspaceMember: workspaceMembers[source],
		})
	}

	budget := &codeExecStageCopyBudget{Max: applicationBudget.MaxWorkspaceBytes}
	for _, module := range modules {
		if copyErr := copyCodeExecStageTree(stageCtx, module.Source, module.Dest, cfg.DeniedPaths, budget); copyErr != nil {
			return "", "", false, copyErr
		}
	}

	commandRel, relativeErr := filepath.Rel(commandSource, hostProjectRoot)
	if relativeErr != nil || commandRel == ".." || strings.HasPrefix(commandRel, ".."+string(filepath.Separator)) {
		return "", "", false, fmt.Errorf("project dependency closure: project root escaped command module")
	}
	stagedProjectRoot := filepath.Join(destBySource[commandSource], commandRel)
	if !pathWithinResolved(stageRoot, stagedProjectRoot) {
		return "", "", false, fmt.Errorf("project dependency closure: staged project root escaped run")
	}

	for _, module := range modules {
		if err := rewriteCodeExecStagedGoMod(module, destBySource, goStage); err != nil {
			return "", "", false, err
		}
	}

	stagedGoWork := ""
	if hostGoWork != "" {
		stagedGoWork = filepath.Join(stageRoot, "go.work")
		if err := writeCodeExecStagedGoWork(
			hostGoWork,
			stagedGoWork,
			workEdit,
			destBySource,
			goStage,
		); err != nil {
			return "", "", false, err
		}
	}
	if validationErr := validateCodeExecStagedLocalModuleDirectRequires(
		modules,
		stagedProjectRoot,
		stagedGoWork,
		goStage,
	); validationErr != nil {
		return "", "", false, validationErr
	}

	vendored, vendorErr := vendorCodeExecStagedGoProject(
		stageRoot,
		stagedProjectRoot,
		stagedGoWork,
		goStage,
	)
	if vendorErr != nil {
		return "", "", false, vendorErr
	}
	if err := ensureCodeExecStagedMetadataHasNoHostPaths(
		stageCtx,
		stageRoot,
		stagedGoWork,
		modules,
		append(append([]string(nil), sources...), workDir),
		applicationBudget.MaxWorkspaceBytes,
	); err != nil {
		return "", "", false, err
	}
	if size, err := codeExecGoVendorDirSizeContext(stageCtx, stageRoot); err != nil {
		return "", "", false, fmt.Errorf("project dependency closure: measure staged workspace: %w", err)
	} else if size > applicationBudget.MaxWorkspaceBytes {
		return "", "", false, fmt.Errorf(
			"project dependency closure exceeds max workspace bytes after vendoring: %d > %d",
			size,
			applicationBudget.MaxWorkspaceBytes,
		)
	}
	return stagedProjectRoot, stagedGoWork, vendored, nil
}

// selectCodeExecGoStageSources 只保留命令模块及其本地 require/replace 传递闭包，
// 避免把同一 go.work 中与本次命令无关的大型仓库复制进沙箱。
func selectCodeExecGoStageSources(
	sources []string,
	commandSource string,
	moduleEdits map[string]codeExecGoModEdit,
	workEdit codeExecGoWorkEdit,
	workDir string,
) ([]string, error) {
	if commandSource == "" {
		return nil, errors.New("project dependency closure: command module is unavailable")
	}
	moduleSources := make(map[string][]string)
	for _, source := range sources {
		modulePath := strings.TrimSpace(moduleEdits[source].Module.Path)
		if modulePath != "" {
			moduleSources[modulePath] = append(moduleSources[modulePath], source)
		}
	}
	workReplacements := make(map[string]string)
	for _, replace := range workEdit.Replace {
		source, local, replaceErr := codeExecLocalReplacementSource(workDir, replace)
		if replaceErr != nil {
			return nil, replaceErr
		}
		if local {
			workReplacements[codeExecGoReplacementKey(replace.Old)] = source
			workReplacements[codeExecGoReplacementKey(codeExecGoModuleRef{Path: replace.Old.Path})] = source
		}
	}

	selected := map[string]bool{commandSource: true}
	queue := []string{commandSource}
	add := func(source string) {
		if source == "" || selected[source] {
			return
		}
		selected[source] = true
		queue = append(queue, source)
	}
	for len(queue) > 0 {
		source := queue[0]
		queue = queue[1:]
		edit := moduleEdits[source]
		for _, replace := range edit.Replace {
			replacement, local, replaceErr := codeExecLocalReplacementSource(source, replace)
			if replaceErr != nil {
				return nil, replaceErr
			}
			if local {
				add(replacement)
			}
		}
		for _, requirement := range edit.Require {
			if replacement := firstCodeExecReplacement(
				workReplacements,
				codeExecGoReplacementKey(requirement),
				codeExecGoReplacementKey(codeExecGoModuleRef{Path: requirement.Path}),
			); replacement != "" {
				add(replacement)
				continue
			}
			candidates := moduleSources[strings.TrimSpace(requirement.Path)]
			if len(candidates) > 1 {
				return nil, errors.New("project dependency closure: duplicate local module path")
			}
			if len(candidates) == 1 {
				add(candidates[0])
			}
		}
	}

	result := make([]string, 0, len(selected))
	for _, source := range sources {
		if selected[source] {
			result = append(result, source)
		}
	}
	return result, nil
}

func codeExecGoWorkContainsProject(
	hostGoWork string,
	edit codeExecGoWorkEdit,
	projectRoot string,
) (bool, error) {
	workDir := filepath.Dir(hostGoWork)
	if resolveRealPath(workDir) == resolveRealPath(projectRoot) {
		return true, nil
	}
	for _, use := range edit.Use {
		source, err := resolveCodeExecStageSource(workDir, use.DiskPath)
		if err != nil {
			return false, err
		}
		if pathWithinResolved(source, projectRoot) {
			return true, nil
		}
	}
	return false, nil
}

func discoverCodeExecHostGoWork(projectRoot string) (string, error) {
	// 宿主环境不属于项目输入；只从已授权项目根向上发现受控 go.work。
	for dir := filepath.Clean(projectRoot); ; dir = filepath.Dir(dir) {
		workFile := filepath.Join(dir, "go.work")
		if fileExists(workFile) {
			return resolveRealPath(workFile), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
	}
}

func readCodeExecGoWorkEdit(path string, runner *codeExecGoStageRunner) (codeExecGoWorkEdit, error) {
	out, err := runner.inspectMetadata(
		path,
		"off",
		"work", "edit", "-json", path,
	)
	if err != nil {
		return codeExecGoWorkEdit{}, err
	}
	var edit codeExecGoWorkEdit
	if err := json.Unmarshal(out, &edit); err != nil {
		return codeExecGoWorkEdit{}, fmt.Errorf("project dependency closure: decode go.work: %w", err)
	}
	return edit, nil
}

func readCodeExecGoModEdit(path string, runner *codeExecGoStageRunner) (codeExecGoModEdit, error) {
	out, err := runner.inspectMetadata(
		path,
		"off",
		"mod", "edit", "-json", path,
	)
	if err != nil {
		return codeExecGoModEdit{}, err
	}
	var edit codeExecGoModEdit
	if err := json.Unmarshal(out, &edit); err != nil {
		return codeExecGoModEdit{}, fmt.Errorf("project dependency closure: decode go.mod: %w", err)
	}
	return edit, nil
}

type codeExecGoListModule struct {
	Path string
}

type codeExecGoListPackage struct {
	ImportPath   string
	Standard     bool
	Module       *codeExecGoListModule
	Imports      []string
	TestImports  []string
	XTestImports []string
}

type codeExecGoImportEdge struct {
	ImporterModule string
	ImportPath     string
}

func validateCodeExecStagedLocalModuleDirectRequires(
	modules []codeExecStageModule,
	stagedProjectRoot string,
	stagedGoWork string,
	runner *codeExecGoStageRunner,
) error {
	localModules := make(map[string]codeExecStageModule, len(modules))
	for _, module := range modules {
		modulePath := strings.TrimSpace(module.Mod.Module.Path)
		if modulePath != "" {
			localModules[modulePath] = module
		}
	}
	if len(localModules) < 2 {
		return nil
	}

	// 只检查命令模块的激活依赖图；工作区根目录则检查各 use 成员。
	// 仅替换模块不会单独成为分析根，避免未激活包覆盖同名激活包。
	var roots []codeExecStageModule
	commandModule := -1
	for i := range modules {
		modulePath := strings.TrimSpace(modules[i].Mod.Module.Path)
		if modulePath == "" || !pathWithinResolved(modules[i].Dest, stagedProjectRoot) {
			continue
		}
		if commandModule < 0 || len(modules[i].Dest) > len(modules[commandModule].Dest) {
			commandModule = i
		}
	}
	if commandModule >= 0 {
		roots = append(roots, modules[commandModule])
	} else if stagedGoWork != "" {
		for _, module := range modules {
			if module.WorkspaceMember && strings.TrimSpace(module.Mod.Module.Path) != "" {
				roots = append(roots, module)
			}
		}
	}

	for _, module := range roots {
		goWork := "off"
		if stagedGoWork != "" && module.WorkspaceMember {
			goWork = stagedGoWork
		}
		out, err := runner.Run(
			module.Dest,
			goWork,
			"list",
			"-deps",
			"-test",
			"-mod=readonly",
			"-json=ImportPath,Standard,Module,Imports,TestImports,XTestImports",
			"./...",
		)
		if err != nil {
			return err
		}
		packages := make(map[string]codeExecGoListPackage)
		var edges []codeExecGoImportEdge
		decoder := json.NewDecoder(strings.NewReader(string(out)))
		for {
			var pkg codeExecGoListPackage
			if err := decoder.Decode(&pkg); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return fmt.Errorf("project dependency closure: decode go list: %w", err)
			}
			packages[pkg.ImportPath] = pkg
			if pkg.Module == nil || strings.TrimSpace(pkg.Module.Path) == "" {
				continue
			}
			for _, importPath := range append(
				append(append([]string(nil), pkg.Imports...), pkg.TestImports...),
				pkg.XTestImports...,
			) {
				edges = append(edges, codeExecGoImportEdge{
					ImporterModule: pkg.Module.Path,
					ImportPath:     importPath,
				})
			}
		}

		for _, edge := range edges {
			importer, local := localModules[edge.ImporterModule]
			if !local {
				continue
			}
			dependency, ok := packages[edge.ImportPath]
			if !ok || dependency.Standard || dependency.Module == nil {
				continue
			}
			dependencyModule := strings.TrimSpace(dependency.Module.Path)
			if dependencyModule == "" || dependencyModule == edge.ImporterModule {
				continue
			}
			if _, local = localModules[dependencyModule]; !local {
				continue
			}
			direct := false
			for _, requirement := range importer.Mod.Require {
				if !requirement.Indirect && requirement.Path == dependencyModule {
					direct = true
					break
				}
			}
			if !direct {
				return fmt.Errorf(
					"project dependency closure: module %s imports local module %s without a direct require",
					edge.ImporterModule,
					dependencyModule,
				)
			}
		}
	}
	return nil
}

func resolveCodeExecStageSource(base, raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", fmt.Errorf("project dependency closure: empty local module path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, filepath.FromSlash(path))
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("project dependency closure: stat local module: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("project dependency closure: local module is not a directory")
	}
	return resolveRealPath(path), nil
}

func codeExecLocalReplacementSource(base string, replace codeExecGoReplace) (string, bool, error) {
	if strings.TrimSpace(replace.New.Path) == "" || strings.TrimSpace(replace.New.Version) != "" {
		return "", false, nil
	}
	source, err := resolveCodeExecStageSource(base, replace.New.Path)
	if err != nil {
		return "", false, err
	}
	return source, true, nil
}

func rewriteCodeExecStagedGoMod(
	module codeExecStageModule,
	destBySource map[string]string,
	runner *codeExecGoStageRunner,
) error {
	modFile := filepath.Join(module.Dest, "go.mod")
	if !fileExists(modFile) || len(module.Mod.Replace) == 0 {
		return nil
	}
	args := []string{"mod", "edit"}
	for _, replace := range module.Mod.Replace {
		source, local, err := codeExecLocalReplacementSource(module.Source, replace)
		if err != nil {
			return err
		}
		if !local {
			continue
		}
		dest, ok := destBySource[source]
		if !ok {
			return fmt.Errorf("project dependency closure: local replace target was not staged")
		}
		rel, err := filepath.Rel(module.Dest, dest)
		if err != nil {
			return fmt.Errorf("project dependency closure: rewrite local replace: %w", err)
		}
		args = append(args, "-replace="+codeExecGoModuleRefText(replace.Old)+"="+codeExecGoRelativePath(rel))
	}
	if len(args) == 2 {
		return nil
	}
	args = append(args, modFile)
	_, err := runner.Run(module.Dest, "off", args...)
	return err
}

func writeCodeExecStagedGoWork(
	hostPath string,
	stagedPath string,
	edit codeExecGoWorkEdit,
	destBySource map[string]string,
	runner *codeExecGoStageRunner,
) error {
	data, err := os.ReadFile(hostPath)
	if err != nil {
		return fmt.Errorf("project dependency closure: read go.work: %w", err)
	}
	if err := os.WriteFile(stagedPath, data, 0644); err != nil {
		return fmt.Errorf("project dependency closure: write staged go.work: %w", err)
	}
	if sum, err := os.ReadFile(hostPath + ".sum"); err == nil {
		if err := os.WriteFile(stagedPath+".sum", sum, 0644); err != nil {
			return fmt.Errorf("project dependency closure: write staged go.work.sum: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("project dependency closure: read go.work.sum: %w", err)
	}

	hostDir := filepath.Dir(hostPath)
	stageDir := filepath.Dir(stagedPath)
	args := []string{"work", "edit"}
	for _, use := range edit.Use {
		args = append(args, "-dropuse="+use.DiskPath)
	}
	for _, use := range edit.Use {
		source, err := resolveCodeExecStageSource(hostDir, use.DiskPath)
		if err != nil {
			return err
		}
		dest, ok := destBySource[source]
		if !ok {
			continue
		}
		rel, err := filepath.Rel(stageDir, dest)
		if err != nil {
			return fmt.Errorf("project dependency closure: rewrite go.work use: %w", err)
		}
		args = append(args, "-use="+codeExecGoRelativePath(rel))
	}
	for _, replace := range edit.Replace {
		source, local, err := codeExecLocalReplacementSource(hostDir, replace)
		if err != nil {
			return err
		}
		if !local {
			continue
		}
		old := codeExecGoModuleRefText(replace.Old)
		args = append(args, "-dropreplace="+old)
		dest, ok := destBySource[source]
		if !ok {
			continue
		}
		rel, err := filepath.Rel(stageDir, dest)
		if err != nil {
			return fmt.Errorf("project dependency closure: rewrite go.work replace: %w", err)
		}
		args = append(args, "-replace="+old+"="+codeExecGoRelativePath(rel))
	}
	args = append(args, stagedPath)
	_, err = runner.Run(stageDir, "off", args...)
	return err
}

func vendorCodeExecStagedGoProject(
	stageRoot string,
	projectRoot string,
	goWorkPath string,
	runner *codeExecGoStageRunner,
) (bool, error) {
	var (
		dir    string
		gowork string
		args   []string
		vendor string
	)
	if goWorkPath != "" {
		dir = stageRoot
		gowork = goWorkPath
		args = []string{"work", "vendor"}
		vendor = filepath.Join(stageRoot, "vendor")
	} else if fileExists(filepath.Join(projectRoot, "go.mod")) {
		dir = projectRoot
		gowork = "off"
		args = []string{"mod", "vendor"}
		vendor = filepath.Join(projectRoot, "vendor")
	} else {
		return false, nil
	}
	if _, err := runner.RunVendor(dir, gowork, vendor, args...); err != nil {
		return false, err
	}
	return fileExists(vendor), nil
}

type codeExecGoClosureInspection struct {
	Modules         []codeExecStageModule
	NeedsProjection bool
	NeedsHostCache  bool
}

// prepareCodeExecRunGoDependencyClosure 仅让受信任辅助过程读取必要的宿主模块缓存，
// 最终进程只能看到本次运行目录中的 vendor 闭包和私有空缓存。
func prepareCodeExecRunGoDependencyClosure(ctx context.Context, run *codeExecRun) (returnErr error) {
	if run == nil || !run.Plan.GoRuntime {
		return nil
	}
	defer func() {
		if returnErr != nil {
			return
		}
		run.Config, returnErr = protectCodeExecHostGoCaches(run.Config, run.Scratch)
	}()
	if run.StagedProject || run.Plan.Toolchain == nil {
		return nil
	}
	modFile := filepath.Join(run.Workspace, "go.mod")
	if !fileExists(modFile) {
		return nil
	}
	tempDir := filepath.Join(run.CacheDir, "go-projection")
	if err := os.MkdirAll(tempDir, 0700); err != nil {
		return fmt.Errorf("project dependency closure: create private projection directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	inspector, err := newCodeExecGoStageRunner(
		ctx,
		run.Config,
		run.Workspace,
		"",
		tempDir,
		run.Plan,
	)
	if err != nil {
		return err
	}
	inspection, err := inspectCodeExecRunGoClosure(ctx, run.Workspace, inspector)
	if err != nil {
		return err
	}
	if !inspection.NeedsProjection {
		return nil
	}
	hostModCache := ""
	if inspection.NeedsHostCache {
		hostModCache = hostGoModCachePath()
	}
	projector, err := newCodeExecGoStageRunner(
		ctx,
		run.Config,
		run.Workspace,
		hostModCache,
		tempDir,
		run.Plan,
	)
	if err != nil {
		return err
	}
	vendored, err := vendorCodeExecStagedGoProject(
		run.Workspace,
		run.Workspace,
		"",
		projector,
	)
	if err != nil {
		return err
	}
	if err := ensureCodeExecStagedMetadataHasNoHostPaths(
		ctx,
		run.Workspace,
		"",
		inspection.Modules,
		[]string{hostModCache},
		run.applicationBudget().MaxWorkspaceBytes,
	); err != nil {
		return err
	}
	run.GoVendored = vendored
	return nil
}

func inspectCodeExecRunGoClosure(
	ctx context.Context,
	workspace string,
	runner *codeExecGoStageRunner,
) (codeExecGoClosureInspection, error) {
	inspection := codeExecGoClosureInspection{}
	queue := []string{workspace}
	visited := make(map[string]bool)
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return codeExecGoClosureInspection{}, err
		}
		moduleDir := resolveRealPath(queue[0])
		queue = queue[1:]
		if visited[moduleDir] {
			continue
		}
		visited[moduleDir] = true
		if !pathWithinResolved(workspace, moduleDir) {
			return codeExecGoClosureInspection{}, errors.New("project dependency closure: local replacement escapes the run workspace")
		}
		modFile := filepath.Join(moduleDir, "go.mod")
		if !fileExists(modFile) {
			return codeExecGoClosureInspection{}, errors.New("project dependency closure: local replacement target is missing go.mod")
		}
		edit, err := readCodeExecGoModEdit(modFile, runner)
		if err != nil {
			return codeExecGoClosureInspection{}, err
		}
		inspection.Modules = append(inspection.Modules, codeExecStageModule{
			Source: moduleDir,
			Dest:   moduleDir,
			Mod:    edit,
		})
		localReplacements := make(map[string]string)
		remoteReplacements := make(map[string]bool)
		for _, replace := range edit.Replace {
			if err := ctx.Err(); err != nil {
				return codeExecGoClosureInspection{}, err
			}
			key := codeExecGoReplacementKey(replace.Old)
			source, local, err := codeExecLocalReplacementSource(moduleDir, replace)
			if err != nil {
				return codeExecGoClosureInspection{}, err
			}
			if !local {
				remoteReplacements[key] = true
				continue
			}
			if filepath.IsAbs(filepath.FromSlash(strings.TrimSpace(replace.New.Path))) ||
				!pathWithinResolved(workspace, source) {
				return codeExecGoClosureInspection{}, errors.New("project dependency closure: local replacement escapes the run workspace")
			}
			localReplacements[key] = source
		}
		for _, requirement := range edit.Require {
			inspection.NeedsProjection = true
			keyWithVersion := codeExecGoReplacementKey(requirement)
			keyWithoutVersion := codeExecGoReplacementKey(codeExecGoModuleRef{Path: requirement.Path})
			if source := firstCodeExecReplacement(localReplacements, keyWithVersion, keyWithoutVersion); source != "" {
				queue = append(queue, source)
				continue
			}
			if remoteReplacements[keyWithVersion] || remoteReplacements[keyWithoutVersion] {
				inspection.NeedsHostCache = true
				continue
			}
			inspection.NeedsHostCache = true
		}
	}
	return inspection, nil
}

func codeExecGoReplacementKey(ref codeExecGoModuleRef) string {
	return strings.TrimSpace(ref.Path) + "\x00" + strings.TrimSpace(ref.Version)
}

func firstCodeExecReplacement(replacements map[string]string, keys ...string) string {
	for _, key := range keys {
		if replacement := replacements[key]; replacement != "" {
			return replacement
		}
	}
	return ""
}

type codeExecGoStageRunner struct {
	Context      context.Context
	Config       sandbox.Config
	Workspace    string
	HostModCache string
	TempDir      string
	Plan         codeExecExecutionPlan
}

func newCodeExecGoStageRunner(
	ctx context.Context,
	cfg sandbox.Config,
	workspace string,
	hostModCache string,
	tempDir string,
	plan codeExecExecutionPlan,
) (*codeExecGoStageRunner, error) {
	canonicalWorkspace, err := resolveCodeExecBoundaryPath(workspace)
	if err != nil {
		return nil, errors.New("project dependency closure: Go staging workspace boundary is invalid")
	}
	canonicalTempDir, err := resolveCodeExecBoundaryPath(tempDir)
	if err != nil || !isPathInside(canonicalWorkspace, canonicalTempDir) {
		return nil, errors.New("project dependency closure: Go staging temp boundary is invalid")
	}
	canonicalHostModCache := ""
	if strings.TrimSpace(hostModCache) != "" {
		canonicalHostModCache, err = canonicalCodeExecPath(hostModCache)
		if err != nil {
			return nil, errors.New("project dependency closure: host module cache boundary is invalid")
		}
	}
	cfg.Workspace = canonicalWorkspace
	if plan.stageDefaultOutput {
		cfg.MaxOutputBytes = codeExecGoHelperMaxOutputBytes
	}
	if plan.stageDefaultStderr {
		cfg.MaxStderrBytes = codeExecGoHelperMaxStderrBytes
	}
	// 依赖投影只读取已存在的宿主模块缓存，不允许辅助过程访问网络。
	cfg.Network = sandbox.NetworkDisabled
	return &codeExecGoStageRunner{
		Context:      ctx,
		Config:       cfg,
		Workspace:    canonicalWorkspace,
		HostModCache: canonicalHostModCache,
		TempDir:      canonicalTempDir,
		Plan:         plan,
	}, nil
}

func (r *codeExecGoStageRunner) Run(dir string, gowork string, args ...string) ([]byte, error) {
	return r.run(dir, gowork, []string{dir}, nil, args...)
}

// inspectMetadata 始终从私有暂存工作区启动 Go，只把已授权的宿主元数据目录只读暴露给沙箱。
// 这样宿主目录不会被提升为 Command.Dir，且 go 命令仍通过显式绝对文件参数解析元数据。
func (r *codeExecGoStageRunner) inspectMetadata(
	path string,
	gowork string,
	args ...string,
) ([]byte, error) {
	canonicalPath, err := resolveCodeExecBoundaryPath(path)
	if err != nil {
		return nil, errors.New("project dependency closure: Go metadata boundary is invalid")
	}
	return r.run(r.Workspace, gowork, []string{filepath.Dir(canonicalPath)}, nil, args...)
}

// RunVendor 只在依赖归档子进程成功退出后清理与当前工具链平台无关的 Go 源文件，
// 再按原辅助命令的硬上限复核持久状态；临时全平台闭包不得被当作用户持久输出放行。
func (r *codeExecGoStageRunner) RunVendor(
	dir string,
	gowork string,
	vendorRoot string,
	args ...string,
) ([]byte, error) {
	canonicalVendor, err := resolveCodeExecBoundaryPath(vendorRoot)
	if err != nil || !isPathInside(r.Workspace, canonicalVendor) {
		return nil, errors.New("project dependency closure: vendor boundary is invalid")
	}
	return r.run(dir, gowork, []string{dir}, func() error {
		if err := pruneCodeExecVendorForeignPlatforms(r.Context, canonicalVendor, *r.Plan.Toolchain); err != nil {
			return fmt.Errorf("normalize vendored platform sources: %w", err)
		}
		size, err := codeExecGoVendorDirSizeContext(r.Context, r.Workspace)
		if err != nil {
			return fmt.Errorf("measure vendored dependency closure: %w", err)
		}
		if size > codeExecGoHelperApplicationWorkspaceBudget(r.Config) {
			return sandbox.ErrStorageLimitExceeded
		}
		return nil
	}, args...)
}

func (r *codeExecGoStageRunner) run(
	dir string,
	gowork string,
	readablePaths []string,
	postcondition func() error,
	args ...string,
) ([]byte, error) {
	if r.Context == nil {
		return nil, errors.New("project dependency closure: Go helper context is required")
	}
	if r.Plan.Toolchain == nil {
		return nil, errors.New("project dependency closure: Go toolchain is unavailable")
	}
	canonicalDir, boundaryErr := resolveCodeExecBoundaryPath(dir)
	if boundaryErr != nil {
		return nil, errors.New("project dependency closure: Go staging working directory boundary is invalid")
	}
	dir = canonicalDir
	if verifyErr := verifyCodeExecGoToolchainDescriptor(*r.Plan.Toolchain); verifyErr != nil {
		return nil, errors.New("project dependency closure: Go toolchain binding changed")
	}
	for _, path := range []string{
		r.TempDir,
		filepath.Join(r.TempDir, "go-build"),
		filepath.Join(r.TempDir, "home"),
	} {
		if err := os.MkdirAll(path, 0755); err != nil {
			return nil, fmt.Errorf("project dependency closure: create Go staging directory: %w", err)
		}
	}

	readablePaths = append(append([]string(nil), readablePaths...), r.Plan.Toolchain.GOROOT)
	if r.HostModCache != "" && fileExists(r.HostModCache) {
		readablePaths = append(readablePaths, r.HostModCache)
	}

	exports := map[string]string{
		"APPDATA":         filepath.Join(r.TempDir, "home", "AppData", "Roaming"),
		"GOCACHE":         filepath.Join(r.TempDir, "go-build"),
		"GOENV":           "off",
		"GOFLAGS":         "",
		"GONOPROXY":       "",
		"GONOSUMDB":       "",
		"GOPROXY":         "off",
		"GOSUMDB":         "off",
		"GOTOOLCHAIN":     "local",
		"GOWORK":          gowork,
		"GOROOT":          r.Plan.Toolchain.GOROOT,
		"HOME":            filepath.Join(r.TempDir, "home"),
		"LOCALAPPDATA":    filepath.Join(r.TempDir, "home", "AppData", "Local"),
		"TMP":             r.TempDir,
		"TEMP":            r.TempDir,
		"TMPDIR":          r.TempDir,
		"XDG_CONFIG_HOME": filepath.Join(r.TempDir, "home", ".config"),
	}
	if r.HostModCache != "" {
		exports["GOMODCACHE"] = r.HostModCache
	}
	if err := ensureCodeExecGoTelemetryOff(exports); err != nil {
		return nil, fmt.Errorf("project dependency closure: %w", err)
	}
	result, err := r.Plan.Helper.runWithHardTimeout(
		r.Context,
		r.Config,
		r.Workspace,
		dir,
		readablePaths,
		r.Plan.Toolchain.Binary,
		args,
		exports,
		codeExecGoStageHardTimeout,
	)
	completed := result != nil && result.ExitCode == 0 &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	if completed && postcondition != nil {
		if postErr := postcondition(); postErr != nil {
			if errors.Is(postErr, sandbox.ErrStorageLimitExceeded) {
				return nil, codeExecGoStageError(args, result, postErr)
			}
			return nil, fmt.Errorf(
				"project dependency closure: %s: %s",
				codeExecGoStageOperation(args),
				codeExecGoStagePostconditionCategory(postErr),
			)
		}
		if errors.Is(err, sandbox.ErrStorageLimitExceeded) {
			err = nil
		}
	}
	if !codeExecGoHelperResultSucceeded(result, err) {
		return nil, codeExecGoStageError(args, result, err)
	}
	return []byte(result.Stdout), nil
}

func codeExecGoStagePostconditionCategory(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "postcondition timed out"
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "traversal file limit exceeded"):
		return "vendor file limit exceeded"
	case strings.Contains(message, "traversal directory limit exceeded"):
		return "vendor directory limit exceeded"
	case strings.Contains(message, "traversal entry limit exceeded"):
		return "vendor entry limit exceeded"
	case strings.Contains(message, "workspace changed while measuring"):
		return "vendor workspace changed during measurement"
	case strings.HasPrefix(message, "normalize vendored platform sources:"):
		return "vendor normalization failed"
	case strings.HasPrefix(message, "measure vendored dependency closure:"):
		return "vendor measurement failed"
	default:
		return "postcondition failed"
	}
}

// pruneCodeExecVendorForeignPlatforms 仅删除由 Go 文件名明确绑定到其他 GOOS/GOARCH
// 的源码；通用文件和自定义构建标签文件全部保留，避免改变请求的 -tags 语义。
func pruneCodeExecVendorForeignPlatforms(
	ctx context.Context,
	vendorRoot string,
	toolchain codeExecGoToolchainDescriptor,
) error {
	if ctx == nil {
		return errors.New("vendor pruning context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(vendorRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return errors.New("vendor root is not a regular directory")
	}
	return filepath.WalkDir(vendorRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if path == vendorRoot || entry.IsDir() {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("vendor entry is not a regular file")
		}
		if !codeExecForeignPlatformGoFile(entry.Name(), toolchain.GOOS, toolchain.GOARCH) {
			return nil
		}
		return removeCodeExecVendorFileNoFollow(path, info)
	})
}

func codeExecForeignPlatformGoFile(name, targetOS, targetArch string) bool {
	if filepath.Ext(name) != ".go" {
		return false
	}
	stem := strings.TrimSuffix(name, ".go")
	stem = strings.TrimSuffix(stem, "_test")
	parts := strings.Split(stem, "_")
	if len(parts) < 2 {
		return false
	}
	last := parts[len(parts)-1]
	if codeExecKnownGOARCH(last) {
		if last != targetArch {
			return true
		}
		if len(parts) > 2 {
			candidateOS := parts[len(parts)-2]
			return codeExecKnownGOOS(candidateOS) && !codeExecGOOSMatches(targetOS, candidateOS)
		}
		return false
	}
	return codeExecKnownGOOS(last) && !codeExecGOOSMatches(targetOS, last)
}

func codeExecKnownGOOS(value string) bool {
	switch value {
	case "aix", "android", "darwin", "dragonfly", "freebsd", "hurd", "illumos", "ios",
		"js", "linux", "nacl", "netbsd", "openbsd", "plan9", "solaris", "wasip1",
		"windows", "zos":
		return true
	default:
		return false
	}
}

func codeExecKnownGOARCH(value string) bool {
	switch value {
	case "386", "amd64", "amd64p32", "arm", "arm64", "loong64", "mips", "mipsle",
		"mips64", "mips64le", "mips64p32", "mips64p32le", "ppc64", "ppc64le",
		"riscv64", "s390x", "sparc64", "wasm":
		return true
	default:
		return false
	}
}

func codeExecGOOSMatches(target, candidate string) bool {
	if target == candidate {
		return true
	}
	switch target {
	case "android":
		return candidate == "linux"
	case "illumos":
		return candidate == "solaris"
	case "ios":
		return candidate == "darwin"
	default:
		return false
	}
}

func removeCodeExecVendorFileNoFollow(path string, before os.FileInfo) (returnErr error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer joinCodeExecResourceClose(&returnErr, root, "close vendor directory")
	name := filepath.Base(path)
	pathInfo, err := root.Lstat(name)
	if err != nil || pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() ||
		!os.SameFile(before, pathInfo) || codeExecFileLinkCount(pathInfo) != 1 {
		return errors.New("vendor file changed before removal")
	}
	file, err := root.Open(name)
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !opened.Mode().IsRegular() ||
		!os.SameFile(pathInfo, opened) || codeExecFileLinkCount(opened) != 1 {
		return errors.New("vendor file changed while opening")
	}
	postPath, err := root.Lstat(name)
	if err != nil || postPath.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, postPath) ||
		codeExecFileLinkCount(postPath) != 1 {
		return errors.New("vendor file changed before removal")
	}
	return root.Remove(name)
}

func codeExecGoStageError(args []string, result *sandbox.ExecResult, cause error) error {
	operation := codeExecGoStageOperation(args)
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("project dependency closure: %s: %w", operation, cause)
	}
	if result != nil && result.ExitCode != 0 {
		return fmt.Errorf("project dependency closure: %s: exit status %d", operation, result.ExitCode)
	}
	if errors.Is(cause, sandbox.ErrStorageLimitExceeded) {
		return fmt.Errorf("project dependency closure: %s: storage limit exceeded", operation)
	}
	return fmt.Errorf("project dependency closure: %s: sandbox execution failed", operation)
}

func codeExecGoStageOperation(args []string) string {
	operation := []string{"go"}
	for _, arg := range args {
		arg = strings.ToLower(strings.TrimSpace(arg))
		if arg == "" || strings.HasPrefix(arg, "-") {
			continue
		}
		switch arg {
		case "mod", "work", "list", "edit", "vendor":
			operation = append(operation, arg)
			if len(operation) == 3 || arg == "list" || arg == "vendor" {
				return strings.Join(operation, " ")
			}
		default:
			return strings.Join(operation, " ")
		}
	}
	return strings.Join(operation, " ")
}

func openCodeExecDirectoryStream(root *os.Root) (*os.File, os.FileInfo, error) {
	before, err := root.Stat(".")
	if err != nil {
		return nil, nil, err
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, nil, err
	}
	opened, err := directory.Stat()
	if err != nil || !opened.IsDir() || !sameCodeExecFileSnapshot(before, opened) {
		_ = directory.Close()
		return nil, nil, errors.New("directory changed while opening traversal stream")
	}
	return directory, opened, nil
}

func readCodeExecDirectoryBatch(
	ctx context.Context,
	directory *os.File,
	budget *codeExecTraversalBudget,
) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	budget.observeRead()
	entries, err := directory.ReadDir(codeExecTraversalReadBatch)
	if addErr := budget.addEntries(len(entries)); addErr != nil {
		return nil, addErr
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return entries, err
}

func verifyCodeExecDirectoryStream(root *os.Root, directory *os.File, opened os.FileInfo) error {
	after, afterErr := directory.Stat()
	postPath, pathErr := root.Stat(".")
	if afterErr != nil || pathErr != nil || !postPath.IsDir() ||
		!sameCodeExecFileSnapshot(opened, after) || !sameCodeExecFileSnapshot(after, postPath) {
		return errors.New("directory changed while traversing")
	}
	return nil
}

func copyCodeExecStageTree(
	ctx context.Context,
	sourceRoot string,
	destRoot string,
	deniedPaths []string,
	budget *codeExecStageCopyBudget,
) (returnErr error) {
	if ctx == nil {
		return errors.New("project dependency closure: context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	absoluteSourceRoot, resolveErr := filepath.Abs(sourceRoot)
	if resolveErr != nil {
		return fmt.Errorf("project dependency closure: resolve source root: %w", resolveErr)
	}
	sourceRoot = filepath.Clean(absoluteSourceRoot)
	if codeExecStagePathDenied(sourceRoot, deniedPaths) {
		return fmt.Errorf("project dependency closure: source root is denied")
	}
	source, sourceInfo, sourceErr := openCodeExecRootNoFollow(sourceRoot)
	if sourceErr != nil {
		return fmt.Errorf("project dependency closure: open source root: %w", sourceErr)
	}
	defer joinCodeExecResourceClose(&returnErr, source, "close project source root")
	if mkdirErr := os.MkdirAll(destRoot, 0700); mkdirErr != nil {
		return fmt.Errorf("project dependency closure: create staged root: %w", mkdirErr)
	}
	destination, _, destinationErr := openCodeExecRootNoFollow(destRoot)
	if destinationErr != nil {
		return fmt.Errorf("project dependency closure: open staged root: %w", destinationErr)
	}
	defer joinCodeExecResourceClose(&returnErr, destination, "close staged project root")
	if budget == nil {
		budget = &codeExecStageCopyBudget{}
	}
	traversal := budget.traversalBudget()
	if traversalErr := traversal.addDirectory("", 0); traversalErr != nil {
		return fmt.Errorf("project dependency closure: %w", traversalErr)
	}
	skipDirs := map[string]bool{
		".git":   true,
		".hg":    true,
		".svn":   true,
		"bin":    true,
		"dist":   true,
		"vendor": true,
	}
	if copyErr := copyCodeExecStageDirectory(
		ctx,
		source,
		destination,
		sourceRoot,
		"",
		deniedPaths,
		skipDirs,
		budget,
		0,
	); copyErr != nil {
		return copyErr
	}
	afterRoot, rootStatErr := source.Stat(".")
	postRoot, pathErr := os.Lstat(sourceRoot)
	if rootStatErr != nil || pathErr != nil || postRoot.Mode()&os.ModeSymlink != 0 || !postRoot.IsDir() ||
		!sameCodeExecFileSnapshot(sourceInfo, afterRoot) || !sameCodeExecFileSnapshot(afterRoot, postRoot) {
		return errors.New("project dependency closure: source root changed while staging")
	}
	return ctx.Err()
}

func copyCodeExecStageDirectory(
	ctx context.Context,
	source *os.Root,
	destination *os.Root,
	sourceRoot string,
	prefix string,
	deniedPaths []string,
	skipDirs map[string]bool,
	budget *codeExecStageCopyBudget,
	depth int,
) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, openedDirectory, err := openCodeExecDirectoryStream(source)
	if err != nil {
		return fmt.Errorf("project dependency closure: read source directory: %w", err)
	}
	defer joinCodeExecResourceClose(&returnErr, directory, "close project source directory")
	traversal := budget.traversalBudget()
	for {
		entries, readErr := readCodeExecDirectoryBatch(ctx, directory, traversal)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return fmt.Errorf("project dependency closure: read source directory: %w", readErr)
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			stagePath := name
			if prefix != "" {
				stagePath = prefix + "/" + name
			}
			entryDepth := depth + 1
			if err := traversal.validatePath(stagePath, entryDepth); err != nil {
				return fmt.Errorf("project dependency closure: %w", err)
			}
			before, entryErr := source.Lstat(name)
			if entryErr != nil {
				return fmt.Errorf("project dependency closure: stat source %s: %w", stagePath, entryErr)
			}
			if before.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("project dependency closure: symlink source is not allowed: %s", stagePath)
			}
			fullSourcePath := filepath.Join(sourceRoot, filepath.FromSlash(stagePath))
			if codeExecStagePathDenied(fullSourcePath, deniedPaths) {
				continue
			}
			if before.IsDir() {
				if err := traversal.addDirectory(stagePath, entryDepth); err != nil {
					return fmt.Errorf("project dependency closure: %w", err)
				}
				if skipDirs[name] {
					continue
				}
				childSource, sourceOpenErr := source.OpenRoot(name)
				if sourceOpenErr != nil {
					return fmt.Errorf("project dependency closure: open source directory %s: %w", stagePath, sourceOpenErr)
				}
				opened, statErr := childSource.Stat(".")
				if statErr != nil || !sameCodeExecFileSnapshot(before, opened) {
					_ = childSource.Close()
					return fmt.Errorf("project dependency closure: source directory %s changed while opening", stagePath)
				}
				if mkdirErr := destination.Mkdir(name, before.Mode().Perm()|0700); mkdirErr != nil {
					_ = childSource.Close()
					return fmt.Errorf("project dependency closure: create staged directory %s: %w", stagePath, mkdirErr)
				}
				childDestination, destinationOpenErr := destination.OpenRoot(name)
				if destinationOpenErr != nil {
					_ = childSource.Close()
					return fmt.Errorf("project dependency closure: open staged directory %s: %w", stagePath, destinationOpenErr)
				}
				walkErr := copyCodeExecStageDirectory(
					ctx,
					childSource,
					childDestination,
					sourceRoot,
					stagePath,
					deniedPaths,
					skipDirs,
					budget,
					entryDepth,
				)
				after, afterErr := childSource.Stat(".")
				destCloseErr := childDestination.Close()
				sourceCloseErr := childSource.Close()
				postPath, pathErr := source.Lstat(name)
				if walkErr != nil {
					return walkErr
				}
				if afterErr != nil || pathErr != nil || destCloseErr != nil || sourceCloseErr != nil ||
					postPath.Mode()&os.ModeSymlink != 0 || !postPath.IsDir() ||
					!sameCodeExecFileSnapshot(opened, after) || !sameCodeExecFileSnapshot(after, postPath) {
					return fmt.Errorf("project dependency closure: source directory %s changed while staging", stagePath)
				}
				continue
			}
			if !before.Mode().IsRegular() {
				return fmt.Errorf("project dependency closure: unsupported source file type: %s", stagePath)
			}
			if err := traversal.addFile(stagePath, entryDepth, before.Size()); err != nil {
				return fmt.Errorf("project dependency closure: %w", err)
			}
			if err := copyCodeExecStageRegularFile(
				ctx,
				source,
				name,
				before,
				destination,
				name,
				budget,
			); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if err := verifyCodeExecDirectoryStream(source, directory, openedDirectory); err != nil {
		return fmt.Errorf("project dependency closure: %w", err)
	}
	return ctx.Err()
}

// copyCodeExecStageRegularFile 将检查、识别和复制绑定到同一源文件句柄，路径或内容漂移时失败关闭。
func copyCodeExecStageRegularFile(
	ctx context.Context,
	sourceRoot *os.Root,
	sourceName string,
	before os.FileInfo,
	destRoot *os.Root,
	destName string,
	budget *codeExecStageCopyBudget,
) (returnErr error) {
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return errors.New("project dependency closure: source file is not regular")
	}
	file, sourceOpenErr := openCodeExecRegularFileNoFollow(sourceRoot, sourceName)
	if sourceOpenErr != nil {
		return fmt.Errorf("project dependency closure: open source file: %w", sourceOpenErr)
	}
	opened, snapshotErr := snapshotCodeExecOpenedFile(file)
	if snapshotErr != nil || !opened.Info.Mode().IsRegular() ||
		!codeExecPathMatchesOpenedSnapshot(before, opened) {
		_ = file.Close()
		return errors.New("project dependency closure: source file changed while opening")
	}
	if opened.Platform.Links != 1 {
		_ = file.Close()
		return errors.New("project dependency closure: source file has multiple hard links")
	}
	defer joinCodeExecResourceClose(&returnErr, file, "close project source file")
	if opened.Info.Size() < 0 {
		return errors.New("project dependency closure: source file has invalid size")
	}
	prefix := make([]byte, 4)
	read, readErr := io.ReadFull(file, prefix)
	if errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, io.EOF) {
		readErr = nil
	}
	if readErr != nil {
		return fmt.Errorf("project dependency closure: inspect source artifact: %w", readErr)
	}
	generated := codeExecNativeExecutableMagic(prefix[:read]) &&
		(runtime.GOOS == "windows" || opened.Info.Mode().Perm()&0111 != 0 || codeExecNativeArtifactExtension(sourceName))
	if generated {
		return verifyCodeExecStageSourceFile(sourceRoot, sourceName, before, opened, file)
	}
	if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
		return fmt.Errorf("project dependency closure: rewind source file: %w", seekErr)
	}
	parent := filepath.Dir(destName)
	if parent != "." {
		if mkdirErr := destRoot.MkdirAll(parent, 0755); mkdirErr != nil {
			return fmt.Errorf("project dependency closure: create staged file directory: %w", mkdirErr)
		}
	}
	out, destinationOpenErr := destRoot.OpenFile(destName, os.O_CREATE|os.O_EXCL|os.O_RDWR, opened.Info.Mode().Perm()|0200)
	if destinationOpenErr != nil {
		return fmt.Errorf("project dependency closure: create staged file: %w", destinationOpenErr)
	}
	keep := false
	defer func() {
		_ = out.Close()
		if !keep {
			_ = destRoot.Remove(destName)
		}
	}()
	sourceHash := sha256.New()
	if copyErr := copyCodeExecExactFile(ctx, io.MultiWriter(out, sourceHash), file, opened.Info.Size()); copyErr != nil {
		return copyErr
	}
	if verifyErr := verifyCodeExecStageSourceFile(sourceRoot, sourceName, before, opened, file); verifyErr != nil {
		return verifyErr
	}
	// 暂存树失败时整体清理，完整性由同一打开句柄的双向哈希与快照校验保证；
	// 逐文件 fsync 只增加大量持久化延迟，不参与本次执行的安全判定。
	if _, seekErr := out.Seek(0, io.SeekStart); seekErr != nil {
		return fmt.Errorf("project dependency closure: rewind staged file: %w", seekErr)
	}
	destinationHash, hashErr := hashCodeExecExactOpenedFile(ctx, out, opened.Info.Size())
	if hashErr != nil {
		return fmt.Errorf("project dependency closure: hash staged file: %w", hashErr)
	}
	if !strings.EqualFold(hex.EncodeToString(sourceHash.Sum(nil)), destinationHash) {
		return errors.New("project dependency closure: staged file hash does not match source")
	}
	destSnapshot, statErr := snapshotCodeExecOpenedFile(out)
	destPath, pathErr := destRoot.Lstat(destName)
	if statErr != nil || pathErr != nil || !destSnapshot.Info.Mode().IsRegular() ||
		!codeExecPathMatchesOpenedSnapshot(destPath, destSnapshot) || destSnapshot.Platform.Links != 1 ||
		destSnapshot.Info.Size() != opened.Info.Size() {
		return errors.New("project dependency closure: staged file changed while writing")
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("project dependency closure: close staged file: %w", err)
	}
	keep = true
	return nil
}

// copyCodeExecExactFile 只读取固定大小并额外探测一个字节，源文件缩短或增长都拒绝。
func copyCodeExecExactFile(ctx context.Context, dest io.Writer, source io.Reader, expected int64) error {
	buffer := make([]byte, 32*1024)
	remaining := expected
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := int64(len(buffer))
		if remaining < want {
			want = remaining
		}
		read, readErr := source.Read(buffer[:int(want)])
		if read > 0 {
			written, writeErr := dest.Write(buffer[:read])
			if writeErr != nil {
				return fmt.Errorf("project dependency closure: copy staged file: %w", writeErr)
			}
			if written != read {
				return fmt.Errorf("project dependency closure: copy staged file: %w", io.ErrShortWrite)
			}
			remaining -= int64(read)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return errors.New("project dependency closure: source file shrank while copying")
			}
			return fmt.Errorf("project dependency closure: copy staged file: %w", readErr)
		}
		if read == 0 {
			return fmt.Errorf("project dependency closure: copy staged file: %w", io.ErrNoProgress)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var extra [1]byte
	read, err := source.Read(extra[:])
	if read != 0 {
		return errors.New("project dependency closure: source file grew while copying")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("project dependency closure: verify staged file boundary: %w", err)
	}
	return nil
}

func verifyCodeExecStageSourceFile(
	root *os.Root,
	name string,
	before os.FileInfo,
	opened codeExecOpenedFileSnapshot,
	file *os.File,
) error {
	after, afterErr := snapshotCodeExecOpenedFile(file)
	postPath, pathErr := root.Lstat(name)
	if afterErr != nil || pathErr != nil || !codeExecPathMatchesOpenedSnapshot(before, opened) ||
		!sameCodeExecOpenedFileSnapshot(opened, after) || !codeExecPathMatchesOpenedSnapshot(postPath, after) ||
		postPath.Mode()&os.ModeSymlink != 0 || !postPath.Mode().IsRegular() || after.Platform.Links != 1 {
		return errors.New("project dependency closure: source file changed while staging")
	}
	return nil
}

func hashCodeExecExactOpenedFile(ctx context.Context, file *os.File, expected int64) (string, error) {
	if expected < 0 {
		return "", errors.New("file has an invalid size")
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: file, N: expected + 1}
	buffer := make([]byte, 32*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		read, readErr := limited.Read(buffer)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
			total += int64(read)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
		if read == 0 {
			return "", io.ErrNoProgress
		}
	}
	if total != expected || limited.N != 1 {
		return "", errors.New("file changed size while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// sameCodeExecFileSnapshot 比较一次路径快照，阻止同一 inode 在打开前被增长、截断或改权。
func sameCodeExecFileSnapshot(before, current os.FileInfo) bool {
	if before == nil || current == nil || !os.SameFile(before, current) ||
		before.Size() != current.Size() || before.Mode() != current.Mode() ||
		!before.ModTime().Equal(current.ModTime()) {
		return false
	}
	beforeIdentity, beforeAvailable := codeExecPlatformPathIdentity(before)
	currentIdentity, currentAvailable := codeExecPlatformPathIdentity(current)
	return !beforeAvailable && !currentAvailable ||
		beforeAvailable && currentAvailable && beforeIdentity == currentIdentity
}

func codeExecNativeArtifactExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".exe", ".test", ".dll", ".so", ".dylib":
		return true
	default:
		return false
	}
}

func codeExecNativeExecutableMagic(prefix []byte) bool {
	if len(prefix) >= 2 && prefix[0] == 'M' && prefix[1] == 'Z' {
		return true
	}
	if len(prefix) < 4 {
		return false
	}
	magic := [4]byte{prefix[0], prefix[1], prefix[2], prefix[3]}
	switch magic {
	case [4]byte{0x7f, 'E', 'L', 'F'},
		[4]byte{0xfe, 0xed, 0xfa, 0xce},
		[4]byte{0xfe, 0xed, 0xfa, 0xcf},
		[4]byte{0xce, 0xfa, 0xed, 0xfe},
		[4]byte{0xcf, 0xfa, 0xed, 0xfe},
		[4]byte{0xca, 0xfe, 0xba, 0xbe},
		[4]byte{0xbe, 0xba, 0xfe, 0xca}:
		return true
	default:
		return false
	}
}

func codeExecStagePathDenied(path string, deniedPaths []string) bool {
	for _, denied := range deniedPaths {
		if strings.TrimSpace(denied) != "" && pathWithinResolved(denied, path) {
			return true
		}
	}
	return false
}

func codeExecStagedModuleDirName(source string) string {
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, filepath.Base(source))
	if strings.Trim(base, "-.") == "" {
		base = "module"
	}
	sum := sha256.Sum256([]byte(source))
	return fmt.Sprintf("%s-%x", base, sum[:6])
}

func codeExecGoModuleRefText(ref codeExecGoModuleRef) string {
	if strings.TrimSpace(ref.Version) == "" {
		return ref.Path
	}
	return ref.Path + "@" + ref.Version
}

func codeExecGoRelativePath(path string) string {
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "." || strings.HasPrefix(path, ".") {
		return path
	}
	return "./" + path
}

func ensureCodeExecStagedMetadataHasNoHostPaths(
	ctx context.Context,
	stageRoot string,
	goWorkPath string,
	modules []codeExecStageModule,
	hostPaths []string,
	maxBytes int64,
) error {
	if ctx == nil {
		return errors.New("project dependency closure: metadata context is required")
	}
	if maxBytes <= 0 {
		return errors.New("project dependency closure: metadata size limit must be positive")
	}
	var metadataFiles []string
	if goWorkPath != "" {
		metadataFiles = append(metadataFiles, goWorkPath)
	}
	for _, module := range modules {
		if modFile := filepath.Join(module.Dest, "go.mod"); fileExists(modFile) {
			metadataFiles = append(metadataFiles, modFile)
		}
	}
	if vendorManifest := filepath.Join(stageRoot, "vendor", "modules.txt"); fileExists(vendorManifest) {
		metadataFiles = append(metadataFiles, vendorManifest)
	}
	for _, metadataFile := range metadataFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		data, err := readCodeExecSecureFile(
			filepath.Dir(metadataFile),
			filepath.Base(metadataFile),
			maxBytes,
		)
		if err != nil {
			return fmt.Errorf("project dependency closure: inspect staged metadata: %w", err)
		}
		for _, hostPath := range hostPaths {
			if err := ctx.Err(); err != nil {
				return err
			}
			hostPath = strings.TrimSpace(hostPath)
			if hostPath == "" || hostPath == string(filepath.Separator) {
				continue
			}
			if strings.Contains(string(data), hostPath) {
				return fmt.Errorf("project dependency closure: staged metadata retained a host absolute path")
			}
		}
	}
	return nil
}

func prepareCodeExecCommand(ctx context.Context, req codeExecRequest, run codeExecRun) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Command) > 0 {
		return normalizeCommandRuntime(req.Command), nil
	}

	switch req.Mode {
	case "snippet":
		return prepareSnippetCommand(req, run)
	case "file":
		return prepareFileCommand(ctx, req, run)
	case "module":
		return prepareModuleCommand(ctx, req, run)
	case "project":
		return defaultProjectCommand(ctx, run.ProjectRoot)
	default:
		return nil, fmt.Errorf("command is required for mode=%s", req.Mode)
	}
}

// prepareCodeExecCommandWithCapabilities 在命令推导后、创建任何执行沙箱前冻结能力要求。
func prepareCodeExecCommandWithCapabilities(
	ctx context.Context,
	req codeExecRequest,
	run codeExecRun,
	goos string,
) ([]string, sandbox.Config, error) {
	command, err := prepareCodeExecCommand(ctx, req, run)
	if err != nil {
		return nil, sandbox.Config{}, err
	}
	cfg, err := withCodeExecCommandRequiredCapabilities(run.Config, goos, command)
	if err != nil {
		return nil, sandbox.Config{}, err
	}
	return command, cfg, nil
}

func prepareSnippetCommand(req codeExecRequest, run codeExecRun) ([]string, error) {
	ext, cmd, args, err := runnerForLanguage(req.Language)
	if err != nil {
		return nil, err
	}
	fileName := "_hexclaw_exec" + ext
	// Go 工具链会忽略以下划线或点开头的源文件，Go snippet 使用普通受控文件名。
	if normalizeCodeExecLanguage(req.Language) == "go" {
		fileName = "hexclaw_exec" + ext
	}
	if err := os.WriteFile(filepath.Join(run.Workspace, fileName), []byte(req.Code), 0644); err != nil {
		return nil, fmt.Errorf("write snippet: %w", err)
	}
	return append([]string{cmd}, append(args, fileName)...), nil
}

func prepareFileCommand(ctx context.Context, req codeExecRequest, run codeExecRun) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry := req.EntryPoint
	if entry == "" {
		var err error
		entry, err = inferRunnableFile(ctx, run.Base, req.Language)
		if err != nil {
			return nil, err
		}
		if entry == "" {
			return nil, fmt.Errorf("entrypoint or command is required for mode=file")
		}
	}
	if !filepath.IsAbs(entry) {
		if abs, err := filepath.Abs(entry); err == nil {
			entry = abs
		}
	}
	data, err := os.ReadFile(entry)
	if err != nil {
		return nil, fmt.Errorf("read entrypoint %s: %w", req.EntryPoint, err)
	}
	localName := filepath.Base(entry)
	if localName == "." || localName == string(filepath.Separator) || localName == "" {
		return nil, fmt.Errorf("invalid entrypoint: %s", req.EntryPoint)
	}
	localPath := filepath.Join(run.Workspace, localName)
	if err := os.WriteFile(localPath, data, 0644); err != nil {
		return nil, fmt.Errorf("copy entrypoint: %w", err)
	}
	lang := req.Language
	if lang == "" {
		lang = languageFromPath(localName)
	}
	_, cmd, args, err := runnerForLanguage(lang)
	if err != nil {
		return nil, err
	}
	return append([]string{cmd}, append(args, localName)...), nil
}

func prepareModuleCommand(ctx context.Context, req codeExecRequest, run codeExecRun) ([]string, error) {
	for _, f := range req.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rel, err := cleanRunRelativePath(f.Path)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(run.Workspace, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return nil, fmt.Errorf("create file dir: %w", err)
		}
		if err := os.WriteFile(dst, []byte(f.Content), 0644); err != nil {
			return nil, fmt.Errorf("write file %s: %w", f.Path, err)
		}
	}
	if req.Code != "" {
		snippetReq := req
		snippetReq.Mode = "snippet"
		return prepareSnippetCommand(snippetReq, run)
	}
	if req.Language == "" {
		var err error
		req.Language, err = inferLanguageFromWorkspace(ctx, run.Workspace)
		if err != nil {
			return nil, err
		}
	}
	switch req.Language {
	case "go", "golang":
		if fileExists(filepath.Join(run.Workspace, "go.mod")) {
			return []string{"go", "test", "./..."}, nil
		}
		return []string{"go", "run", "."}, nil
	case "python", "python3":
		if fileExists(filepath.Join(run.Workspace, "pytest.ini")) || fileExists(filepath.Join(run.Workspace, "pyproject.toml")) {
			return []string{"python3", "-m", "pytest"}, nil
		}
		if fileExists(filepath.Join(run.Workspace, "main.py")) {
			return []string{"python3", "main.py"}, nil
		}
		if first, err := firstFileWithExt(ctx, run.Workspace, ".py"); err != nil {
			return nil, err
		} else if first != "" {
			return []string{"python3", first}, nil
		}
	case "javascript", "js", "node":
		if fileExists(filepath.Join(run.Workspace, "package.json")) {
			if commandExists("pnpm") {
				return []string{"pnpm", "test"}, nil
			}
			return []string{"npm", "test"}, nil
		}
		if fileExists(filepath.Join(run.Workspace, "index.js")) {
			return []string{"node", "index.js"}, nil
		}
		if first, err := firstFileWithExt(ctx, run.Workspace, ".js"); err != nil {
			return nil, err
		} else if first != "" {
			return []string{"node", first}, nil
		}
	}
	return nil, fmt.Errorf("cannot infer module runner; provide language or command")
}

func defaultProjectCommand(ctx context.Context, projectRoot string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch {
	case fileExists(filepath.Join(projectRoot, "go.mod")):
		if fileExists(filepath.Join(projectRoot, "skill", "builtin")) {
			return []string{"go", "test", "./skill/builtin", "-run", "TestCodeExecSkill_Meta", "-count=1"}, nil
		}
		return []string{"go", "test", "./..."}, nil
	case fileExists(filepath.Join(projectRoot, "pyproject.toml")) || fileExists(filepath.Join(projectRoot, "pytest.ini")):
		return []string{"python3", "-m", "pytest"}, nil
	case fileExists(filepath.Join(projectRoot, "package.json")):
		if commandExists("pnpm") {
			return []string{"pnpm", "test"}, nil
		}
		return []string{"npm", "test"}, nil
	default:
		if first, err := inferRunnableFile(ctx, projectRoot, ""); err != nil {
			return nil, err
		} else if first != "" {
			req := codeExecRequest{EntryPoint: first, Language: languageFromPath(first)}
			run := codeExecRun{Workspace: projectRoot}
			return prepareFileCommand(ctx, req, run)
		}
		return nil, fmt.Errorf("command is required for mode=project")
	}
}

func inferRunnableFile(ctx context.Context, root, language string) (string, error) {
	exts := []string{".py", ".js", ".go"}
	switch normalizeCodeExecLanguage(language) {
	case "python", "python3":
		exts = []string{".py"}
	case "javascript", "js", "node":
		exts = []string{".js", ".mjs", ".cjs"}
	case "go", "golang":
		exts = []string{".go"}
	}
	for _, ext := range exts {
		first, err := firstFileWithExt(ctx, root, ext)
		if err != nil {
			return "", err
		}
		if first != "" {
			return filepath.Join(root, first), nil
		}
	}
	return "", nil
}

func runSandboxCommand(ctx context.Context, sb sandbox.Sandbox, run codeExecRun, command []string) (*sandbox.ExecResult, error) {
	exports := codeExecEnv(run)
	if err := ensureCodeExecEnvDirs(run, exports); err != nil {
		return nil, err
	}
	workingDir := run.ProjectRoot
	if strings.TrimSpace(workingDir) == "" {
		workingDir = run.Workspace
	}
	return runStructuredSandboxCommand(ctx, sb, workingDir, command, exports)
}

func runCodeExecPlannedCommand(
	ctx context.Context,
	sb sandbox.Sandbox,
	run codeExecRun,
	command []string,
) (*sandbox.ExecResult, error) {
	if run.Plan.Toolchain != nil {
		if err := verifyCodeExecGoToolchainDescriptor(*run.Plan.Toolchain); err != nil {
			return nil, err
		}
		if len(command) == 0 || command[0] != run.Plan.Toolchain.Binary {
			return nil, errors.New("go execution command is not bound to the selected toolchain descriptor")
		}
	}
	return runSandboxCommand(ctx, sb, run, command)
}

func codeExecEnv(run codeExecRun) map[string]string {
	exports := map[string]string{
		"HEXCLAW_RUN_ID":       run.ID,
		"HEXCLAW_WORKSPACE":    run.Scratch,
		"HEXCLAW_ARTIFACT_DIR": run.ArtifactDir,
		"HOME":                 run.Scratch,
		"XDG_CONFIG_HOME":      filepath.Join(run.Scratch, ".config"),
		"APPDATA":              filepath.Join(run.Scratch, "AppData", "Roaming"),
		"LOCALAPPDATA":         filepath.Join(run.Scratch, "AppData", "Local"),
		"TMPDIR":               filepath.Join(run.Scratch, "tmp"),
		"TMP":                  filepath.Join(run.Scratch, "tmp"),
		"TEMP":                 filepath.Join(run.Scratch, "tmp"),
		"GOCACHE":              filepath.Join(run.CacheDir, "go-build"),
		"PIP_CACHE_DIR":        filepath.Join(run.CacheDir, "pip"),
		"PYTHONPYCACHEPREFIX":  filepath.Join(run.CacheDir, "pycache"),
		"npm_config_cache":     filepath.Join(run.CacheDir, "npm"),
		"GOWORK":               "off",
	}
	if run.Plan.Toolchain != nil {
		exports["GOROOT"] = run.Plan.Toolchain.GOROOT
		pathEntries := append([]string{filepath.Dir(run.Plan.Toolchain.Binary)}, filepath.SplitList(codeExecRuntimePath())...)
		exports["PATH"] = strings.Join(compactCleanPaths(pathEntries), string(os.PathListSeparator))
	}
	if run.GoWorkPath != "" {
		exports["GOWORK"] = run.GoWorkPath
	}
	if run.Plan.GoRuntime {
		exports["GOMODCACHE"] = filepath.Join(run.CacheDir, "gomod")
		exports["GOPROXY"] = "off"
		exports["GOSUMDB"] = "off"
		exports["GOTOOLCHAIN"] = "local"
	}
	if run.GoVendored {
		exports["GOFLAGS"] = "-mod=vendor"
	}
	for key, value := range run.Plan.Environment {
		exports[key] = value
	}
	return exports
}

var codeExecWritableEnvKeys = []string{
	"HEXCLAW_ARTIFACT_DIR",
	"TMPDIR",
	"TMP",
	"TEMP",
	"GOCACHE",
	"PIP_CACHE_DIR",
	"PYTHONPYCACHEPREFIX",
	"npm_config_cache",
	"GOMODCACHE",
	"XDG_CONFIG_HOME",
	"APPDATA",
	"LOCALAPPDATA",
}

func ensureCodeExecEnvDirs(run codeExecRun, exports map[string]string) error {
	seen := map[string]bool{}
	for _, key := range codeExecWritableEnvKeys {
		dir := strings.TrimSpace(exports[key])
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create %s dir %s: %w", key, dir, err)
		}
	}
	return ensureCodeExecGoTelemetryOff(exports)
}

// Go's GOTELEMETRY value is intentionally non-settable through the process
// environment. A fresh isolated HOME therefore defaults to local collection
// and may spawn a telemetry child that needs host devices such as /dev/null.
// Materialize the documented per-user mode file in the isolated config root
// before sandbox entry so Go workloads remain deterministic and offline.
func ensureCodeExecGoTelemetryOff(exports map[string]string) error {
	home := strings.TrimSpace(exports["HOME"])
	var configRoot string
	switch runtime.GOOS {
	case "darwin":
		configRoot = filepath.Join(home, "Library", "Application Support")
	case "windows":
		configRoot = strings.TrimSpace(exports["APPDATA"])
	default:
		configRoot = strings.TrimSpace(exports["XDG_CONFIG_HOME"])
		if configRoot == "" && home != "" {
			configRoot = filepath.Join(home, ".config")
		}
	}
	if configRoot == "" {
		return errors.New("create Go telemetry policy: isolated config root is empty")
	}
	modePath := filepath.Join(configRoot, "go", "telemetry", "mode")
	if err := os.MkdirAll(filepath.Dir(modePath), 0700); err != nil {
		return fmt.Errorf("create Go telemetry policy directory: %w", err)
	}
	mode := "off " + time.Now().UTC().Format("2006-01-02")
	if err := os.WriteFile(modePath, []byte(mode), 0600); err != nil {
		return fmt.Errorf("write Go telemetry policy: %w", err)
	}
	return nil
}

func runPosixSandboxCommandInDir(ctx context.Context, sb sandbox.Sandbox, projectRoot string, command []string, exports map[string]string) (*sandbox.ExecResult, error) {
	return runStructuredSandboxCommand(ctx, sb, projectRoot, command, exports)
}

func runStructuredSandboxCommand(
	ctx context.Context,
	sb sandbox.Sandbox,
	workingDir string,
	command []string,
	exports map[string]string,
) (*sandbox.ExecResult, error) {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return nil, errors.New("structured sandbox command path is required")
	}
	environment := codeExecSortedCompleteEnvironment(workingDir, exports)
	commandPath, err := resolveCodeExecStructuredCommandPath(command[0], workingDir, environment)
	if err != nil {
		return nil, err
	}
	return sb.Exec(ctx, sandbox.Command{
		Path: commandPath,
		Args: append([]string(nil), command[1:]...),
		Dir:  workingDir,
		Env:  environment,
	})
}

// resolveCodeExecStructuredCommandPath 在进入 Sandbox 前把命令冻结为规范绝对路径。
// 查找只使用 CodeExec 构造的完整安全环境，绝不读取调用进程的任意 PATH。
func resolveCodeExecStructuredCommandPath(commandPath, workingDir string, environment []string) (string, error) {
	if commandPath == "" || commandPath != strings.TrimSpace(commandPath) || strings.IndexByte(commandPath, 0) >= 0 {
		return "", errors.New("structured sandbox command path is invalid")
	}
	if filepath.IsAbs(commandPath) {
		return codeExecCanonicalRuntimeExecutable(commandPath)
	}
	if strings.ContainsAny(commandPath, `/\`) {
		return codeExecCanonicalRuntimeExecutable(filepath.Join(workingDir, commandPath))
	}
	searchPath := codeExecEnvironmentValue(environment, "PATH")
	for _, directory := range filepath.SplitList(searchPath) {
		if !filepath.IsAbs(directory) {
			continue
		}
		for _, name := range codeExecCommandFileNames(commandPath) {
			resolved, err := codeExecCanonicalRuntimeExecutable(filepath.Join(directory, name))
			if err == nil {
				return resolved, nil
			}
		}
	}
	return "", fmt.Errorf("resolve structured sandbox command %q: executable was not found", commandPath)
}

func codeExecEnvironmentValue(environment []string, name string) string {
	prefix := name + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			return strings.TrimPrefix(item, prefix)
		}
	}
	return ""
}

func codeExecCommandFileNames(name string) []string {
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		return []string{name + ".exe", name}
	}
	return []string{name}
}

func codeExecSortedCompleteEnvironment(projectRoot string, exports map[string]string) []string {
	cleanEnv := codeExecCleanEnvironment(projectRoot, exports)
	keys := make([]string, 0, len(cleanEnv))
	for key := range cleanEnv {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+cleanEnv[key])
	}
	return environment
}

func codeExecCleanEnvironment(projectRoot string, exports map[string]string) map[string]string {
	home := strings.TrimSpace(projectRoot)
	if home == "" {
		home = string(os.PathSeparator) + "nonexistent"
	}
	clean := map[string]string{
		"GOENV":   "off",
		"HOME":    home,
		"LANG":    "C.UTF-8",
		"LC_ALL":  "C.UTF-8",
		"LOGNAME": "hexclaw",
		"PATH":    codeExecRuntimePath(),
		"PWD":     home,
		"USER":    "hexclaw",
	}
	for key, value := range exports {
		clean[key] = value
	}
	if runtime.GOOS == "windows" {
		if systemRoot := strings.TrimSpace(os.Getenv("SystemRoot")); systemRoot != "" {
			clean["SystemRoot"] = systemRoot
		}
	}
	return clean
}

func codeExecRuntimePath() string {
	dirs := []string{
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/local/go/bin",
		"/usr/bin",
		"/bin",
		"/usr/sbin",
		"/sbin",
	}
	if goroot := strings.TrimSpace(runtime.GOROOT()); goroot != "" {
		dirs = append(dirs, filepath.Join(goroot, "bin"))
	}
	// Preserve explicitly supported runtimes installed in version-manager
	// directories without inheriting the host's broad PATH verbatim.
	for _, runtimeName := range []string{"python3", "python", "node", "npm", "npx", "go"} {
		if path, err := exec.LookPath(runtimeName); err == nil && filepath.IsAbs(path) {
			dirs = append(dirs, filepath.Dir(path))
		}
	}
	return strings.Join(compactCleanPaths(dirs), string(os.PathListSeparator))
}

func buildCodeExecReport(req codeExecRequest, run codeExecRun, command []string, result *sandbox.ExecResult, execErr error, missingDeps []string) codeExecReport {
	// 直接字段读写 sandbox.Config：编译器守契约，字段改名/删除会编译错，
	// 不再走反射在发版构建下静默返回 0 导致限额在报告里蒸发。
	cwd := run.Workspace
	if run.WorkingDir != "" {
		cwd = run.WorkingDir
	} else if run.ProjectRoot != "" {
		cwd = run.ProjectRoot
	}
	applicationBudget := run.applicationBudget()
	report := codeExecReport{
		RunID:             run.ID,
		Mode:              req.Mode,
		Language:          req.Language,
		Command:           append([]string(nil), command...),
		EntryPoint:        req.EntryPoint,
		Status:            "success",
		ExitCode:          0,
		DependencyMissing: append([]string(nil), missingDeps...),
		MaxStdoutBytes:    run.Config.MaxOutputBytes,
		MaxStderrBytes:    run.Config.MaxStderrBytes,
		MaxWorkspaceBytes: applicationBudget.MaxWorkspaceBytes,
		MaxArtifactBytes:  applicationBudget.MaxArtifactBytes,
		MaxProcesses:      run.Config.MaxProcesses,
		MaxMemoryBytes:    run.Config.MaxMemoryBytes,
		Paths: map[string]string{
			"run_root":     run.Root,
			"workspace":    run.Scratch,
			"cwd":          cwd,
			"artifacts":    run.ArtifactDir,
			"manifest":     run.ManifestPath,
			"project_root": run.ProjectRoot,
		},
	}
	if result != nil {
		report.ExitCode = result.ExitCode
		report.StdoutBytes = result.StdoutBytes
		if report.StdoutBytes == 0 && result.Stdout != "" {
			report.StdoutBytes = int64(len(result.Stdout))
		}
		report.StderrBytes = result.StderrBytes
		if report.StderrBytes == 0 && result.Stderr != "" {
			report.StderrBytes = int64(len(result.Stderr))
		}
		report.StdoutTruncated = result.StdoutTruncated
		report.StderrTruncated = result.StderrTruncated
		report.Truncated = report.StdoutTruncated || report.StderrTruncated
		if result.ExitCode != 0 {
			report.Status = "failed"
		}
	}

	// 能力位接真：resource_limits/fail_closed 等不再硬编码常量，改读 ExecResult.Limits
	// （LimitReport）逐维如实报告。result 为 nil（如文件系统隔离不可用，直接拒绝执行、
	// 未产生 ExecResult）时，各维为空 → 视为未生效，绝不假装 enforced。
	var limits sandbox.LimitReport
	if result != nil {
		limits = result.Limits
	}
	report.FilesystemIsolation = string(limits.Filesystem)
	report.FilesystemDegraded = limits.Filesystem != sandbox.LimitStatusEnforced
	report.Capabilities = buildCodeExecCapabilities(limits)

	if execErr != nil {
		classifyCodeExecError(&report, execErr)
	}
	report.RuntimeMissing = detectRuntimeMissing(command, result, execErr)
	if report.RuntimeMissing && report.Status == "success" {
		report.Status = "failed"
	}
	return report
}

// buildCodeExecCapabilities 由沙箱后端上报的 LimitReport 逐维如实构造能力位，
// 不再把 fail_closed/resource_limits 当常量硬编码（发版构建曾据此撒谎）。
func buildCodeExecCapabilities(limits sandbox.LimitReport) map[string]any {
	enforced := func(s sandbox.LimitStatus) bool { return s == sandbox.LimitStatusEnforced }
	return map[string]any{
		"platform":            runtime.GOOS,
		"per_run_workspace":   true,
		"artifact_manifest":   false,
		"process_containment": enforced(limits.ProcessContainment),
		// 逐维如实（enforced / weak / unsupported）：
		"limit_memory":         string(limits.Memory),
		"limit_processes":      string(limits.Processes),
		"limit_storage":        string(limits.Storage),
		"limit_output":         string(limits.Output),
		"filesystem_isolation": string(limits.Filesystem),
		// 派生汇总位（据实推导，不再恒 true）：
		"bounded_output": enforced(limits.Output),
		// resource_limits：内存/进程/存储/输出四资源全部 enforced 才为真
		// （如 darwin 无法下调内存 rlimit → 此处如实为 false）。
		"resource_limits": enforced(limits.Memory) && enforced(limits.Processes) &&
			enforced(limits.Storage) && enforced(limits.Output),
		// fail_closed：文件系统 deny-by-default 强隔离是否真实生效
		// （enforced=真 fail-closed；unsupported=降级，不再谎报 true）。
		"fail_closed": enforced(limits.Filesystem),
	}
}

// classifyCodeExecError 归类沙箱执行错误，避免把「存储超限/隔离不可用」误报成通用失败。
func classifyCodeExecError(report *codeExecReport, execErr error) {
	switch {
	case errors.Is(execErr, sandbox.ErrFilesystemContainmentUnavailable):
		// 文件系统强隔离后端缺失，沙箱已拒绝执行以防泄露：给出明确可读错误，
		// 不吞成通用失败，也不误报为「后端不可用/未知错误」。
		report.Status = "failed"
		report.FilesystemDegraded = true
		if report.FilesystemIsolation == "" {
			report.FilesystemIsolation = string(sandbox.LimitStatusUnsupported)
		}
		report.Error = "Strong filesystem isolation is unavailable; execution was refused to prevent host file disclosure: " + execErr.Error()
	case errors.Is(execErr, sandbox.ErrStorageLimitExceeded):
		// 存储违规归类为「产物超限」而非后端不可用。
		report.Status = "resource_limited"
		report.WorkspaceLimited = true
		report.Error = "Sandbox artifacts or workspace exceeded the storage limit: " + execErr.Error()
	default:
		report.Error = execErr.Error()
		report.Status = "failed"
		if strings.Contains(strings.ToLower(execErr.Error()), "timeout") || strings.Contains(strings.ToLower(execErr.Error()), "deadline") {
			report.Timeout = true
			report.Status = "timeout"
		}
	}
}

func appendCodeExecReportError(report *codeExecReport, reportErr error) {
	if reportErr == nil {
		return
	}
	if strings.TrimSpace(report.Error) == "" {
		report.Error = reportErr.Error()
	} else {
		report.Error = errors.Join(errors.New(report.Error), reportErr).Error()
	}
	report.Status = "failed"
}

func finalizeCodeExecReport(
	ctx context.Context,
	run codeExecRun,
	artifactsEnabled bool,
	report *codeExecReport,
) error {
	return finalizeCodeExecReportWithObserver(ctx, run, artifactsEnabled, report, nil)
}

func finalizeCodeExecReportWithObserver(
	ctx context.Context,
	run codeExecRun,
	artifactsEnabled bool,
	report *codeExecReport,
	readDirObserver func(),
) error {
	if ctx == nil {
		return errors.New("finalize context must not be nil")
	}
	if report == nil {
		return errors.New("finalize report must not be nil")
	}
	if !artifactsEnabled {
		report.Artifacts = nil
		return nil
	}
	if processContainment, ok := report.Capabilities["process_containment"].(bool); !ok || !processContainment {
		return errors.New("artifact collection requires enforced process containment")
	}
	workspaceBytes, err := codeExecDirSizeContextObserved(ctx, run.Scratch, readDirObserver)
	if err != nil {
		return err
	}
	report.WorkspaceBytes = workspaceBytes
	if report.MaxWorkspaceBytes > 0 && workspaceBytes > report.MaxWorkspaceBytes {
		report.WorkspaceLimited = true
		report.Status = "resource_limited"
	}
	artifacts, err := collectCodeExecArtifactsObserved(
		ctx,
		run.Scratch,
		run.ArtifactDir,
		report.MaxArtifactBytes,
		report.MaxWorkspaceBytes,
		readDirObserver,
	)
	if err != nil {
		return err
	}
	report.Artifacts = artifacts
	return nil
}

func formatCodeExecOutput(result *sandbox.ExecResult, report codeExecReport) string {
	var out strings.Builder
	if result != nil {
		out.WriteString(result.Stdout)
		if result.Stderr != "" {
			if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
				out.WriteString("\n")
			}
			out.WriteString("[stderr]\n")
			out.WriteString(result.Stderr)
		}
	}
	if report.ExitCode != 0 {
		if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
			out.WriteString("\n")
		}
		out.WriteString(fmt.Sprintf("[exit code %d]\n", report.ExitCode))
	}
	if report.Truncated {
		out.WriteString(fmt.Sprintf("\n[hexclaw] truncated=true stdout_bytes=%d stderr_bytes=%d max_stdout_bytes=%d max_stderr_bytes=%d\n", report.StdoutBytes, report.StderrBytes, report.MaxStdoutBytes, report.MaxStderrBytes))
	}
	if report.RuntimeMissing {
		out.WriteString("\n[hexclaw] runtime_missing=true\n")
	}
	if report.Timeout {
		out.WriteString("\n[hexclaw] timeout=true\n")
	}
	if report.FilesystemDegraded {
		// 如实标注文件系统隔离降级（unsupported），让上层/模型知晓当前环境
		// 未提供强隔离，敏感任务应改在具备强隔离的环境执行。
		iso := report.FilesystemIsolation
		if iso == "" {
			iso = string(sandbox.LimitStatusUnsupported)
		}
		_, _ = fmt.Fprintf(&out, "\n[hexclaw] filesystem_isolation=%s (degraded: deny-by-default isolation is unavailable)\n", iso)
	}
	if len(report.Artifacts) > 0 {
		out.WriteString(fmt.Sprintf("\n[hexclaw] artifacts=%d\n", len(report.Artifacts)))
	}

	b, _ := json.Marshal(report)
	if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
		out.WriteString("\n")
	}
	out.WriteString("\n[hexclaw_sandbox_result]\n")
	out.Write(b)
	return out.String()
}

func writeCodeExecManifest(run codeExecRun, report codeExecReport) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return writeCodeExecSecureFile(
		filepath.Dir(run.ManifestPath),
		filepath.Base(run.ManifestPath),
		b,
		0644,
	)
}

func collectCodeExecArtifactsObserved(
	ctx context.Context,
	scratchDir string,
	artifactDir string,
	maxArtifactBytes int64,
	maxTotalBytes int64,
	readDirObserver func(),
) ([]codeExecArtifact, error) {
	budget := newCodeExecTraversalBudget(maxTotalBytes, readDirObserver)
	return collectCodeExecArtifactsWithTraversal(ctx, scratchDir, artifactDir, maxArtifactBytes, budget)
}

func collectCodeExecArtifactsWithTraversal(
	ctx context.Context,
	scratchDir string,
	artifactDir string,
	maxArtifactBytes int64,
	budget *codeExecTraversalBudget,
) (_ []codeExecArtifact, returnErr error) {
	artifacts := make([]codeExecArtifact, 0)
	if ctx == nil {
		return nil, errors.New("artifact collection context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxArtifactBytes <= 0 || maxArtifactBytes >= int64(^uint64(0)>>1) {
		return nil, errors.New("artifact size limit must be positive and bounded")
	}
	if budget == nil {
		return nil, errors.New("artifact traversal budget is required")
	}
	if err := budget.addDirectory("", 0); err != nil {
		return nil, fmt.Errorf("artifact collection: %w", err)
	}
	scratchRoot, scratchInfo, err := openCodeExecRootNoFollow(scratchDir)
	if err != nil {
		return nil, fmt.Errorf("open artifact scratch root: %w", err)
	}
	defer joinCodeExecResourceClose(&returnErr, scratchRoot, "close artifact scratch root")
	relativeRoot, err := filepath.Rel(scratchDir, artifactDir)
	if err != nil || relativeRoot == "." || filepath.IsAbs(relativeRoot) || relativeRoot == ".." ||
		strings.HasPrefix(relativeRoot, ".."+string(filepath.Separator)) {
		return nil, errors.New("artifact directory escapes scratch root")
	}
	artifactInfo, err := scratchRoot.Lstat(relativeRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return artifacts, nil
		}
		return nil, fmt.Errorf("inspect artifact directory: %w", err)
	}
	if artifactInfo.Mode()&os.ModeSymlink != 0 || !artifactInfo.IsDir() {
		return nil, errors.New("artifact directory is not a regular directory")
	}
	artifactRoot, err := scratchRoot.OpenRoot(relativeRoot)
	if err != nil {
		return nil, fmt.Errorf("open artifact directory: %w", err)
	}
	defer joinCodeExecResourceClose(&returnErr, artifactRoot, "close artifact directory")
	openedArtifactInfo, err := artifactRoot.Stat(".")
	if err != nil || !os.SameFile(artifactInfo, openedArtifactInfo) {
		return nil, errors.New("artifact directory changed while opening")
	}
	if err := collectCodeExecArtifactDirectory(
		ctx,
		artifactRoot,
		"",
		maxArtifactBytes,
		&artifacts,
		budget,
		0,
	); err != nil {
		return nil, err
	}
	afterArtifactInfo, artifactStatErr := artifactRoot.Stat(".")
	postArtifactInfo, artifactPathErr := scratchRoot.Lstat(relativeRoot)
	afterScratchInfo, scratchStatErr := scratchRoot.Stat(".")
	postScratchInfo, scratchPathErr := os.Lstat(scratchDir)
	if artifactStatErr != nil || artifactPathErr != nil || scratchStatErr != nil || scratchPathErr != nil ||
		postArtifactInfo.Mode()&os.ModeSymlink != 0 || !postArtifactInfo.IsDir() ||
		!sameCodeExecFileSnapshot(openedArtifactInfo, afterArtifactInfo) ||
		!sameCodeExecFileSnapshot(afterArtifactInfo, postArtifactInfo) ||
		postScratchInfo.Mode()&os.ModeSymlink != 0 || !postScratchInfo.IsDir() ||
		!sameCodeExecFileSnapshot(scratchInfo, afterScratchInfo) ||
		!sameCodeExecFileSnapshot(afterScratchInfo, postScratchInfo) {
		return nil, errors.New("artifact collection boundary changed while scanning")
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return artifacts, ctx.Err()
}

func collectCodeExecArtifactDirectory(
	ctx context.Context,
	root *os.Root,
	prefix string,
	maxArtifactBytes int64,
	artifacts *[]codeExecArtifact,
	budget *codeExecTraversalBudget,
	depth int,
) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, openedDirectory, openErr := openCodeExecDirectoryStream(root)
	if openErr != nil {
		return openErr
	}
	defer joinCodeExecResourceClose(&returnErr, directory, "close artifact directory stream")
	for {
		entries, readErr := readCodeExecDirectoryBatch(ctx, directory, budget)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := entry.Name()
			artifactPath := name
			if prefix != "" {
				artifactPath = prefix + "/" + name
			}
			entryDepth := depth + 1
			if err := budget.validatePath(artifactPath, entryDepth); err != nil {
				return fmt.Errorf("artifact collection: %w", err)
			}
			before, entryErr := root.Lstat(name)
			if entryErr != nil {
				return fmt.Errorf("inspect artifact %s: %w", artifactPath, entryErr)
			}
			if before.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("artifact %s is a symlink", artifactPath)
			}
			if before.IsDir() {
				if budgetErr := budget.addDirectory(artifactPath, entryDepth); budgetErr != nil {
					return fmt.Errorf("artifact collection: %w", budgetErr)
				}
				child, childOpenErr := root.OpenRoot(name)
				if childOpenErr != nil {
					return fmt.Errorf("open artifact directory %s: %w", artifactPath, childOpenErr)
				}
				opened, statErr := child.Stat(".")
				if statErr != nil || !sameCodeExecFileSnapshot(before, opened) {
					_ = child.Close()
					return fmt.Errorf("artifact directory %s changed while opening", artifactPath)
				}
				walkErr := collectCodeExecArtifactDirectory(
					ctx,
					child,
					artifactPath,
					maxArtifactBytes,
					artifacts,
					budget,
					entryDepth,
				)
				after, afterErr := child.Stat(".")
				closeErr := child.Close()
				postPath, pathErr := root.Lstat(name)
				if walkErr != nil {
					return walkErr
				}
				if afterErr != nil || pathErr != nil || closeErr != nil ||
					postPath.Mode()&os.ModeSymlink != 0 || !postPath.IsDir() ||
					!sameCodeExecFileSnapshot(opened, after) || !sameCodeExecFileSnapshot(after, postPath) {
					return fmt.Errorf("artifact directory %s changed while scanning", artifactPath)
				}
				continue
			}
			if !before.Mode().IsRegular() {
				return fmt.Errorf("artifact %s is not a regular file", artifactPath)
			}
			if budgetErr := budget.addFile(artifactPath, entryDepth, before.Size()); budgetErr != nil {
				return fmt.Errorf("artifact collection: %w", budgetErr)
			}
			size, sha, hashErr := hashCodeExecArtifact(ctx, root, name, before, maxArtifactBytes)
			if hashErr != nil {
				return fmt.Errorf("collect artifact %s: %w", artifactPath, hashErr)
			}
			*artifacts = append(*artifacts, codeExecArtifact{
				ID:     "artifact_" + sha[:12],
				Name:   name,
				Path:   artifactPath,
				Size:   size,
				SHA256: sha,
				MIME:   mime.TypeByExtension(filepath.Ext(name)),
			})
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	if err := verifyCodeExecDirectoryStream(root, directory, openedDirectory); err != nil {
		return fmt.Errorf("artifact collection: %w", err)
	}
	return ctx.Err()
}

// hashCodeExecArtifact 以 MaxArtifactBytes+1 为唯一读取上界，并固定文件身份直到哈希完成。
func hashCodeExecArtifact(
	ctx context.Context,
	root *os.Root,
	name string,
	before os.FileInfo,
	maxArtifactBytes int64,
) (_ int64, _ string, returnErr error) {
	if before.Size() < 0 || before.Size() > maxArtifactBytes {
		return 0, "", errors.New("artifact exceeds size limit")
	}
	file, err := openCodeExecRegularFileNoFollow(root, name)
	if err != nil {
		return 0, "", err
	}
	defer joinCodeExecResourceClose(&returnErr, file, "close artifact file")
	opened, err := snapshotCodeExecOpenedFile(file)
	if err != nil || !opened.Info.Mode().IsRegular() || !codeExecPathMatchesOpenedSnapshot(before, opened) ||
		opened.Platform.Links != 1 {
		return 0, "", errors.New("artifact changed while opening")
	}
	limited := &io.LimitedReader{R: file, N: maxArtifactBytes + 1}
	hash := sha256.New()
	buffer := make([]byte, 32*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		read, readErr := limited.Read(buffer)
		if read > 0 {
			if _, err := hash.Write(buffer[:read]); err != nil {
				return 0, "", err
			}
			size += int64(read)
			if size > maxArtifactBytes {
				return 0, "", errors.New("artifact exceeds size limit")
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, "", readErr
		}
		if read == 0 {
			return 0, "", io.ErrNoProgress
		}
	}
	if size != opened.Info.Size() {
		return 0, "", errors.New("artifact changed size while reading")
	}
	after, afterErr := snapshotCodeExecOpenedFile(file)
	postPath, pathErr := root.Lstat(name)
	if afterErr != nil || pathErr != nil || !sameCodeExecOpenedFileSnapshot(opened, after) ||
		!codeExecPathMatchesOpenedSnapshot(postPath, after) || postPath.Mode()&os.ModeSymlink != 0 ||
		!postPath.Mode().IsRegular() || after.Platform.Links != 1 {
		return 0, "", errors.New("artifact changed while reading")
	}
	return size, hex.EncodeToString(hash.Sum(nil)), nil
}

func runnerForLanguage(language string) (string, string, []string, error) {
	switch normalizeCodeExecLanguage(language) {
	case "python", "python3":
		if runtime.GOOS == "windows" {
			return ".py", "python", nil, nil
		}
		return ".py", "python3", nil, nil
	case "javascript", "js", "node":
		return ".js", "node", nil, nil
	case "go", "golang":
		return ".go", "go", []string{"run"}, nil
	default:
		return "", "", nil, fmt.Errorf("unsupported language: %s", language)
	}
}

func normalizeCodeExecLanguage(language string) string {
	return strings.ToLower(strings.TrimSpace(language))
}

func normalizeCommandRuntime(command []string) []string {
	if len(command) == 0 {
		return command
	}
	out := append([]string(nil), command...)
	switch out[0] {
	case "python":
		if runtime.GOOS != "windows" {
			out[0] = "python3"
		}
	case "python3":
		if runtime.GOOS == "windows" {
			out[0] = "python"
		}
	}
	return out
}

func languageFromPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		return "python3"
	case ".js", ".mjs", ".cjs":
		return "node"
	case ".go":
		return "go"
	default:
		return ""
	}
}

func inferLanguageFromWorkspace(ctx context.Context, dir string) (string, error) {
	for _, candidate := range []struct {
		metadata  string
		extension string
		language  string
	}{
		{metadata: "go.mod", extension: ".go", language: "go"},
		{metadata: "package.json", extension: ".js", language: "node"},
		{metadata: "pyproject.toml", extension: ".py", language: "python3"},
	} {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if fileExists(filepath.Join(dir, candidate.metadata)) {
			return candidate.language, nil
		}
		first, err := firstFileWithExt(ctx, dir, candidate.extension)
		if err != nil {
			return "", err
		}
		if first != "" {
			return candidate.language, nil
		}
	}
	return "", nil
}

func inferLanguageFromCode(code string) string {
	code = strings.TrimSpace(code)
	lower := strings.ToLower(code)
	switch {
	case strings.HasPrefix(code, "package main") || strings.Contains(code, "\npackage main"):
		return "go"
	case strings.Contains(lower, "console.log(") || strings.Contains(lower, "require(") || strings.Contains(lower, "module.exports"):
		return "node"
	case strings.Contains(lower, "import ") ||
		strings.Contains(lower, "from ") ||
		strings.Contains(lower, "print(") ||
		strings.Contains(lower, "def ") ||
		strings.Contains(lower, "with open(") ||
		strings.Contains(lower, "pathlib") ||
		strings.Contains(lower, "__name__"):
		return "python3"
	default:
		return ""
	}
}

// authorizeCodeExecHostPaths 集中裁决触达宿主机文件系统的 code_exec 请求：
//   - mode=file：显式 entrypoint（绝对或相对的既存文件）必须落在 broker 授权目录内；
//   - mode=project：解析后的 project_root 必须落在 broker 授权目录内。
//
// 沙箱自身的 workspace（per-run 隔离目录）视为始终允许。broker 为 nil 时不额外裁决
// （仅限未接线的嵌入/测试场景；生产装配始终注入 broker）。未授权路径一律拒绝（fail-closed）。
func authorizeCodeExecHostPaths(broker *FileAccessBroker, workspace string, req codeExecRequest) error {
	if broker == nil {
		return nil
	}
	switch req.Mode {
	case "file":
		entry := strings.TrimSpace(req.EntryPoint)
		if entry == "" {
			return nil // 无显式入口 → 由 run workspace 推断，天然受限于 workspace
		}
		return authorizeCodeExecPath(broker, workspace, entry)
	case "project":
		root, err := resolveProjectRoot(req.ProjectRoot)
		if err != nil {
			return err
		}
		return authorizeCodeExecPath(broker, workspace, root)
	}
	return nil
}

// authorizeCodeExecPath 判定单个宿主路径是否可交给沙箱：workspace 子路径直接放行，
// 其余复用 FileAccessBroker 的 allow-list（含归一化 + symlink 安全判定）。
func authorizeCodeExecPath(broker *FileAccessBroker, workspace, path string) error {
	if workspace != "" && pathWithinResolved(workspace, path) {
		return nil
	}
	if _, err := broker.authorizeExisting(path); err != nil {
		return fmt.Errorf("code_exec: path not authorized for execution: %w", err)
	}
	return nil
}

// pathWithinResolved 在归一化 + symlink 解析后判定 target 是否落在 root 之内。
func pathWithinResolved(root, target string) bool {
	return isPathInside(resolveRealPath(root), resolveRealPath(target))
}

func resolveRealPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	return filepath.Clean(p)
}

func resolveProjectRoot(raw string) (string, error) {
	if strings.TrimSpace(raw) != "" {
		canonical, err := resolveCodeExecBoundaryPath(raw)
		if err != nil {
			return "", fmt.Errorf("resolve project_root: %w", err)
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return "", fmt.Errorf("stat project_root: %w", err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("project_root must be a directory: %s", raw)
		}
		return canonical, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	canonical, err := resolveCodeExecBoundaryPath(nearestProjectRoot(wd))
	if err != nil {
		return "", fmt.Errorf("resolve project_root: %w", err)
	}
	return canonical, nil
}

func nearestProjectRoot(start string) string {
	dir := filepath.Clean(start)
	for {
		if fileExists(filepath.Join(dir, "go.mod")) ||
			fileExists(filepath.Join(dir, "package.json")) ||
			fileExists(filepath.Join(dir, "pyproject.toml")) ||
			fileExists(filepath.Join(dir, ".git")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start
		}
		dir = parent
	}
}

func codeExecNeedsGoRuntime(req codeExecRequest) bool {
	usesGo, err := codeExecGoExecutionIntent(req)
	return err == nil && usesGo
}

func codeExecGoExecutionIntent(req codeExecRequest) (bool, error) {
	language := normalizeCodeExecLanguage(req.Language)
	explicitGo := language == "go" || language == "golang"
	if explicitGo && strings.TrimSpace(req.CommandText) != "" {
		return false, errors.New("go execution accepts only structured direct go argv")
	}
	if len(req.Command) == 0 {
		return explicitGo, nil
	}
	first := strings.TrimSpace(req.Command[0])
	directCandidate := codeExecLiteralCommand(first, "go", "go.exe")
	goPathCandidate := codeExecCommandBase(first) == "go" || codeExecCommandBase(first) == "go.exe"
	if !explicitGo && !directCandidate && !goPathCandidate {
		return false, nil
	}
	if _, err := codeExecDirectGoCommandIndex(req.Command); err != nil {
		return false, err
	}
	return true, nil
}

func validateCodeExecProjectRuntimeMetadata(req codeExecRequest, projectRoot string) error {
	language := normalizeCodeExecLanguage(req.Language)
	if (language != "" && language != "go" && language != "golang") || !codeExecProjectHasGoMetadata(projectRoot) {
		return nil
	}
	if strings.TrimSpace(req.CommandText) == "" && len(req.Command) == 0 {
		return nil
	}
	usesGo, err := codeExecGoExecutionIntent(req)
	if err != nil {
		return err
	}
	if usesGo {
		return nil
	}
	return errors.New("go execution for projects accepts only structured direct go argv; declare a non-Go language for a different runtime")
}

func codeExecProjectHasGoMetadata(projectRoot string) bool {
	projectRoot = resolveRealPath(projectRoot)
	if fileExists(filepath.Join(projectRoot, "go.work")) {
		return true
	}
	for dir := projectRoot; ; dir = filepath.Dir(dir) {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
	}
}

func codeExecCommandBase(command string) string {
	return strings.ToLower(filepath.Base(strings.Trim(command, `"'`)))
}

func codeExecMayNeedGoRuntime(req codeExecRequest, projectRoot string) bool {
	if codeExecNeedsGoRuntime(req) {
		return true
	}
	switch req.Mode {
	case "project":
		return len(req.Command) == 0 && req.CommandText == "" && fileExists(filepath.Join(projectRoot, "go.mod"))
	case "module":
		if normalizeCodeExecLanguage(req.Language) != "" {
			return false
		}
		if inferLanguageFromCode(req.Code) == "go" {
			return true
		}
		for _, f := range req.Files {
			name := filepath.Base(filepath.Clean(f.Path))
			if name == "go.mod" || strings.HasSuffix(name, ".go") {
				return true
			}
		}
	}
	return false
}

func hostGoModCachePath() string {
	if gomod := strings.TrimSpace(os.Getenv("GOMODCACHE")); gomod != "" {
		return cleanExistingHostPath(gomod)
	}
	for _, gp := range filepath.SplitList(os.Getenv("GOPATH")) {
		if strings.TrimSpace(gp) != "" {
			return cleanExistingHostPath(filepath.Join(gp, "pkg", "mod"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return cleanExistingHostPath(filepath.Join(home, "go", "pkg", "mod"))
	}
	return ""
}

// protectCodeExecHostGoCaches 即使上层配置放行了 HOME，也显式拒绝最终 Go 进程枚举宿主缓存。
func protectCodeExecHostGoCaches(cfg sandbox.Config, workspace string) (sandbox.Config, error) {
	workspace, err := resolveCodeExecBoundaryPath(workspace)
	if err != nil {
		return sandbox.Config{}, errors.New("resolve final Go workspace boundary failed")
	}
	denied := append([]string(nil), cfg.DeniedPaths...)
	for _, cachePath := range codeExecExistingHostGoCachePaths() {
		if pathWithinResolved(cachePath, workspace) || pathWithinResolved(workspace, cachePath) {
			return sandbox.Config{}, errors.New("host Go cache and final workspace must not overlap")
		}
		denied = append(denied, cachePath)
	}
	canonicalDenied, err := canonicalCodeExecPaths(denied)
	if err != nil {
		return sandbox.Config{}, errors.New("resolve host Go cache boundary failed")
	}
	cfg.DeniedPaths = canonicalDenied
	return cfg, nil
}

func codeExecExistingHostGoCachePaths() []string {
	var candidates []string
	appendPath := func(path string) {
		path = strings.TrimSpace(path)
		if path != "" && !strings.EqualFold(path, "off") {
			candidates = append(candidates, path)
		}
	}
	appendPath(os.Getenv("GOMODCACHE"))
	appendPath(os.Getenv("GOCACHE"))
	for _, goPath := range filepath.SplitList(os.Getenv("GOPATH")) {
		if strings.TrimSpace(goPath) == "" {
			continue
		}
		appendPath(filepath.Join(goPath, "pkg", "mod"))
		appendPath(filepath.Join(goPath, "pkg", "sumdb"))
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		appendPath(filepath.Join(home, "go", "pkg", "mod"))
		appendPath(filepath.Join(home, "go", "pkg", "sumdb"))
		appendPath(filepath.Join(home, ".cache", "go-build"))
		appendPath(filepath.Join(home, "Library", "Caches", "go-build"))
	}
	if cacheHome := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); cacheHome != "" {
		appendPath(filepath.Join(cacheHome, "go-build"))
	}
	if localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localAppData != "" {
		appendPath(filepath.Join(localAppData, "go-build"))
	}
	var existing []string
	for _, candidate := range compactCleanPaths(candidates) {
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		canonical, err := canonicalCodeExecPath(candidate)
		if err == nil {
			existing = append(existing, canonical)
		}
	}
	return compactCleanPaths(existing)
}

func cleanExistingHostPath(path string) string {
	clean := filepath.Clean(path)
	if real, err := filepath.EvalSymlinks(clean); err == nil {
		return real
	}
	return clean
}

// canonicalCodeExecPaths 为安全配置生成稳定、去重的绝对路径集合。
func canonicalCodeExecPaths(paths []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		canonical, err := canonicalCodeExecPath(path)
		if err != nil {
			return nil, err
		}
		key := canonical
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, canonical)
	}
	return out, nil
}

func compactCleanPaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if canonical, err := canonicalCodeExecPath(p); err == nil {
			p = canonical
		} else if abs, absErr := filepath.Abs(p); absErr == nil {
			p = filepath.Clean(abs)
		}
		key := p
		if runtime.GOOS == "windows" {
			key = strings.ToLower(key)
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, p)
		}
	}
	return out
}

func cleanRunRelativePath(path string) (string, error) {
	rel := filepath.Clean(strings.TrimSpace(path))
	if rel == "" || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("file path must be relative and stay inside run workspace: %s", path)
	}
	return rel, nil
}

func detectRuntimeMissing(command []string, result *sandbox.ExecResult, execErr error) bool {
	text := strings.ToLower(strings.Join(command, " ") + "\n" + execText(result, execErr))
	markers := []string{
		"executable file not found",
		"no such file or directory",
		"command not found",
		"not found",
		"filenotfounderror",
		"cannot find module",
	}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func execText(result *sandbox.ExecResult, execErr error) string {
	var b strings.Builder
	if result != nil {
		b.WriteString(result.Stdout)
		b.WriteString("\n")
		b.WriteString(result.Stderr)
	}
	if execErr != nil {
		b.WriteString("\n")
		b.WriteString(execErr.Error())
	}
	return b.String()
}

// codeExecDirSizeContext 在遍历每个目录项时响应取消，避免计量越过执行预算。
func codeExecDirSizeContext(ctx context.Context, root string) (int64, error) {
	return codeExecDirSizeContextObserved(ctx, root, nil)
}

func codeExecDirSizeContextObserved(ctx context.Context, root string, readDirObserver func()) (int64, error) {
	return codeExecDirSizeContextWithLimits(
		ctx,
		root,
		defaultCodeExecTraversalLimits(0),
		readDirObserver,
	)
}

// codeExecGoVendorDirSizeContext 使用适合大型 Go 依赖闭包的独立硬预算，
// 不改变普通产物和工作区遍历的更小默认上限。
func codeExecGoVendorDirSizeContext(ctx context.Context, root string) (int64, error) {
	limits := defaultCodeExecTraversalLimits(0)
	limits.MaxFiles = codeExecGoVendorMaxFiles
	limits.MaxDirectories = codeExecGoVendorMaxDirectories
	limits.MaxEntries = codeExecGoVendorMaxEntries
	return codeExecDirSizeContextWithLimits(ctx, root, limits, nil)
}

func codeExecDirSizeContextWithLimits(
	ctx context.Context,
	root string,
	limits codeExecTraversalLimits,
	readDirObserver func(),
) (_ int64, returnErr error) {
	if ctx == nil {
		return 0, errors.New("workspace measurement context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var total int64
	rootHandle, rootInfo, rootErr := openCodeExecRootNoFollow(root)
	if rootErr != nil {
		if os.IsNotExist(rootErr) {
			return 0, nil
		}
		return 0, rootErr
	}
	defer joinCodeExecResourceClose(&returnErr, rootHandle, "close workspace root")
	budget := &codeExecTraversalBudget{Limits: limits, ReadObserver: readDirObserver}
	if budgetErr := budget.addDirectory("", 0); budgetErr != nil {
		return 0, budgetErr
	}
	measureErr := measureCodeExecDirectory(ctx, rootHandle, "", 0, budget, &total)
	if measureErr != nil {
		return total, measureErr
	}
	afterRoot, statErr := rootHandle.Stat(".")
	postRoot, pathErr := os.Lstat(root)
	if statErr != nil || pathErr != nil || postRoot.Mode()&os.ModeSymlink != 0 || !postRoot.IsDir() ||
		!sameCodeExecFileSnapshot(rootInfo, afterRoot) || !sameCodeExecFileSnapshot(afterRoot, postRoot) {
		return total, errors.New("workspace changed while measuring")
	}
	return total, ctx.Err()
}

func measureCodeExecDirectory(
	ctx context.Context,
	root *os.Root,
	prefix string,
	depth int,
	budget *codeExecTraversalBudget,
	total *int64,
) (returnErr error) {
	directory, openedDirectory, err := openCodeExecDirectoryStream(root)
	if err != nil {
		return err
	}
	defer joinCodeExecResourceClose(&returnErr, directory, "close workspace directory stream")
	for {
		entries, readErr := readCodeExecDirectoryBatch(ctx, directory, budget)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			path := entry.Name()
			if prefix != "" {
				path = prefix + "/" + entry.Name()
			}
			entryDepth := depth + 1
			info, err := root.Lstat(entry.Name())
			if err != nil {
				return err
			}
			if info.IsDir() {
				if err := budget.addDirectory(path, entryDepth); err != nil {
					return err
				}
				child, err := root.OpenRoot(entry.Name())
				if err != nil {
					return err
				}
				walkErr := measureCodeExecDirectory(ctx, child, path, entryDepth, budget, total)
				closeErr := child.Close()
				if walkErr != nil {
					return walkErr
				}
				if closeErr != nil {
					return closeErr
				}
				continue
			}
			if err := budget.addFile(path, entryDepth, info.Size()); err != nil {
				return err
			}
			*total += info.Size()
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	return verifyCodeExecDirectoryStream(root, directory, openedDirectory)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstFileWithExt(ctx context.Context, root, ext string) (string, error) {
	if ctx == nil {
		return "", errors.New("file discovery context must not be nil")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || found != "" {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ext) {
			rel, err := filepath.Rel(root, path)
			if err == nil {
				found = filepath.ToSlash(rel)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return found, ctx.Err()
}

func commandExists(name string) bool {
	if strings.ContainsRune(name, os.PathSeparator) {
		_, err := os.Stat(name)
		return err == nil
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func ensureCodeExecConfigDefaults(cfg sandbox.Config) sandbox.Config {
	// 直接字段读写 sandbox.Config：编译器守契约，字段改名会编译错，
	// 不再走 reflect.FieldByName 在 GOWORK=off/发版构建下静默跳过导致限额蒸发。
	if cfg.Timeout <= 0 {
		cfg.Timeout = codeExecDefaultTimeoutSeconds
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 64 * 1024
	}
	if cfg.MaxStderrBytes <= 0 {
		cfg.MaxStderrBytes = 64 * 1024
	}
	// 默认遮蔽宿主机 secrets：即使 project 模式把 $HOME 当 workspace 放行，这些路径
	// 仍被沙箱 deny 规则（darwin deny-after-allow / linux 掩蔽）优先遮蔽。合并而非覆盖，
	// 保留调用方自定义的 DeniedPaths。
	cfg.DeniedPaths = mergeDeniedPaths(cfg.DeniedPaths, defaultSandboxDeniedPaths())
	return withCodeExecRequiredCapabilities(cfg)
}

func codeExecRequestTimeout(configuredSeconds, requestedSeconds int) time.Duration {
	seconds := configuredSeconds
	if seconds <= 0 {
		seconds = codeExecDefaultTimeoutSeconds
	}
	if requestedSeconds > 0 && requestedSeconds < seconds {
		seconds = requestedSeconds
	}
	return time.Duration(seconds) * time.Second
}

// withCodeExecRequiredCapabilities 由最终配置统一派生 CodeExec 的能力合同。
func withCodeExecRequiredCapabilities(cfg sandbox.Config) sandbox.Config {
	// 资源位必须完全由最终正值限额重建；保留非资源附加要求，但不能继承过期资源位。
	// 用户代码始终进入不可信执行档案，不能从调用方或构建阶段继承 TrustedBuild。
	cfg.ExecutionProfile = sandbox.ExecutionProfileUntrusted
	resourceCapabilities := sandbox.CapabilityMemory |
		sandbox.CapabilityProcesses |
		sandbox.CapabilityStorage
	required := cfg.RequiredCapabilities &^ resourceCapabilities
	required |= sandbox.UntrustedCodeIsolationCapabilities
	if cfg.MaxMemoryBytes > 0 {
		required |= sandbox.CapabilityMemory
	}
	if cfg.MaxProcesses > 0 {
		required |= sandbox.CapabilityProcesses
	}
	if cfg.MaxWorkspaceBytes > 0 || cfg.MaxArtifactBytes > 0 {
		required |= sandbox.CapabilityStorage
	}
	cfg.RequiredCapabilities = required
	return cfg
}

// withCodeExecCommandRequiredCapabilities 在创建沙箱前把命令自身需要的能力并入最终合同。
func withCodeExecCommandRequiredCapabilities(
	cfg sandbox.Config,
	goos string,
	command []string,
) (sandbox.Config, error) {
	required, err := codeExecCommandRequiredCapabilities(goos, command)
	if err != nil {
		return sandbox.Config{}, err
	}
	cfg.RequiredCapabilities |= required
	return withCodeExecRequiredCapabilities(cfg), nil
}

// codeExecCommandRequiredCapabilities 识别会派生子进程的 Node 项目命令。
// macOS 不可信执行档案明确禁止派生，因此必须在载荷启动前拒绝，绝不切换可信构建档案。
func codeExecCommandRequiredCapabilities(goos string, command []string) (sandbox.CapabilitySet, error) {
	if !codeExecNodeProjectCommandNeedsProcessCreation(command) {
		return 0, nil
	}
	if strings.EqualFold(strings.TrimSpace(goos), "darwin") {
		return 0, fmt.Errorf(
			"macOS Node project commands are unsupported in the untrusted sandbox because process creation is unavailable: %w",
			sandbox.ErrRequiredCapabilitiesUnavailable,
		)
	}
	return sandbox.CapabilityProcessCreation, nil
}

// validateCodeExecDynamicCommandCapabilities 阻止运行期补救路径绕过 macOS 命令能力前置检查。
func validateCodeExecDynamicCommandCapabilities(goos string, command []string) error {
	_, err := codeExecCommandRequiredCapabilities(goos, command)
	return err
}

func codeExecNodeProjectCommandNeedsProcessCreation(command []string) bool {
	index := codeExecCommandPayloadIndex(command)
	if index < 0 || index >= len(command) {
		return false
	}
	name := codeExecPortableCommandBase(command[index])
	switch name {
	case "npm", "npx", "pnpm", "pnpx", "yarn", "yarnpkg", "corepack":
		return true
	case "node":
		for _, argument := range command[index+1:] {
			argument = strings.TrimSpace(argument)
			if argument == "--" {
				break
			}
			if argument == "--test" || strings.HasPrefix(argument, "--test=") {
				return true
			}
		}
	}
	return false
}

func codeExecCommandPayloadIndex(command []string) int {
	if len(command) == 0 {
		return -1
	}
	if codeExecPortableCommandBase(command[0]) != "env" {
		return 0
	}
	for index := 1; index < len(command); index++ {
		argument := strings.TrimSpace(command[index])
		if argument == "" || strings.HasPrefix(argument, "-") {
			continue
		}
		if codeExecEnvironmentAssignment(argument) {
			continue
		}
		return index
	}
	return -1
}

func codeExecPortableCommandBase(command string) string {
	command = strings.TrimSpace(command)
	if separator := strings.LastIndexAny(command, `/\`); separator >= 0 {
		command = command[separator+1:]
	}
	command = strings.ToLower(command)
	for _, extension := range []string{".exe", ".cmd", ".bat"} {
		command = strings.TrimSuffix(command, extension)
	}
	return command
}

// defaultSandboxDeniedPaths 返回 code_exec 沙箱默认应遮蔽的 secrets 路径集合。
//
// 覆盖两类：①常见外部凭据目录（~/.ssh、~/.aws、~/.config/gcloud、~/.gnupg）；
// ②本应用自身的密钥/凭据落盘——主密钥 master.key（可解密全部静态加密凭据）、
// 配置文件 hexclaw.yaml（provider API key / app secret）、加密数据库 data.db
// （实例/连接器凭据）。集中在此单一函数，避免散落多处。
func defaultSandboxDeniedPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return nil
	}
	appDir := filepath.Join(home, ".hexclaw")
	return []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".config", "gcloud"),
		filepath.Join(home, ".gnupg"),
		// 本应用自身的密钥/凭据（精确到文件，避免误伤同目录下的沙箱 workspace）。
		filepath.Join(appDir, "master.key"),
		filepath.Join(appDir, "hexclaw.yaml"),
		filepath.Join(appDir, "data.db"),
	}
}

// mergeDeniedPaths 合并两组 deny 路径并去重（保留出现顺序）。
func mergeDeniedPaths(existing, defaults []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range append(append([]string(nil), existing...), defaults...) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func newCodeExecRunID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run_%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("run_%d_%s", time.Now().UnixNano(), hex.EncodeToString(b[:]))
}

func codeExecScratchBase() string {
	if runtime.GOOS == "windows" {
		return os.TempDir()
	}
	return "/tmp"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return v
}

func boolArgDefault(args map[string]any, key string, def bool) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	default:
		return def
	}
}

func intArgDefault(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			return int(i)
		}
	}
	return def
}

func commandArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func firstCommandArg(args map[string]any, keys ...string) ([]string, string) {
	for _, key := range keys {
		if raw, ok := args[key].(string); ok && strings.TrimSpace(raw) != "" {
			if cmd := jsonStringCommandArg(raw); len(cmd) > 0 {
				return cmd, ""
			}
			return nil, strings.TrimSpace(raw)
		}
		if cmd := commandArg(args, key); len(cmd) > 0 {
			return cmd, ""
		}
	}
	return nil, ""
}

func jsonStringCommandArg(raw string) []string {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "[") {
		return nil
	}
	var direct []string
	if err := json.Unmarshal([]byte(raw), &direct); err == nil && len(direct) > 0 {
		return direct
	}
	if cmd := looseJSONStringCommandArg(raw); len(cmd) > 0 {
		return cmd
	}
	var values []any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, item := range values {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func looseJSONStringCommandArg(raw string) []string {
	matches := regexp.MustCompile(`(?s)^\[\s*["']([^"']+)["']\s*,\s*["']([^"']+)["']\s*,\s*["'](.*)["']\s*\]$`).FindStringSubmatch(strings.TrimSpace(raw))
	if len(matches) != 4 {
		return nil
	}
	cmd := strings.TrimSpace(matches[1])
	arg := strings.TrimSpace(matches[2])
	if cmd == "" || arg == "" {
		return nil
	}
	return []string{cmd, arg, unescapeLooseCommandArg(matches[3])}
}

func unescapeLooseCommandArg(raw string) string {
	if unquoted, err := strconv.Unquote(`"` + raw + `"`); err == nil {
		return unquoted
	}
	out := strings.ReplaceAll(raw, `\"`, `"`)
	out = strings.ReplaceAll(out, `\n`, "\n")
	out = strings.ReplaceAll(out, `\t`, "\t")
	out = strings.ReplaceAll(out, `\\`, `\`)
	return out
}

func filesArg(args map[string]any, key string) []codeExecInputFile {
	switch v := args[key].(type) {
	case []codeExecInputFile:
		return append([]codeExecInputFile(nil), v...)
	case []map[string]any:
		files := make([]codeExecInputFile, 0, len(v))
		for _, item := range v {
			files = append(files, codeExecInputFile{
				Path:    stringArg(item, "path"),
				Content: stringArg(item, "content"),
			})
		}
		return files
	case []any:
		files := make([]codeExecInputFile, 0, len(v))
		for _, raw := range v {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			files = append(files, codeExecInputFile{
				Path:    stringArg(item, "path"),
				Content: stringArg(item, "content"),
			})
		}
		return files
	default:
		return nil
	}
}

func looksLikeExistingPath(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, "\n\r") {
		return false
	}
	if !filepath.IsAbs(s) {
		if abs, err := filepath.Abs(s); err == nil {
			s = abs
		}
	}
	info, err := os.Stat(s)
	return err == nil && !info.IsDir()
}

// Python: "ModuleNotFoundError: No module named 'pandas'"
var pyModuleNotFound = regexp.MustCompile(`(?:ModuleNotFoundError|ImportError):\s+No module named '([^']+)'`)

// Node: "Cannot find module 'lodash'"
var nodeModuleNotFound = regexp.MustCompile(`Cannot find module '([^']+)'`)

func detectMissingPackages(language, stderr string) []string {
	var re *regexp.Regexp
	switch normalizeCodeExecLanguage(language) {
	case "python", "python3":
		re = pyModuleNotFound
	case "javascript", "node", "js":
		re = nodeModuleNotFound
	default:
		return nil
	}

	matches := re.FindAllStringSubmatch(stderr, -1)
	seen := make(map[string]bool)
	var pkgs []string
	for _, m := range matches {
		if len(m) >= 2 {
			pkg := strings.Split(m[1], ".")[0] // "foo.bar" → "foo"
			if pkg == "" || !isSafePackageName(pkg) || seen[pkg] {
				continue
			}
			seen[pkg] = true
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs
}

func buildInstallCommand(language string, pkgs []string) []string {
	switch normalizeCodeExecLanguage(language) {
	case "python", "python3":
		return append([]string{"python3", "-m", "pip", "install"}, pkgs...)
	case "javascript", "node", "js":
		return append([]string{"npm", "install", "--no-save"}, pkgs...)
	default:
		return nil
	}
}

func isSafePackageName(pkg string) bool {
	for _, r := range pkg {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}
