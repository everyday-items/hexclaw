package engine

import (
	"context"
	"strings"
	"testing"
)

// 这组是 2026-06-27 四个 solve bug 的**回归锁**（前身是 solve_problems_evidence_test.go 的 4 条 RED 取证）。
// 修前：每条断言「应有的正确行为」在 buggy 代码上 FAIL（失败信息即症状）；修后全部 GREEN，留作回归。
// 命名用 TestSolve_* 前缀 → 自动纳入 hex-test 必跑确定性门 `go test -run 'TestSolve|TestListAgents'`。
//   ①🔴 false-AGREE 硬化  ②🟡 语义等值（数/分数/集合容差）  ③🟡 method-diversity 多元答案分组  ④🟡 过程级校验语义

// ①🔴 BUG: verifier 说 AGREE 但 computed(42)≠候选(48,错)，旧码只硬化了 DISAGREE→AGREE 这一安全向，
// 危险正向（false-AGREE）没堵 → 错答案当「已验证」(K12 最坏失效)。修后必须降级为非 AGREE。
func TestSolve_FalseAgreeHardened(t *testing.T) {
	se := &solveExec{verifierOut: "VERDICT: AGREE\nCOMPUTED: 42"}
	o := NewSolveSkill(se.fn, nil)
	if v, c := o.verify(context.Background(), "6×7 等于多少", "48"); v == verdictAgree {
		t.Errorf("回归(BUG①): computed=%q 与候选48可确信不等，verify 却返回 AGREE —— false-AGREE 未硬化，错答案被当『已验证』", c)
	}
	// 不误伤①：computed≡候选 → AGREE 维持。
	se2 := &solveExec{verifierOut: "VERDICT: AGREE\nCOMPUTED: 42"}
	if v2, _ := NewSolveSkill(se2.fn, nil).verify(context.Background(), "6×7", "42"); v2 != verdictAgree {
		t.Errorf("回归(BUG①): computed≡候选(42)应维持 AGREE，得 %s", verdictString(v2))
	}
	// 不误伤②：候选『42支』解析不出数（等价文字形式）→ 不武断下调，AGREE 维持。
	se3 := &solveExec{verifierOut: "VERDICT: AGREE\nCOMPUTED: 42"}
	if v3, _ := NewSolveSkill(se3.fn, nil).verify(context.Background(), "几支铅笔", "42支"); v3 != verdictAgree {
		t.Errorf("回归(BUG①): 『42支』≡computed 42 属等价文字形式，不应被误降，得 %s", verdictString(v3))
	}
}

// AP-151：真模型偶发把可计算题口头标成 UNVERIFIABLE，但同时给出 COMPUTED 数值。
// 只要 computed 与候选可客观比较，就必须用数值纠偏，不能把 2550/2500 这种明确对错降成不可验证。
func TestSolve_UnverifiableWithComputedHardened(t *testing.T) {
	o := NewSolveSkill((&solveExec{verifierOut: "VERDICT: UNVERIFIABLE\nCOMPUTED: 2550"}).fn, nil)
	if v, c := o.verify(context.Background(), "求 1 到 100 所有偶数的和。", "2550"); v != verdictAgree {
		t.Errorf("回归(AP-151): computed=%q 与候选2550相等时应纠为 AGREE，得 %s", c, verdictString(v))
	}

	o = NewSolveSkill((&solveExec{verifierOut: "VERDICT: UNVERIFIABLE\nCOMPUTED: 2550"}).fn, nil)
	if v, c := o.verify(context.Background(), "求 1 到 100 所有偶数的和。", "2500"); v != verdictDisagree {
		t.Errorf("回归(AP-151): computed=%q 与候选2500可确信不等时应纠为 DISAGREE，得 %s", c, verdictString(v))
	}
}

// ②🟡 BUG: 0.5≡1/2 等价，旧 normalizeAnswer 字符串比对认不出 → 模型误判 DISAGREE 时代码救不回。
// 修后：语义等值原语认出等价，并据「算出的数≡候选」把误判的 DISAGREE 纠回 AGREE。
func TestSolve_EquivAnswerSemanticEqual(t *testing.T) {
	for _, c := range []struct{ a, b string }{
		{"0.5", "1/2"}, {"50%", "0.5"}, {"1/4", "0.25"}, {"12和8", "8和12"}, {"2550", "2550"},
	} {
		if !answersEqual(c.a, c.b) {
			t.Errorf("回归(BUG②): answersEqual(%q,%q) 应等值", c.a, c.b)
		}
	}
	se := &solveExec{verifierOut: "VERDICT: DISAGREE\nCOMPUTED: 0.5"}
	o := NewSolveSkill(se.fn, nil)
	if v, _ := o.verify(context.Background(), "1 除以 2", "1/2"); v == verdictDisagree {
		t.Errorf("回归(BUG②): 0.5≡1/2，模型误判 DISAGREE 应被代码纠回（仍得 disagree）")
	}
}

// ③🟡 BUG: '12和8' 与 '8和12' 是同一答案集，旧 groupByAnswer 按字符串分两组 → 误当「两法分歧」。
// 修后：canonicalAnswerKey 顺序无关 → 归 1 组。
func TestSolve_MethodDiversityMultiPartGrouped(t *testing.T) {
	groups := groupByAnswer([]solverSolution{
		{output: "答案：12和8", answer: extractFinalAnswer("答案：12和8")},
		{output: "答案：8和12", answer: extractFinalAnswer("答案：8和12")},
	})
	if len(groups) != 1 {
		t.Errorf("回归(BUG③): '12和8'≡'8和12' 应归 1 组，得 %d 组 —— 否则相同结论被误判成『两法分歧』", len(groups))
	}
}

// ④🟡 BUG: verifier prompt 只比对「最终答案」，无逐步过程校验 → 「答案对、推理错」抓不出。
// 修后：prompt 含过程级（逐步/每一步/过程是否…）校验语义。
func TestSolve_VerifierProcessLevelCheck(t *testing.T) {
	p := buildVerifierPrompt("任意题", "任意候选")
	hasStepCheck := false
	for _, kw := range []string{"逐步", "每一步", "每步", "step", "过程是否", "推理是否正确"} {
		if strings.Contains(p, kw) {
			hasStepCheck = true
		}
	}
	if !hasStepCheck {
		t.Errorf("回归(BUG④): verifier prompt 应含过程级(逐步)校验语义，治『答案对但推理错』")
	}
}

// AP-122 回归锁：solver 输出无干净最终答案（截断、无『答案：』行，末行=垃圾推理句）时，
// 即便 verifier 措辞 AGREE，solve **也不得**盖「✅ 已核验一致（高置信）」——否则徽标失准
// （盖在不存在的"已核验最终答案"上）。修前症状：末行回退成垃圾候选→verifier 对非数值候选不降级→
// verdict 维持 agree→formatSolve 盖高置信。修后：agree 徽标 gate「有可抽取的合法最终答案」，否则降复核。
func TestSolve_NoCleanFinalAnswer_NoFalseHighConfidence(t *testing.T) {
	// solver 截断：无『答案：』标记，末行是一句推理（extractFinalAnswer 回退成它=垃圾候选）。
	se := &solveExec{
		solverOuts:  []string{"我们一步步解。\n先确定要锯断几次……\n现在让我们用代码再算一次试试"},
		verifierOut: "VERDICT: AGREE\nCOMPUTED: 15",
	}
	o := NewSolveSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), solveArgs("一根木头锯成 6 段，每锯断一次 3 分钟，一共多少分钟？"))
	if err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	if strings.Contains(res.Content, "高置信") {
		t.Errorf("回归(AP-122): solver 无干净最终答案(末行=垃圾推理句)时不应盖『高置信』徽标，得：%s", res.Content)
	}
	if !strings.Contains(res.Content, "复核") {
		t.Errorf("回归(AP-122): 应降级为『…请复核』而非高置信，得：%s", res.Content)
	}
	// 不误伤：solver 有干净『答案：』行 + AGREE → 仍应盖高置信。
	se2 := &solveExec{solverOuts: []string{"解题…\n答案：15"}, verifierOut: "VERDICT: AGREE\nCOMPUTED: 15"}
	res2, _ := NewSolveSkill(se2.fn, nil).Execute(context.Background(), solveArgs("一根木头锯成 6 段，每锯断一次 3 分钟，一共多少分钟？"))
	if !strings.Contains(res2.Content, "高置信") {
		t.Errorf("回归(AP-122): 有干净最终答案『答案：15』+AGREE 仍应盖高置信(防过度抑制)，得：%s", res2.Content)
	}
}

// 语义等值原语（①②③ 与 BUG-D 硬化共用）的直接单元锁：相等/确不等/规范键三向边界。
func TestSolve_SemanticEqualityPrimitive(t *testing.T) {
	// answersDefinitelyDiffer 仅在两边都可解析成数且确不等时为真（保守，避免误伤）。
	if !answersDefinitelyDiffer("42", "48") {
		t.Error("42 vs 48 应可确信不等")
	}
	if answersDefinitelyDiffer("42", "42支") {
		t.Error("'42支' 解析不出数 → 不应判可确信不等（避免误伤等价文字形式）")
	}
	if answersDefinitelyDiffer("0.5", "1/2") {
		t.Error("0.5≡1/2 不应判不等")
	}
	if !answersDefinitelyDiffer("12和8", "12和9") {
		t.Error("{12,8} vs {12,9} 应可确信不等")
	}
	// canonicalAnswerKey：顺序无关 + 数值等值归同键；真不同则异键。
	if canonicalAnswerKey("12和8") != canonicalAnswerKey("8和12") {
		t.Error("'12和8' 与 '8和12' 规范键应相同")
	}
	if canonicalAnswerKey("0.5") != canonicalAnswerKey("1/2") {
		t.Error("'0.5' 与 '1/2' 规范键应相同")
	}
	if canonicalAnswerKey("42") == canonicalAnswerKey("43") {
		t.Error("'42' 与 '43' 规范键应不同")
	}
}
