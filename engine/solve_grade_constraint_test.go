package engine

import (
	"context"
	"strings"
	"testing"
)

// 回归锁（修复①）：grade + constraint 注入 solver prompt（此前 buildSolverPrompt 无年级约束）。
func TestBuildSolverPrompt_InjectsGradeConstraint(t *testing.T) {
	p := buildSolverPrompt("解方程 2x=10", "math", "", "五年级上", "分数加减、简易方程")
	if !strings.Contains(p, "五年级上") {
		t.Error("solver prompt 应含年级")
	}
	if !strings.Contains(p, "分数加减、简易方程") {
		t.Error("solver prompt 应含允许范围白名单")
	}
	if !strings.Contains(p, "超纲") {
		t.Error("solver prompt 应告知超纲禁用")
	}
	// 无约束时不注入「学习阶段：」「允许使用的范围：」这两行（requirements 文案里的"学习阶段/允许范围"不算）。
	bare := buildSolverPrompt("1+1", "", "", "", "")
	if strings.Contains(bare, "学习阶段：") || strings.Contains(bare, "允许使用的范围") {
		t.Error("无 grade/constraint 时不应注入约束行")
	}
}

// 回归锁（修复②）：verifier prompt 在有约束时加 OUT_OF_SCOPE 判定；parseVerdict 能识别。
func TestVerifier_OutOfScope(t *testing.T) {
	vp := buildVerifierPrompt("解方程", "x=5", "分数加减")
	if !strings.Contains(vp, "OUT_OF_SCOPE") || !strings.Contains(vp, "超纲") {
		t.Error("有约束时 verifier prompt 应含超纲判定")
	}
	// 无约束时不加超纲维度（不干扰普通解题）。
	if strings.Contains(buildVerifierPrompt("解方程", "x=5", ""), "OUT_OF_SCOPE") {
		t.Error("无约束时 verifier 不应提超纲")
	}
	// parseVerdict 识别 OUT_OF_SCOPE（优先于同现的 AGREE）。
	if v, _ := parseVerdict("VERDICT: AGREE / OUT_OF_SCOPE\nCOMPUTED: 5"); v != verdictOutOfScope {
		t.Errorf("应识别 out_of_scope, got %d", v)
	}
	if verdictString(verdictOutOfScope) != "out_of_scope" {
		t.Errorf("verdictString 应输出 out_of_scope, got %s", verdictString(verdictOutOfScope))
	}
}

func TestSolve_VerifierSeesWorkedStepsAndReverifyKeepsConstraint(t *testing.T) {
	se := &solveExec{
		solverOuts: []string{
			"超纲解法标记：使用微积分。\n答案：42",
			"学段内解法标记：只用乘法竖式。\n答案：42",
		},
		verifierOuts: []string{
			"VERDICT: OUT_OF_SCOPE\nCOMPUTED: 42\n说明：方法超纲",
			"VERDICT: AGREE\nCOMPUTED: 42\n说明：正确且未超纲",
		},
	}
	o := NewSolveSkill(se.fn, nil)
	_, err := o.Execute(context.Background(), map[string]any{
		"problem":          "小明买了六盒彩笔，每盒七支，共有多少支？",
		"constraint":       "只允许整数乘法和乘法竖式",
		"self_consistency": float64(1),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var verifierTasks []string
	for _, spec := range se.specs {
		if spec.Agent == verifierAgentName {
			verifierTasks = append(verifierTasks, spec.Task)
		}
	}
	if len(verifierTasks) != 2 {
		t.Fatalf("want initial verification + constrained re-verification, got %d", len(verifierTasks))
	}
	if !strings.Contains(verifierTasks[0], "超纲解法标记") {
		t.Fatalf("initial verifier must receive the full worked solution, task=%q", verifierTasks[0])
	}
	if !strings.Contains(verifierTasks[1], "学段内解法标记") {
		t.Fatalf("re-verifier must receive the replacement worked solution, task=%q", verifierTasks[1])
	}
	for i, task := range verifierTasks {
		if !strings.Contains(task, "只允许整数乘法和乘法竖式") || !strings.Contains(task, "OUT_OF_SCOPE") {
			t.Fatalf("verifier[%d] lost the scope constraint, task=%q", i, task)
		}
	}
}

// 回归锁（BUG-20260708）：parseVerdict 只在 VERDICT 行判定，说明文字里的"not out of scope"不误伤。
func TestParseVerdict_NegatedOutOfScopeNotMisparsed(t *testing.T) {
	// AGREE + 说明含否定式"not out of scope" → 应 agree，不是 out_of_scope。
	if v, _ := parseVerdict("VERDICT: AGREE\nCOMPUTED: 11.4\n说明：解法正确，not out of scope。"); v != verdictAgree {
		t.Errorf("含'not out of scope'的 AGREE 应判 agree, got %d", v)
	}
	// 真超纲：VERDICT 行就是 OUT_OF_SCOPE → 判超纲。
	if v, _ := parseVerdict("VERDICT: OUT_OF_SCOPE\nCOMPUTED: 5\n说明：用了未学的方程组。"); v != verdictOutOfScope {
		t.Errorf("VERDICT行 OUT_OF_SCOPE 应判超纲, got %d", v)
	}
	// 无 VERDICT 行的宽容回退：全文含 AGREE → agree（out_of_scope 不进回退）。
	if v, _ := parseVerdict("这题算对了，agree。"); v != verdictAgree {
		t.Errorf("回退应识别 agree, got %d", v)
	}
	// DISAGREE 优先于 AGREE（DISAGREE 含子串 AGREE）。
	if v, _ := parseVerdict("VERDICT: DISAGREE\n说明：不一致"); v != verdictDisagree {
		t.Errorf("应 disagree, got %d", v)
	}
}
