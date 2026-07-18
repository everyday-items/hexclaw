package apihttp_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

// 布尔 Correct 删除（架构设计 §4.5：布尔 correct 只能在迁移读取层短期兼容，切换完成后从
// 领域模型删除；§6.14 授权破坏性契约变更）：批改判定统一走 Verdict 五值
// （agree / disagree / unverifiable / out_of_scope / verbatim）。
//
// 反向契约：
//  1. /grade 响应 JSON 不得再含 "correct" 键；
//  2. verdict 字段承载批改判定（agree=答对 / disagree=答错），判错入库行为不变；
//  3. 解题分叉（solve_only）与超纲仍沿用验算/超纲 verdict，无批改判定可用时不伪造。

type gradeCorrectTrueExec struct{}

func (gradeCorrectTrueExec) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	if _, grading := args["student_answer"]; grading {
		return &skill.Result{Metadata: map[string]string{
			"solve_verdict": "agree", "solve_evidence": "numeric_exec", "grade_correct": "true",
		}}, nil
	}
	return &skill.Result{Content: "解：11.4", Metadata: map[string]string{"solve_verdict": "agree", "solve_evidence": "numeric_exec"}}, nil
}

func TestGradeResponse_NoBooleanCorrectKey_WrongAnswer(t *testing.T) {
	h := newServer(t) // fakeSolveExec: grade_correct=false（判错）
	rec, out := do(t, h, "POST", "/grade",
		`{"agent":"mingming","grade":"五年级上","problem":"3.8×3=?","student_answer":"10.4","knowledge_points":["小数乘法"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("/grade 应 200, got %d body=%v", rec.Code, out)
	}
	if _, has := out["correct"]; has {
		t.Errorf("响应 JSON 不得再含 correct 键（布尔判定已删除，统一 Verdict 五值）: %v", out)
	}
	if out["verdict"] != "disagree" {
		t.Errorf("判错的批改判定应 verdict=disagree, got %v", out["verdict"])
	}
	if out["record_created"] != true {
		t.Errorf("判错（verdict=disagree）仍应无感入库错题, got record_created=%v", out["record_created"])
	}
	// 徽章仍由验算证据决定（solve agree + numeric_exec = 强信任），与批改判定解耦。
	if out["badge"] != "verified-strong" {
		t.Errorf("验算徽章不应随批改判定改变, got badge=%v", out["badge"])
	}
}

func TestGradeResponse_NoBooleanCorrectKey_RightAnswer(t *testing.T) {
	h := newServerWithSolver(t, gradeCorrectTrueExec{})
	rec, out := do(t, h, "POST", "/grade",
		`{"agent":"mingming","grade":"五年级上","problem":"3.8×3=?","student_answer":"11.4","knowledge_points":["小数乘法"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("/grade 应 200, got %d body=%v", rec.Code, out)
	}
	if _, has := out["correct"]; has {
		t.Errorf("响应 JSON 不得再含 correct 键: %v", out)
	}
	if out["verdict"] != "agree" {
		t.Errorf("判对的批改判定应 verdict=agree, got %v", out["verdict"])
	}
	if out["record_created"] == true {
		t.Errorf("判对不入错题本, got record_created=%v", out["record_created"])
	}
}

func TestGradeResponse_SolveOnlyKeepsEvidenceVerdict(t *testing.T) {
	h := newServer(t)
	// student_answer 为空 = 解题分叉：无批改判定，verdict 沿用验算结论（agree）。
	rec, out := do(t, h, "POST", "/grade",
		`{"agent":"mingming","grade":"五年级上","problem":"3.8×3=?","knowledge_points":["小数乘法"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("/grade(solve_only) 应 200, got %d body=%v", rec.Code, out)
	}
	if _, has := out["correct"]; has {
		t.Errorf("解题分叉响应也不得含 correct 键: %v", out)
	}
	if out["solve_only"] != true {
		t.Fatalf("应走解题分叉, got %v", out["solve_only"])
	}
	if out["verdict"] != "agree" {
		t.Errorf("解题分叉 verdict 沿用验算结论, got %v", out["verdict"])
	}
}
