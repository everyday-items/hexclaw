package skill

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/featureflag"
)

// recordingObserver 收集 phase 进入 / 退出顺序。
type recordingObserver struct {
	enter []Phase
	exit  []Phase
	errs  map[Phase]error
}

func newObs() *recordingObserver { return &recordingObserver{errs: map[Phase]error{}} }

func (o *recordingObserver) OnPhaseEnter(_ context.Context, p Phase, _ Skill) {
	o.enter = append(o.enter, p)
}
func (o *recordingObserver) OnPhaseExit(_ context.Context, p Phase, _ Skill, err error, _ time.Duration) {
	o.exit = append(o.exit, p)
	if err != nil {
		o.errs[p] = err
	}
}

// pipelineSkill 实现完整接口（execute 可指定结果 / 错误，并可注入 LoadContent / VerifyContent 失败）。
type pipelineSkill struct {
	name        string
	desc        string
	execResult  string
	execErr     error
	loadErr     error
	verifyErr   error
	loadCalled  bool
	verifyCalled bool
}

func (s *pipelineSkill) Name() string                                          { return s.name }
func (s *pipelineSkill) Description() string                                   { return s.desc }
func (s *pipelineSkill) Match(string) bool                                     { return false }
func (s *pipelineSkill) Execute(_ context.Context, _ map[string]any) (*Result, error) {
	if s.execErr != nil {
		return nil, s.execErr
	}
	return &Result{Content: s.execResult}, nil
}
func (s *pipelineSkill) ToolDefinition() llm.ToolDefinition {
	return llm.ToolDefinition{Type: "function", Function: llm.ToolFunctionDef{Name: s.name}}
}
func (s *pipelineSkill) LoadContent() (string, error) {
	s.loadCalled = true
	if s.loadErr != nil {
		return "", s.loadErr
	}
	return "loaded", nil
}
func (s *pipelineSkill) VerifyContent() error {
	s.verifyCalled = true
	return s.verifyErr
}

func withPipelineFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagSkillPipelineV1: on,
	})
	return featureflag.WithContext(ctx, flags)
}

func TestRunPipeline_FlagOffReturnsErr(t *testing.T) {
	reg := NewRegistry()
	ctx := withPipelineFlag(context.Background(), false)
	_, err := RunPipeline(ctx, reg, PipelineOptions{})
	if !errors.Is(err, ErrPipelineDisabled) {
		t.Errorf("flag OFF 应返回 ErrPipelineDisabled；got %v", err)
	}
}

func TestRunPipeline_HappyPath_AllPhasesInOrder(t *testing.T) {
	reg := NewRegistry()
	s := &pipelineSkill{name: "math", desc: "math", execResult: "42"}
	_ = reg.Register(s)

	obs := newObs()
	persistCalled := false
	improveCalled := false
	opts := PipelineOptions{
		Query:    "math",
		Args:     map[string]any{"q": "1+1"},
		Observer: obs,
		OnPersist: func(_ context.Context, _ ExecutionTrace) error {
			persistCalled = true
			return nil
		},
		OnImprove: func(_ context.Context, _ ExecutionTrace) error {
			improveCalled = true
			return nil
		},
	}

	ctx := withPipelineFlag(context.Background(), true)
	res, err := RunPipeline(ctx, reg, opts)
	if err != nil {
		t.Fatalf("happy path 不应报错；got %v", err)
	}
	if res.Skill.Name() != "math" || res.Result.Content != "42" {
		t.Errorf("结果错；got %+v", res)
	}

	wantOrder := []Phase{
		PhaseDiscovery, PhaseActivation, PhaseLoading,
		PhaseVerification, PhaseExecution, PhasePersistence, PhaseImprovement,
	}
	if !equalPhases(obs.enter, wantOrder) {
		t.Errorf("Enter 顺序错\nwant %v\ngot  %v", wantOrder, obs.enter)
	}
	if !equalPhases(obs.exit, wantOrder) {
		t.Errorf("Exit 顺序错\nwant %v\ngot  %v", wantOrder, obs.exit)
	}
	if !s.loadCalled {
		t.Error("LoadContent 应被调用")
	}
	if !s.verifyCalled {
		t.Error("VerifyContent 应被调用")
	}
	if !persistCalled || !improveCalled {
		t.Error("OnPersist/OnImprove 应被调用")
	}
}

func TestRunPipeline_LoadFailureShortCircuits(t *testing.T) {
	reg := NewRegistry()
	s := &pipelineSkill{name: "math", loadErr: errors.New("disk error")}
	_ = reg.Register(s)
	obs := newObs()

	ctx := withPipelineFlag(context.Background(), true)
	_, err := RunPipeline(ctx, reg, PipelineOptions{Query: "math", Observer: obs})
	if err == nil {
		t.Fatal("Load 失败应短路")
	}
	if obs.errs[PhaseLoading] == nil {
		t.Error("Loading phase 应被标记 error")
	}
	// 后续 phase 不应被进入
	for _, p := range []Phase{PhaseVerification, PhaseExecution, PhasePersistence, PhaseImprovement} {
		for _, e := range obs.enter {
			if e == p {
				t.Errorf("失败后不应进入 %s", p)
			}
		}
	}
}

func TestRunPipeline_VerifyFailureShortCircuits(t *testing.T) {
	reg := NewRegistry()
	s := &pipelineSkill{name: "math", verifyErr: errors.New("hash mismatch (TOCTOU)")}
	_ = reg.Register(s)
	obs := newObs()

	ctx := withPipelineFlag(context.Background(), true)
	_, err := RunPipeline(ctx, reg, PipelineOptions{Query: "math", Observer: obs})
	if err == nil {
		t.Fatal("Verify 失败应短路")
	}
	for _, p := range []Phase{PhaseExecution, PhasePersistence, PhaseImprovement} {
		for _, e := range obs.enter {
			if e == p {
				t.Errorf("Verify 失败后不应进入 %s", p)
			}
		}
	}
}

func TestRunPipeline_ExecutionFailureStillPersistsAndImproves(t *testing.T) {
	reg := NewRegistry()
	s := &pipelineSkill{name: "math", execErr: errors.New("model timeout")}
	_ = reg.Register(s)

	persistCalled, improveCalled := false, false
	opts := PipelineOptions{
		Query:     "math",
		OnPersist: func(_ context.Context, _ ExecutionTrace) error { persistCalled = true; return nil },
		OnImprove: func(_ context.Context, _ ExecutionTrace) error { improveCalled = true; return nil },
	}
	ctx := withPipelineFlag(context.Background(), true)
	_, err := RunPipeline(ctx, reg, opts)
	if err == nil {
		t.Fatal("Execute 失败应在最终错误中体现")
	}
	if !persistCalled || !improveCalled {
		t.Error("Execute 失败时 Persistence/Improvement 仍应被调用作为 audit trail")
	}
}

func TestRunPipeline_NoMatchingSkill(t *testing.T) {
	reg := NewRegistry()
	ctx := withPipelineFlag(context.Background(), true)
	_, err := RunPipeline(ctx, reg, PipelineOptions{Query: "nonexistent"})
	if err == nil {
		t.Error("空 registry 应返回错误")
	}
}

func TestRunPipeline_ObserverPanicIsolated(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(&pipelineSkill{name: "math", execResult: "ok"})

	panicObs := &panicObserver{}
	ctx := withPipelineFlag(context.Background(), true)
	_, err := RunPipeline(ctx, reg, PipelineOptions{Query: "math", Observer: panicObs})
	if err != nil {
		t.Errorf("observer panic 应被隔离；got %v", err)
	}
}

type panicObserver struct{}

func (p *panicObserver) OnPhaseEnter(_ context.Context, _ Phase, _ Skill) {
	panic("observer boom")
}
func (p *panicObserver) OnPhaseExit(_ context.Context, _ Phase, _ Skill, _ error, _ time.Duration) {
}

func TestRunPipeline_NilRegistryErrors(t *testing.T) {
	ctx := withPipelineFlag(context.Background(), true)
	if _, err := RunPipeline(ctx, nil, PipelineOptions{}); err == nil {
		t.Error("nil registry 应报错")
	}
}

func equalPhases(a, b []Phase) bool {
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
