package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/skill"
)

// testSkill is a minimal Skill implementation for testing.
type testSkill struct {
	name   string
	result string
	err    error
}

func (s *testSkill) Name() string        { return s.name }
func (s *testSkill) Description() string  { return "test skill" }
func (s *testSkill) Match(_ string) bool  { return false }
func (s *testSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(s.name, "test", nil)
}
func (s *testSkill) Execute(_ context.Context, _ map[string]any) (*skill.Result, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &skill.Result{Content: s.result}, nil
}

// blockingHook is a BeforeToolHook that blocks execution.
type blockingHook struct{ err error }

func (h *blockingHook) BeforeToolCall(_ context.Context, _ *ToolCallInfo) error {
	return h.err
}

// recordingHook records hook invocations for order verification.
type recordingHook struct {
	beforeCalled bool
	afterCalled  bool
	afterContent string
}

func (h *recordingHook) BeforeToolCall(_ context.Context, _ *ToolCallInfo) error {
	h.beforeCalled = true
	return nil
}

func (h *recordingHook) AfterToolCall(_ context.Context, _ *ToolCallInfo, result *ToolCallResult) {
	h.afterCalled = true
	h.afterContent = result.Content
}

func TestToolExecutor_ExecuteSkill(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&testSkill{name: "weather", result: "Sunny 25°C"})

	executor := NewToolExecutor(reg, nil)
	recorder := &recordingHook{}
	executor.AddHook(recorder)

	result, err := executor.Execute(context.Background(), "weather", map[string]any{"location": "Beijing"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "Sunny 25°C" {
		t.Errorf("got %q, want %q", result, "Sunny 25°C")
	}
	if !recorder.beforeCalled {
		t.Error("beforeHook was not called")
	}
	if !recorder.afterCalled {
		t.Error("afterHook was not called")
	}
}

func TestToolExecutor_BeforeHookAbort(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&testSkill{name: "shell", result: "output"})

	executor := NewToolExecutor(reg, nil)
	executor.AddHook(&blockingHook{err: errors.New("permission denied")})

	_, err := executor.Execute(context.Background(), "shell", nil)
	if err == nil {
		t.Fatal("expected error from blocking hook")
	}
	if !strings.Contains(err.Error(), "blocked by hook") {
		t.Errorf("error should contain 'blocked by hook', got: %v", err)
	}
}

func TestToolExecutor_ToolNotFound(t *testing.T) {
	reg := skill.NewRegistry()
	executor := NewToolExecutor(reg, nil)

	_, err := executor.Execute(context.Background(), "nonexistent", nil)
	if err == nil {
		t.Fatal("expected error for missing tool")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should contain 'not found', got: %v", err)
	}
}

func TestTruncateHook(t *testing.T) {
	hook := &TruncateHook{MaxChars: 100}
	result := &ToolCallResult{Content: strings.Repeat("a", 200)}

	hook.AfterToolCall(context.Background(), &ToolCallInfo{Name: "test"}, result)

	if len(result.Content) > 150 {
		t.Errorf("truncation failed: content length %d, expected <= 150", len(result.Content))
	}
	if !strings.Contains(result.Content, "truncated") {
		t.Error("truncated content should contain 'truncated' marker")
	}
}

func TestTruncateHook_ShortContent(t *testing.T) {
	hook := &TruncateHook{MaxChars: 100}
	original := "short content"
	result := &ToolCallResult{Content: original}

	hook.AfterToolCall(context.Background(), &ToolCallInfo{Name: "test"}, result)

	if result.Content != original {
		t.Errorf("short content should not be truncated, got %q", result.Content)
	}
}

func TestToolExecutor_HasTool(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&testSkill{name: "search"})

	executor := NewToolExecutor(reg, nil)

	if !executor.HasTool("search") {
		t.Error("HasTool should return true for registered skill")
	}
	if executor.HasTool("nonexistent") {
		t.Error("HasTool should return false for unregistered tool")
	}
}
