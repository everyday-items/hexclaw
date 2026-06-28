package engine

import (
	"testing"

	hruntime "github.com/hexagon-codes/hexagon/runtime"
)

// TestRuntimeToolCallsToAdapter_StatusDuration 钉死下沉透传契约：
// hexagon 在执行点产出的 Status/DurationMs 必须经 wire 透到客户端，不得在转换时丢弃。
func TestRuntimeToolCallsToAdapter_StatusDuration(t *testing.T) {
	calls := []hruntime.ToolCallRecord{
		{
			ID:        "tc1",
			Name:      "weather",
			Arguments: `{"location":"杭州"}`,
			Result: hruntime.ToolResult{
				Content:    "🌍 杭州 27°C",
				Status:     hruntime.ToolStatusSuccess,
				DurationMs: 1234,
			},
		},
		{
			ID:   "tc2",
			Name: "glob",
			Result: hruntime.ToolResult{
				Content:    "Error: path rejected",
				Error:      "path rejected",
				Status:     hruntime.ToolStatusError,
				DurationMs: 7,
			},
		},
	}

	out := runtimeToolCallsToAdapter(calls)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}

	if out[0].Status != "success" {
		t.Errorf("out[0].Status = %q, want success", out[0].Status)
	}
	if out[0].DurationMs != 1234 {
		t.Errorf("out[0].DurationMs = %d, want 1234", out[0].DurationMs)
	}
	if out[1].Status != "error" {
		t.Errorf("out[1].Status = %q, want error", out[1].Status)
	}
	if out[1].DurationMs != 7 {
		t.Errorf("out[1].DurationMs = %d, want 7", out[1].DurationMs)
	}
}
