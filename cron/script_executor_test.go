package cron

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// hasPython3 跳过依赖 python3 的测试（理论上 hexclaw 部署机一定有，CI 可能没有）。
func hasPython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("跳过：本机找不到 python3")
	}
}

// newTestExecutor 把 workdir / venvCache 指向 t.TempDir() 避免污染 ~/.hexclaw/。
func newTestExecutor(t *testing.T) *ScriptExecutor {
	t.Helper()
	return NewScriptExecutor().
		WithWorkdir(t.TempDir()).
		WithVenvCache(t.TempDir())
}

// ── 3.1 hello world 最小路径 ───────────────────────

func TestExecutor_RunsHelloWorldScript(t *testing.T) {
	hasPython3(t)
	e := newTestExecutor(t)
	spec := &JobSpec{
		Runtime:    "python3",
		Script:     `import json; print(json.dumps({"status":"success","data":"hello"}))`,
		TimeoutSec: 10,
	}
	res, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "success" {
		t.Errorf("Status: %s err=%s stdout=%q stderr=%q", res.Status, res.Error, res.Stdout, res.Stderr)
	}
	if res.Data != "hello" {
		t.Errorf("Data: %v", res.Data)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode: %d", res.ExitCode)
	}
}

// ── 3.2 解析最后一行 JSON ───────────────────────────

func TestExecutor_ParsesStructuredJSONOutput(t *testing.T) {
	hasPython3(t)
	e := newTestExecutor(t)
	spec := &JobSpec{
		Runtime: "python3",
		Script: `import json
print("debug line 1")
print("debug line 2")
print(json.dumps({"status":"success","data":{"count":42}}))
`,
		TimeoutSec: 10,
	}
	res, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("Status: %s", res.Status)
	}
	m, ok := res.Data.(map[string]any)
	if !ok || m["count"] != float64(42) {
		t.Errorf("Data: %+v", res.Data)
	}
}

// ── 3.3 timeout SIGKILL ────────────────────────────

func TestExecutor_TimeoutKillsProcess(t *testing.T) {
	hasPython3(t)
	e := newTestExecutor(t)
	spec := &JobSpec{
		Runtime: "python3",
		Script: `import time, json
time.sleep(5)
print(json.dumps({"status":"success"}))
`,
		TimeoutSec: 1,
	}
	start := time.Now()
	res, err := e.Run(context.Background(), spec)
	dur := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "timeout" {
		t.Errorf("Status: %s err=%s", res.Status, res.Error)
	}
	if dur > 3*time.Second {
		t.Errorf("超时控制失效，实际耗时 %s", dur)
	}
}

// ── 3.4 venv install deps + 3.5 venv 缓存命中 ──────

func TestExecutor_InstallsAndUsesDeps(t *testing.T) {
	hasPython3(t)
	if testing.Short() {
		t.Skip("跳过：-short 模式不跑 pip install")
	}
	e := newTestExecutor(t)
	// 用 stdlib-only 包名替代真实安装慢——选择体积很小的 `wheel` 包（pip 自带依赖，安装秒级）
	spec := &JobSpec{
		Runtime: "python3",
		Script: `import json
import wheel
print(json.dumps({"status":"success","data":wheel.__version__ != ""}))
`,
		Deps:       []string{"wheel"},
		TimeoutSec: 120,
		Compiled:   CompileMeta{Hash: "test-deps-cache-key"},
	}
	res, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("Status: %s stderr=%s", res.Status, res.Stderr)
	}
	if res.Data != true {
		t.Errorf("Data: %v", res.Data)
	}

	// 3.5 第二次同 Hash 应直接命中 venv，不再 pip install
	start := time.Now()
	res2, err := e.Run(context.Background(), spec)
	if err != nil || res2.Status != "success" {
		t.Fatalf("Run2: err=%v res=%+v", err, res2)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("venv 缓存未命中，第二次耗时 %s", time.Since(start))
	}
}

// ── 3.6 stdout tail 截断 ───────────────────────────

func TestExecutor_TailsLargeStdout(t *testing.T) {
	hasPython3(t)
	e := newTestExecutor(t)
	// 输出 ~200KB 噪音，最后一行才是 JSON
	spec := &JobSpec{
		Runtime: "python3",
		Script: `import json
for _ in range(200000):
    print("noise")
print(json.dumps({"status":"success","data":"tail"}))
`,
		TimeoutSec: 30,
	}
	res, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("Status: %s", res.Status)
	}
	if len(res.Stdout) > 80*1024 {
		t.Errorf("Stdout 应被 tail 截断至 64KB+marker，实际 %d 字节", len(res.Stdout))
	}
	if !strings.Contains(res.Stdout, "...[truncated]...") {
		t.Errorf("缺截断 marker")
	}
	// 最后一行 JSON 必须保留以便 Data 解析成功
	if res.Data != "tail" {
		t.Errorf("最后行 JSON 应保留，Data=%v", res.Data)
	}
}

// ── 3.7 脚本异常退出 ──────────────────────────────

func TestExecutor_ScriptExitNonZero(t *testing.T) {
	hasPython3(t)
	e := newTestExecutor(t)
	spec := &JobSpec{
		Runtime: "python3",
		Script: `import sys, json
print(json.dumps({"status":"error","error":"业务异常"}))
sys.exit(1)
`,
		TimeoutSec: 10,
	}
	res, err := e.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("Status: %s", res.Status)
	}
	if res.Error != "业务异常" {
		t.Errorf("Error: %q", res.Error)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode 应非零，实际 0")
	}
}

// Review L9: scripts occasionally emit nonstandard status values. "ok"-family
// values must normalize to "success" (otherwise the heal trigger treats a
// passing run as a failure), and unknown values must normalize to "error"
// (otherwise they slip past consecutiveFailures' status whitelist).
func TestParseLastJSONLine_NormalizesStatus(t *testing.T) {
	cases := []struct {
		emitted string
		want    string
	}{
		{"ok", "success"},
		{"OK", "success"},
		{"succeeded", "success"},
		{"success", "success"},
		{"error", "error"},
		{"failed", "failed"},
		{"timeout", "timeout"},
		{"weird-status", "error"},
	}
	for _, c := range cases {
		res := &RunResult{}
		parseLastJSONLine(`{"status":"`+c.emitted+`","data":null}`, res)
		if res.Status != c.want {
			t.Errorf("status %q should normalize to %q, got %q", c.emitted, c.want, res.Status)
		}
	}
}
