package engine

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/featureflag"
	"github.com/hexagon-codes/hexclaw/skill"
)

// withV2Flag 在测试 ctx 中显式注入 tool.lifecycle.v2 的 Flags，用于覆盖默认值。
func withV2Flag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagToolLifecycleV2: on,
	})
	return featureflag.WithContext(ctx, flags)
}

// orderedHook 记录调用顺序到共享 slice，用于验证 priority 排序生效。
type orderedHook struct {
	id       string
	priority int
	rec      *[]string
	mu       *sync.Mutex
}

func (h *orderedHook) Priority() int { return h.priority }
func (h *orderedHook) BeforeToolCall(_ context.Context, _ *ToolCallInfo) error {
	h.mu.Lock()
	*h.rec = append(*h.rec, "before:"+h.id)
	h.mu.Unlock()
	return nil
}
func (h *orderedHook) AfterToolCall(_ context.Context, _ *ToolCallInfo, _ *ToolCallResult) {
	h.mu.Lock()
	*h.rec = append(*h.rec, "after:"+h.id)
	h.mu.Unlock()
}

// panickyAfterHook 模拟 after hook panic，验证错误隔离。
type panickyAfterHook struct{ called *bool }

func (h *panickyAfterHook) AfterToolCall(_ context.Context, _ *ToolCallInfo, _ *ToolCallResult) {
	*h.called = true
	panic("simulated panic")
}

// witnessAfterHook 在 panicky hook 之后注册，验证 panic 不影响后续 hook。
type witnessAfterHook struct{ called *bool }

func (h *witnessAfterHook) AfterToolCall(_ context.Context, _ *ToolCallInfo, _ *ToolCallResult) {
	*h.called = true
}

// durationCaptureHook 捕获 result.Duration 用于断言。
type durationCaptureHook struct {
	gotStartedAtSet bool
	gotDuration     bool
}

func (h *durationCaptureHook) AfterToolCall(_ context.Context, _ *ToolCallInfo, r *ToolCallResult) {
	h.gotStartedAtSet = !r.StartedAt.IsZero()
	h.gotDuration = r.Duration > 0
}

// stopwatchSkill 让执行至少耗一点时间，便于 Duration > 0
type stopwatchSkill struct{}

func (s *stopwatchSkill) Name() string        { return "stopwatch" }
func (s *stopwatchSkill) Description() string { return "" }
func (s *stopwatchSkill) Match(_ string) bool { return false }
func (s *stopwatchSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("stopwatch", "", nil)
}
func (s *stopwatchSkill) Execute(_ context.Context, _ map[string]any) (*skill.Result, error) {
	// 任何非零工作量；time.Sleep(0) 仍可能让 Duration=0 (单核编译器)，所以加微小循环
	x := 0
	for i := 0; i < 1000; i++ {
		x += i
	}
	_ = x
	return &skill.Result{Content: "ok"}, nil
}

// lifecycleSkill 实现 LifecycleTool 用于验证 Init/Shutdown 调度。
type lifecycleSkill struct {
	stopwatchSkill
	initCalled, shutdownCalled bool
}

func (s *lifecycleSkill) Name() string                     { return "lifecycle-test" }
func (s *lifecycleSkill) Init(_ context.Context) error     { s.initCalled = true; return nil }
func (s *lifecycleSkill) Shutdown(_ context.Context) error { s.shutdownCalled = true; return nil }

func TestToolExecutor_PrioritySortApplied_WhenFlagOn(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&stopwatchSkill{})
	executor := NewToolExecutor(reg, nil)

	var rec []string
	mu := &sync.Mutex{}
	// 注册顺序故意倒着写：B(prio=10), A(prio=50), C(prio=80)
	executor.AddHook(&orderedHook{id: "B", priority: 10, rec: &rec, mu: mu})
	executor.AddHook(&orderedHook{id: "A", priority: 50, rec: &rec, mu: mu})
	executor.AddHook(&orderedHook{id: "C", priority: 80, rec: &rec, mu: mu})

	ctx := withV2Flag(context.Background(), true)
	if _, err := executor.Execute(ctx, "stopwatch", nil); err != nil {
		t.Fatal(err)
	}

	want := []string{"before:B", "before:A", "before:C", "after:B", "after:A", "after:C"}
	if !equalStrSlice(rec, want) {
		t.Errorf("hook 调用顺序错误\nwant %v\ngot  %v", want, rec)
	}
}

func TestToolExecutor_RegistrationOrder_WhenFlagOff(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&stopwatchSkill{})
	executor := NewToolExecutor(reg, nil)

	var rec []string
	mu := &sync.Mutex{}
	executor.AddHook(&orderedHook{id: "B", priority: 10, rec: &rec, mu: mu})
	executor.AddHook(&orderedHook{id: "A", priority: 50, rec: &rec, mu: mu})

	// flag OFF：保持注册顺序（B → A）
	ctx := withV2Flag(context.Background(), false)
	if _, err := executor.Execute(ctx, "stopwatch", nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"before:B", "before:A", "after:B", "after:A"}
	if !equalStrSlice(rec, want) {
		t.Errorf("flag OFF 应保持注册顺序\nwant %v\ngot  %v", want, rec)
	}
}

func TestToolExecutor_AfterHookPanicIsolated_WhenFlagOn(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&stopwatchSkill{})
	executor := NewToolExecutor(reg, nil)

	panicCalled := false
	witnessCalled := false
	executor.AddHook(&panickyAfterHook{called: &panicCalled})
	executor.AddHook(&witnessAfterHook{called: &witnessCalled})

	ctx := withV2Flag(context.Background(), true)
	got, err := executor.Execute(ctx, "stopwatch", nil)
	if err != nil {
		t.Fatalf("after hook panic 应被隔离，但 err=%v", err)
	}
	if got != "ok" {
		t.Errorf("结果未返回；got %q", got)
	}
	if !panicCalled {
		t.Error("panicky hook 应被调用")
	}
	if !witnessCalled {
		t.Error("witness hook 应在 panic 后仍被调用")
	}
}

func TestToolExecutor_DurationFilled_WhenFlagOn(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&stopwatchSkill{})
	executor := NewToolExecutor(reg, nil)

	cap := &durationCaptureHook{}
	executor.AddHook(cap)

	ctx := withV2Flag(context.Background(), true)
	if _, err := executor.Execute(ctx, "stopwatch", nil); err != nil {
		t.Fatal(err)
	}
	if !cap.gotStartedAtSet {
		t.Error("StartedAt 应被填充")
	}
	if !cap.gotDuration {
		t.Error("Duration 应 > 0")
	}
}

func TestToolExecutor_DurationZero_WhenFlagOff(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&stopwatchSkill{})
	executor := NewToolExecutor(reg, nil)

	cap := &durationCaptureHook{}
	executor.AddHook(cap)

	ctx := withV2Flag(context.Background(), false)
	if _, err := executor.Execute(ctx, "stopwatch", nil); err != nil {
		t.Fatal(err)
	}
	if cap.gotStartedAtSet {
		t.Error("flag OFF 时 StartedAt 应为零值")
	}
	if cap.gotDuration {
		t.Error("flag OFF 时 Duration 应为 0")
	}
}

func TestToolExecutor_LifecycleInitShutdown_FlagOn(t *testing.T) {
	reg := skill.NewRegistry()
	lts := &lifecycleSkill{}
	reg.Register(lts)
	executor := NewToolExecutor(reg, nil)

	ctx := withV2Flag(context.Background(), true)
	if err := executor.InitLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	if !lts.initCalled {
		t.Error("Init 应被调用")
	}
	executor.ShutdownLifecycle(ctx)
	if !lts.shutdownCalled {
		t.Error("Shutdown 应被调用")
	}
}

func TestToolExecutor_LifecycleNoop_FlagOff(t *testing.T) {
	reg := skill.NewRegistry()
	lts := &lifecycleSkill{}
	reg.Register(lts)
	executor := NewToolExecutor(reg, nil)

	ctx := withV2Flag(context.Background(), false)
	if err := executor.InitLifecycle(ctx); err != nil {
		t.Fatal(err)
	}
	executor.ShutdownLifecycle(ctx)
	if lts.initCalled || lts.shutdownCalled {
		t.Error("flag OFF 时 Init/Shutdown 应被跳过")
	}
}

func TestToolExecutor_LifecycleInitErrorAborts(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&erroringInitSkill{})
	executor := NewToolExecutor(reg, nil)

	ctx := withV2Flag(context.Background(), true)
	if err := executor.InitLifecycle(ctx); err == nil {
		t.Error("Init error 应被传播")
	}
}

type erroringInitSkill struct{ stopwatchSkill }

func (s *erroringInitSkill) Name() string                     { return "broken" }
func (s *erroringInitSkill) Init(_ context.Context) error     { return errors.New("init boom") }
func (s *erroringInitSkill) Shutdown(_ context.Context) error { return nil }

func equalStrSlice(a, b []string) bool {
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
