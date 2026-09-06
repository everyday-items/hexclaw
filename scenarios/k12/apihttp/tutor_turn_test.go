package apihttp_test

import (
	"testing"
)

func TestTutorTurn_FirstTurnHasSolution(t *testing.T) {
	h := newServer(t)
	rec, out := do(t, h, "POST", "/tutor-turn",
		`{"agent":"mingming","prior_stage":0,"parent_message":"这道题怎么讲","problem":"3.8×3","grade":"五年级上"}`)
	if rec.Code != 200 {
		t.Fatalf("状态 %d", rec.Code)
	}
	if int(out["stage"].(float64)) != 3 {
		t.Errorf("首轮应完整家长参考, got %v", out["stage"])
	}
	if s, _ := out["solution"].(string); s == "" {
		t.Error("首轮应自动给出完整解")
	}
}

func TestTutorTurn_Stage3HasVerifiedSolution(t *testing.T) {
	h := newServer(t)
	rec, out := do(t, h, "POST", "/tutor-turn",
		`{"agent":"mingming","prior_stage":2,"parent_message":"直接讲吧","problem":"3.8×3","grade":"五年级上"}`)
	if rec.Code != 200 {
		t.Fatalf("状态 %d", rec.Code)
	}
	if int(out["stage"].(float64)) != 3 {
		t.Errorf("应阶段三, got %v", out["stage"])
	}
	if s, _ := out["solution"].(string); s == "" {
		t.Error("阶段三应带验算解")
	}
}

func TestTutorTurn_EmotionComfort(t *testing.T) {
	h := newServer(t)
	rec, out := do(t, h, "POST", "/tutor-turn",
		`{"agent":"mingming","prior_stage":2,"parent_message":"他急哭了"}`)
	if rec.Code != 200 {
		t.Fatalf("状态 %d", rec.Code)
	}
	if c, _ := out["comfort"].(bool); !c {
		t.Errorf("应触发情绪守门, got %v", out["comfort"])
	}
	if s, _ := out["solution"].(string); s != "" {
		t.Errorf("守门轮不给解, got %q", s)
	}
}

// 回归锁（契约核对 #1）：阶段一二不下发 badge（前端契约=徽章仅阶段三）。
func TestTutorTurn_NoBadgeBeforeStage3(t *testing.T) {
	h := newServer(t)
	// 阶段一（首轮方向提示，无解）→ badge 应缺省/空。
	_, out := do(t, h, "POST", "/tutor-turn", `{"agent":"mingming","prior_stage":0,"parent_message":"这题怎么讲"}`)
	if b, ok := out["badge"]; ok && b != "" {
		t.Errorf("阶段一不应有 badge, got %v", b)
	}
	// 阶段三（有验算解）→ 应有 badge。
	_, out3 := do(t, h, "POST", "/tutor-turn", `{"agent":"mingming","prior_stage":2,"parent_message":"直接讲吧","problem":"3.8×3","grade":"五年级上"}`)
	if b, _ := out3["badge"].(string); b == "" {
		t.Errorf("阶段三应有 badge, got 空")
	}
}
