package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// 安全不变量（设计⑤根治后）：solve 派生携带**不可伪造 grant** 的沙箱 code_exec 自动放行（否则执行
// 验证形同虚设）；其它系统派生（spawn，无 grant）的 code_exec 仍硬拒——不放宽用户面/通用派生的 RCE 面。
func TestSolve_VerifierCodeExecAutoApproved(t *testing.T) {
	hub := NewPermissionHub(5 * time.Second)
	hook := NewPermissionHook(hub)
	ctxSolve := withSolveGrant(context.Background())
	if err := hook.BeforeToolCall(ctxSolve, &ToolCallInfo{Name: codeExecToolName, Source: "skill"}); err != nil {
		t.Fatalf("solve grant 下沙箱 code_exec 应自动放行，得 err=%v", err)
	}
	ctxSpawn := withSystemDispatch(context.Background(), spawnDispatchSource)
	if err := hook.BeforeToolCall(ctxSpawn, &ToolCallInfo{Name: codeExecToolName, Source: "skill"}); err == nil {
		t.Fatal("非 solve（spawn，无 grant）来源的 code_exec 必须仍被硬拒（防 RCE 面）")
	}
}

// P0（K12 正确性）：Solver–Verifier，verifier 走 code_exec 执行验证 + fresh-context 独立重解。
// 不变量：①验证一致→高置信徽标 ②不一致→诚实并列双答+请复核（不直接采信）③不可验证→标注人工复核
// ④verifier spec 被限定为「只有 code_exec 工具」⑤self-consistency 多数表决。

type solveExec struct {
	mu           sync.Mutex
	specs        []SubAgentSpec
	solverOuts   []string // 按序返回的 solver 输出（self-consistency 用）
	solverIdx    int
	verifierOut  string
	verifierOuts []string // 按序返回的 verifier 输出（校验重试用）
	verifierIdx  int
	graderOut    string
	graderOuts   []string
	graderIdx    int
}

func (s *solveExec) fn(_ context.Context, spec SubAgentSpec) (SubAgentResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs = append(s.specs, spec)
	switch spec.Agent {
	case graderAgentName:
		if s.graderIdx < len(s.graderOuts) {
			o := s.graderOuts[s.graderIdx]
			s.graderIdx++
			return SubAgentResult{Output: o}, nil
		}
		return SubAgentResult{Output: s.graderOut}, nil
	case verifierAgentName:
		if s.verifierIdx < len(s.verifierOuts) {
			o := s.verifierOuts[s.verifierIdx]
			s.verifierIdx++
			return SubAgentResult{Output: o}, nil
		}
		return SubAgentResult{Output: s.verifierOut}, nil
	case solverAgentName:
		out := "解题步骤……\n答案：42"
		if s.solverIdx < len(s.solverOuts) {
			out = s.solverOuts[s.solverIdx]
			s.solverIdx++
		}
		return SubAgentResult{Output: out}, nil
	}
	return SubAgentResult{Output: "?"}, nil
}

func (s *solveExec) specFor(agent string) (SubAgentSpec, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sp := range s.specs {
		if sp.Agent == agent {
			return sp, true
		}
	}
	return SubAgentSpec{}, false
}

func solveArgs(problem string) map[string]any { return map[string]any{"problem": problem} }

// ① 验证一致 → 高置信徽标；solver + verifier 各被调用。
func TestSolve_VerifiedAgreement(t *testing.T) {
	se := &solveExec{verifierOut: "我用 Python 重算。\nVERDICT: AGREE\nCOMPUTED: 42"}
	o := NewSolveSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), solveArgs("小明有 6 盒铅笔，每盒 7 支，一共多少支？"))
	if err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	if !se.has(solverAgentName) || !se.has(verifierAgentName) {
		t.Fatalf("应调 solver + verifier，实得 %v", se.agents())
	}
	if !strings.Contains(res.Content, "答案：42") {
		t.Errorf("应含解题正文，得：%s", res.Content)
	}
	if !strings.Contains(res.Content, "核验") || !strings.Contains(res.Content, "✅") {
		t.Errorf("一致应有已核验高置信徽标，得：%s", res.Content)
	}
}

// ④ verifier 被限定为「只有 code_exec 工具」。
func TestSolve_VerifierToolScopedToCodeExec(t *testing.T) {
	se := &solveExec{verifierOut: "VERDICT: AGREE\nCOMPUTED: 42"}
	o := NewSolveSkill(se.fn, nil)
	if _, err := o.Execute(context.Background(), solveArgs("一个长方形长 8 厘米、宽 5 厘米，面积是多少？")); err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	sp, ok := se.specFor(verifierAgentName)
	if !ok {
		t.Fatal("未派 verifier")
	}
	if len(sp.ToolAllow) != 1 || sp.ToolAllow[0] != codeExecToolName {
		t.Fatalf("verifier 应只许 code_exec，实得 ToolAllow=%v", sp.ToolAllow)
	}
}

// ② 不一致 → 并列双答 + 请复核（不自信采信）。
func TestSolve_Disagreement_HonestDualSurface(t *testing.T) {
	se := &solveExec{verifierOut: "我重算得 43。\nVERDICT: DISAGREE\nCOMPUTED: 43"}
	o := NewSolveSkill(se.fn, nil)
	res, _ := o.Execute(context.Background(), solveArgs("小明有 6 盒铅笔，每盒 7 支，一共多少支？"))
	if !strings.Contains(res.Content, "42") || !strings.Contains(res.Content, "43") {
		t.Errorf("不一致应并列两个答案，得：%s", res.Content)
	}
	if !strings.Contains(res.Content, "复核") || !strings.Contains(res.Content, "⚠️") {
		t.Errorf("不一致应提示复核、不直接采信，得：%s", res.Content)
	}
}

// ③ 不可验证（非计算题）→ 标注人工复核。
func TestSolve_Unverifiable(t *testing.T) {
	se := &solveExec{verifierOut: "本题无法用代码计算。\nVERDICT: UNVERIFIABLE"}
	o := NewSolveSkill(se.fn, nil)
	res, _ := o.Execute(context.Background(), solveArgs("赏析《静夜思》的意境"))
	if !strings.Contains(res.Content, "无法") && !strings.Contains(res.Content, "人工") {
		t.Errorf("不可验证应标注人工复核，得：%s", res.Content)
	}
}

// ⑤ self-consistency：3 次解题多数表决得共识答案。
func TestSolve_SelfConsistencyMajority(t *testing.T) {
	se := &solveExec{
		solverOuts:  []string{"答案：42", "答案：42", "答案：43"},
		verifierOut: "VERDICT: AGREE\nCOMPUTED: 42",
	}
	o := NewSolveSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), map[string]any{"problem": "6×7", "self_consistency": float64(3)})
	if err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	if se.solverCalls() != 3 {
		t.Fatalf("self_consistency=3 应跑 3 次 solver，实得 %d", se.solverCalls())
	}
	if !strings.Contains(res.Content, "42") {
		t.Errorf("多数表决共识应为 42，得：%s", res.Content)
	}
}

// 方法多样：跑 2 个「不同方法」的 solver；两法一致 + 核验 → 高置信。
func TestSolve_MethodDiversity_TwoDistinctMethods(t *testing.T) {
	se := &solveExec{verifierOut: "VERDICT: AGREE\nCOMPUTED: 42"}
	o := NewSolveSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), map[string]any{"problem": "6×7", "method_diversity": true})
	if err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	if se.solverCalls() != 2 {
		t.Fatalf("method_diversity 应跑 2 个不同方法的 solver，实得 %d", se.solverCalls())
	}
	var tasks []string
	for _, sp := range se.specs {
		if sp.Agent == solverAgentName {
			tasks = append(tasks, sp.Task)
		}
	}
	if len(tasks) == 2 && tasks[0] == tasks[1] {
		t.Errorf("两个 solver 应使用不同方法（prompt 应不同）")
	}
	if !strings.Contains(res.Content, "解法") || !strings.Contains(res.Content, "✅") {
		t.Errorf("两法一致 + 核验应高置信，得：%s", res.Content)
	}
}

// 两法分歧 → code_exec 独立核验充当裁决者（判出正确解法 + 提示另一解法有误）。
func TestSolve_MethodDiversity_VerifierAdjudicates(t *testing.T) {
	se := &solveExec{
		solverOuts:  []string{"用代数解……\n答案：42", "用代入解……\n答案：43"},
		verifierOut: "我用代码重算。\nVERDICT: AGREE\nCOMPUTED: 42",
	}
	o := NewSolveSkill(se.fn, nil)
	res, _ := o.Execute(context.Background(), map[string]any{"problem": "6×7", "method_diversity": true})
	if !strings.Contains(res.Content, "42") || !strings.Contains(res.Content, "43") {
		t.Errorf("两法分歧应并列两个答案，得：%s", res.Content)
	}
	if !strings.Contains(res.Content, "核验") {
		t.Errorf("应说明由代码核验裁决，得：%s", res.Content)
	}
}

// P0.5 triage：单步算术 → 直接作答，略过校验子 Agent（延迟优先）。
func TestSolve_Triage_TrivialSkipsVerifier(t *testing.T) {
	se := &solveExec{verifierOut: "VERDICT: AGREE\nCOMPUTED: 42"}
	o := NewSolveSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), solveArgs("8 × 9 = ?"))
	if err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	if se.has(verifierAgentName) {
		t.Fatalf("简单题应直接作答、不调 verifier（calls=%v）", se.agents())
	}
	if strings.Contains(res.Content, "核验") || strings.Contains(res.Content, "⚠️") {
		t.Errorf("简单题不应带校验徽标，得：%s", res.Content)
	}
}

// P0.5 triage：难题（求证/多问）→ 自动上 method_diversity（2 solver）+ 校验。
func TestSolve_Triage_HardEnablesMethodDiversity(t *testing.T) {
	se := &solveExec{verifierOut: "VERDICT: AGREE\nCOMPUTED: 42"}
	o := NewSolveSkill(se.fn, nil)
	if _, err := o.Execute(context.Background(), solveArgs("求证：三角形内角和等于 180 度。")); err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	if se.solverCalls() != 2 {
		t.Fatalf("难题应自动上 method_diversity（2 个 solver），实得 %d", se.solverCalls())
	}
	if !se.has(verifierAgentName) {
		t.Errorf("难题应仍走代码校验")
	}
}

// 显式传参 → triage 让位（即便题面是单步算术也照常校验）。
func TestSolve_Triage_ExplicitParamDisablesTriage(t *testing.T) {
	se := &solveExec{verifierOut: "VERDICT: AGREE\nCOMPUTED: 42"}
	o := NewSolveSkill(se.fn, nil)
	if _, err := o.Execute(context.Background(), map[string]any{"problem": "8 × 9 = ?", "self_consistency": float64(1)}); err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	if !se.has(verifierAgentName) {
		t.Fatalf("显式传参时应让位 triage、照常校验（calls=%v）", se.agents())
	}
}

// P1.5 批改：学生答对 → 肯定。
func TestSolve_Grading_Correct(t *testing.T) {
	se := &solveExec{
		verifierOut: "VERDICT: AGREE\nCOMPUTED: 42",
		graderOut:   "CORRECT: yes\nGUIDANCE: 思路清晰，继续保持。",
	}
	o := NewSolveSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), map[string]any{
		"problem": "小明有 6 盒铅笔，每盒 7 支，一共多少支？", "student_answer": "42",
	})
	if err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	if !se.has(graderAgentName) {
		t.Fatalf("批改模式应派 grader（calls=%v）", se.agents())
	}
	if !strings.Contains(res.Content, "✅") || !strings.Contains(res.Content, "对") {
		t.Errorf("答对应肯定，得：%s", res.Content)
	}
}

// P1.5 批改：学生答错 → 指出错步 + 误区 + 引导（不直接把完整答案塞过去）。
func TestSolve_Grading_WrongFindsStep(t *testing.T) {
	se := &solveExec{
		verifierOut: "VERDICT: AGREE\nCOMPUTED: 42",
		graderOut:   "CORRECT: no\nWRONG_STEP: 第2步把 6×7 当成 6+7 了\nMISCONCEPTION: 把乘法当成加法\nGUIDANCE: 再看看“每盒7支”应该用哪种运算？",
	}
	o := NewSolveSkill(se.fn, nil)
	res, _ := o.Execute(context.Background(), map[string]any{
		"problem": "小明有 6 盒铅笔，每盒 7 支，一共多少支？", "student_answer": "13",
	})
	if !strings.Contains(res.Content, "第2步") || !strings.Contains(res.Content, "误区") {
		t.Errorf("答错应指出错步与误区，得：%s", res.Content)
	}
	if !strings.Contains(res.Content, "提示") && !strings.Contains(res.Content, "引导") {
		t.Errorf("答错应给引导（留余地），得：%s", res.Content)
	}
}

// 对标 Hermes JSON-mode：verifier 首次输出无法解析出 verdict → 校验重试一次 → 第二次干净。
func TestSolve_Verifier_ValidateRetryOnBadFormat(t *testing.T) {
	se := &solveExec{verifierOuts: []string{"嗯我觉得这题答案大概是对的吧～", "VERDICT: AGREE\nCOMPUTED: 42"}}
	o := NewSolveSkill(se.fn, nil)
	res, err := o.Execute(context.Background(), solveArgs("小明有 6 盒铅笔，每盒 7 支，一共多少支？"))
	if err != nil {
		t.Fatalf("solve 报错：%v", err)
	}
	if se.verifierCalls() != 2 {
		t.Fatalf("verifier 解析失败应校验重试一次（共 2 次），实得 %d", se.verifierCalls())
	}
	if !strings.Contains(res.Content, "✅") {
		t.Errorf("重试后得到干净 AGREE，应高置信，得：%s", res.Content)
	}
}

// grader 首次输出无 CORRECT 行 → 校验重试一次。
func TestSolve_Grader_ValidateRetryOnBadFormat(t *testing.T) {
	se := &solveExec{
		verifierOut: "VERDICT: AGREE\nCOMPUTED: 42",
		graderOuts:  []string{"你写得不错呀，再想想哦", "CORRECT: no\nWRONG_STEP: 第2步\nMISCONCEPTION: 乘法当加法\nGUIDANCE: 再看看"},
	}
	o := NewSolveSkill(se.fn, nil)
	res, _ := o.Execute(context.Background(), map[string]any{
		"problem": "小明有 6 盒铅笔，每盒 7 支，一共多少支？", "student_answer": "13",
	})
	if se.graderCalls() != 2 {
		t.Fatalf("grader 解析失败应校验重试一次（共 2 次），实得 %d", se.graderCalls())
	}
	if !strings.Contains(res.Content, "第2步") {
		t.Errorf("重试后应解析出错步，得：%s", res.Content)
	}
}

// 真模型实测 BUG-D 回归锁：模型算对 computed 却说 DISAGREE → 以算出的数为准纠正为 AGREE。
func TestSolve_VerifierInconsistencyHardened(t *testing.T) {
	se := &solveExec{verifierOut: "我算了一下。\nVERDICT: DISAGREE\nCOMPUTED: 2550"}
	o := NewSolveSkill(se.fn, nil)
	v, _ := o.verify(context.Background(), "求 1 到 100 偶数和", "2550")
	if v != verdictAgree {
		t.Fatalf("computed==候选却判 DISAGREE 应纠正为 AGREE，得 %s", verdictString(v))
	}
	// 反向不误伤：computed 与候选不同时，DISAGREE 维持。
	se2 := &solveExec{verifierOut: "VERDICT: DISAGREE\nCOMPUTED: 2550"}
	o2 := NewSolveSkill(se2.fn, nil)
	if v2, _ := o2.verify(context.Background(), "x", "2500"); v2 != verdictDisagree {
		t.Fatalf("computed!=候选应维持 DISAGREE，得 %s", verdictString(v2))
	}
}

// ---- helpers ----
func (s *solveExec) has(agent string) bool { _, ok := s.specFor(agent); return ok }
func (s *solveExec) agents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.specs))
	for i, sp := range s.specs {
		out[i] = sp.Agent
	}
	return out
}
func (s *solveExec) solverCalls() int   { return s.agentCalls(solverAgentName) }
func (s *solveExec) verifierCalls() int { return s.agentCalls(verifierAgentName) }
func (s *solveExec) graderCalls() int   { return s.agentCalls(graderAgentName) }
func (s *solveExec) agentCalls(agent string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sp := range s.specs {
		if sp.Agent == agent {
			n++
		}
	}
	return n
}
