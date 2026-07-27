package apihttp_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/skill"
)

// gradeCapturingExec 记录 solve 收到的 grade 参数，用于验证「年级确定性注入」。
type gradeCapturingExec struct{ lastGrade *string }

func (e gradeCapturingExec) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	if g, ok := args["grade"].(string); ok {
		*e.lastGrade = g
	}
	if _, grading := args["student_answer"]; grading {
		return &skill.Result{Metadata: map[string]string{
			"solve_verdict": "agree", "solve_evidence": "numeric_exec", "grade_correct": "false",
		}}, nil
	}
	return &skill.Result{Content: "解：11.4", Metadata: map[string]string{
		"solve_verdict": "agree", "solve_evidence": "numeric_exec",
	}}, nil
}

// BUG-3（修正方向）：tutor-turn 前端契约 agent 必填、grade 可选（PRD §3.3.3 + AP-4：年级来自
// 孩子档案确定性注入）。阶段三 solve 携带生效年级约束是 K12 正确性命脉——省略 grade 时后端必须
// 据 agent 从档案推导年级，而非留空（原实现忽略 agent → 阶段三 solve 丢年级边界）。
func TestBUG3_TutorTurnDerivesGradeFromProfileWhenOmitted(t *testing.T) {
	captured := ""
	profiles := &memProfiles{m: map[string]k12.ChildProfile{
		"mingming": {ChildName: "明明", GradeTerm: "五年级上"},
	}}
	h := newServerWithSolver(t, gradeCapturingExec{lastGrade: &captured},
		assembly.WithProfiles(profiles))

	// 阶段三（"直接讲" 命中 fullRequestCue）+ 有题目 + **不传 grade** → 应从档案取五年级上。
	rec, _ := do(t, h, "POST", "/tutor-turn",
		`{"agent":"mingming","prior_stage":2,"parent_message":"直接讲","problem":"3.8×3=?"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("tutor-turn 应 200, got %d", rec.Code)
	}
	if captured != "五年级上" {
		t.Errorf("阶段三 solve 应携带档案推导的年级『五年级上』，实收 %q（年级边界丢失=超纲解法风险）", captured)
	}
}

// 显式传 grade 时以显式为准（不被档案覆盖）。
func TestBUG3_TutorTurnExplicitGradeWins(t *testing.T) {
	captured := ""
	profiles := &memProfiles{m: map[string]k12.ChildProfile{
		"mingming": {ChildName: "明明", GradeTerm: "五年级上"},
	}}
	h := newServerWithSolver(t, gradeCapturingExec{lastGrade: &captured},
		assembly.WithProfiles(profiles))
	do(t, h, "POST", "/tutor-turn",
		`{"agent":"mingming","prior_stage":2,"parent_message":"直接讲","problem":"x","grade":"六年级下"}`)
	if captured != "六年级下" {
		t.Errorf("显式 grade 应优先, 实收 %q", captured)
	}
}
