// checker.go 实现 Loop Engineering #3：**独立 checker + 自一致投票**。
//
// 现状的监工是"单次 LLM 判定 + 与 maker 同一个 default 模型"，这正是"谁来监督监督者"
// 在裸奔。本文件把判定升级成两层，优先确定性：
//
//  1. 形式化验收（AcceptanceEngine）——goal 带断言就跑这层；done = 断言全过，
//     置信度=机器判定（高）。这是把判定从 LLM 主观挪回可证的硬门。
//  2. LLM 自一致投票——无形式化断言的主观目标兜底：用一个**独立的 checker 模型**
//     （≠ maker，由调用方注入）采样 N 次投 DONE/NOT_DONE，多数票决；置信度=投票一致度。
//
// 纪律：checker 与 maker 必须是不同角色（理想是不同/更强模型）；done 判定 **fail-closed**
// ——裁判出错或含糊一律按 NOT_DONE 计，绝不因裁判抖动而误放行。Confidence 供 #8 HITL
// 升级用（低置信 → 升级给人）。
package engine

import (
	"context"
	"fmt"
	"strings"
)

// Checker 判定一个 Goal 是否达成。
type Checker struct {
	engine  *AcceptanceEngine // 可空：无则只能走 LLM 投票
	judge   LLMJudge          // 独立 checker 模型（≠ maker）；可空：则只能走形式化验收
	samples int               // 自一致采样次数；1=单次，>1 取多数票
}

// NewChecker 构造 checker。samples<=1 关闭投票（单次判定），>1 启用自一致多数票。
func NewChecker(engine *AcceptanceEngine, judge LLMJudge, samples int) *Checker {
	if samples < 1 {
		samples = 1
	}
	return &Checker{engine: engine, judge: judge, samples: samples}
}

// CheckerVerdict 是一次达成判定的结果。
type CheckerVerdict struct {
	Done       bool
	Confidence float64 // 0..1（供 HITL 升级判据）
	Mode       string  // "acceptance" | "llm_vote" | "none"
	Verdict    Verdict // 形式化验收明细（acceptance 模式）
	Votes      []bool  // 各次 LLM 投票（llm_vote 模式）
	Reason     string
}

// Assess 判定目标是否达成。
func (c *Checker) Assess(ctx context.Context, goal Goal, output string) CheckerVerdict {
	// 优先：可机器判定的形式化验收。
	if goal.HasFormalAcceptance() && c.engine != nil {
		v := c.engine.Evaluate(ctx, output, goal.Acceptance)
		conf := v.Score * 0.5
		if v.Passed {
			conf = 0.5 + 0.5*v.Score // 机器判定通过：高置信
		}
		return CheckerVerdict{Done: v.Passed, Confidence: clamp01(conf), Mode: "acceptance", Verdict: v, Reason: v.Reason}
	}
	// 兜底：独立 LLM 自一致投票。
	if c.judge == nil {
		return CheckerVerdict{Mode: "none", Reason: "无形式化验收且无 LLM 裁判，无法判定（按未达成处理）"}
	}
	return c.voteDone(ctx, goal, output)
}

func (c *Checker) voteDone(ctx context.Context, goal Goal, output string) CheckerVerdict {
	prompt := fmt.Sprintf(`你是独立验收裁判（不是干活的那个 Agent）。判断下面这个目标是否已被产出充分达成。

目标：
%s

产出：
%s

只按这个格式回：第一行只写 DONE 或 NOT_DONE；第二行一句理由（<40 字）。把握不准就回 NOT_DONE。`,
		strings.TrimSpace(goal.Intent), truncate(output, 4000))

	doneCount := 0
	errCount := 0
	votes := make([]bool, 0, c.samples)
	var reasons []string
	for i := 0; i < c.samples; i++ {
		resp, err := c.judge.Judge(ctx, prompt)
		if err != nil {
			errCount++
			votes = append(votes, false) // fail-closed：裁判出错记 NOT_DONE
			continue
		}
		v, reason := parseDoneVote(resp)
		votes = append(votes, v)
		if v {
			doneCount++
		}
		if reason != "" {
			reasons = append(reasons, reason)
		}
	}
	n := len(votes)
	isDone := doneCount*2 > n // 严格多数
	agree := doneCount
	if !isDone {
		agree = n - doneCount
	}
	conf := 0.0
	if n > 0 {
		conf = float64(agree) / float64(n)
	}
	// 裁判全部出错 = 无有效证据：置信归零以触发低置信 HITL；否则全 false 会被算成
	// "一致不通过"的高置信，恰在 checker 失灵时绕过升级（#5）。
	if errCount == n {
		conf = 0
	}
	reason := fmt.Sprintf("LLM 自一致投票 %d/%d 判 DONE", doneCount, n)
	if len(reasons) > 0 {
		reason += "：" + reasons[0]
	}
	return CheckerVerdict{Done: isDone, Confidence: conf, Mode: "llm_vote", Votes: votes, Reason: reason}
}

// parseDoneVote 从裁判输出里解析一票 + 理由。先判否定形（避免 "NOT_DONE" 命中 "done"），
// 含糊一律 fail-closed（NOT_DONE）。
func parseDoneVote(resp string) (bool, string) {
	lower := strings.ToLower(strings.TrimSpace(resp))
	for _, neg := range []string{"not_done", "not done", "notdone", "未完成", "没完成", "未达成"} {
		if strings.Contains(lower, neg) {
			return false, voteReason(resp)
		}
	}
	for _, pos := range []string{"done", "完成", "达成"} {
		if strings.Contains(lower, pos) {
			return true, voteReason(resp)
		}
	}
	return false, voteReason(resp)
}

// voteReason 取裁判输出的第二行作理由（没有则取整体）。
func voteReason(s string) string {
	lines := strings.SplitN(strings.TrimSpace(s), "\n", 2)
	if len(lines) == 2 {
		return strings.TrimSpace(lines[1])
	}
	return strings.TrimSpace(lines[0])
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
