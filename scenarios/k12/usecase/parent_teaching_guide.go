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
	guide, err := d.generateParentTeachingGuide(ctx, guideRequest)
	if err != nil {
		return result, err
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
