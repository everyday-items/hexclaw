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
	stdoutTail int
}

// defaultScriptTimeoutSec is the fallback per-run budget for a spec whose
// TimeoutSec is unset (0). It is the SINGLE source of truth shared by the
// scheduler's outer execution budget (runBudget) and the executor's inner
// deadline — they must agree, or the outer ctx can SIGKILL a script before its
// own deadline fires. Review M4: a 0-timeout spec gave the executor a 5-minute
// inner deadline but the scheduler only a 60-second outer budget, so any
// default-timeout script was killed at 60s and mislabeled timeout.
const defaultScriptTimeoutSec = 300

// runCleanupHeadroom is the slack added on top of the effective timeout so the
// outer budget outlives the inner deadline (venv teardown, process reaping).
const runCleanupHeadroom = 30 * time.Second

// effectiveTimeoutSec resolves a spec's TimeoutSec to the seconds a run is
// actually granted: 0/negative → the default. Both the scheduler and the
// executor route through this so the two layers can never disagree.
func effectiveTimeoutSec(specTimeoutSec int) int {
	if specTimeoutSec <= 0 {
		return defaultScriptTimeoutSec
	}
	return specTimeoutSec
}

// runBudget is the outer execution budget the scheduler grants a run: the
// effective inner timeout plus cleanup headroom, floored to a usable minimum.
// Invariant (asserted in tests): runBudget(ts) >= effective inner deadline, so
// the inner timeout is always the one that fires first.
func runBudget(specTimeoutSec int) time.Duration {
	b := time.Duration(effectiveTimeoutSec(specTimeoutSec))*time.Second + runCleanupHeadroom
	if b < 60*time.Second {
		b = 60 * time.Second
	}
	return b
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

// NewScriptExecutor 创建默认 executor（python3 + ~/.hexclaw/cron-sandbox）。
func NewScriptExecutor() *ScriptExecutor {
	home, _ := os.UserHomeDir()
	return &ScriptExecutor{
		pythonBin:  "python3",
		workdir:    filepath.Join(home, ".hexclaw", "cron-sandbox"),
		stdoutTail: 64 * 1024,
	}
}

// WithWorkdir 仅用于测试覆盖默认沙箱目录。
func (e *ScriptExecutor) WithWorkdir(p string) *ScriptExecutor { e.workdir = p; return e }

// WithVenvCache is a retained no-op: the stdlib-only sandbox never builds a
// venv (review M5/F1). Kept so existing call sites compile unchanged.
func (e *ScriptExecutor) WithVenvCache(string) *ScriptExecutor { return e }

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

	// Stdlib-only sandbox: the pip path is a dead end (unreliable on the
	// sandboxed host, and the AST validator already forbids non-stdlib imports).
	// New specs carry no deps (forced empty at the compile boundary, review M5),
	// but a spec PERSISTED before that fix may still carry them — drop them here
	// rather than enter the venv/pip path that can only fail (review F1).
	pythonExec := e.pythonBin
	if len(spec.Deps) > 0 {
		slog.Warn("[cron] ignoring deps — stdlib-only sandbox, pip disabled",
			"source", "cron", "deps", spec.Deps)
	}

	timeout := time.Duration(effectiveTimeoutSec(spec.TimeoutSec)) * time.Second
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env := sandboxEnv()
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

// sandboxEnvAllowlist are the only parent env var names propagated to a cron
// script. Everything else (API keys, tokens, provider secrets) is withheld so
// an LLM-generated script cannot read or exfiltrate them (BUG-20260613).
var sandboxEnvAllowlist = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true,
	"LC_ALL": true, "LC_CTYPE": true, "TZ": true, "TERM": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, // let urllib find the CA bundle
}

// sandboxEnv returns a minimal allowlisted environment for sandboxed scripts.
func sandboxEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if ok && (sandboxEnvAllowlist[name] || strings.HasPrefix(name, "LC_")) {
			env = append(env, kv)
		}
	}
	return env
}
