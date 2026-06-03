package cron

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

// TestCompiler_ProgressEventOrder 验证 LLMCompiler.CompileWithProgress 发出
// calling_llm → validating 两阶段事件，顺序固定。
//
// Compiler 不发 analyzing / persisting；那两个在 Scheduler 层。
func TestCompiler_ProgressEventOrder(t *testing.T) {
	fp := &fakeProvider{
		resp: &llm.CompletionResponse{
			Content: `{"runtime":"python3","script":"` + escapeJSON(validScript) + `","deps":[],"timeout_s":60}`,
		},
	}
	c := NewLLMCompilerStatic(fp, "test-model")

	var stages []ProgressStage
	_, err := c.CompileWithProgress(context.Background(), "采集数据", CompileHints{}, func(p CompileProgress) {
		stages = append(stages, p.Stage)
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	want := []ProgressStage{StageCallingLLM, StageValidating}
	if len(stages) != len(want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
	for i, s := range stages {
		if s != want[i] {
			t.Errorf("stages[%d] = %q, want %q", i, s, want[i])
		}
	}
}

// TestCompiler_ProgressNilCallbackNoop CompileWithProgress(... nil) 等价于 Compile，
// 不应 panic 也不应 emit 任何回调。
func TestCompiler_ProgressNilCallbackNoop(t *testing.T) {
	fp := &fakeProvider{
		resp: &llm.CompletionResponse{
			Content: `{"runtime":"python3","script":"` + escapeJSON(validScript) + `","deps":[],"timeout_s":60}`,
		},
	}
	c := NewLLMCompilerStatic(fp, "")
	spec, err := c.CompileWithProgress(context.Background(), "x", CompileHints{}, nil)
	if err != nil || spec == nil {
		t.Fatalf("nil callback should still succeed: %v", err)
	}
}

// TestScheduler_ProgressFullChain analyzing → calling_llm → validating → persisting 完整 4 阶段。
func TestScheduler_ProgressFullChain(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	fp := &fakeProvider{
		resp: &llm.CompletionResponse{
			Content: `{"runtime":"python3","script":"` + escapeJSON(validScript) + `","deps":[],"timeout_s":60}`,
		},
	}
	c := NewLLMCompilerStatic(fp, "test-model")
	s := NewScheduler(db, c, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	var stages []ProgressStage
	job, err := s.AddJobFromPromptWithProgress(ctx, AddJobRequest{
		Name: "test", Schedule: "@daily", Prompt: "采集数据", UserID: "u1",
	}, func(p CompileProgress) {
		stages = append(stages, p.Stage)
	})
	if err != nil {
		t.Fatalf("AddJobFromPromptWithProgress: %v", err)
	}
	if job == nil || job.Spec == nil {
		t.Fatal("job/spec nil")
	}

	wantOrder := []ProgressStage{StageAnalyzing, StageCallingLLM, StageValidating, StagePersisting}
	if !sliceEqualStages(stages, wantOrder) {
		t.Errorf("stages = %v, want %v", stages, wantOrder)
	}
}

// TestScheduler_ProgressOnCompileError 编译失败后不应发 persisting 阶段。
func TestScheduler_ProgressOnCompileError(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	fp := &fakeProvider{
		resp: &llm.CompletionResponse{Content: `not json`}, // 解析必败
	}
	c := NewLLMCompilerStatic(fp, "")
	s := NewScheduler(db, c, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	var stages []ProgressStage
	_, err := s.AddJobFromPromptWithProgress(ctx, AddJobRequest{
		Name: "x", Schedule: "@daily", Prompt: "x", UserID: "u1",
	}, func(p CompileProgress) { stages = append(stages, p.Stage) })
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "编译") {
		t.Errorf("err: %v", err)
	}
	for _, s := range stages {
		if s == StagePersisting {
			t.Fatal("不应到 persisting 阶段")
		}
	}
}

func sliceEqualStages(a, b []ProgressStage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
