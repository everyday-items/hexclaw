package builtin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
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
	mu             sync.RWMutex
	sb             sandbox.Sandbox
	cfg            sandbox.Config // 保留配置以支持热更新
	sandboxFactory func(sandbox.Config) (sandbox.Sandbox, error)
	// fileAccess 集中裁决触达宿主机文件系统的 code_exec 请求：mode=file 的入口文件、
	// mode=project 的项目根（及要放行的父目录）在读取/授予前必须落在 broker 的 allow-list
	// 内，否则一律拒绝执行（fail-closed）。nil 时不额外裁决（仅限未接线的嵌入/测试场景）。
	fileAccess *FileAccessBroker
}

// SetFileAccess 注入集中文件访问裁决器（FileAccessBroker）。
//
// code_exec 的 mode=file 入口文件、mode=project 项目根（含要加入沙箱只读放行的父目录）
// 在读取/授予前必须过 broker 的 allow-list 授权；未授权路径一律拒绝执行（fail-closed）。
// 若 broker 允许集为空（用户未授权任何目录），project_root=$HOME 之类必被拒。
func (s *CodeExecSkill) SetFileAccess(b *FileAccessBroker) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fileAccess = b
}

func (s *CodeExecSkill) broker() *FileAccessBroker {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.fileAccess
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

type codeExecRun struct {
	ID           string
	Base         string
	Root         string
	Workspace    string
	Scratch      string
	ArtifactDir  string
	LogDir       string
	CacheDir     string
	ProjectRoot  string
	ManifestPath string
	Config       sandbox.Config
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
	// FilesystemIsolation 如实反映本次执行的文件系统隔离强度（enforced/weak/unsupported），
	// 来自沙箱后端上报的 ExecResult.Limits.Filesystem，而非硬编码常量。
	FilesystemIsolation string             `json:"filesystem_isolation,omitempty"`
	FilesystemDegraded  bool               `json:"filesystem_degraded,omitempty"`
	Paths               map[string]string  `json:"paths"`
	Artifacts           []codeExecArtifact `json:"artifacts"`
	Capabilities        map[string]any     `json:"capabilities"`
}

// NewCodeExecSkill 创建代码执行 Skill
func NewCodeExecSkill(sb sandbox.Sandbox, cfg sandbox.Config) *CodeExecSkill {
	return &CodeExecSkill{sb: sb, cfg: cfg, sandboxFactory: sandbox.New}
}

// UpdateNetwork 热更新沙箱网络策略。重建沙箱实例。
func (s *CodeExecSkill) UpdateNetwork(enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Network == enabled {
		return nil // 没变化
	}
	nextCfg := s.cfg
	nextCfg.Network = enabled
	newSb, err := s.buildSandboxLocked(nextCfg)
	if err != nil {
		return fmt.Errorf("rebuild sandbox failed: %w", err)
	}
	s.cfg = nextCfg
	s.sb = newSb
	return nil
}

// NetworkEnabled 返回当前网络策略。
func (s *CodeExecSkill) NetworkEnabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Network
}

// UpdateReadablePaths hot-updates extra read-only host paths granted to code_exec.
func (s *CodeExecSkill) UpdateReadablePaths(paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	nextPaths := append([]string(nil), paths...)
	if slices.Equal(s.cfg.ReadablePaths, nextPaths) {
		return nil
	}
	nextCfg := s.cfg
	nextCfg.ReadablePaths = nextPaths
	newSb, err := s.buildSandboxLocked(nextCfg)
	if err != nil {
		return fmt.Errorf("rebuild sandbox failed: %w", err)
	}
	s.cfg = nextCfg
	s.sb = newSb
	return nil
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
				Description: "Runtime language for snippet/module/file execution: python, python3, javascript, js, node, go, golang. Required when mode=snippet.",
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
				Description: "Command argv for mode=project/file/module, for example ['go','test','./...'] or ['node','scripts/check.js'].",
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
				Description: "Whether to scan artifacts/ and include artifact manifest entries. Defaults true.",
			},
			"timeout": {
				Type:        "integer",
				Description: "Optional per-run timeout in seconds. Defaults to sandbox config timeout.",
			},
		},
	})
}

func (s *CodeExecSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	req, err := parseCodeExecRequest(args)
	if err != nil {
		return nil, err
	}

	cfg, factory := s.snapshot()
	broker := s.broker()
	// P0 收口：mode=file/project 触达宿主机的路径在读取/授予前必须过集中裁决（fail-closed）。
	if err := authorizeCodeExecHostPaths(broker, cfg.Workspace, req); err != nil {
		return nil, err
	}
	run, err := prepareCodeExecRun(cfg, req, broker)
	if err != nil {
		return nil, err
	}
	sb, err := factory(run.Config)
	if err != nil {
		return nil, fmt.Errorf("create sandbox run %s failed: %w", run.ID, err)
	}

	command, err := prepareCodeExecCommand(req, run)
	if err != nil {
		return nil, err
	}

	result, execErr := runSandboxCommand(ctx, sb, run, command)
	missingDeps := detectMissingPackages(req.Language, execText(result, execErr))
	if run.Config.Network && result != nil && result.ExitCode != 0 && len(missingDeps) > 0 {
		if installCmd := buildInstallCommand(req.Language, missingDeps); installCmd != "" {
			installResult, installErr := runSandboxCommand(ctx, sb, run, []string{"sh", "-c", installCmd})
			if installErr == nil && installResult != nil && installResult.ExitCode == 0 {
				result, execErr = runSandboxCommand(ctx, sb, run, command)
			}
		}
	}

	report := buildCodeExecReport(req, run, command, result, execErr, missingDeps)
	if err := finalizeCodeExecReport(run, &report); err != nil && report.Error == "" {
		report.Error = err.Error()
		if report.Status == "success" {
			report.Status = "failed"
		}
	}
	if err := writeCodeExecManifest(run, report); err != nil && report.Error == "" {
		report.Error = err.Error()
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

func (s *CodeExecSkill) snapshot() (sandbox.Config, func(sandbox.Config) (sandbox.Sandbox, error)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cfg := s.cfg
	cfg.ReadablePaths = append([]string(nil), s.cfg.ReadablePaths...)
	factory := s.sandboxFactory
	if factory == nil {
		factory = sandbox.New
	}
	return cfg, factory
}

func (s *CodeExecSkill) buildSandboxLocked(cfg sandbox.Config) (sandbox.Sandbox, error) {
	factory := s.sandboxFactory
	if factory == nil {
		factory = sandbox.New
	}
	return factory(cfg)
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
		Artifacts: boolArgDefault(args, "artifacts", true),
	}
	req.Command, req.CommandText = firstCommandArg(args, "command", "cmd", "argv", "args")
	req.Files = filesArg(args, "files")
	if req.EntryPoint == "" && req.Code != "" && looksLikeExistingPath(req.Code) {
		req.EntryPoint = strings.TrimSpace(req.Code)
		req.Code = ""
	}
	if req.CommandText == "" && len(req.Command) == 0 && req.Mode == "project" && strings.TrimSpace(req.Code) != "" {
		req.CommandText = strings.TrimSpace(req.Code)
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

func prepareCodeExecRun(cfg sandbox.Config, req codeExecRequest, broker *FileAccessBroker) (codeExecRun, error) {
	if cfg.Workspace == "" {
		return codeExecRun{}, fmt.Errorf("sandbox workspace is required")
	}
	base := filepath.Clean(cfg.Workspace)
	if err := os.MkdirAll(base, 0755); err != nil {
		return codeExecRun{}, fmt.Errorf("create sandbox base workspace: %w", err)
	}
	runID := newCodeExecRunID()
	root := filepath.Join(base, "runs", runID)
	workspace := filepath.Join(root, "work")
	scratch := workspace
	projectRoot := ""

	if req.Mode == "project" {
		var err error
		projectRoot, err = resolveProjectRoot(req.ProjectRoot)
		if err != nil {
			return codeExecRun{}, err
		}
		workspace = projectRoot
		scratch = filepath.Join(codeExecScratchBase(), "hexclaw-sandbox-runs", runID)
	}

	run := codeExecRun{
		ID:           runID,
		Base:         base,
		Root:         root,
		Workspace:    workspace,
		Scratch:      scratch,
		ArtifactDir:  filepath.Join(scratch, "artifacts"),
		LogDir:       filepath.Join(root, "logs"),
		CacheDir:     filepath.Join(scratch, "cache"),
		ProjectRoot:  projectRoot,
		ManifestPath: filepath.Join(root, "manifest.json"),
		Config:       cfg,
	}
	for _, dir := range []string{run.Root, run.Scratch, run.ArtifactDir, run.LogDir, run.CacheDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return codeExecRun{}, fmt.Errorf("create run dir %s: %w", dir, err)
		}
	}

	run.Config.Workspace = run.Workspace
	if req.Timeout > 0 {
		run.Config.Timeout = req.Timeout
	}
	run.Config = ensureCodeExecConfigDefaults(run.Config)
	run.Config.ReadablePaths = append([]string(nil), cfg.ReadablePaths...)
	run.Config.ReadablePaths = append(run.Config.ReadablePaths, base)
	if req.Mode == "project" {
		run.Config.ReadablePaths = append(run.Config.ReadablePaths, projectReadablePaths(projectRoot, broker)...)
	}
	if codeExecNeedsGoRuntime(req) {
		run.Config.ReadablePaths = append(run.Config.ReadablePaths, goRuntimeReadablePaths()...)
	}
	run.Config.ReadablePaths = compactCleanPaths(run.Config.ReadablePaths)
	return run, nil
}

func prepareCodeExecCommand(req codeExecRequest, run codeExecRun) ([]string, error) {
	if req.CommandText != "" {
		return []string{"sh", "-c", req.CommandText}, nil
	}
	if len(req.Command) > 0 {
		return normalizeCommandRuntime(req.Command), nil
	}

	switch req.Mode {
	case "snippet":
		return prepareSnippetCommand(req, run)
	case "file":
		return prepareFileCommand(req, run)
	case "module":
		return prepareModuleCommand(req, run)
	case "project":
		return defaultProjectCommand(run.ProjectRoot)
	default:
		return nil, fmt.Errorf("command is required for mode=%s", req.Mode)
	}
}

func prepareSnippetCommand(req codeExecRequest, run codeExecRun) ([]string, error) {
	ext, cmd, args, err := runnerForLanguage(req.Language)
	if err != nil {
		return nil, err
	}
	fileName := "_hexclaw_exec" + ext
	if err := os.WriteFile(filepath.Join(run.Workspace, fileName), []byte(req.Code), 0644); err != nil {
		return nil, fmt.Errorf("write snippet: %w", err)
	}
	return append([]string{cmd}, append(args, fileName)...), nil
}

func prepareFileCommand(req codeExecRequest, run codeExecRun) ([]string, error) {
	entry := req.EntryPoint
	if entry == "" {
		entry = inferRunnableFile(run.Base, req.Language)
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

func prepareModuleCommand(req codeExecRequest, run codeExecRun) ([]string, error) {
	for _, f := range req.Files {
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
		req.Language = inferLanguageFromWorkspace(run.Workspace)
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
		if first := firstFileWithExt(run.Workspace, ".py"); first != "" {
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
		if first := firstFileWithExt(run.Workspace, ".js"); first != "" {
			return []string{"node", first}, nil
		}
	}
	return nil, fmt.Errorf("cannot infer module runner; provide language or command")
}

func defaultProjectCommand(projectRoot string) ([]string, error) {
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
		if first := inferRunnableFile(projectRoot, ""); first != "" {
			req := codeExecRequest{EntryPoint: first, Language: languageFromPath(first)}
			run := codeExecRun{Workspace: projectRoot}
			return prepareFileCommand(req, run)
		}
		return nil, fmt.Errorf("command is required for mode=project")
	}
}

func inferRunnableFile(root, language string) string {
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
		if first := firstFileWithExt(root, ext); first != "" {
			return filepath.Join(root, first)
		}
	}
	return ""
}

func runSandboxCommand(ctx context.Context, sb sandbox.Sandbox, run codeExecRun, command []string) (*sandbox.ExecResult, error) {
	exports := codeExecEnv(run)
	if err := ensureCodeExecEnvDirs(exports); err != nil {
		return nil, err
	}
	if runtime.GOOS == "windows" {
		return runWindowsSandboxCommand(ctx, sb, command, exports)
	}
	return runPosixSandboxCommand(ctx, sb, command, exports)
}

func codeExecEnv(run codeExecRun) map[string]string {
	exports := map[string]string{
		"HEXCLAW_RUN_ID":       run.ID,
		"HEXCLAW_WORKSPACE":    run.Scratch,
		"HEXCLAW_ARTIFACT_DIR": run.ArtifactDir,
		"TMPDIR":               filepath.Join(run.Scratch, "tmp"),
		"TMP":                  filepath.Join(run.Scratch, "tmp"),
		"TEMP":                 filepath.Join(run.Scratch, "tmp"),
		"GOCACHE":              filepath.Join(run.CacheDir, "go-build"),
		"PIP_CACHE_DIR":        filepath.Join(run.CacheDir, "pip"),
		"PYTHONPYCACHEPREFIX":  filepath.Join(run.CacheDir, "pycache"),
		"npm_config_cache":     filepath.Join(run.CacheDir, "npm"),
	}
	if run.Config.Network {
		exports["GOMODCACHE"] = filepath.Join(run.CacheDir, "gomod")
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
}

var codeExecUnsetEnvKeys = []string{
	// Host workspaces must not leak into isolated code_exec runs. Leaving
	// GOWORK set makes a temp Go module try to load the developer's repo
	// workspace, which either fails the sandbox boundary or breaks hermeticity.
	"GOWORK",
}

func ensureCodeExecEnvDirs(exports map[string]string) error {
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
	return nil
}

func runPosixSandboxCommand(ctx context.Context, sb sandbox.Sandbox, command []string, exports map[string]string) (*sandbox.ExecResult, error) {
	var script strings.Builder
	for _, k := range codeExecUnsetEnvKeys {
		script.WriteString("unset ")
		script.WriteString(k)
		script.WriteString("\n")
	}
	keys := make([]string, 0, len(exports))
	for k := range exports {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		script.WriteString("export ")
		script.WriteString(k)
		script.WriteString("=")
		script.WriteString(shellQuote(exports[k]))
		script.WriteString("\n")
	}
	script.WriteString("exec ")
	script.WriteString(shellJoin(command))
	return sb.Exec(ctx, "sh", []string{"-c", script.String()})
}

func runWindowsSandboxCommand(ctx context.Context, sb sandbox.Sandbox, command []string, exports map[string]string) (*sandbox.ExecResult, error) {
	var script strings.Builder
	for _, k := range codeExecUnsetEnvKeys {
		script.WriteString("set \"")
		script.WriteString(k)
		script.WriteString("=\"\r\n")
	}
	keys := make([]string, 0, len(exports))
	for k := range exports {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		script.WriteString("set \"")
		script.WriteString(k)
		script.WriteString("=")
		script.WriteString(strings.ReplaceAll(exports[k], `"`, `\"`))
		script.WriteString("\"\r\n")
	}
	script.WriteString(windowsCmdJoin(command))
	return sb.Exec(ctx, "cmd", []string{"/d", "/s", "/c", script.String()})
}

func buildCodeExecReport(req codeExecRequest, run codeExecRun, command []string, result *sandbox.ExecResult, execErr error, missingDeps []string) codeExecReport {
	// 直接字段读写 sandbox.Config：编译器守契约，字段改名/删除会编译错，
	// 不再走反射在发版构建下静默返回 0 导致限额在报告里蒸发。
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
		MaxWorkspaceBytes: run.Config.MaxWorkspaceBytes,
		MaxArtifactBytes:  run.Config.MaxArtifactBytes,
		MaxProcesses:      run.Config.MaxProcesses,
		MaxMemoryBytes:    run.Config.MaxMemoryBytes,
		Paths: map[string]string{
			"run_root":     run.Root,
			"workspace":    run.Scratch,
			"cwd":          run.Workspace,
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
		"platform":          runtime.GOOS,
		"per_run_workspace": true,
		"artifact_manifest": true,
		"process_tree_kill": true,
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
		// （enforced=真 fail-closed；weak/unsupported=降级，不再谎报 true）。
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
		report.Error = "当前环境缺少强文件系统隔离后端（如 linux 缺 bubblewrap），已拒绝执行以防止宿主文件泄露：" + execErr.Error()
	case errors.Is(execErr, sandbox.ErrStorageLimitExceeded):
		// 存储违规归类为「产物超限」而非后端不可用。
		report.Status = "resource_limited"
		report.WorkspaceLimited = true
		report.Error = "沙箱产物/工作区超出存储限额：" + execErr.Error()
	default:
		report.Error = execErr.Error()
		report.Status = "failed"
		if strings.Contains(strings.ToLower(execErr.Error()), "timeout") || strings.Contains(strings.ToLower(execErr.Error()), "deadline") {
			report.Timeout = true
			report.Status = "timeout"
		}
	}
}

func finalizeCodeExecReport(run codeExecRun, report *codeExecReport) error {
	workspaceBytes, err := dirSize(run.Scratch)
	if err != nil {
		return err
	}
	report.WorkspaceBytes = workspaceBytes
	if report.MaxWorkspaceBytes > 0 && workspaceBytes > report.MaxWorkspaceBytes {
		report.WorkspaceLimited = true
		report.Status = "resource_limited"
	}
	artifacts, err := collectCodeExecArtifacts(run.ArtifactDir, report.MaxArtifactBytes)
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
		// 如实标注文件系统隔离降级（weak/unsupported），让上层/模型知晓当前环境
		// 未提供强隔离，敏感任务应改在具备强隔离的环境执行。
		iso := report.FilesystemIsolation
		if iso == "" {
			iso = string(sandbox.LimitStatusUnsupported)
		}
		out.WriteString(fmt.Sprintf("\n[hexclaw] filesystem_isolation=%s (降级：未提供 deny-by-default 强隔离)\n", iso))
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
	if err := os.MkdirAll(filepath.Dir(run.ManifestPath), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(run.ManifestPath, b, 0644)
}

func collectCodeExecArtifacts(dir string, maxArtifactBytes int64) ([]codeExecArtifact, error) {
	artifacts := make([]codeExecArtifact, 0)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return artifacts, nil
		}
		return nil, err
	}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if maxArtifactBytes > 0 && info.Size() > maxArtifactBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		sha := hex.EncodeToString(sum[:])
		artifacts = append(artifacts, codeExecArtifact{
			ID:     "artifact_" + sha[:12],
			Name:   filepath.Base(path),
			Path:   rel,
			Size:   info.Size(),
			SHA256: sha,
			MIME:   mime.TypeByExtension(filepath.Ext(path)),
		})
		return nil
	})
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return artifacts, err
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

func inferLanguageFromWorkspace(dir string) string {
	switch {
	case fileExists(filepath.Join(dir, "go.mod")) || firstFileWithExt(dir, ".go") != "":
		return "go"
	case fileExists(filepath.Join(dir, "package.json")) || firstFileWithExt(dir, ".js") != "":
		return "node"
	case fileExists(filepath.Join(dir, "pyproject.toml")) || firstFileWithExt(dir, ".py") != "":
		return "python3"
	default:
		return ""
	}
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

// brokerAuthorizes 报告 broker 是否授权某既存路径（nil broker → 一律不授权，fail-closed）。
func brokerAuthorizes(broker *FileAccessBroker, path string) bool {
	if broker == nil {
		return false
	}
	_, err := broker.authorizeExisting(path)
	return err == nil
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
		abs, err := filepath.Abs(raw)
		if err != nil {
			return "", fmt.Errorf("resolve project_root: %w", err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return "", fmt.Errorf("stat project_root: %w", err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("project_root must be a directory: %s", raw)
		}
		return abs, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return nearestProjectRoot(wd), nil
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

func projectReadablePaths(projectRoot string, broker *FileAccessBroker) []string {
	var paths []string
	// 父目录只有在 broker 明确授权时才追加只读放行，否则不做隐式越权放行（P0 收口）：
	// 旧实现无条件把 projectRoot 的父目录塞进 ReadablePaths，等于把未授权的上级目录暴露给沙箱。
	if parent := filepath.Dir(projectRoot); parent != "" && parent != "." && parent != projectRoot {
		if brokerAuthorizes(broker, parent) {
			paths = append(paths, parent)
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths,
			filepath.Join(home, ".npm"),
			filepath.Join(home, ".cache", "pip"),
		)
	}
	paths = append(paths, goRuntimeReadablePaths()...)
	return paths
}

func codeExecNeedsGoRuntime(req codeExecRequest) bool {
	if normalizeCodeExecLanguage(req.Language) == "go" {
		return true
	}
	if len(req.Command) > 0 && filepath.Base(req.Command[0]) == "go" {
		return true
	}
	cmd := strings.TrimSpace(req.CommandText)
	return cmd == "go" || strings.HasPrefix(cmd, "go ") || strings.Contains(cmd, " go ")
}

func goRuntimeReadablePaths() []string {
	var paths []string
	if goroot := runtime.GOROOT(); goroot != "" {
		paths = append(paths, goroot)
	}
	if gomod := os.Getenv("GOMODCACHE"); strings.TrimSpace(gomod) != "" {
		paths = append(paths, gomod)
	}
	for _, gp := range filepath.SplitList(os.Getenv("GOPATH")) {
		if strings.TrimSpace(gp) != "" {
			paths = append(paths, filepath.Join(gp, "pkg", "mod"))
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths,
			filepath.Join(home, "go", "pkg", "mod"),
			filepath.Join(home, ".cache", "go-build"),
			filepath.Join(home, "Library", "Caches", "go-build"),
		)
	}
	return paths
}

func compactCleanPaths(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		abs, err := filepath.Abs(p)
		if err == nil {
			p = abs
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		if !seen[p] {
			seen[p] = true
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

func shellJoin(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func windowsCmdJoin(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, windowsCmdQuote(arg))
	}
	return strings.Join(parts, " ")
}

func windowsCmdQuote(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t&|<>()^%!\"") {
		return s
	}
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
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

func dirSize(root string) (int64, error) {
	var total int64
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstFileWithExt(root, ext string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
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
	return found
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
		cfg.Timeout = 60
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = 64 * 1024
	}
	if cfg.MaxStderrBytes <= 0 {
		cfg.MaxStderrBytes = 64 * 1024
	}
	if cfg.MaxWorkspaceBytes <= 0 {
		cfg.MaxWorkspaceBytes = 1024 * 1024 * 1024
	}
	if cfg.MaxArtifactBytes <= 0 {
		cfg.MaxArtifactBytes = 50 * 1024 * 1024
	}
	if cfg.MaxMemoryBytes <= 0 {
		cfg.MaxMemoryBytes = 256 * 1024 * 1024
	}
	if cfg.MaxProcesses <= 0 {
		cfg.MaxProcesses = 64
	}
	// 默认遮蔽宿主机 secrets：即使 project 模式把 $HOME 当 workspace 放行，这些路径
	// 仍被沙箱 deny 规则（darwin deny-after-allow / linux 掩蔽）优先遮蔽。合并而非覆盖，
	// 保留调用方自定义的 DeniedPaths。
	cfg.DeniedPaths = mergeDeniedPaths(cfg.DeniedPaths, defaultSandboxDeniedPaths())
	return cfg
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

func buildInstallCommand(language string, pkgs []string) string {
	switch normalizeCodeExecLanguage(language) {
	case "python", "python3":
		return "python3 -m pip install " + strings.Join(pkgs, " ")
	case "javascript", "node", "js":
		return "npm install --no-save " + strings.Join(pkgs, " ")
	default:
		return ""
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
