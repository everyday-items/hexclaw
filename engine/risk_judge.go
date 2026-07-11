package engine

// 一次性 LLM 判级入口。
//
// 无人值守风险自审门（skill/builtin.NewLLMRiskReviewer）需要一个"给一段 prompt、
// 拿一句回复"的轻量 LLM 调用，但又不想耦合到 llmrouter。引擎天然持有 router，于是在
// 这里暴露一个窄方法 JudgeText，main 把它作为 RiskJudgeFunc 注入。

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/egress"
)

// JudgeText runs a single cheap, non-streaming completion against the default
// provider and returns the raw reply text. It is a narrow one-shot helper for
// in-process judges (e.g. the unattended risk reviewer) that need a quick
// LLM verdict without the full ReAct loop. Errors out (fail-closed upstream) when
// no provider is configured.
func (e *ReActEngine) JudgeText(ctx context.Context, prompt string) (string, error) {
	if e == nil || e.router == nil {
		return "", fmt.Errorf("no LLM router configured")
	}
	provider := e.router.Default()
	if provider == nil {
		return "", fmt.Errorf("no default LLM provider configured")
	}
	if _, ok := egress.RequestsFromContext(ctx); ok {
		egress.AddDataClasses(ctx, egress.ClassGeneral)
	} else {
		ctx = egress.WithRequest(ctx, egress.PurposeGeneralChat, "risk-judge", egress.ClassGeneral)
	}
	resp, err := provider.Complete(ctx, llm.CompletionRequest{
		Model:     e.router.ProviderModel(e.router.DefaultName()),
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: prompt}},
		MaxTokens: 16, // a one-word verdict (low/medium/high) — keep it cheap
	})
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}
