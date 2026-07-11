// skill_judge.go 实现 v0.4.x C3 LLM-as-judge —— 给 skill.ImproveStore 注入一个
// 真实可用的评分函数。
//
// 设计取舍：
//
//   - 每次工具调用都打分会消耗 LLM 配额，因此用采样率（默认 0.1 = 10%）
//   - 不采样到的调用一律返回 10 分（"完美"），不触发 draft 写入 —— 这与"判定为
//     差表现才写 draft"的语义一致：没采样到的样本视作 OK
//   - LLM 调用失败 / 超时 / 解析失败时返回 10 分（fail-open），不要因为 judge
//     抖动产生误报 draft 噪音
//   - prompt 是通用版（评估"工具输出是否解决用户输入"），不针对特定领域，
//     场景包可在 cmd/main 替换为定制版

package engine

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"

	hexagon "github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/toolkit/lang/stringx"
)

// LLMSkillJudgeOptions LLM-as-judge 配置。
type LLMSkillJudgeOptions struct {
	// SampleRate 0-1，每次调用按此概率走 LLM；其余直接返回 10 分。默认 0.1。
	SampleRate float64
	// Timeout LLM 调用超时；默认 15 秒。
	Timeout time.Duration
	// Prompt 自定义评分模板，含 %s 占位符（顺序：skill / userInput / output / success）。
	// 留空使用默认通用版。
	Prompt string
}

// NewLLMSkillJudge 用给定 provider/model 构造一个 skill.JudgeFunc。
//
// provider==nil 或 SampleRate<=0 时返回的函数永远直接返回 10 分（fail-open）。
func NewLLMSkillJudge(provider hexagon.Provider, model string, opts LLMSkillJudgeOptions) skill.JudgeFunc {
	rate := opts.SampleRate
	if rate <= 0 {
		rate = 0.1
	}
	if rate > 1 {
		rate = 1
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	tmpl := opts.Prompt
	if tmpl == "" {
		tmpl = defaultJudgePrompt
	}
	if provider == nil {
		return func(skill.Execution) (int, string) { return 10, "" }
	}
	return func(exec skill.Execution) (int, string) {
		if rand.Float64() > rate {
			return 10, ""
		}
		successText := "失败"
		if exec.Success {
			successText = "成功"
		}
		prompt := fmt.Sprintf(tmpl,
			truncate(exec.SkillName, 64),
			truncate(exec.UserInput, 1500),
			truncate(exec.Output, 2000),
			successText,
		)

		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		ctx = egress.WithRequest(ctx, egress.PurposeGeneralChat, "skill-judge", egress.ClassGeneral, egress.ClassRecord)

		temp := 0.1
		req := hexagon.CompletionRequest{
			Model:       model,
			Messages:    []hexagon.Message{{Role: "user", Content: prompt}},
			Temperature: &temp,
		}
		resp, err := provider.Complete(ctx, req)
		if err != nil {
			trace.L(ctx).Debug("LLMSkillJudge LLM 调用失败，返回 10 分（fail-open）",
				"skill", exec.SkillName, "error", err)
			return 10, ""
		}
		score, reason := parseJudgeOutput(resp.Content)
		return score, reason
	}
}

// parseJudgeOutput 解析 LLM 输出。
//
// 期望格式：
//
//	7
//	答案部分正确，但缺少推导过程
//
// 容错：第一行解析失败 → 用 regex 从全文找第一个 0-10 整数；都找不到 → 返回 10
func parseJudgeOutput(content string) (int, string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return 10, ""
	}
	lines := strings.SplitN(content, "\n", 2)
	first := strings.TrimSpace(lines[0])
	score, ok := tryParseScore(first)
	if !ok {
		score, ok = scanFirstScore(content)
	}
	if !ok {
		return 10, ""
	}
	reason := ""
	if len(lines) > 1 {
		reason = strings.TrimSpace(lines[1])
	}
	return score, reason
}

func tryParseScore(s string) (int, bool) {
	s = strings.TrimSpace(strings.Trim(s, ".,，。:：;；"))
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 10 {
		return 0, false
	}
	return n, true
}

// scanFirstScore 从任意字符串里抓第一个 0-10 的整数（含中文标点前后）。
// 例如 "评分：8" / "score=7" / "(7) 准确" 都能命中。
func scanFirstScore(s string) (int, bool) {
	digit := false
	num := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			num = num*10 + int(r-'0')
			digit = true
			if num > 10 {
				digit = false
				num = 0
			}
		} else if digit {
			break
		}
	}
	if !digit || num < 0 || num > 10 {
		return 0, false
	}
	return num, true
}

func truncate(s string, max int) string {
	// rune-safe 截断（委托 toolkit stringx.SubString，保留单字符省略号），避免 CJK 字节切断（BUG-20260625 F-4）。
	head := stringx.SubString(s, 0, max)
	if head == s {
		return s
	}
	return head + "…"
}

const defaultJudgePrompt = `你是一个 AI 工具评估员。请对以下"工具调用结果"打 0-10 分。

工具名: %s
用户输入: %s
工具输出: %s
执行状态: %s

评分标准：
  0-3: 严重错误 / 答非所问 / 完全无用
  4-6: 部分有用但有明显缺陷
  7-8: 准确实用
  9-10: 完美回应用户需求

输出格式（严格）：
第一行：仅一个 0-10 的整数
第二行：用一句话简要说明评分理由（< 40 字）`
