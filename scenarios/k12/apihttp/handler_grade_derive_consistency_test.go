package apihttp_test

import (
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
)

// 年级确定性注入一致性（hex-test 审计发现 · AP-187 泛化类 · AP-4 / PRD §3.3.3+§5.2.4）：
// tutor-turn 已据 agent 从档案推导年级，但 /grade 与 /prep-card 仍裸信客户端 grade。
// 三个入口做同一件事（solve 携带生效年级边界），却各写各的——应全部一致地从档案推导。
// 本测试钉死 /grade 与 /prep-card 同样在省略 grade 时据档案注入年级。

func TestGradeDeriveConsistency_GradeEndpointDerivesFromProfile(t *testing.T) {
	captured := ""
	h := newServerWithSolver(t, gradeCapturingExec{lastGrade: &captured},
		assembly.WithProfiles(&memProfiles{m: map[string]k12.ChildProfile{}}))
	if rec, _ := do(t, h, "PUT", "/profile", `{"agent":"mingming","child_name":"明明","grade_term":"五年级上"}`); rec.Code != http.StatusOK {
		t.Fatalf("设档案应 200, got %d", rec.Code)
	}
	// POST /grade 不传 grade → solve 应携带档案年级（否则丢年级边界=超纲解法风险）。
	if rec, _ := do(t, h, "POST", "/grade",
		`{"agent":"mingming","problem":"3.8×3=?","student_answer":"10.4","knowledge_points":["小数乘法"]}`); rec.Code != http.StatusOK {
		t.Fatalf("/grade 应 200, got %d", rec.Code)
	}
	if captured != "五年级上" {
		t.Errorf("/grade 省略 grade 应据档案注入『五年级上』，solve 实收 %q", captured)
	}
}

func TestGradeDeriveConsistency_PrepCardDerivesFromProfile(t *testing.T) {
	captured := ""
	h := newServerWithSolver(t, gradeCapturingExec{lastGrade: &captured},
		assembly.WithProfiles(&memProfiles{m: map[string]k12.ChildProfile{}}))
	if rec, _ := do(t, h, "PUT", "/profile", `{"agent":"mingming","child_name":"明明","grade_term":"五年级上"}`); rec.Code != http.StatusOK {
		t.Fatalf("设档案应 200, got %d", rec.Code)
	}
	// POST /prep-card 不传 grade → 热身题 solve（sectionWarmup）应携带档案年级。
	if rec, _ := do(t, h, "POST", "/prep-card",
		`{"agent":"mingming","knowledge_points":["小数乘法"]}`); rec.Code != http.StatusOK {
		t.Fatalf("/prep-card 应 200, got %d", rec.Code)
	}
	if captured != "五年级上" {
		t.Errorf("/prep-card 省略 grade 应据档案注入『五年级上』，warmup solve 实收 %q", captured)
	}
}
