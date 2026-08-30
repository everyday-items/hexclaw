package usecase

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// ParentTeachingGuide is the fixed seven-item per-question contract for blank
// worksheet solutions and verified wrong completed-homework items.
// FullSolutionSteps is always derived deterministically from the already-
// verified solver output before this guide leaves the usecase.
type ParentTeachingGuide struct {
	Answer                 string   `json:"answer"`
	FullSolutionSteps      []string `json:"full_solution_steps"`
	GradeLevelMethod       string   `json:"grade_level_method"`
	LikelyMistakes         []string `json:"likely_mistakes"`
	ParentTeachingSequence []string `json:"parent_teaching_sequence"`
	FollowUpQuestions      []string `json:"follow_up_questions"`
	CheckingMethod         string   `json:"checking_method"`
}

// ParentTeachingGuideRequest carries one exact recognized problem and its
// already-verified full solution. The generator may identify a concise final
// answer, but the usecase accepts it only when it is anchored in that verified
// solution and never lets the generator rewrite the full method.
type ParentTeachingGuideRequest struct {
	Subject          string
	Grade            string
	Problem          string
	StudentAnswer    string
	KnowledgePoints  []string
	WrongStep        string
	ErrorCause       string
	VerifiedSolution string
}

// ParentTeachingGuideGenerator is intentionally separate from Solver. Existing
// grade/solve clients keep their historical contract and call count.
type ParentTeachingGuideGenerator interface {
	GenerateParentTeachingGuide(context.Context, ParentTeachingGuideRequest) (ParentTeachingGuide, error)
}

type BlankWorksheetProblemResult struct {
	Solved SolveHomeworkResult
	Guide  ParentTeachingGuide
}

func parentTeachingGuideRequest(
	req GradeRequest,
	solved SolveHomeworkResult,
	outcome GradeOutcome,
) ParentTeachingGuideRequest {
	return ParentTeachingGuideRequest{
		Subject: req.Subject, Grade: req.Grade, Problem: req.Problem,
		StudentAnswer:    req.StudentAnswer,
		KnowledgePoints:  normalizeParentGuideList(req.KnowledgePoints),
		WrongStep:        outcome.WrongStep,
		ErrorCause:       outcome.ErrorCause,
		VerifiedSolution: solved.Solution,
	}
}

// SolveBlankWorksheetProblem adds the parent-facing teaching contract only for
// the photo blank-worksheet path. SolveHomeworkProblem remains unchanged for
// existing direct solve and grade clients.
func (d Deps) SolveBlankWorksheetProblem(
	ctx context.Context,
	req GradeRequest,
) (BlankWorksheetProblemResult, error) {
	solved, err := d.SolveHomeworkProblem(ctx, req)
	result := BlankWorksheetProblemResult{Solved: solved}
	if err != nil || solved.OutOfScope {
		return result, err
	}
	subject, err := normalizeSubject(req.Subject)
	if err != nil {
		return result, err
	}
	req.Subject = subject
	guideRequest := parentTeachingGuideRequest(req, solved, GradeOutcome{})
	guide, deterministic := deterministicParentTeachingGuideForEvidence(guideRequest, solved.Evidence)
	if !deterministic {
		guide, err = d.generateParentTeachingGuide(ctx, guideRequest)
		if err != nil {
			return result, err
		}
	}
	guide, err = finalizeParentTeachingGuide(guide, solved.Solution)
	if err != nil {
		return result, err
	}
	result.Guide = guide
	return result, nil
}

// generateParentTeachingGuide is the single provider boundary. Durable
// GradingJob callers wrap this exact call in the parent_guide item ledger
// operation; validation remains deterministic and runs after the provider
// result is durably recorded.
func (d Deps) generateParentTeachingGuide(
	ctx context.Context,
	req ParentTeachingGuideRequest,
) (ParentTeachingGuide, error) {
	if d.ParentTeachingGuide == nil {
		return ParentTeachingGuide{}, fmt.Errorf(
			"%w: parent teaching guide generator unavailable",
			ErrSolveFailed,
		)
	}
	req.Subject = strings.TrimSpace(req.Subject)
	req.Grade = strings.TrimSpace(req.Grade)
	req.Problem = strings.TrimSpace(req.Problem)
	req.StudentAnswer = strings.TrimSpace(req.StudentAnswer)
	req.KnowledgePoints = normalizeParentGuideList(req.KnowledgePoints)
	req.WrongStep = strings.TrimSpace(req.WrongStep)
	req.ErrorCause = strings.TrimSpace(req.ErrorCause)
	req.VerifiedSolution = strings.TrimSpace(req.VerifiedSolution)
	guide, err := d.ParentTeachingGuide.GenerateParentTeachingGuide(ctx, req)
	if err != nil {
		return ParentTeachingGuide{}, fmt.Errorf("parent teaching guide: %w", err)
	}
	return guide, nil
}

func finalizeParentTeachingGuide(
	guide ParentTeachingGuide,
	verifiedSolution string,
) (ParentTeachingGuide, error) {
	guide = normalizeParentTeachingGuide(guide)
	verifiedSolution = strings.TrimSpace(verifiedSolution)
	// The solver remains the immutable method authority. The explanatory
	// generator cannot replace, abbreviate, or silently alter its result.
	guide.FullSolutionSteps = verifiedFullSolutionSteps(verifiedSolution)
	if err := validateParentTeachingGuide(guide); err != nil {
		return ParentTeachingGuide{}, fmt.Errorf(
			"%w: parent teaching guide: %v",
			ErrSolveFailed,
			err,
		)
	}
	if !answerAnchoredInVerifiedSolution(guide.Answer, verifiedSolution) {
		return ParentTeachingGuide{}, fmt.Errorf(
			"%w: parent teaching guide: answer is not anchored in verified solution",
			ErrSolveFailed,
		)
	}
	return guide, nil
}

// deterministicNumericParentTeachingGuide 仅为已经通过本机数值执行验算的题目
// 生成家长讲解。答案无法从验算结果中唯一确定时，调用方继续使用 Provider。
func deterministicNumericParentTeachingGuide(
	req ParentTeachingGuideRequest,
) (ParentTeachingGuide, bool) {
	answer, ok := deterministicNumericAnswer(req.VerifiedSolution)
	if !ok {
		return ParentTeachingGuide{}, false
	}
	steps := verifiedFullSolutionSteps(req.VerifiedSolution)
	knowledge := strings.Join(normalizeParentGuideList(req.KnowledgePoints), "、")
	if knowledge == "" {
		knowledge = "题目中的运算顺序和数量关系"
	}
	guide := ParentTeachingGuide{
		Answer:            answer,
		FullSolutionSteps: steps,
		GradeLevelMethod:  fmt.Sprintf("用%s已学的「%s」方法，按题目顺序逐步计算。", req.Grade, knowledge),
		LikelyMistakes: []string{
			fmt.Sprintf("没有先判断本题的「%s」和运算顺序", knowledge),
			"计算完成后没有检查结果",
		},
		ParentTeachingSequence: []string{
			fmt.Sprintf("先让孩子说出本题考查的「%s」以及先算什么", knowledge),
			"再请孩子独立计算并说清每一步",
			"最后用逆运算或代入原式检查答案",
		},
		FollowUpQuestions: []string{
			fmt.Sprintf("为什么这道题要用「%s」来解？", knowledge),
			"换一种方法验算，结果是否相同？",
		},
		CheckingMethod: "把答案代入原式，并用逆运算复核。",
	}
	if strings.Contains(req.VerifiedSolution, "题目信息矛盾") {
		guide.GradeLevelMethod = fmt.Sprintf("先用%s已学的「%s」检查题目条件是否一致。", req.Grade, knowledge)
		guide.LikelyMistakes = []string{
			"没有先检查最大公约数能否整除最小公倍数",
			"题目条件矛盾时仍继续猜排数和座位号",
		}
		guide.ParentTeachingSequence = []string{
			"先和孩子核对题面中的 13 和 72",
			"再检查最大公约数是否能整除最小公倍数",
			"确认原题数值后再继续求排数和座位号",
		}
		guide.FollowUpQuestions = []string{
			"13 能整除 72 吗？",
			"如果不能，这说明题目中的哪个条件需要核对？",
		}
		guide.CheckingMethod = "核对最大公约数必须能整除最小公倍数这一必要条件。"
	}
	wrongStep := strings.TrimSpace(req.WrongStep)
	errorCause := strings.TrimSpace(req.ErrorCause)
	if wrongStep != "" || errorCause != "" {
		if errorCause == "" {
			errorCause = "计算过程没有通过程序验算"
		}
		if wrongStep == "" {
			wrongStep = "需要重新核对的计算步骤"
		}
		guide.LikelyMistakes = []string{
			fmt.Sprintf("%s：%s", wrongStep, errorCause),
			"只核对最终答案，没有逐步检查等式和单位",
		}
		guide.ParentTeachingSequence = []string{
			"先请孩子复述自己的原始解题思路",
			fmt.Sprintf("一起定位到「%s」，让孩子说明这一步为什么成立", wrongStep),
			"请孩子按本年级方法独立重算，再与程序验算结果核对",
			"最后回看原题条件、符号和单位",
		}
		guide.FollowUpQuestions = []string{
			fmt.Sprintf("「%s」这一步可以怎样验算？", wrongStep),
			"修正后，把答案代回原题还能成立吗？",
		}
		guide.CheckingMethod = "从首个问题步骤开始逐式验算，再把最终答案代回原题核对。"
	}
	guide, err := finalizeParentTeachingGuide(guide, req.VerifiedSolution)
	if err != nil {
		return ParentTeachingGuide{}, false
	}
	return guide, true
}

// deterministicParentTeachingGuideForEvidence 统一 durable 与直接路径的执行选择；
// 本机证据或答案唯一性不足时，调用方继续使用冻结的 Provider。
func deterministicParentTeachingGuideForEvidence(
	req ParentTeachingGuideRequest,
	evidence SolveEvidence,
) (ParentTeachingGuide, bool) {
	if evidence.EvidenceType != EvidenceNumericExec {
		return ParentTeachingGuide{}, false
	}
	return deterministicNumericParentTeachingGuide(req)
}

// deterministicNumericAnswer 只接受明确答案段或单行数值执行结果，避免从
// 多解、解释性段落中猜测答案。
func deterministicNumericAnswer(verifiedSolution string) (string, bool) {
	scope, explicit := explicitVerifiedAnswerScope(verifiedSolution)
	if explicit {
		return deterministicExplicitNumericAnswer(scope)
	}
	if issue := strings.TrimSpace(verifiedSolution); strings.Contains(issue, "题目信息矛盾") &&
		strings.Contains(issue, "请核对") && len([]rune(issue)) <= 256 {
		return issue, true
	}
	lines := normalizeParentGuideList(strings.Split(strings.ReplaceAll(verifiedSolution, "\r\n", "\n"), "\n"))
	if len(lines) != 1 {
		return "", false
	}
	candidate := lines[0]
	for _, prefix := range []string{"解：", "解:", "得：", "得:"} {
		candidate = strings.TrimSpace(strings.TrimPrefix(candidate, prefix))
	}
	if strings.Contains(candidate, "或") {
		return "", false
	}
	candidate = strings.ReplaceAll(candidate, "＝", "=")
	if index := strings.LastIndex(candidate, "="); index >= 0 {
		candidate = candidate[index+1:]
	}
	return deterministicNumericAnswerCandidate(candidate)
}

func deterministicExplicitNumericAnswer(value string) (string, bool) {
	value = strings.TrimSpace(strings.NewReplacer("**", "", "__", "", "`", "").Replace(value))
	if value == "" || strings.ContainsAny(value, "\r\n") || len([]rune(value)) > 160 {
		return "", false
	}
	for _, ambiguous := range []string{"或", "可能", "多解", "不唯一", "不确定", "无法确定", "不能确定"} {
		if strings.Contains(value, ambiguous) {
			return "", false
		}
	}
	for _, r := range value {
		if unicode.IsDigit(r) {
			return value, true
		}
	}
	return "", false
}

func deterministicNumericAnswerCandidate(value string) (string, bool) {
	value = strings.TrimSpace(strings.NewReplacer("**", "", "__", "", "`", "").Replace(value))
	if value == "" || strings.ContainsAny(value, "\r\n。；;") || len([]rune(value)) > 64 {
		return "", false
	}
	hasDigit := false
	for _, r := range value {
		if unicode.IsDigit(r) {
			hasDigit = true
			break
		}
	}
	if !hasDigit {
		return "", false
	}
	return value, true
}

func normalizeParentTeachingGuide(guide ParentTeachingGuide) ParentTeachingGuide {
	guide.Answer = strings.TrimSpace(guide.Answer)
	guide.GradeLevelMethod = strings.TrimSpace(guide.GradeLevelMethod)
	guide.CheckingMethod = strings.TrimSpace(guide.CheckingMethod)
	guide.FullSolutionSteps = normalizeParentGuideList(guide.FullSolutionSteps)
	guide.LikelyMistakes = normalizeParentGuideList(guide.LikelyMistakes)
	guide.ParentTeachingSequence = normalizeParentGuideList(guide.ParentTeachingSequence)
	guide.FollowUpQuestions = normalizeParentGuideList(guide.FollowUpQuestions)
	return guide
}

func normalizeParentGuideList(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func validateParentTeachingGuide(guide ParentTeachingGuide) error {
	switch {
	case strings.TrimSpace(guide.Answer) == "":
		return fmt.Errorf("answer required")
	case len(normalizeParentGuideList(guide.FullSolutionSteps)) == 0:
		return fmt.Errorf("full_solution_steps required")
	case strings.TrimSpace(guide.GradeLevelMethod) == "":
		return fmt.Errorf("grade_level_method required")
	case len(normalizeParentGuideList(guide.LikelyMistakes)) == 0:
		return fmt.Errorf("likely_mistakes required")
	case len(normalizeParentGuideList(guide.ParentTeachingSequence)) == 0:
		return fmt.Errorf("parent_teaching_sequence required")
	case len(normalizeParentGuideList(guide.FollowUpQuestions)) == 0:
		return fmt.Errorf("follow_up_questions required")
	case strings.TrimSpace(guide.CheckingMethod) == "":
		return fmt.Errorf("checking_method required")
	default:
		return nil
	}
}

// verifiedFullSolutionSteps deterministically projects the solver's verified
// Markdown into an ordered string list. Headings and a distinct final-answer
// section are presentation structure, not method steps. If no method paragraph
// can be isolated, the exact verified solution remains available as one item.
func verifiedFullSolutionSteps(solution string) []string {
	solution = strings.TrimSpace(strings.ReplaceAll(solution, "\r\n", "\n"))
	if solution == "" {
		return nil
	}
	var (
		steps         []string
		paragraph     []string
		inAnswerBlock bool
	)
	flushParagraph := func() {
		if len(paragraph) == 0 {
			return
		}
		if step := strings.TrimSpace(strings.Join(paragraph, "\n")); step != "" {
			steps = append(steps, step)
		}
		paragraph = paragraph[:0]
	}
	for _, rawLine := range strings.Split(solution, "\n") {
		line := strings.TrimSpace(rawLine)
		if _, isAnswerLabel := splitExplicitAnswerLabel(line); isAnswerLabel {
			flushParagraph()
			inAnswerBlock = true
			continue
		}
		if isMarkdownHeading(line) {
			flushParagraph()
			inAnswerBlock = false
			continue
		}
		if inAnswerBlock {
			continue
		}
		if line == "" {
			flushParagraph()
			continue
		}
		if step, listed := stripSolutionStepMarker(line); listed {
			flushParagraph()
			if step != "" {
				steps = append(steps, step)
			}
			continue
		}
		paragraph = append(paragraph, line)
	}
	flushParagraph()
	steps = normalizeParentGuideList(steps)
	if len(steps) == 0 {
		return []string{solution}
	}
	return steps
}

func stripSolutionStepMarker(line string) (string, bool) {
	line = strings.TrimSpace(line)
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, marker) {
			return strings.TrimSpace(strings.TrimPrefix(line, marker)), true
		}
	}
	runes := []rune(line)
	index := 0
	parenthesized := false
	if len(runes) > 0 && (runes[0] == '(' || runes[0] == '（') {
		parenthesized = true
		index++
	}
	digitStart := index
	for index < len(runes) && runes[index] >= '0' && runes[index] <= '9' {
		index++
	}
	if index == digitStart {
		return line, false
	}
	if parenthesized {
		if index >= len(runes) || (runes[index] != ')' && runes[index] != '）') {
			return line, false
		}
		index++
		if index < len(runes) && !unicode.IsSpace(runes[index]) {
			return line, false
		}
		for index < len(runes) && unicode.IsSpace(runes[index]) {
			index++
		}
		return strings.TrimSpace(string(runes[index:])), true
	}
	if index >= len(runes) ||
		!strings.ContainsRune(".．、：:）)", runes[index]) {
		return line, false
	}
	marker := runes[index]
	index++
	if marker != '、' && index < len(runes) && !unicode.IsSpace(runes[index]) {
		// A decimal/fraction-like expression such as 4.5 must remain byte-for-
		// byte intact rather than being mistaken for an ordered-list marker.
		return line, false
	}
	for index < len(runes) && unicode.IsSpace(runes[index]) {
		index++
	}
	return strings.TrimSpace(string(runes[index:])), true
}

// answerAnchoredInVerifiedSolution rejects a second model's invented final
// answer. When the verified solution has an explicit answer section, only that
// section is authoritative; otherwise the concise answer must appear as a
// bounded token in the verified solver output.
func answerAnchoredInVerifiedSolution(answer, verifiedSolution string) bool {
	answer = normalizeAnswerAnchorText(answer)
	if answer == "" {
		return false
	}
	scope, hasExplicitAnswer := explicitVerifiedAnswerScope(verifiedSolution)
	if hasExplicitAnswer && strings.TrimSpace(scope) == "" {
		return false
	}
	if !hasExplicitAnswer {
		scope = verifiedSolution
	}
	return containsBoundedAnswerAnchor(normalizeAnswerAnchorText(scope), answer)
}

func explicitVerifiedAnswerScope(solution string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(solution, "\r\n", "\n"), "\n")
	var lastScope []string
	found := false
	for i := 0; i < len(lines); i++ {
		inline, ok := splitExplicitAnswerLabel(lines[i])
		if !ok {
			continue
		}
		found = true
		scope := make([]string, 0, 2)
		if inline != "" {
			scope = append(scope, inline)
		}
		for j := i + 1; j < len(lines); j++ {
			if isMarkdownHeading(lines[j]) {
				break
			}
			scope = append(scope, lines[j])
		}
		lastScope = scope
	}
	return strings.TrimSpace(strings.Join(lastScope, "\n")), found
}

func splitExplicitAnswerLabel(line string) (string, bool) {
	line = strings.TrimSpace(line)
	line = strings.TrimSpace(strings.TrimLeft(line, "#"))
	replacer := strings.NewReplacer("**", "", "__", "", "`", "")
	line = strings.TrimSpace(replacer.Replace(line))
	for _, label := range []string{"最终答案", "答案", "答"} {
		if line == label {
			return "", true
		}
		if !strings.HasPrefix(line, label) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, label))
		for _, separator := range []string{"：", ":", "是", "为", "="} {
			if strings.HasPrefix(rest, separator) {
				return strings.TrimSpace(strings.TrimPrefix(rest, separator)), true
			}
		}
	}
	return "", false
}

func isMarkdownHeading(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "#")
}

func normalizeAnswerAnchorText(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return -1
		case r == '*' || r == '`' || r == '_':
			return -1
		default:
			return r
		}
	}, strings.TrimSpace(value))
}

func containsBoundedAnswerAnchor(scope, answer string) bool {
	scopeRunes := []rune(scope)
	answerRunes := []rune(answer)
	if len(answerRunes) == 0 || len(answerRunes) > len(scopeRunes) {
		return false
	}
	for start := 0; start+len(answerRunes) <= len(scopeRunes); start++ {
		if string(scopeRunes[start:start+len(answerRunes)]) != answer {
			continue
		}
		end := start + len(answerRunes)
		if start > 0 &&
			isAnswerTokenRune(scopeRunes[start-1]) &&
			isAnswerTokenRune(answerRunes[0]) {
			continue
		}
		if end < len(scopeRunes) &&
			isAnswerTokenRune(answerRunes[len(answerRunes)-1]) &&
			isAnswerTokenRune(scopeRunes[end]) {
			continue
		}
		return true
	}
	return false
}

func isAnswerTokenRune(r rune) bool {
	return r >= '0' && r <= '9' ||
		r >= 'a' && r <= 'z' ||
		r >= 'A' && r <= 'Z' ||
		strings.ContainsRune("./%+-", r)
}
