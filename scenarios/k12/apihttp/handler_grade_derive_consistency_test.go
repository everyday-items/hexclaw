package apihttp_test

import (
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
)

// 年级确定性注入一致性：/grade 省略 grade 时必须据档案注入年级。

func TestGradeDeriveConsistency_GradeEndpointDerivesFromProfile(t *testing.T) {
	captured := ""
	profiles := &memProfiles{m: map[string]k12.ChildProfile{
		"mingming": {ChildName: "明明", GradeTerm: "五年级上"},
	}}
	h := newServerWithSolver(t, gradeCapturingExec{lastGrade: &captured},
		assembly.WithProfiles(profiles))
	// POST /grade 不传 grade → solve 应携带档案年级（否则丢年级边界=超纲解法风险）。
	if rec, _ := do(t, h, "POST", "/grade",
		`{"agent":"mingming","problem":"3.8×3=?","student_answer":"10.4","knowledge_points":["小数乘法"]}`); rec.Code != http.StatusOK {
		t.Fatalf("/grade 应 200, got %d", rec.Code)
	}
	if captured != "五年级上" {
		t.Errorf("/grade 省略 grade 应据档案注入『五年级上』，solve 实收 %q", captured)
	}
}
