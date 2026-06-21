package builtin

// 无人值守风险自审门 —— LLM 判级实现。
//
// cron 无人值守时，consequential 动作（送达/发布/扣费）先过一次轻量 LLM 判级
// （low/med/high）。cronConfirmer 只放行 low；med/high 与判级失败一律拒（保守 fail-closed）。
// 实现为固定关键词规则 + 单次 LLM 判级，不做复杂策略引擎。

import (
	"context"
	"fmt"
	"strings"
)

// RiskJudgeFunc 是单次 LLM 分类调用（窄接口，让 skill/builtin 与 llmrouter 解耦，便于测试）。
// 返回模型原始回复（期望一个词：low/medium/high）。
type RiskJudgeFunc func(ctx context.Context, prompt string) (string, error)

// highRiskKeywords 是固定规则层：命中即判 high，连 LLM 都不放（省 token，且兜住明显高危）。
// 仅做粗筛，真正的语义判级交给 LLM。
var highRiskKeywords = []string{
	"password", "api key", "api_key", "secret", "token ", "credential",
	"私钥", "密码", "口令", "助记词", "seed phrase", "信用卡", "身份证",
}

type llmRiskReviewer struct {
	judge RiskJudgeFunc
}

// NewLLMRiskReviewer 构造 LLM 风险自审器。judge 为 nil 时一律返回 high（fail-closed）。
func NewLLMRiskReviewer(judge RiskJudgeFunc) RiskReviewer {
	return llmRiskReviewer{judge: judge}
}

func (r llmRiskReviewer) Assess(ctx context.Context, action, payload string) (RiskLevel, error) {
	// 固定规则层：明显高危关键词直接 high，不再问 LLM。
	hay := strings.ToLower(action + "\n" + payload)
	for _, kw := range highRiskKeywords {
		if strings.Contains(hay, kw) {
			return RiskHigh, nil
		}
	}
	if r.judge == nil {
		return RiskHigh, fmt.Errorf("no risk judge configured") // fail-closed
	}
	reply, err := r.judge(ctx, buildRiskPrompt(action, payload))
	if err != nil {
		return RiskHigh, err // 判级失败 → 保守拒（cronConfirmer 见 err 也拒）
	}
	return parseRiskLevel(reply), nil
}

// buildRiskPrompt 构造轻量判级 prompt。
func buildRiskPrompt(action, payload string) string {
	return fmt.Sprintf(`You are a safety classifier for an UNATTENDED automation agent that is about to perform a consequential action with no human watching.

Action: %s
Content/payload: %s

Classify the risk of performing this automatically, WITHOUT human review.
Reply with EXACTLY one word — low, medium, or high:
- low: routine and clearly safe (e.g. a normal digest delivered to the user's own channel)
- medium: uncertain or possibly sensitive
- high: spam / mass-send / leaks credentials or personal data / financial or irreversible / anything you are unsure about

Answer:`, action, clipRiskPayload(payload, 1200))
}

// parseRiskLevel maps a model reply to a level. Conservative: anything that is
// not an explicit "low" (and free of higher-risk words) defaults toward high.
func parseRiskLevel(reply string) RiskLevel {
	s := strings.ToLower(strings.TrimSpace(reply))
	switch {
	case strings.Contains(s, "high"):
		return RiskHigh
	case strings.Contains(s, "medium") || strings.Contains(s, "med"):
		return RiskMedium
	case strings.Contains(s, "low"):
		return RiskLow
	default:
		return RiskHigh // unparseable → conservative deny
	}
}

func clipRiskPayload(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
