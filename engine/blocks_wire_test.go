package engine

import (
	"testing"

	"github.com/hexagon-codes/ai-core/template"
)

// TestRuntimeBlocksToAdapter 钉死 P3 wire 转换：template.Blocks → adapter.Block，
// 顺序保真、tool_result 的 toolName 由同 id 的 tool_use 回填、camelCase 字段对齐前端。
func TestRuntimeBlocksToAdapter(t *testing.T) {
	in := template.NewBlockBuilder().
		Text("先查天气。").
		ToolUse("t1", "weather", `{"city":"杭州"}`).
		ToolResult("t1", "27°C", false, "success").
		Text("再查空气。").
		ToolUse("t2", "aqi", "{}").
		ToolResult("t2", "良", false, "success").
		Build()

	out := runtimeBlocksToAdapter(in)

	if len(out) != 6 {
		t.Fatalf("块数 = %d, want 6: %+v", len(out), out)
	}
	wantTypes := []string{"text", "tool_use", "tool_result", "text", "tool_use", "tool_result"}
	for i, w := range wantTypes {
		if out[i].Type != w {
			t.Fatalf("out[%d].Type = %q, want %q（顺序未保真）", i, out[i].Type, w)
		}
	}
	// tool_use 字段
	if out[1].ID != "t1" || out[1].Name != "weather" || out[1].Input == "" {
		t.Fatalf("tool_use 字段错: %+v", out[1])
	}
	// tool_result 的 toolName 由 tool_use 回填（前端卡片展示需要）
	if out[2].ToolUseID != "t1" || out[2].ToolName != "weather" {
		t.Fatalf("tool_result toolName 未回填: %+v", out[2])
	}
	// 工具间文本如实保留
	if out[3].Type != "text" || out[3].Text != "再查空气。" {
		t.Fatalf("工具间文本块丢失: %+v", out[3])
	}
}

func TestRuntimeBlocksToAdapter_Empty(t *testing.T) {
	if runtimeBlocksToAdapter(nil) != nil {
		t.Fatal("空块应返回 nil")
	}
}
