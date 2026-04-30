package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

func TestImproveHook_NilStoreNoop(t *testing.T) {
	h := NewImproveHook(nil)
	if h != nil {
		t.Fatal("nil store 应返回 nil hook")
	}
	// AfterToolCall 在 nil hook 上调用应不 panic（go interface 的 typed nil 行为）
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil hook AfterToolCall panic: %v", r)
		}
	}()
	(h).AfterToolCall(context.Background(), &ToolCallInfo{}, &ToolCallResult{})
}

func TestImproveHook_RecordsOnlySkillSource(t *testing.T) {
	dir := t.TempDir()
	store := skill.NewImproveStore(dir)
	h := NewImproveHook(store)

	// MCP 工具：不应记录
	h.AfterToolCall(context.Background(),
		&ToolCallInfo{Name: "mcp-tool", Source: "mcp", Arguments: map[string]any{"q": "x"}},
		&ToolCallResult{Content: "ok"})

	if got := len(store.Snapshot()); got != 0 {
		t.Errorf("MCP 工具不应进 Snapshot，got %d", got)
	}

	// Skill：应记录
	h.AfterToolCall(context.Background(),
		&ToolCallInfo{Name: "math-tutor", Source: "skill", Arguments: map[string]any{"query": "1+1"}},
		&ToolCallResult{Content: "2"})

	snap := store.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("应记录 1 条；got %d", len(snap))
	}
	if snap[0].SkillName != "math-tutor" || snap[0].UserInput != "1+1" {
		t.Errorf("Execution 字段错；got=%+v", snap[0])
	}
	if !snap[0].Success {
		t.Error("err==nil 时应为 success")
	}
}

func TestImproveHook_FailedExecutionRecorded(t *testing.T) {
	store := skill.NewImproveStore(t.TempDir())
	h := NewImproveHook(store)
	h.AfterToolCall(context.Background(),
		&ToolCallInfo{Name: "x", Source: "skill"},
		&ToolCallResult{Error: errors.New("boom")})

	snap := store.Snapshot()
	if len(snap) != 1 || snap[0].Success {
		t.Errorf("失败也应记录且 Success=false；snap=%+v", snap)
	}
}

func TestImproveHook_LowScoreWritesDraft(t *testing.T) {
	dir := t.TempDir()
	store := skill.NewImproveStore(dir)
	store.Judge = func(skill.Execution) (int, string) { return 3, "答非所问" }
	h := NewImproveHook(store)

	h.AfterToolCall(context.Background(),
		&ToolCallInfo{Name: "math-tutor", Source: "skill", Arguments: map[string]any{"query": "1+1"}},
		&ToolCallResult{Content: "答案是 5"})

	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatalf("应写 1 个 draft；got %d", len(files))
	}
	if !strings.HasPrefix(files[0].Name(), "math-tutor-v2-") {
		t.Errorf("draft 文件名不对：%s", files[0].Name())
	}
	body, _ := os.ReadFile(filepath.Join(dir, files[0].Name()))
	if !strings.Contains(string(body), "答非所问") {
		t.Errorf("draft 应含 reason；body=%s", body)
	}
}

func TestArgString_PrefersCommonKeys(t *testing.T) {
	if got := argString(map[string]any{"query": "x", "noise": "y"}); got != "x" {
		t.Errorf("应优先取 query；got=%s", got)
	}
	if got := argString(map[string]any{"text": "hello"}); got != "hello" {
		t.Errorf("应取 text；got=%s", got)
	}
	if got := argString(nil); got != "" {
		t.Errorf("nil 应返回空")
	}
}
