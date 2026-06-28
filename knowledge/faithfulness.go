package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// 忠实性 / RAGAS 式 LLM-judge 评测（KB 深度质量门 #1）。
//
// 把"无答案场景零编造"从场景化布尔升级为可量化分：用一个强 chat 模型作裁判，对
// (问题, 检索上下文, 生成答案) 三元组打分，指标对齐 RAGAS：
//   - Faithfulness：答案里有多少比例的事实陈述能在检索上下文中找到支撑（不臆造）。
//     答案若是拒绝/说明资料不足而未陈述任何事实 → 1.0（未编造）。
//   - AnswerRelevance：答案是否切题地回答了问题。
//   - ContextRelevance：检索上下文与问题的相关程度（RAGAS context precision 的近似）。
//
// 裁判被强约束只输出 JSON；解析做稳健兜底（容忍代码围栏/前后缀散文、越界裁剪），
// 绝不 panic。裁判调用本身确定性不可控（真模型），故指标计算/解析在 *_test.go 用
// fakeJudge 单测，真模型跑分见 rag_faithfulness_real_test.go。

// FaithfulnessCase 一条忠实性评测样本。
type FaithfulnessCase struct {
	Name     string
	Question string
	Context  string // 喂给生成模型的检索上下文（通常来自 Manager.Query）
	Answer   string // 生成模型基于 Context 给出的答案（被评判对象）
}

// FaithfulnessScore 单条样本的裁判打分（各项 ∈ [0,1]，越高越好）。
type FaithfulnessScore struct {
	Faithfulness     float64  // 答案受上下文支撑的比例（拒答=1.0）
	AnswerRelevance  float64  // 答案切题程度
	ContextRelevance float64  // 上下文与问题的相关程度
	Unsupported      []string // 裁判判定"上下文未支撑"的答案陈述（疑似编造）
	Reason           string   // 裁判简述
	Raw              string   // 裁判原始输出（取证/调试）
}

// FaithfulnessReport 一组样本的聚合报告。
type FaithfulnessReport struct {
	N                int
	MeanFaithfulness float64
	MeanAnswerRel    float64
	MeanContextRel   float64
	Scores           []FaithfulnessScore
}

// EvalFaithfulness 用 LLM 裁判给一条 (问题, 上下文, 答案) 三元组打 RAGAS 式分。
// judge 应为强 chat 模型；judge 出错时返回 err（调用方记日志/跳过）。
func EvalFaithfulness(ctx context.Context, judge RerankLLM, c FaithfulnessCase) (FaithfulnessScore, error) {
	if judge == nil {
		return FaithfulnessScore{}, fmt.Errorf("faithfulness: judge 为空")
	}
	out, err := judge.Complete(ctx, buildFaithfulnessPrompt(c))
	if err != nil {
		return FaithfulnessScore{}, fmt.Errorf("faithfulness[%s] judge: %w", c.Name, err)
	}
	return parseFaithfulness(out)
}

// EvalFaithfulnessBatch 顺序评判多条样本并聚合均值（任一 judge 调用失败即返回错误）。
func EvalFaithfulnessBatch(ctx context.Context, judge RerankLLM, cases []FaithfulnessCase) (FaithfulnessReport, error) {
	rep := FaithfulnessReport{N: len(cases)}
	var fSum, aSum, cSum float64
	for _, c := range cases {
		s, err := EvalFaithfulness(ctx, judge, c)
		if err != nil {
			return rep, err
		}
		rep.Scores = append(rep.Scores, s)
		fSum += s.Faithfulness
		aSum += s.AnswerRelevance
		cSum += s.ContextRelevance
	}
	n := float64(len(cases))
	if n == 0 {
		n = 1
	}
	rep.MeanFaithfulness = fSum / n
	rep.MeanAnswerRel = aSum / n
	rep.MeanContextRel = cSum / n
	return rep, nil
}

func buildFaithfulnessPrompt(c FaithfulnessCase) string {
	return fmt.Sprintf(`你是严格的 RAG 答案评判员。给定【问题】【检索上下文】【答案】，按以下规则打分，只输出 JSON。

评判规则：
1. faithfulness（忠实度，0~1）：把【答案】拆成原子事实陈述，逐条判断是否能在【检索上下文】中找到支撑。
   faithfulness = 被支撑的陈述数 / 全部陈述数。
   特例：若【答案】是拒绝回答或说明"资料中没有相关信息"而未陈述任何事实，则 faithfulness=1.0（未编造）。
2. answer_relevance（切题度，0~1）：【答案】是否直接、切题地回应了【问题】。
3. context_relevance（上下文相关度，0~1）：【检索上下文】整体与【问题】的相关程度。
4. unsupported_claims：列出【答案】中无法被上下文支撑的事实陈述（疑似编造）；没有则空数组。
5. reason：一句话简述。

严格只输出如下 JSON，不要任何解释或代码围栏：
{"faithfulness": <0~1>, "answer_relevance": <0~1>, "context_relevance": <0~1>, "unsupported_claims": ["..."], "reason": "..."}

【问题】
%s

【检索上下文】
%s

【答案】
%s`, c.Question, c.Context, c.Answer)
}

// parseFaithfulness 从裁判输出里稳健地抽出 JSON 并裁剪到合法区间。
func parseFaithfulness(raw string) (FaithfulnessScore, error) {
	jsonStr := extractJSONObject(raw)
	if jsonStr == "" {
		return FaithfulnessScore{Raw: raw}, fmt.Errorf("faithfulness: 裁判输出无 JSON 对象: %s", clipRaw(raw))
	}
	var p struct {
		Faithfulness     float64  `json:"faithfulness"`
		AnswerRelevance  float64  `json:"answer_relevance"`
		ContextRelevance float64  `json:"context_relevance"`
		Unsupported      []string `json:"unsupported_claims"`
		Reason           string   `json:"reason"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return FaithfulnessScore{Raw: raw}, fmt.Errorf("faithfulness: 解析 JSON 失败: %w (%s)", err, clipRaw(jsonStr))
	}
	return FaithfulnessScore{
		Faithfulness:     clamp01(p.Faithfulness),
		AnswerRelevance:  clamp01(p.AnswerRelevance),
		ContextRelevance: clamp01(p.ContextRelevance),
		Unsupported:      p.Unsupported,
		Reason:           p.Reason,
		Raw:              raw,
	}, nil
}

// extractJSONObject 返回 s 中第一个 '{' 到与之配平的 '}' 之间的子串（含括号），
// 容忍代码围栏与前后缀散文；找不到平衡对象返回空串。按字符串字面量感知，避免被
// JSON 字符串里的花括号误导。
func extractJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	esc := false
	for i := start; i < len(s); i++ {
		ch := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case ch == '\\':
				esc = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
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

func clipRaw(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > 160 {
		return string(r[:160]) + "…"
	}
	return string(r)
}
