package engineadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// ParentTeachingGuideGenerateFunc is a dedicated single-completion seam for a
// parent guide. It is not shared with tutoring tips, whose contract explicitly
// forbids exercise answers.
type ParentTeachingGuideGenerateFunc func(
	ctx context.Context,
	subject, prompt, grade string,
) (string, error)

func WithParentTeachingGuideGen(fn ParentTeachingGuideGenerateFunc) SolveAdapterOption {
	return func(a *SolveAdapter) { a.parentTeachingGuideGen = fn }
}

func (a *SolveAdapter) SetParentTeachingGuideGen(fn ParentTeachingGuideGenerateFunc) {
	a.parentTeachingGuideGen = fn
}

var _ usecase.ParentTeachingGuideGenerator = (*SolveAdapter)(nil)

func (a *SolveAdapter) GenerateParentTeachingGuide(
	ctx context.Context,
	req usecase.ParentTeachingGuideRequest,
) (usecase.ParentTeachingGuide, error) {
	if a.parentTeachingGuideGen == nil {
		return usecase.ParentTeachingGuide{}, fmt.Errorf("parent teaching guide: 未注入专用生成闭包")
	}
	facts, err := json.Marshal(struct {
		Problem          string   `json:"problem"`
		StudentAnswer    string   `json:"student_answer,omitempty"`
		VerifiedSolution string   `json:"verified_solution"`
		KnowledgePoints  []string `json:"knowledge_points,omitempty"`
		WrongStep        string   `json:"wrong_step,omitempty"`
		ErrorCause       string   `json:"error_cause,omitempty"`
	}{
		Problem: req.Problem, StudentAnswer: req.StudentAnswer,
		VerifiedSolution: req.VerifiedSolution, KnowledgePoints: req.KnowledgePoints,
		WrongStep: req.WrongStep, ErrorCause: req.ErrorCause,
	})
	if err != nil {
		return usecase.ParentTeachingGuide{}, fmt.Errorf("parent teaching guide: encode exact problem facts: %w", err)
	}
	prompt := `请只针对下面这一道题生成家长可照着使用的辅导指南。verified_solution 是已验算的完整解答，是答案和完整方法的唯一依据，不得改写为其他答案或方法。
answer 只能填写 verified_solution 中明确出现的简短最终答案，禁止把整段解答塞入 answer；full_solution_steps 按 verified_solution 的解题顺序填写必要步骤，服务端还会用可信原文确定性分段后覆盖核对。
student_answer、wrong_step、error_cause 是已经冻结的孩子作答与批改事实，只用于生成本题讲解顺序和易错提醒，不得改写或否定这些事实。
严格只输出一个 JSON 对象，字段必须且只能是：
answer（字符串）、full_solution_steps（非空字符串数组，完整方法与必要步骤）、grade_level_method（字符串，当前年级/学期/教材允许的方法）、
likely_mistakes（非空字符串数组，最容易出错的步骤与原因）、
parent_teaching_sequence（非空字符串数组，家长先讲什么、再问什么、何时让孩子自己算）、
follow_up_questions（非空字符串数组，可追问孩子的理解问题）、
checking_method（字符串，家长可独立执行的检查或反向验算）。每一项必须针对本题，禁止复用通用话术。
题目事实：` + string(facts)
	out, err := a.parentTeachingGuideGen(ctx, req.Subject, prompt, req.Grade)
	if err != nil {
		return usecase.ParentTeachingGuide{}, providerResponseError(err)
	}
	var guide usecase.ParentTeachingGuide
	decoder := json.NewDecoder(strings.NewReader(extractParentTeachingGuideJSON(out)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&guide); err != nil {
		return usecase.ParentTeachingGuide{}, fmt.Errorf("parent teaching guide: 解析 JSON 失败: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return usecase.ParentTeachingGuide{}, fmt.Errorf("parent teaching guide: JSON 包含额外内容")
	}
	return guide, nil
}

func extractParentTeachingGuideJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	left, right := strings.IndexByte(raw, '{'), strings.LastIndexByte(raw, '}')
	if left >= 0 && right > left {
		return raw[left : right+1]
	}
	return raw
}
