package usecase

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const (
	TutoringTipsSourceTextbook         = "📖 依据课本"
	TutoringTipsSourceAI               = "🤖 AI 归纳·供参考"
	TutoringTipsSourceLearningEvidence = "🧠 学情信号"
)

// TutoringTipsSection is one of the three fixed, ordered sections rendered in
// the confirmed homework flow. It is never a standalone navigation object.
type TutoringTipsSection struct {
	Title       string `json:"title"`
	Content     string `json:"content"`
	SourceLabel string `json:"source_label"`
}

// TutoringTipsProblem is the trusted internal exact-set used to prove that the
// third section covers every answerable durable Problem once. HTTP responses do
// not expose it because the current contract returns only knowledge_points and
// the three sections.
type TutoringTipsProblem struct {
	ProblemID    string
	StemMarkdown string
	Subject      string
	ConceptIDs   []string
}

// TutoringTips is an ephemeral, read-only projection of one confirmed
// GradingJob. No additional table or mutable current-selection state exists.
type TutoringTips struct {
	GradingJobID    string
	SubmissionID    string
	Grade           string
	Subject         string
	KnowledgePoints []string              `json:"knowledge_points"`
	Problems        []TutoringTipsProblem `json:"-"`
	Sections        []TutoringTipsSection `json:"sections"`
}

var tutoringTipsBuildBudget = 90 * time.Second

// BuildTutoringTips resolves every content fact from an owner-scoped confirmed
// GradingJob. The client cannot supply grade, subject, concepts, or problems.
func (d Deps) BuildTutoringTips(ctx context.Context, agentName, gradingJobID string) (TutoringTips, error) {
	return d.BuildTutoringTipsSubject(ctx, agentName, gradingJobID)
}

// BuildTutoringTipsSubject is the subject-aware canonical builder. Subject is
// derived from the durable Problem exact-set rather than accepted as an input.
func (d Deps) BuildTutoringTipsSubject(ctx context.Context, agentName, gradingJobID string) (TutoringTips, error) {
	agentName = strings.TrimSpace(agentName)
	gradingJobID = strings.TrimSpace(gradingJobID)
	if agentName == "" || gradingJobID == "" {
		return TutoringTips{}, fmt.Errorf("%w: agent / grading_job_id required", ErrInvalidInput)
	}
	if d.Records == nil {
		return TutoringTips{}, fmt.Errorf("usecase: canonical K12 store unavailable")
	}

	job, err := d.GetGradingJob(ctx, agentName, gradingJobID)
	if err != nil {
		return TutoringTips{}, err
	}
	if job.Fields.ConfirmationState != k12.GradingConfirmationConfirmed {
		return TutoringTips{}, fmt.Errorf("%w: GradingJob confirmation_state must be confirmed", records.ErrIllegalTransition)
	}
	if !tutoringTipsStageAllowed(job.Record.Status) {
		return TutoringTips{}, fmt.Errorf("%w: GradingJob stage %s cannot generate tutoring tips", records.ErrIllegalTransition, job.Record.Status)
	}

	profile, err := d.GetProfile(ctx, agentName)
	if err != nil {
		return TutoringTips{}, fmt.Errorf("usecase: derive tutoring tips profile: %w", err)
	}
	grade := strings.TrimSpace(profile.GradeTerm)
	if err := validateGradeInput(grade); err != nil {
		return TutoringTips{}, fmt.Errorf("%w: durable profile grade required", ErrInvalidInput)
	}
	childName := strings.TrimSpace(profile.ChildName)
	if childName == "" {
		return TutoringTips{}, fmt.Errorf("%w: durable profile child_name required", ErrInvalidInput)
	}

	snapshot, err := d.Records.GetProblemAttemptSnapshot(ctx, agentName, job.Fields.SubmissionID)
	if err != nil {
		return TutoringTips{}, fmt.Errorf("usecase: derive tutoring tips facts: %w", err)
	}
	questions, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		return TutoringTips{}, err
	}
	problems, subject, knowledgePoints, err := validateTutoringTipsFacts(questions, grade)
	if err != nil {
		return TutoringTips{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, tutoringTipsBuildBudget)
	defer cancel()
	history, err := d.mistakesFor(ctx, agentName, knowledgePoints)
	if err != nil {
		return TutoringTips{}, err
	}
	tips := TutoringTips{
		GradingJobID: gradingJobID, SubmissionID: job.Fields.SubmissionID,
		Grade: grade, Subject: subject, KnowledgePoints: knowledgePoints, Problems: problems,
	}
	tips.Sections = []TutoringTipsSection{
		d.tutoringTipsOverview(ctx, agentName, grade, subject, knowledgePoints),
		tutoringTipsLearningEvidence(childName, history),
		tutoringTipsPerProblem(problems),
	}
	return tips, nil
}

func tutoringTipsStageAllowed(stage string) bool {
	switch stage {
	case k12.GradingStageAssessing, k12.GradingStageRendering,
		k12.GradingStageProjecting, k12.GradingStageCompleted:
		return true
	default:
		return false
	}
}

func validateTutoringTipsFacts(questions []RecognizedQuestion, grade string) ([]TutoringTipsProblem, string, []string, error) {
	if len(questions) == 0 {
		return nil, "", nil, fmt.Errorf("%w: durable Problem exact-set is empty", ErrInvalidInput)
	}
	parents := make(map[string]string)
	for _, question := range questions {
		if question.ProblemKind == ProblemKindCompoundParent {
			parents[question.ProblemID] = strings.TrimSpace(question.CanonicalMarkdown)
		}
	}
	recomputed := FreezeRecognizedQuestionInputDigests(questions, grade)
	byID := make(map[string]RecognizedQuestion, len(recomputed))
	for _, question := range recomputed {
		byID[question.ProblemID] = question
	}

	seenProblem := make(map[string]struct{})
	seenConcept := make(map[string]struct{})
	knowledgePoints := make([]string, 0)
	problems := make([]TutoringTipsProblem, 0)
	subject := ""
	for _, question := range questions {
		if question.ProblemKind == ProblemKindCompoundParent {
			continue
		}
		problemID := strings.TrimSpace(question.ProblemID)
		if problemID == "" {
			return nil, "", nil, fmt.Errorf("%w: answerable Problem missing problem_id", ErrInvalidInput)
		}
		if _, duplicate := seenProblem[problemID]; duplicate {
			return nil, "", nil, fmt.Errorf("%w: duplicate answerable problem_id %s", ErrInvalidInput, problemID)
		}
		seenProblem[problemID] = struct{}{}
		if strings.TrimSpace(question.AttemptID) == "" || question.ConfirmedVersion < 1 || strings.TrimSpace(question.InputDigest) == "" {
			return nil, "", nil, fmt.Errorf("%w: Problem %s has no confirmed Attempt", ErrInvalidInput, problemID)
		}
		if expected := byID[problemID].InputDigest; expected == "" || question.InputDigest != expected {
			return nil, "", nil, fmt.Errorf("%w: Problem %s confirmed input digest mismatch", ErrInvalidInput, problemID)
		}
		problemSubject, err := normalizeSubject(question.Subject)
		if err != nil || problemSubject == "" {
			return nil, "", nil, fmt.Errorf("%w: Problem %s has no valid durable subject", ErrInvalidInput, problemID)
		}
		if subject == "" {
			subject = problemSubject
		} else if subject != problemSubject {
			return nil, "", nil, fmt.Errorf("%w: Problem exact-set spans multiple subjects", ErrInvalidInput)
		}
		stem := strings.TrimSpace(question.CanonicalMarkdown)
		if question.ProblemKind == ProblemKindSubproblem {
			parentStem := strings.TrimSpace(parents[question.ParentProblemID])
			if parentStem == "" {
				return nil, "", nil, fmt.Errorf("%w: subproblem %s missing canonical parent", ErrInvalidInput, problemID)
			}
			stem = parentStem + "\n\n" + stem
		}
		concepts := make([]string, 0, len(question.KnowledgePoints))
		for _, raw := range question.KnowledgePoints {
			concept := strings.TrimSpace(raw)
			if concept == "" {
				continue
			}
			concepts = append(concepts, concept)
			if _, exists := seenConcept[concept]; !exists {
				seenConcept[concept] = struct{}{}
				knowledgePoints = append(knowledgePoints, concept)
			}
		}
		problems = append(problems, TutoringTipsProblem{
			ProblemID: problemID, StemMarkdown: stem, Subject: subject, ConceptIDs: concepts,
		})
	}
	if len(problems) == 0 || len(knowledgePoints) == 0 {
		return nil, "", nil, fmt.Errorf("%w: durable Problem exact-set has no answerable concept facts", ErrInvalidInput)
	}
	return problems, subject, knowledgePoints, nil
}

func (d Deps) tutoringTipsOverview(ctx context.Context, agentName, grade, subject string, concepts []string) TutoringTipsSection {
	var content strings.Builder
	groundedCount := 0
	for _, concept := range concepts {
		if d.Grounding != nil {
			if evidence, found, err := d.groundForSubject(ctx, agentName, subject, concept, grade); err == nil && found {
				teaching := groundedTutoringTipsMarkdown(ctx, d.TutoringTipsReview, subject, concept, grade, evidence)
				fmt.Fprintf(&content, "### %s\n\n%s\n\n", concept, teaching)
				groundedCount++
				continue
			}
		}
		if d.TutoringTipsReview != nil {
			if text, err := d.TutoringTipsReview.GenerateTutoringTipsReview(ctx, subject, concept, grade); err == nil && strings.TrimSpace(text) != "" {
				fmt.Fprintf(&content, "### %s\n\n%s\n\n", concept, strings.TrimSpace(text))
				continue
			}
		}
		fmt.Fprintf(&content, "### %s\n\n本次未生成可靠讲解，请结合当前教材核对。\n\n", concept)
	}
	label := TutoringTipsSourceTextbook
	if groundedCount != len(concepts) {
		label = TutoringTipsSourceAI
	}
	return TutoringTipsSection{Title: "这页在练什么", Content: strings.TrimSpace(content.String()), SourceLabel: label}
}

func groundedTutoringTipsMarkdown(ctx context.Context, generator TutoringTipsReviewGenerator,
	subject, concept, grade, evidence string,
) string {
	evidence = strings.TrimSpace(evidence)
	if grounded, ok := generator.(GroundedTutoringTipsReviewGenerator); ok {
		if text, err := grounded.GenerateGroundedTutoringTipsReview(ctx, subject, concept, grade, evidence); err == nil && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return evidence
}

func (d Deps) groundForSubject(ctx context.Context, agentName, subject, concept, grade string) (string, bool, error) {
	if scoped, ok := d.Grounding.(SubjectGrounding); ok {
		return scoped.GroundSubject(ctx, agentName, subject, concept, grade)
	}
	return d.Grounding.Ground(ctx, agentName, concept, grade)
}

func tutoringTipsLearningEvidence(childName string, history []ReviewItem) TutoringTipsSection {
	if len(history) == 0 {
		return TutoringTipsSection{
			Title: childName + "要留意", Content: "暂无历史证据。先观察孩子如何读题、选择方法和检查结果。",
			SourceLabel: TutoringTipsSourceLearningEvidence,
		}
	}
	sort.SliceStable(history, func(i, j int) bool {
		return history[i].Record.RecordID < history[j].Record.RecordID
	})
	var content strings.Builder
	for _, item := range history {
		concept := strings.TrimSpace(item.Fields.KnowledgePoint)
		cause := strings.TrimSpace(item.Fields.ErrorCause)
		if cause == "" {
			cause = "错因尚未归纳"
		}
		fmt.Fprintf(&content, "- %s：%s\n", concept, cause)
	}
	return TutoringTipsSection{
		Title: childName + "要留意", Content: strings.TrimSpace(content.String()),
		SourceLabel: TutoringTipsSourceLearningEvidence,
	}
}

func tutoringTipsPerProblem(problems []TutoringTipsProblem) TutoringTipsSection {
	var content strings.Builder
	for index, problem := range problems {
		fmt.Fprintf(&content, "### 第 %d 题 · %s\n\n", index+1, problem.ProblemID)
		fmt.Fprintf(&content, "%s\n\n", problem.StemMarkdown)
		content.WriteString("先请孩子用自己的话说清题目在问什么；再问他准备使用哪个已经学过的知识点、为什么；完成后请他自己检查步骤、符号和单位。\n\n")
	}
	return TutoringTipsSection{
		Title: "每道题怎么带（不直接给答案）", Content: strings.TrimSpace(content.String()),
		SourceLabel: TutoringTipsSourceAI,
	}
}

// mistakesFor aggregates only durable records matching the current exact-set's
// concepts. A missing history is a valid, explicitly rendered fact.
func (d Deps) mistakesFor(ctx context.Context, agentName string, concepts []string) ([]ReviewItem, error) {
	all, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, "")
	if err != nil {
		if errors.Is(err, records.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("usecase: aggregate learning evidence: %w", err)
	}
	wanted := make(map[string]struct{}, len(concepts))
	for _, concept := range concepts {
		wanted[concept] = struct{}{}
	}
	out := make([]ReviewItem, 0)
	for _, record := range all {
		fields, parseErr := k12.ParseMistakeFields(record.Fields)
		if parseErr != nil {
			return nil, fmt.Errorf("usecase: parse learning evidence: %w", parseErr)
		}
		if _, matched := wanted[fields.KnowledgePoint]; matched {
			out = append(out, ReviewItem{Record: record, Fields: fields})
		}
	}
	return out, nil
}
