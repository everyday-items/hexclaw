package apihttp_test

import (
	"testing"
)

// TestRecordMistakeEndpoint（BUG-20260712 治本 · 契约）：POST /record-mistake 直接入错题本，
// 家长填的错因如实落库，且**无条件入库**（不判对错——与 /grade「答对不入库」相反）。
func TestRecordMistakeEndpoint(t *testing.T) {
	h := newFaithfulServer(t)
	rec, out := do(t, h, "POST", "/record-mistake",
		`{"agent":"mingming","subject":"数学","grade":"五年级上","problem":"解方程 2x+15=43","student_answer":"x=13","error_cause":"移项符号错","knowledge_points":["简易方程"]}`)
	if rec.Code != 200 {
		t.Fatalf("/record-mistake 应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if created, _ := out["record_created"].(bool); !created {
		t.Fatalf("记一条错题应新入库, got %v", out)
	}
	if ec, _ := out["error_cause"].(string); ec != "移项符号错" {
		t.Errorf("应保留家长错因, got %v", out["error_cause"])
	}

	// 已入错题本（含题面 + 错因）。
	_, mist := do(t, h, "GET", "/mistakes?agent=mingming", "")
	items, _ := mist["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("应入库 1 条错题, got %d", len(items))
	}
	m0, _ := items[0].(map[string]any)
	if q, _ := m0["question"].(string); q != "解方程 2x+15=43" {
		t.Errorf("题面未如实入库, got %v", q)
	}
	if ec, _ := m0["error_cause"].(string); ec != "移项符号错" {
		t.Errorf("错因未如实入库, got %v", ec)
	}
}

// TestRecordMistake_EmptyProblem_400 空题面 → 400（客户端可修正）。
func TestRecordMistake_EmptyProblem_400(t *testing.T) {
	h := newFaithfulServer(t)
	rec, _ := do(t, h, "POST", "/record-mistake", `{"agent":"mingming","problem":""}`)
	if rec.Code != 400 {
		t.Fatalf("空题面应 400, got %d", rec.Code)
	}
}
