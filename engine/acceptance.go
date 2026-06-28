// acceptance.go 实现 Loop Engineering #2：**把评估接进 loop**。
//
// eval/runner.go 那套 golden 很强，但只跑在发版线（offline/CI）。本文件把"验收"
// 下沉成 loop 内每轮都能跑的**终止门**：给定一段产出 + 一组 AcceptanceSpec，
// 确定性地（命令/正则/JSON 全本地，rubric 走注入裁判）算出一个 Verdict——
// done 由此从"LLM 主观一句话"变成"验收断言全过"。
//
// 能力按需注入（capability-based）：不带 CommandRunner，命令断言判 inconclusive；
// 不带 Judge，llm_rubric 判 inconclusive。inconclusive 一律**不算通过**（fail-closed：
// 验不了就别说做完了），并在 Detail 里讲清原因，交给观测/HITL。
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// CheckResult 是单条断言的判定结果。
type CheckResult struct {
	Spec   AcceptanceSpec
	Passed bool
	Score  float64 // 0..1（确定性断言非 0 即 1；rubric 为 score/10）
	Detail string  // 人类可读：过/不过的原因
}

// Verdict 是一组断言的聚合判定。
type Verdict struct {
	Passed  bool          // 所有**非可选**断言都通过
	Score   float64       // 加权通过率 0..1
	Results []CheckResult // 逐条明细（观测/留痕用）
	Reason  string        // 一句话总结（喂回 maker / 给人看）
}

// LLMJudge 是 llm_rubric 断言需要的最小 LLM 能力。与 ReActEngine.JudgeText、
// builtin.RiskJudgeFunc 同签名，便于直接适配；测试可注入 fake。
type LLMJudge interface {
	Judge(ctx context.Context, prompt string) (string, error)
}

// LLMJudgeFunc 把一个函数适配成 LLMJudge。
type LLMJudgeFunc func(ctx context.Context, prompt string) (string, error)

// Judge 实现 LLMJudge。
func (f LLMJudgeFunc) Judge(ctx context.Context, prompt string) (string, error) {
	return f(ctx, prompt)
}

// CommandRunner 跑一条验收命令（如 "go test ./..."）。命令是目标作者声明的可信断言，
// 不是 LLM 现写的代码。抽成接口便于测试 + 便于上层换成沙箱化执行。
type CommandRunner interface {
	Run(ctx context.Context, command, workdir string) (stdout string, exitCode int, err error)
}

// execCommandRunner 是基于 os/exec 的默认实现（sh -c，带超时）。
type execCommandRunner struct{ timeout time.Duration }

// NewExecCommandRunner 构造默认命令执行器；timeout<=0 用 60s。
func NewExecCommandRunner(timeout time.Duration) CommandRunner {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return execCommandRunner{timeout: timeout}
}

func (r execCommandRunner) Run(ctx context.Context, command, workdir string) (string, int, error) {
	cctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	if workdir != "" {
		cmd.Dir = workdir
	}
	out, err := cmd.CombinedOutput()
	stdout := string(out)
	if err == nil {
		return stdout, 0, nil
	}
	var ee *exec.ExitError
	if ok := asExitError(err, &ee); ok {
		return stdout, ee.ExitCode(), nil // 非 0 退出不是 Run 的错误，是断言结果
	}
	return stdout, -1, err // 启动失败 / 超时 等真错误
}

// asExitError 是 errors.As 的窄包装（避免 import errors 仅为一处）。
func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

// AcceptanceEngine 评估一组 AcceptanceSpec。Judge / CmdRunner 可空（对应断言判 inconclusive）。
type AcceptanceEngine struct {
	Judge     LLMJudge      // 可空 → llm_rubric inconclusive
	CmdRunner CommandRunner // 可空 → command inconclusive
	Workdir   string        // command 的工作目录（#7 写隔离时指向 worktree）
}

// NewAcceptanceEngine 构造一个空能力引擎；按需 Set 字段或用 With* 注入。
func NewAcceptanceEngine() *AcceptanceEngine { return &AcceptanceEngine{} }

// Evaluate 跑全部断言，聚合成 Verdict。无断言 → Passed=false + Reason 标注"无形式化验收"
// （让上层据此回退 LLM 裁判并标注，而不是误判为已达成）。
func (e *AcceptanceEngine) Evaluate(ctx context.Context, output string, specs []AcceptanceSpec) Verdict {
	if len(specs) == 0 {
		return Verdict{Passed: false, Score: 0, Reason: "无形式化验收断言（需回退 LLM 裁判）"}
	}
	v := Verdict{Results: make([]CheckResult, 0, len(specs))}
	var totalW int
	var gotWf float64 // 加权得分用浮点累加，避免逐条四舍五入扭曲（rubric 0.7 不该被记满分）（#12）
	requiredCount := 0
	allRequiredPassed := true
	var failNotes []string
	for _, s := range specs {
		r := e.evalOne(ctx, output, s)
		v.Results = append(v.Results, r)
		w := s.effectiveWeight()
		totalW += w
		gotWf += float64(w) * r.Score
		if !s.Optional {
			requiredCount++
			if !r.Passed {
				allRequiredPassed = false
				failNotes = append(failNotes, r.Detail)
			}
		}
	}
	if totalW > 0 {
		v.Score = gotWf / float64(totalW)
	}
	// 至少要有一条**必需**断言通过才算达成：全 optional 的断言集无法作为完成判据，
	// 否则会出现"全失败却 Passed=true"的空过（fail-open）（#3）。
	v.Passed = requiredCount > 0 && allRequiredPassed
	switch {
	case requiredCount == 0:
		v.Reason = "验收断言全为 optional，无必需项可判定完成（按未达成处理）"
	case v.Passed:
		v.Reason = fmt.Sprintf("验收全过（%d 条，加权得分 %.0f%%）", len(specs), v.Score*100)
	default:
		v.Reason = "验收未过：" + strings.Join(failNotes, "；")
	}
	return v
}

// evalOne 判定单条断言。
func (e *AcceptanceEngine) evalOne(ctx context.Context, output string, s AcceptanceSpec) CheckResult {
	label := s.Description
	if label == "" {
		label = string(s.Kind)
	}
	pass := func(ok bool, okMsg, failMsg string) CheckResult {
		if ok {
			return CheckResult{Spec: s, Passed: true, Score: 1, Detail: label + " ✓ " + okMsg}
		}
		return CheckResult{Spec: s, Passed: false, Score: 0, Detail: label + " ✗ " + failMsg}
	}
	switch s.Kind {
	case AcceptContains:
		return pass(strings.Contains(output, s.Value), "含子串", fmt.Sprintf("缺子串 %q", s.Value))
	case AcceptRegex:
		re, err := regexp.Compile(s.Value)
		if err != nil {
			return CheckResult{Spec: s, Passed: false, Score: 0, Detail: label + " ✗ 非法正则: " + err.Error()}
		}
		return pass(re.MatchString(output), "匹配正则", fmt.Sprintf("不匹配正则 %q", s.Value))
	case AcceptJSONField:
		got, ok := jsonFieldValue(output, s.Path)
		if !ok {
			return pass(false, "", fmt.Sprintf("产出非 JSON 或缺字段 %q", s.Path))
		}
		gotStr := fmt.Sprintf("%v", got)
		return pass(gotStr == s.Value, fmt.Sprintf("%s=%s", s.Path, gotStr),
			fmt.Sprintf("%s=%s，期望 %s", s.Path, gotStr, s.Value))
	case AcceptCommand:
		if e.CmdRunner == nil {
			return CheckResult{Spec: s, Passed: false, Score: 0, Detail: label + " ⚠ inconclusive：未配置命令执行器"}
		}
		stdout, code, err := e.CmdRunner.Run(ctx, s.Value, e.Workdir)
		if err != nil {
			return CheckResult{Spec: s, Passed: false, Score: 0, Detail: label + " ⚠ inconclusive：命令执行失败 " + err.Error()}
		}
		if code != 0 {
			return pass(false, "", fmt.Sprintf("命令 exit=%d", code))
		}
		if s.Pattern != "" {
			re, perr := regexp.Compile(s.Pattern)
			if perr != nil {
				return CheckResult{Spec: s, Passed: false, Score: 0, Detail: label + " ✗ 非法 stdout 正则: " + perr.Error()}
			}
			return pass(re.MatchString(stdout), "命令通过且 stdout 匹配", "命令 exit=0 但 stdout 不匹配")
		}
		return pass(true, "命令 exit=0", "")
	case AcceptLLMRubric:
		if e.Judge == nil {
			return CheckResult{Spec: s, Passed: false, Score: 0, Detail: label + " ⚠ inconclusive：未配置 LLM 裁判"}
		}
		score, reason := e.judgeRubric(ctx, output, s)
		ok := score >= s.Threshold
		return CheckResult{
			Spec:   s,
			Passed: ok,
			Score:  float64(score) / 10.0,
			Detail: fmt.Sprintf("%s rubric=%d/10（阈值 %d，%s）：%s", label, score, s.Threshold, passFail(ok), reason),
		}
	default:
		return CheckResult{Spec: s, Passed: false, Score: 0, Detail: label + " ✗ 未知断言类型"}
	}
}

// judgeRubric 让裁判按 rubric 给产出打 0-10 分；复用 parseJudgeOutput 解析。
// 裁判出错/不可解析 → 0 分（fail-closed：验收门不能因裁判抖动而误放行）。
func (e *AcceptanceEngine) judgeRubric(ctx context.Context, output string, s AcceptanceSpec) (int, string) {
	prompt := fmt.Sprintf(`你是验收评审员。请对照下面的"验收标准"给"产出"打 0-10 分。

验收标准：
%s

产出：
%s

输出格式（严格）：
第一行：仅一个 0-10 的整数
第二行：一句话理由（< 40 字）`, strings.TrimSpace(s.Value), truncate(output, 4000))
	resp, err := e.Judge.Judge(ctx, prompt)
	if err != nil {
		return 0, "裁判调用失败：" + err.Error()
	}
	// fail-closed：裁判返回空/不可解析时计 0 分，绝不复用 parseJudgeOutput 的"宽松兜底给 10"
	// （那是 skill 评分 UX 用的；验收门误放行会直接把垃圾产出判成 done）（#1）。
	score, reason, ok := parseScoreStrict(resp)
	if !ok {
		return 0, "裁判输出不可解析（fail-closed 计 0）"
	}
	return score, reason
}

// parseScoreStrict 严格解析裁判分数：解析不出 0-10 整数时 ok=false（验收门 fail-closed 专用，
// 区别于 parseJudgeOutput 的宽松兜底）。
func parseScoreStrict(content string) (score int, reason string, ok bool) {
	content = strings.TrimSpace(content)
	if content == "" {
		return 0, "", false
	}
	lines := strings.SplitN(content, "\n", 2)
	n, parsed := tryParseScore(strings.TrimSpace(lines[0]))
	if !parsed {
		n, parsed = scanFirstScore(content)
	}
	if !parsed {
		return 0, "", false
	}
	if len(lines) > 1 {
		reason = strings.TrimSpace(lines[1])
	}
	return n, reason, true
}

func passFail(ok bool) string {
	if ok {
		return "过"
	}
	return "不过"
}

// jsonFieldValue 从产出里抠出第一个 JSON 对象，按点分 path 取字段。
func jsonFieldValue(output, path string) (any, bool) {
	start := strings.IndexByte(output, '{')
	end := strings.LastIndexByte(output, '}')
	if start < 0 || end <= start {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(output[start : end+1]))
	dec.UseNumber() // 数字保留为 json.Number（字符串形态），避免大整数被 float64 渲染成科学计数法（#10）
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}
