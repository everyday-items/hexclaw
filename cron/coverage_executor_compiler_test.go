package cron

// Coverage completion for script_executor.go (Run branches, cleanupOldRuns,
// parseLastJSONLine) and llm_compiler.go (constructor + resolver path +
// stripMarkdownFence). The executor tests need a real python3.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
)

func newExec(t *testing.T) *ScriptExecutor {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3 on host")
	}
	return NewScriptExecutor().WithWorkdir(t.TempDir())
}

func TestExecutorRun_Branches(t *testing.T) {
	e := newExec(t)
	ctx := context.Background()

	// nil / empty spec
	if _, err := e.Run(ctx, nil); err == nil {
		t.Error("nil spec must error")
	}
	if _, err := e.Run(ctx, &JobSpec{Script: "  "}); err == nil {
		t.Error("empty script must error")
	}

	// success path (+ Deps drop warn + Inputs env marshal)
	res, err := e.Run(ctx, &JobSpec{
		Runtime:    "python3",
		Script:     `import os,json; print(json.dumps({"status":"success","data":os.environ.get("HEXCLAW_INPUTS","")}))`,
		Deps:       []string{"requests"}, // persisted-old spec → drop-with-warn branch
		Inputs:     map[string]any{"k": "v"},
		TimeoutSec: 10,
	})
	if err != nil || res.Status != "success" {
		t.Fatalf("success run: err=%v status=%q", err, res.Status)
	}

	// non-JSON last line → parse-error branch
	res, _ = e.Run(ctx, &JobSpec{Runtime: "python3", Script: `print("not json")`, TimeoutSec: 10})
	if res.Status != "error" {
		t.Errorf("non-JSON output must be error, got %q", res.Status)
	}

	// non-zero exit, no stdout → runErr branch + ExitCode
	res, _ = e.Run(ctx, &JobSpec{Runtime: "python3", Script: `import sys; sys.exit(3)`, TimeoutSec: 10})
	if res.Status != "error" || res.ExitCode != 3 {
		t.Errorf("exit 3 → status=%q exit=%d", res.Status, res.ExitCode)
	}

	// timeout branch
	res, _ = e.Run(ctx, &JobSpec{Runtime: "python3", Script: `import time; time.sleep(5)`, TimeoutSec: 1})
	if res.Status != "timeout" {
		t.Errorf("sleeping past budget → status=%q, want timeout", res.Status)
	}
}

func TestExecutorCleanupOldRuns(t *testing.T) {
	dir := t.TempDir()
	e := NewScriptExecutor().WithWorkdir(dir)
	// 13 run-* dirs with increasing mtimes; cleanup keeps the newest 10.
	base := time.Now().Add(-time.Hour)
	for i := range 13 {
		p := filepath.Join(dir, "run-"+string(rune('a'+i)))
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, base.Add(time.Duration(i)*time.Minute), base.Add(time.Duration(i)*time.Minute))
	}
	// a non-run dir must be ignored
	_ = os.MkdirAll(filepath.Join(dir, "keepme"), 0755)
	e.cleanupOldRuns()
	entries, _ := os.ReadDir(dir)
	runs := 0
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), "run-") {
			runs++
		}
	}
	if runs != 10 {
		t.Errorf("cleanup must retain 10 newest run dirs, got %d", runs)
	}
}

func TestParseLastJSONLine_EmptyAndBlank(t *testing.T) {
	r := &RunResult{}
	parseLastJSONLine("", r) // empty → no-op
	parseLastJSONLine("\n\n", r) // blank last line → no-op
	if r.Status != "" {
		t.Errorf("blank stdout must leave status unset, got %q", r.Status)
	}
}

func TestNewLLMCompiler_Constructor(t *testing.T) {
	c := NewLLMCompiler(func() (hexagon.Provider, string, error) { return nil, "", nil })
	if c == nil || c.maxTokens != 4096 {
		t.Errorf("constructor must set maxTokens=4096, got %+v", c)
	}
}

func TestCompileWithProgress_ResolverPaths(t *testing.T) {
	ctx := context.Background()
	// empty prompt
	c0 := NewLLMCompilerStatic(&fakeProvider{}, "m")
	if _, err := c0.Compile(ctx, "  ", CompileHints{}); err == nil {
		t.Error("empty prompt must error")
	}
	// resolver returns error
	cErr := NewLLMCompiler(func() (hexagon.Provider, string, error) { return nil, "", errStubCompile })
	if _, err := cErr.Compile(ctx, "do x", CompileHints{}); err == nil {
		t.Error("resolver error must propagate")
	}
	// resolver returns nil provider
	cNil := NewLLMCompiler(func() (hexagon.Provider, string, error) { return nil, "m", nil })
	if _, err := cNil.Compile(ctx, "do x", CompileHints{}); err == nil {
		t.Error("nil provider must error")
	}
	// static compiler with nil provider
	cStatic := &LLMCompiler{maxTokens: 4096}
	if _, err := cStatic.Compile(ctx, "do x", CompileHints{}); err == nil {
		t.Error("missing provider must error")
	}
	// resolver success path + onProgress callback exercised
	fp := &fakeProvider{resp: &llm.CompletionResponse{
		Content: `{"runtime":"starlark","script":"` + escapeJSON(validStarlark) + `","timeout_s":30}`,
	}}
	cOK := NewLLMCompiler(func() (hexagon.Provider, string, error) { return fp, "glm-4-flash", nil })
	stages := 0
	_, err := cOK.CompileWithProgress(ctx, "do x", CompileHints{}, func(CompileProgress) { stages++ })
	if err != nil {
		t.Fatalf("resolver success path: %v", err)
	}
	if stages == 0 {
		t.Error("onProgress must receive at least one stage")
	}
}

func TestValidators_MkdirTempFailure(t *testing.T) {
	// Point TMPDIR at an un-createable path so os.MkdirTemp fails — covers the
	// scratch-dir error branch in both python validators without a code seam.
	t.Setenv("TMPDIR", "/nonexistent_devtestops_xyz/deeper")
	if err := validatePythonSyntax("x = 1\n"); err == nil {
		t.Error("MkdirTemp failure must propagate from validatePythonSyntax")
	}
	if err := validateNoForbiddenImports("x = 1\n"); err == nil {
		t.Error("MkdirTemp failure must propagate from validateNoForbiddenImports")
	}
}

func TestExecutorRun_WorkdirCreateFailure(t *testing.T) {
	f := filepath.Join(t.TempDir(), "iamafile")
	if err := os.WriteFile(f, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	// workdir nested under a regular file → os.MkdirAll cannot create it.
	e := NewScriptExecutor().WithWorkdir(filepath.Join(f, "sub"))
	if _, err := e.Run(context.Background(), &JobSpec{Runtime: "python3", Script: `print(1)`, TimeoutSec: 5}); err == nil {
		t.Error("an un-createable workdir must make Run error")
	}
}

func TestStripMarkdownFence_Variants(t *testing.T) {
	fenced := "```json\n{\"runtime\":\"starlark\"}\n```"
	if out := stripMarkdownFence(fenced); strings.Contains(out, "```") {
		t.Errorf("fence must be stripped: %q", out)
	}
	// already-bare input is returned unchanged
	if out := stripMarkdownFence(`{"a":1}`); out != `{"a":1}` {
		t.Errorf("bare input must pass through, got %q", out)
	}
	// a lone fence with no newline → returned unchanged (no body to extract)
	if out := stripMarkdownFence("```"); out != "```" {
		t.Errorf("newline-less fence must pass through, got %q", out)
	}
}

func TestAddJobFromPromptWithProgress_ProgressCompilerPath(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	fp := &fakeProvider{resp: &llm.CompletionResponse{
		Content: `{"runtime":"starlark","script":"` + escapeJSON(validStarlark) + `","timeout_s":30}`,
	}}
	// LLMCompiler implements ProgressCompiler → AddJobFromPromptWithProgress takes
	// the streaming branch. A non-reasoning prompt stays on the compile path.
	s := NewScheduler(db, NewLLMCompilerStatic(fp, "m"), NewScriptExecutor().WithWorkdir(t.TempDir()))
	_ = s.Init(ctx)
	stages := 0
	job, err := s.AddJobFromPromptWithProgress(ctx,
		AddJobRequest{Name: "x", Schedule: "@daily", Prompt: "fetch a board and store", UserID: "u"},
		func(CompileProgress) { stages++ })
	if err != nil || job.Spec == nil {
		t.Fatalf("progress-compiler path: err=%v job=%+v", err, job)
	}
	if stages == 0 {
		t.Error("the streaming branch must emit progress stages")
	}
}

func TestCompile_SalvageFromFence(t *testing.T) {
	// A bare ```starlark fence (no JSON) fails strict parse and the repair round,
	// then the script is salvaged from the fence.
	fp := &fakeProvider{resp: &llm.CompletionResponse{
		Content: "```starlark\nemit({\"status\": \"success\"})\n```",
	}}
	spec, err := NewLLMCompilerStatic(fp, "m").Compile(context.Background(), "do x", CompileHints{})
	if err != nil {
		t.Fatalf("salvage path: %v", err)
	}
	if !strings.Contains(spec.Script, "emit(") {
		t.Errorf("salvaged script must carry the body: %q", spec.Script)
	}
}
