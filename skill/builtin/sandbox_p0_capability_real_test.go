package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/os/sandbox"
)

// TestSandboxP0_StaticGapMatrix is a deliberately gated proof test for the P0
// sandbox capability list. It fails while the current implementation lacks the
// requested P0 features, and stays out of normal unit runs by default.
func TestSandboxP0_StaticGapMatrix(t *testing.T) {
	if os.Getenv("HEXCLAW_P0_SANDBOX_PROOF") != "1" {
		t.Skip("set HEXCLAW_P0_SANDBOX_PROOF=1 to prove current P0 sandbox capability gaps")
	}

	root := sandboxP0RepoRoot(t)
	read := func(rel string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return string(b)
	}

	codeExec := read("skill/builtin/code_exec.go")
	sandboxAPI := read("../toolkit/os/sandbox/sandbox.go")
	execPosix := read("../toolkit/os/sandbox/exec_posix.go")
	linux := read("../toolkit/os/sandbox/sandbox_linux.go")
	windows := read("../toolkit/os/sandbox/sandbox_windows.go")

	checks := []struct {
		id        string
		name      string
		supported bool
		evidence  string
	}{
		{
			id:        "P0-1",
			name:      "execute existing files and project commands",
			supported: strings.Contains(codeExec, `"mode"`) && strings.Contains(codeExec, `"command"`) && strings.Contains(codeExec, `"entrypoint"`),
			evidence:  "code_exec tool schema must expose mode/file/project command fields",
		},
		{
			id:        "P0-2",
			name:      "Go project runner",
			supported: strings.Contains(codeExec, `"go", "test"`) || strings.Contains(codeExec, "go test") || strings.Contains(codeExec, "RunnerModeProject"),
			evidence:  "current Go path is single-file ExecCode/go run, not a project runner",
		},
		{
			id:        "P0-3",
			name:      "Python/Node file and project runners",
			supported: strings.Contains(codeExec, "pytest") && strings.Contains(codeExec, "pnpm") && strings.Contains(codeExec, "entrypoint"),
			evidence:  "current tool accepts only language+code; no file/project runner contract",
		},
		{
			id:        "P0-4",
			name:      "per-run workspace",
			supported: strings.Contains(codeExec, "run_id") || strings.Contains(sandboxAPI, "RunSpec"),
			evidence:  "current result has no run_id/session run workspace contract",
		},
		{
			id:        "P0-5",
			name:      "artifact manifest",
			supported: strings.Contains(codeExec, "Artifact") && strings.Contains(codeExec, "Manifest") && strings.Contains(codeExec, "artifacts"),
			evidence:  "code_exec must expose artifact manifest entries for generated run files",
		},
		{
			id:        "P0-6",
			name:      "bounded output truncation",
			supported: strings.Contains(sandboxAPI, "MaxOutput") || strings.Contains(codeExec, "truncated"),
			evidence:  "stdout/stderr are returned as strings with no head/tail truncation contract",
		},
		{
			id:        "P0-7",
			name:      "timeout kills process tree",
			supported: strings.Contains(execPosix, "Setpgid") || strings.Contains(windows, "JobObject") || strings.Contains(windows, "JOB_OBJECT"),
			evidence:  "current POSIX exec path lacks process-group cleanup contract",
		},
		{
			id:        "P0-8",
			name:      "Linux/Windows fail-closed",
			supported: !strings.Contains(linux, "execFallback") && !strings.Contains(windows, "execFallback"),
			evidence:  "linux/windows backends still contain direct exec fallback paths",
		},
		{
			id:        "P0-9",
			name:      "runtime/dependency diagnostics",
			supported: strings.Contains(codeExec, "runtime_missing") || strings.Contains(codeExec, "dependency_missing"),
			evidence:  "current errors are raw stderr/exit code, not structured diagnostics",
		},
		{
			id:        "P0-10",
			name:      "tool schema upgrade",
			supported: strings.Contains(codeExec, `"mode"`) && strings.Contains(codeExec, `"artifacts"`) && strings.Contains(codeExec, `"files"`),
			evidence:  "current schema only exposes language and code",
		},
		{
			id:        "P0-11",
			name:      "baseline resource limits",
			supported: strings.Contains(sandboxAPI, "MaxOutput") && strings.Contains(sandboxAPI, "MaxWorkspace") && strings.Contains(sandboxAPI, "MaxArtifact") && strings.Contains(sandboxAPI, "MaxMemory"),
			evidence:  "current sandbox config/result lacks baseline output/workspace/artifact/memory limit contract",
		},
	}

	for _, c := range checks {
		c := c
		t.Run(c.id+"_"+strings.ReplaceAll(c.name, " ", "_"), func(t *testing.T) {
			if !c.supported {
				t.Fatalf("%s unsupported: %s", c.name, c.evidence)
			}
		})
	}
}

// TestSandboxP0_RealModelToolUseMatrix spends real tokens and uses the configured
// SiliconFlow models to drive the current production code_exec tool definition.
// It distinguishes model tool-use failures from sandbox/runtime capability gaps.
func TestSandboxP0_RealModelToolUseMatrix(t *testing.T) {
	if os.Getenv("HEXCLAW_P0_SANDBOX_REALMODEL") != "1" {
		t.Skip("set HEXCLAW_P0_SANDBOX_REALMODEL=1 to spend real tokens on P0 sandbox validation")
	}

	cfg, err := config.Load(os.Getenv("HEXCLAW_REAL_LLM_CONFIG"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	providerName := sandboxP0Env("HEXCLAW_P0_PROVIDER", "硅基流动")
	pc, ok := cfg.LLM.Providers[providerName]
	if !ok {
		t.Fatalf("provider %q not found in config", providerName)
	}
	if strings.TrimSpace(pc.APIKey) == "" {
		t.Fatalf("provider %q has no API key", providerName)
	}

	models := sandboxP0Models(pc)
	if len(models) == 0 {
		t.Fatalf("no models configured for provider %q", providerName)
	}

	root := sandboxP0RepoRoot(t)
	ws := t.TempDir()
	sandboxP0WriteFile(t, filepath.Join(ws, "scripts", "hello.py"), "print('P0_PY_FILE_OK')\n")
	sandboxP0WriteFile(t, filepath.Join(ws, "node", "hello.js"), "console.log('P0_NODE_FILE_OK')\n")

	sb, err := sandbox.New(sandbox.Config{
		Workspace:     ws,
		Timeout:       30,
		Network:       false,
		ReadablePaths: []string{root},
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	codeExec := NewCodeExecSkill(sb, sandbox.Config{
		Workspace:     ws,
		Timeout:       30,
		Network:       false,
		ReadablePaths: []string{root},
	})

	cases := []sandboxP0RealCase{
		{
			ID:   "P0-1",
			Name: "execute existing file",
			Prompt: fmt.Sprintf(
				"Call code_exec exactly once with exactly these JSON arguments: {\"mode\":\"file\",\"entrypoint\":%q}. Do not omit entrypoint.",
				filepath.Join(ws, "scripts", "hello.py"),
			),
			Want: "P0_PY_FILE_OK",
		},
		{
			ID:   "P0-2",
			Name: "Go project command",
			Prompt: fmt.Sprintf(
				"Call code_exec exactly once with exactly these JSON arguments: {\"mode\":\"project\",\"project_root\":%q,\"command\":[\"go\",\"test\",\"./skill/builtin\",\"-run\",\"TestCodeExecSkill_Meta\",\"-count=1\"]}. Do not omit command.",
				root,
			),
			WantAny: []string{"ok  \tgithub.com/hexagon-codes/hexclaw/skill/builtin", "PASS"},
		},
		{
			ID:   "P0-3",
			Name: "Node existing file",
			Prompt: fmt.Sprintf(
				"Call code_exec exactly once with exactly these JSON arguments: {\"mode\":\"file\",\"entrypoint\":%q}. Do not omit entrypoint.",
				filepath.Join(ws, "node", "hello.js"),
			),
			Want: "P0_NODE_FILE_OK",
		},
		{
			ID:     "P0-4",
			Name:   "per-run workspace/run id",
			Prompt: "Call code_exec exactly once with exactly these JSON arguments: {\"language\":\"python\",\"code\":\"import os; print('run_id', os.environ.get('HEXCLAW_RUN_ID')); print(os.getcwd())\"}.",
			RejectIfMissing: []string{
				"run_id",
			},
		},
		{
			ID:     "P0-5",
			Name:   "artifact manifest",
			Prompt: "Call code_exec exactly once with exactly these JSON arguments: {\"language\":\"python\",\"code\":\"from pathlib import Path; Path('artifacts').mkdir(exist_ok=True); Path('artifacts/p0_artifact.txt').write_text('P0_ARTIFACT_OK'); print('artifact written')\"}.",
			RejectIfMissing: []string{
				"artifact",
				"id",
			},
		},
		{
			ID:     "P0-6",
			Name:   "bounded long output",
			Prompt: "Call code_exec exactly once with exactly these JSON arguments: {\"language\":\"python\",\"code\":\"print('A' * 200000)\"}.",
			Judge: func(out string) error {
				if strings.Contains(out, "truncated=true") || strings.Contains(out, `"truncated":true`) {
					return nil
				}
				return fmt.Errorf("missing truncation marker; returned bytes=%d", len(out))
			},
		},
		{
			ID:     "P0-7",
			Name:   "timeout status",
			Prompt: "Call code_exec exactly once with exactly these JSON arguments: {\"language\":\"python\",\"code\":\"import time; time.sleep(5)\",\"timeout\":1}.",
			Judge: func(out string) error {
				if strings.Contains(strings.ToLower(out), "timeout") {
					return nil
				}
				return fmt.Errorf("missing structured timeout status in output: %s", sandboxP0Trunc(out, 240))
			},
		},
		{
			ID:     "P0-8",
			Name:   "Linux/Windows fail-closed visible",
			Prompt: "Call code_exec exactly once with exactly these JSON arguments: {\"language\":\"python\",\"code\":\"print('capability check')\"}. The runtime metadata must expose fail_closed status.",
			RejectIfMissing: []string{
				"fail",
				"closed",
			},
		},
		{
			ID:     "P0-9",
			Name:   "runtime/dependency diagnostics",
			Prompt: fmt.Sprintf("Call code_exec exactly once with exactly these JSON arguments: {\"mode\":\"project\",\"project_root\":%q,\"command\":[\"hexclaw-p0-missing-runtime-xyz\"]}.", ws),
			RejectIfMissing: []string{
				"runtime_missing",
			},
		},
		{
			ID:     "P0-10",
			Name:   "upgraded tool schema",
			Prompt: fmt.Sprintf("Call code_exec exactly once with exactly these JSON arguments: {\"mode\":\"project\",\"project_root\":%q,\"command\":[\"sh\",\"-c\",\"go test ./skill/builtin -run TestCodeExecSkill_Meta -count=1 && echo P0_SCHEMA_PROJECT_OK\"]}. Do not send a code snippet.", root),
			Judge: func(out string) error {
				if strings.Contains(out, "P0_SCHEMA_PROJECT_OK") {
					return nil
				}
				if strings.Contains(out, "language and code are required") {
					return fmt.Errorf("schema still requires legacy language+code only")
				}
				return fmt.Errorf("project-mode schema was not accepted")
			},
		},
		{
			ID:     "P0-11",
			Name:   "baseline resource limits",
			Prompt: "Call code_exec exactly once with exactly these JSON arguments: {\"language\":\"python\",\"code\":\"print('resource limits metadata')\"}.",
			RejectIfMissing: []string{
				"max_stdout",
				"max_workspace",
				"max_artifact",
				"max_process",
				"max_memory",
			},
		},
	}

	for _, model := range models {
		model := model
		t.Run(strings.ReplaceAll(model, "/", "_"), func(t *testing.T) {
			for _, tc := range cases {
				tc := tc
				t.Run(tc.ID+"_"+strings.ReplaceAll(tc.Name, " ", "_"), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
					defer cancel()
					out, toolCalled, err := sandboxP0RunRealModelTool(ctx, pc, model, tc.Prompt, codeExec)
					if err != nil {
						t.Fatalf("real model/tool run failed: %v", err)
					}
					if !toolCalled {
						t.Fatalf("model did not call code_exec")
					}
					if err := tc.Check(out); err != nil {
						t.Fatalf("%s unsupported via real model %s: %v\noutput:\n%s", tc.ID, model, err, sandboxP0Trunc(out, 1200))
					}
				})
			}
		})
	}
}

// TestSandboxP0_RealModelPythonShellTasks validates the product-critical path:
// a real model receives a natural-language task, writes Python for code_exec,
// and uses the sandbox result to complete the task.
func TestSandboxP0_RealModelPythonShellTasks(t *testing.T) {
	if os.Getenv("HEXCLAW_P0_SANDBOX_REALMODEL") != "1" {
		t.Skip("set HEXCLAW_P0_SANDBOX_REALMODEL=1 to spend real tokens on Python shell validation")
	}

	cfg, err := config.Load(os.Getenv("HEXCLAW_REAL_LLM_CONFIG"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	providerName := sandboxP0Env("HEXCLAW_P0_PROVIDER", "硅基流动")
	pc, ok := cfg.LLM.Providers[providerName]
	if !ok {
		t.Fatalf("provider %q not found in config", providerName)
	}
	if strings.TrimSpace(pc.APIKey) == "" {
		t.Fatalf("provider %q has no API key", providerName)
	}
	models := sandboxP0Models(pc)
	if len(models) == 0 {
		t.Fatalf("no models configured for provider %q", providerName)
	}

	ws := t.TempDir()
	sb, err := sandbox.New(sandbox.Config{
		Workspace: ws,
		Timeout:   45,
		Network:   false,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	codeExec := NewCodeExecSkill(sb, sandbox.Config{
		Workspace: ws,
		Timeout:   45,
		Network:   false,
	})

	cases := []sandboxP0PythonShellCase{
		{
			Name: "compute sales from natural language",
			Prompt: `请调用 code_exec 一次，自己写并运行 Python 脚本完成下面任务，不要心算，不要直接给结论。
订单数据：
- sku=A, qty=3, price=19
- sku=B, qty=2, price=41
- sku=A, qty=5, price=19
- sku=C, qty=7, price=11
脚本必须计算每个 sku 的 revenue、总 revenue、revenue 最大的 sku，并计算 sha256("A:152|B:82|C:77") 的前 12 位。
脚本最后必须打印一行精确格式：
PY_SHELL_OK total_revenue=<总额> best_sku=<sku> checksum=<12位校验值>`,
			Want: []string{"PY_SHELL_OK", "total_revenue=311", "best_sku=A", "checksum=a965ccb2218b"},
		},
		{
			Name: "write artifact from generated python",
			Prompt: `请调用 code_exec 一次，自己写并运行 Python 脚本完成任务，不要心算，不要直接给结论。
任务数据：
- owner=lin, minutes=34, done=true
- owner=guo, minutes=21, done=false
- owner=lin, minutes=55, done=true
- owner=guo, minutes=13, done=true
脚本必须统计 done=true 的 owner 分钟数，统计未完成条数，把 {"lin":89,"guo":13,"backlog":1} 写入 artifacts/python_shell_task_summary.json。
脚本最后必须打印一行精确格式：
PY_ARTIFACT_OK backlog=1 lin=89 guo=13`,
			Want: []string{"PY_ARTIFACT_OK", "backlog=1", "lin=89", "guo=13", "python_shell_task_summary.json"},
		},
	}

	for _, model := range models {
		model := model
		t.Run(strings.ReplaceAll(model, "/", "_"), func(t *testing.T) {
			for _, tc := range cases {
				tc := tc
				t.Run(strings.ReplaceAll(tc.Name, " ", "_"), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
					defer cancel()

					_, calls, err := sandboxP0RunRealModelToolTrace(ctx, pc, model, tc.Prompt, codeExec)
					if err != nil {
						t.Fatalf("real model/tool run failed: %v", err)
					}
					call, ok := sandboxP0FirstCodeExecCall(calls)
					if !ok {
						t.Fatalf("model did not call code_exec")
					}
					if err := sandboxP0RequirePythonShellArgs(call.Args, call.ArgsJSON); err != nil {
						t.Fatalf("model did not write/run Python shell code: %v\nargs:\n%s\nresult:\n%s", err, call.ArgsJSON, sandboxP0Trunc(call.Result, 1200))
					}
					for _, want := range tc.Want {
						if !strings.Contains(call.Result, want) {
							t.Fatalf("missing %q in Python shell result\nargs:\n%s\nresult:\n%s", want, call.ArgsJSON, sandboxP0Trunc(call.Result, 1200))
						}
					}
				})
			}
		})
	}
}

func TestSandboxP0_RealModelPythonCrawler(t *testing.T) {
	if os.Getenv("HEXCLAW_P0_SANDBOX_REALMODEL") != "1" || os.Getenv("HEXCLAW_CODE_EXEC_LIVE_NETWORK") != "1" {
		t.Skip("set HEXCLAW_P0_SANDBOX_REALMODEL=1 and HEXCLAW_CODE_EXEC_LIVE_NETWORK=1 to run real-model Python crawler validation")
	}

	cfg, err := config.Load(os.Getenv("HEXCLAW_REAL_LLM_CONFIG"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	providerName := sandboxP0Env("HEXCLAW_P0_PROVIDER", "硅基流动")
	pc, ok := cfg.LLM.Providers[providerName]
	if !ok {
		t.Fatalf("provider %q not found in config", providerName)
	}
	if strings.TrimSpace(pc.APIKey) == "" {
		t.Fatalf("provider %q has no API key", providerName)
	}
	models := sandboxP0Models(pc)
	if len(models) == 0 {
		t.Fatalf("no models configured for provider %q", providerName)
	}

	ws := t.TempDir()
	sb, err := sandbox.New(sandbox.Config{
		Workspace: ws,
		Timeout:   45,
		Network:   true,
	})
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	codeExec := NewCodeExecSkill(sb, sandbox.Config{
		Workspace: ws,
		Timeout:   45,
		Network:   true,
	})
	prompt := `请调用 code_exec 一次，自己写并运行 Python 网络爬虫脚本，不要直接给结论。
要求：
1. 只使用 Python 标准库 urllib.request 和 re。
2. 抓取 http://example.com/。
3. 解析 <title> 文本，统计页面 href 链接数量和 HTML 字节数。
4. 脚本最后必须打印一行精确格式：
PY_CRAWL_OK title=<title> hrefs=<数量> bytes=<字节数>`

	for _, model := range models {
		model := model
		t.Run(strings.ReplaceAll(model, "/", "_"), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			_, calls, err := sandboxP0RunRealModelToolTrace(ctx, pc, model, prompt, codeExec)
			if err != nil {
				t.Fatalf("real model/tool run failed: %v", err)
			}
			call, ok := sandboxP0FirstCodeExecCall(calls)
			if !ok {
				t.Fatalf("model did not call code_exec")
			}
			if err := sandboxP0RequirePythonShellArgs(call.Args, call.ArgsJSON); err != nil {
				t.Fatalf("model did not write/run Python crawler code: %v\nargs:\n%s\nresult:\n%s", err, call.ArgsJSON, sandboxP0Trunc(call.Result, 1200))
			}
			t.Logf("real-model Python crawler output:\n%s", sandboxP0Trunc(call.Result, 1200))
			for _, want := range []string{"PY_CRAWL_OK", "title=Example Domain", "hrefs=", "bytes="} {
				if !strings.Contains(call.Result, want) {
					t.Fatalf("missing %q in Python crawler result\nargs:\n%s\nresult:\n%s", want, call.ArgsJSON, sandboxP0Trunc(call.Result, 1200))
				}
			}
		})
	}
}

type sandboxP0RealCase struct {
	ID              string
	Name            string
	Prompt          string
	Want            string
	WantAny         []string
	RejectIfMissing []string
	Judge           func(out string) error
}

func (c sandboxP0RealCase) Check(out string) error {
	if c.Judge != nil {
		return c.Judge(out)
	}
	if c.Want != "" && !strings.Contains(out, c.Want) {
		return fmt.Errorf("missing %q", c.Want)
	}
	for _, want := range c.WantAny {
		if strings.Contains(out, want) {
			return nil
		}
	}
	if len(c.WantAny) > 0 {
		return fmt.Errorf("missing any of %v", c.WantAny)
	}
	lower := strings.ToLower(out)
	for _, want := range c.RejectIfMissing {
		if !strings.Contains(lower, strings.ToLower(want)) {
			return fmt.Errorf("missing required marker %q", want)
		}
	}
	return nil
}

type sandboxP0PythonShellCase struct {
	Name   string
	Prompt string
	Want   []string
}

type sandboxP0ToolInvocation struct {
	Name     string
	ArgsJSON string
	Args     map[string]any
	Result   string
}

func sandboxP0RunRealModelTool(ctx context.Context, pc config.LLMProviderConfig, model, prompt string, codeExec *CodeExecSkill) (string, bool, error) {
	_, calls, err := sandboxP0RunRealModelToolTrace(ctx, pc, model, prompt, codeExec)
	if err != nil {
		return "", false, err
	}
	var combined strings.Builder
	toolCalled := false
	for _, call := range calls {
		if call.Name != "code_exec" {
			continue
		}
		toolCalled = true
		combined.WriteString(call.Result)
	}
	return combined.String(), toolCalled, nil
}

func sandboxP0RunRealModelToolTrace(ctx context.Context, pc config.LLMProviderConfig, model, prompt string, codeExec *CodeExecSkill) (string, []sandboxP0ToolInvocation, error) {
	messages := []map[string]any{
		{"role": "system", "content": "You are validating HexClaw's current code_exec tool. You must call code_exec exactly once. When the task asks for computation or file processing, write and run Python code with code_exec instead of solving mentally. Do not claim success without using the tool."},
		{"role": "user", "content": prompt},
	}
	content, calls, err := sandboxP0Chat(ctx, pc, model, messages, codeExec.ToolDefinition())
	if err != nil {
		return "", nil, err
	}
	if len(calls) == 0 {
		return content, nil, nil
	}
	invocations := make([]sandboxP0ToolInvocation, 0, len(calls))
	for _, call := range calls {
		name, argsJSON := sandboxP0ToolCall(call)
		invocation := sandboxP0ToolInvocation{Name: name, ArgsJSON: argsJSON}
		if name != "code_exec" {
			invocations = append(invocations, invocation)
			continue
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return content, invocations, fmt.Errorf("parse tool args %q: %w", argsJSON, err)
		}
		invocation.Args = args
		res, err := codeExec.Execute(ctx, args)
		if err != nil {
			invocation.Result = "tool_args: " + argsJSON + "\ntool_error: " + err.Error()
			invocations = append(invocations, invocation)
			continue
		}
		invocation.Result = res.Content
		invocations = append(invocations, invocation)
	}
	return content, invocations, nil
}

func sandboxP0FirstCodeExecCall(calls []sandboxP0ToolInvocation) (sandboxP0ToolInvocation, bool) {
	for _, call := range calls {
		if call.Name == "code_exec" {
			return call, true
		}
	}
	return sandboxP0ToolInvocation{}, false
}

func sandboxP0RequirePythonShellArgs(args map[string]any, argsJSON string) error {
	if args == nil {
		return fmt.Errorf("missing parsed arguments")
	}
	lang := normalizeCodeExecLanguage(stringArg(args, "language"))
	if strings.HasPrefix(lang, "python") && strings.TrimSpace(stringArg(args, "code")) != "" {
		return nil
	}
	if code := strings.TrimSpace(stringArg(args, "code")); code != "" && strings.HasPrefix(inferLanguageFromCode(code), "python") {
		return nil
	}
	cmd, cmdText := firstCommandArg(args, "command", "cmd", "argv", "args")
	if strings.Contains(strings.ToLower(cmdText), "python") {
		return nil
	}
	if len(cmd) > 0 && strings.Contains(strings.ToLower(strings.Join(cmd, " ")), "python") {
		return nil
	}
	for _, f := range filesArg(args, "files") {
		if strings.HasSuffix(strings.ToLower(f.Path), ".py") && strings.Contains(f.Content, "print") {
			return nil
		}
	}
	return fmt.Errorf("arguments are not a Python script/shell execution")
}

func sandboxP0Chat(ctx context.Context, pc config.LLMProviderConfig, model string, messages []map[string]any, toolDef any) (string, []map[string]any, error) {
	body := map[string]any{
		"model":           model,
		"messages":        messages,
		"temperature":     0,
		"max_tokens":      900,
		"enable_thinking": false,
		"tools":           []any{toolDef},
		"tool_choice":     "auto",
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sandboxP0ChatURL(pc.BaseURL), bytes.NewReader(b))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+pc.APIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content   string           `json:"content"`
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", nil, err
	}
	if out.Error != nil {
		return "", nil, fmt.Errorf("api error: %s", out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", nil, fmt.Errorf("no choices")
	}
	return out.Choices[0].Message.Content, out.Choices[0].Message.ToolCalls, nil
}

func sandboxP0ToolCall(call map[string]any) (string, string) {
	fn, _ := call["function"].(map[string]any)
	name, _ := fn["name"].(string)
	switch v := fn["arguments"].(type) {
	case string:
		return name, v
	case map[string]any:
		b, _ := json.Marshal(v)
		return name, string(b)
	default:
		b, _ := json.Marshal(v)
		return name, string(b)
	}
}

func sandboxP0Models(pc config.LLMProviderConfig) []string {
	if raw := strings.TrimSpace(os.Getenv("HEXCLAW_P0_MODELS")); raw != "" {
		return sandboxP0SplitCSV(raw)
	}
	seen := map[string]bool{}
	var models []string
	for _, m := range append([]string{pc.Model}, pc.Models...) {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		models = append(models, m)
	}
	sort.Strings(models)
	return models
}

func sandboxP0ChatURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

func sandboxP0Env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func sandboxP0SplitCSV(raw string) []string {
	var out []string
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func sandboxP0RepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func sandboxP0WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func sandboxP0Trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...[truncated]"
}
