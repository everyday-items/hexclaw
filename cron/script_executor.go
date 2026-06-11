package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ScriptExecutor 在沙箱 Python subprocess 中执行 JobSpec.Script。
//
// 关键能力：
//   - venv 按 Compiled.Hash 缓存，相同脚本+依赖第二次启动跳过 pip
//   - context.WithTimeout + exec.CommandContext 保证超时被 SIGKILL
//   - 解析 stdout 最后一行 JSON 提取 {status,data,error}
//   - 出 64KB 后做 tail 截断，避免巨型日志撑爆 history
type ScriptExecutor struct {
	pythonBin  string
	workdir    string
	venvCache  string
	stdoutTail int
}

// RunResult 一次脚本执行的结构化结果。
type RunResult struct {
	Status     string `json:"status"` // success / error / timeout
	Data       any    `json:"data,omitempty"`
	Error      string `json:"error,omitempty"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	DurationMs int64  `json:"duration_ms"`
}

// NewScriptExecutor 创建默认 executor（python3 + ~/.hexclaw/cron-{sandbox,venv-cache}）。
func NewScriptExecutor() *ScriptExecutor {
	home, _ := os.UserHomeDir()
	return &ScriptExecutor{
		pythonBin:  "python3",
		workdir:    filepath.Join(home, ".hexclaw", "cron-sandbox"),
		venvCache:  filepath.Join(home, ".hexclaw", "cron-venv-cache"),
		stdoutTail: 64 * 1024,
	}
}

// WithWorkdir / WithVenvCache 仅用于测试覆盖默认路径。
func (e *ScriptExecutor) WithWorkdir(p string) *ScriptExecutor   { e.workdir = p; return e }
func (e *ScriptExecutor) WithVenvCache(p string) *ScriptExecutor { e.venvCache = p; return e }

// Run 执行一份 JobSpec 并返回结构化结果。
//
// 注意：内部 err 表示执行流程本身出错（venv 准备失败等），脚本退出码非零不算 err，
// 而是体现在 RunResult.ExitCode / Status="error"。
func (e *ScriptExecutor) Run(ctx context.Context, spec *JobSpec) (*RunResult, error) {
	if spec == nil || strings.TrimSpace(spec.Script) == "" {
		return nil, fmt.Errorf("Spec 为空或 Script 为空")
	}
	if err := os.MkdirAll(e.workdir, 0755); err != nil {
		return nil, fmt.Errorf("创建 sandbox 工作目录失败: %w", err)
	}

	start := time.Now()
	runID := fmt.Sprintf("run-%d", start.UnixNano())
	runDir := filepath.Join(e.workdir, runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return nil, err
	}
	defer e.cleanupOldRuns()

	scriptPath := filepath.Join(runDir, "script.py")
	if err := os.WriteFile(scriptPath, []byte(spec.Script), 0644); err != nil {
		return nil, err
	}

	pythonExec := e.pythonBin
	if len(spec.Deps) > 0 {
		venvPath := filepath.Join(e.venvCache, spec.Compiled.Hash)
		if spec.Compiled.Hash == "" {
			venvPath = filepath.Join(e.venvCache, "no-hash-"+runID)
		}
		if err := e.ensureVenv(ctx, venvPath, spec.Deps); err != nil {
			return nil, fmt.Errorf("venv 准备失败: %w", err)
		}
		pythonExec = venvPython(venvPath)
	}

	timeout := time.Duration(spec.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := os.Environ()
	if len(spec.Inputs) > 0 {
		if b, err := json.Marshal(spec.Inputs); err == nil {
			env = append(env, "HEXCLAW_INPUTS="+string(b))
		}
	}

	cmd := exec.CommandContext(runCtx, pythonExec, scriptPath)
	cmd.Dir = runDir
	cmd.Env = env

	var stdoutBuf, stderrBuf strings.Builder
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	runErr := cmd.Run()

	result := &RunResult{
		Stdout:     tailString(stdoutBuf.String(), e.stdoutTail),
		Stderr:     tailString(stderrBuf.String(), e.stdoutTail),
		DurationMs: time.Since(start).Milliseconds(),
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}

	// 超时：context deadline + (没有 ProcessState 或 被信号杀)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		result.Status = "timeout"
		result.Error = fmt.Sprintf("脚本执行超过 %ds", int(timeout.Seconds()))
		return result, nil
	}

	// 解析 stdout 最后一行 JSON（即使有 runErr 也尝试解析，脚本可能 print error 后 exit 1）
	parseLastJSONLine(stdoutBuf.String(), result)

	if runErr != nil && result.Status == "" {
		result.Status = "error"
		if result.Error == "" {
			result.Error = runErr.Error()
		}
	}
	if result.Status == "" {
		result.Status = "success"
	}
	return result, nil
}

// parseLastJSONLine 解析 stdout 最后一行非空行为 {status,data,error}。
// 若该行不是 JSON，把 status 设为 error 并保留原错误（如果还未设）。
func parseLastJSONLine(stdout string, result *RunResult) {
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) == 0 {
		return
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	if last == "" {
		return
	}
	var parsed struct {
		Status string `json:"status"`
		Data   any    `json:"data"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(last), &parsed); err != nil {
		if result.Status == "" {
			result.Status = "error"
			result.Error = "脚本未输出合规 JSON 最后行"
		}
		return
	}
	if parsed.Status != "" {
		result.Status = normalizeScriptStatus(parsed.Status)
	}
	if parsed.Data != nil {
		result.Data = parsed.Data
	}
	if parsed.Error != "" && result.Error == "" {
		result.Error = parsed.Error
	}
}

// normalizeScriptStatus maps script-emitted status strings onto the canonical
// set the scheduler understands ('success'/'error'/'failed'/'timeout').
// Scripts occasionally emit "ok"/"succeeded"; an unknown value would slip
// past both the heal trigger's success check and consecutiveFailures'
// whitelist, so it normalizes to "error" with the raw value logged (review L9).
func normalizeScriptStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "ok", "succeeded":
		return "success"
	case "error":
		return "error"
	case "failed":
		return "failed"
	case "timeout":
		return "timeout"
	default:
		slog.Warn("[cron] script emitted unknown status, treating as error",
			"source", "cron", "status", status)
		return "error"
	}
}

// ensureVenv 按 path 缓存：已存在则跳过，否则 python3 -m venv + pip install。
func (e *ScriptExecutor) ensureVenv(ctx context.Context, venvPath string, deps []string) error {
	if _, err := os.Stat(venvPython(venvPath)); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(venvPath), 0755); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, e.pythonBin, "-m", "venv", venvPath).CombinedOutput(); err != nil {
		return fmt.Errorf("venv create: %s", strings.TrimSpace(string(out)))
	}
	pip := venvPip(venvPath)
	args := append([]string{"install", "--quiet", "--disable-pip-version-check"}, deps...)
	if out, err := exec.CommandContext(ctx, pip, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("pip install: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// venvPython / venvPip 处理 Windows / POSIX 差异。
func venvPython(venv string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "python.exe")
	}
	return filepath.Join(venv, "bin", "python3")
}
func venvPip(venv string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "pip.exe")
	}
	return filepath.Join(venv, "bin", "pip")
}

// tailString 把超长字符串截断为后 n 字节，前面加一行 "...[truncated]..."。
func tailString(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return "...[truncated]...\n" + s[len(s)-n:]
}

// cleanupOldRuns 保留 workdir 内最近 10 个 run-* 目录，超出删除。
// 静默失败（清理失败不影响业务）。
func (e *ScriptExecutor) cleanupOldRuns() {
	entries, err := os.ReadDir(e.workdir)
	if err != nil {
		return
	}
	type entry struct {
		path  string
		mtime time.Time
	}
	var runs []entry
	for _, en := range entries {
		if !en.IsDir() || !strings.HasPrefix(en.Name(), "run-") {
			continue
		}
		info, err := en.Info()
		if err != nil {
			continue
		}
		runs = append(runs, entry{path: filepath.Join(e.workdir, en.Name()), mtime: info.ModTime()})
	}
	if len(runs) <= 10 {
		return
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].mtime.Before(runs[j].mtime) })
	for _, r := range runs[:len(runs)-10] {
		_ = os.RemoveAll(r.path)
	}
}
