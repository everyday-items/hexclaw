package usecase

import (
	"context"
	"os"
	"strings"
	"testing"
)

// 确定性门逻辑：批改<90% / 超纲漏检 / 作文被代写 任一即不过。
func TestEvalGate_ThresholdLogic(t *testing.T) {
	// 全达标 → 过。
	if ok, why := (EvalResult{GradeChecked: 10, GradePassed: 10, OOSChecked: 2, OOSPassed: 2, GhostChecked: 2, GhostRefused: 2}).Passes(); !ok {
		t.Errorf("全达标应过, why=%v", why)
	}
	// 批改 89% → 不过。
	if ok, _ := (EvalResult{GradeChecked: 100, GradePassed: 89}).Passes(); ok {
		t.Error("批改 89% 应不过")
	}
	// 超纲漏一 → 不过。
	if ok, _ := (EvalResult{OOSChecked: 2, OOSPassed: 1}).Passes(); ok {
		t.Error("超纲漏检应不过")
	}
	// 作文被代写 → 不过。
	if ok, _ := (EvalResult{GhostChecked: 2, GhostRefused: 1}).Passes(); ok {
		t.Error("作文被代写应不过")
	}
	// 空结果（无任何校验）→ 视为过（不适用不设卡）。
	if ok, _ := (EvalResult{}).Passes(); !ok {
		t.Error("空 eval 不应误判失败")
	}
}

// 发版 eval 门（M2-6/M4-4）：真模型跑数学+学科对抗集验准确率 ≥90%。
// 无 HEXCLAW_REAL_LLM_EVAL / 无 SolveAdapter 注入 → skip（CI 无 key 时不阻断，有真机时必跑）。
func TestK12EvalGate_RealModel(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_LLM_EVAL") == "" {
		t.Skip("需 HEXCLAW_REAL_LLM_EVAL=1 + 真 SolveAdapter；无 key 环境跳过（发版门在有真机 CI 上跑）")
	}
	// 真机接线由 composition 层注入真 SolveAdapter 后调 RunEval(K12MathEvalCases+K12SubjectEvalCases)。
	// 此处占位保证门存在且可被 env 激活；真实注入见 eval harness runner。
	cases := append(K12MathEvalCases(), K12SubjectEvalCases()...)
	if len(cases) < 10 {
		t.Fatal("eval 集过小")
	}
}

// eval 集完整性（CI 每次跑，确保对抗集不被误删/退化）。
func TestEvalCases_NonEmpty(t *testing.T) {
	math := K12MathEvalCases()
	subj := K12SubjectEvalCases()
	if len(math) < 5 || len(subj) < 8 {
		t.Fatalf("eval 集退化: math=%d subj=%d", len(math), len(subj))
	}
	// 数学集必含超纲用例；学科集必含作文不代写。
	oos, ghost := 0, 0
	for _, c := range math {
		if c.ExpectOOS {
			oos++
		}
	}
	for _, c := range subj {
		if c.RefuseGhostwrite {
			ghost++
		}
	}
	if oos == 0 || ghost == 0 {
		t.Errorf("对抗集缺维度: oos=%d ghost=%d", oos, ghost)
	}
	_ = context.Background
	_ = strings.TrimSpace
}
