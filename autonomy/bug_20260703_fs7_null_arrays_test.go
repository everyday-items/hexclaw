package autonomy

// FS-7（BUG-20260703）：前端 TS 把 PreflightResult.estimated/needs_decision 声明为
// 非空数组，但 Go nil slice 序列化成 JSON null → 前端 .map/.length 空指针。
// 契约：这两个字段 JSON 恒为数组（空则 []）。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBug20260703_PreflightArraysNeverNull(t *testing.T) {
	// 全绿场景：needs_decision 为空——最容易漏成 null 的分支。
	res := Preflight(nil, nil, PreflightRequest{Source: "workflow", Tools: []string{"file_ops"}})
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(raw)
	if strings.Contains(s, `"needs_decision":null`) {
		t.Errorf("[FS-7] needs_decision 序列化为 null（前端声明非空数组）：%s", s)
	}
	if strings.Contains(s, `"estimated":null`) {
		t.Errorf("[FS-7] estimated 序列化为 null：%s", s)
	}

	var back struct {
		Estimated     []string `json:"estimated"`
		NeedsDecision []string `json:"needs_decision"`
	}
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Estimated == nil || back.NeedsDecision == nil {
		t.Errorf("[FS-7] 反序列化后仍有 nil 数组：estimated=%v needs_decision=%v",
			back.Estimated, back.NeedsDecision)
	}
}
