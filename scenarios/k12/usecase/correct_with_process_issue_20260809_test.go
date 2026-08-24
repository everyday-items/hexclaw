package usecase

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const processIssueStatusForTest PhotoItemStatus = "correct_with_process_issue"

func TestREGK12CorrectWithProcessIssue20260809001DurableProjectionHasOneStatus(t *testing.T) {
	t.Run("grade outcome preserves explicit final-answer fact without changing legacy JSON", func(t *testing.T) {
		legacy := []byte(`{"Verdict":"disagree","WrongStep":"错步","ErrorCause":"错因","KnowledgePoint":"计算"}`)
		var legacyOutcome GradeOutcome
		if err := json.Unmarshal(legacy, &legacyOutcome); err != nil {
			t.Fatal(err)
		}
		roundTrip, err := json.Marshal(legacyOutcome)
		if err != nil {
			t.Fatal(err)
		}
		if string(roundTrip) != string(legacy) {
			t.Fatalf("legacy result bytes drifted: got=%s want=%s", roundTrip, legacy)
		}

		current := []byte(`{"Verdict":"disagree","WrongStep":"300÷2÷2=50","ErrorCause":"连续除法计算错误","KnowledgePoint":"应用题","FinalAnswerCorrect":true}`)
		var outcome GradeOutcome
		if unmarshalErr := json.Unmarshal(current, &outcome); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		encoded, err := json.Marshal(outcome)
		if err != nil {
			t.Fatal(err)
		}
		var projection map[string]any
		if unmarshalErr := json.Unmarshal(encoded, &projection); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		if projection["FinalAnswerCorrect"] != true {
			t.Fatalf("explicit final-answer fact was dropped: %s", encoded)
		}

		effects, err := (Deps{}).gradingAssessmentEffects(context.Background(), GradeRequest{
			AgentName: "agent", Subject: "数学", Problem: "应用题", StudentAnswer: "答11250",
		}, GradeResult{Outcome: outcome, Solution: "答案：11250"})
		if err != nil {
			t.Fatal(err)
		}
		if effects.Mistake != nil || effects.Review != nil {
			t.Fatalf("process issue must have zero mistake/review effects: %#v", effects)
		}
	})

	t.Run("durable status requires solve grade and parent guide", func(t *testing.T) {
		status, err := gradingAssessmentStatus(processIssueStatusForTest)
		if err != nil || string(status) != string(processIssueStatusForTest) {
			t.Fatalf("photo-to-durable status mapping lost process issue: status=%q err=%v", status, err)
		}
		receipt := k12.GradingAssessmentItem{
			AgentName: "agent", JobID: "job", ProblemID: "problem", AttemptID: "attempt",
			ConfirmedVersion: 1, InputRevision: 1, PublishedRevision: 1,
			CurrentDisposition: k12.GradingAssessmentDispositionCurrent,
			StructureVersion:   1, InputDigest: "input", Status: status,
			ResultJSON: `{}`, ResultDigest: "sha256:result",
			SolveInvocationID: "solve", GradeInvocationID: "grade",
			ParentGuideInvocationID: "guide", ProjectionStatus: k12.GradingProjectionCommitted,
		}
		if err := receipt.ValidateTerminalParentGuideReference(); err != nil {
			t.Fatalf("canonical process issue receipt was rejected: %v", err)
		}
	})

	t.Run("weekly practice does not turn a process issue into wrong or mastered", func(t *testing.T) {
		finalCorrect := true
		assessor := NewVerifiedSolutionWeeklyAssessor(processIssueVerifiedGrader{outcome: GradeOutcome{
			Verdict: VerdictDisagree, FinalAnswerCorrect: &finalCorrect,
			WrongStep: "300÷2÷2=50", ErrorCause: "连续除法计算错误",
		}})
		assessment, err := assessor.AssessWeeklyPracticeAnswer(context.Background(), WeeklyPracticeAnswerRequest{
			SnapshotID: "snapshot", StudentAnswer: "11250", VerifiedSolution: "11250",
			Item: k12.WeeklyPracticeItem{ItemID: "item", PromptMarkdown: "鱼塘产鱼"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if assessment.Result != k12.WeeklyAttemptNeedsReview {
			t.Fatalf("process issue must not be projected as wrong/mastered: %s", assessment.Result)
		}
	})

	t.Run("markdown annotation and final artifact retain the warning classification", func(t *testing.T) {
		box := &BBox{X: 0.1, Y: 0.2, W: 0.3, H: 0.1}
		guide := &ParentTeachingGuide{
			Answer: "11250", FullSolutionSteps: []string{"核对每个等式"},
			GradeLevelMethod: "逐步计算", LikelyMistakes: []string{"连续除法误算"},
			ParentTeachingSequence: []string{"先让孩子重算错步"},
			FollowUpQuestions:      []string{"这一步等于多少？"}, CheckingMethod: "逐式代回",
		}
		item := PhotoGradeItem{
			Recognized: RecognizedQuestion{ProblemID: "q15", Question: "鱼塘产鱼", StudentAnswer: "答11250", BBox: box},
			Status:     processIssueStatusForTest,
			Grade: GradeResult{Outcome: GradeOutcome{
				Verdict: VerdictDisagree, WrongStep: "300÷2÷2=50", ErrorCause: "连续除法计算错误",
			}},
			ParentGuide: guide,
		}

		markdown := photoGradeMarkdown(PhotoGradeResult{Mode: PhotoModeGrade, Items: []PhotoGradeItem{item}})
		for _, want := range []string{"Process issues (1)", "300÷2÷2=50", "not recorded as wrong", "How the parent can explain it"} {
			if !strings.Contains(markdown, want) {
				t.Fatalf("photo markdown lacks %q:\n%s", want, markdown)
			}
		}
		if strings.Contains(markdown, "需要订正（1）") {
			t.Fatalf("process issue was counted as wrong:\n%s", markdown)
		}

		marks := photoAnnotations([]PhotoGradeItem{item})
		if len(marks) != 1 {
			t.Fatalf("process issue needs one trusted warning mark, got %#v", marks)
		}
		statusField := reflect.ValueOf(marks[0]).FieldByName("Status")
		if !statusField.IsValid() || statusField.String() != string(processIssueStatusForTest) {
			t.Fatalf("annotation lost typed status: %#v", marks[0])
		}

		canonicalResult, err := json.Marshal(gradingAssessmentCanonicalResult(item))
		if err != nil {
			t.Fatal(err)
		}
		final := renderCanonicalGradingFinal([]gradingFinalEntry{{
			question: item.Recognized,
			assessment: &k12.GradingAssessmentItem{
				Status:     k12.GradingAssessmentStatus(processIssueStatusForTest),
				ResultJSON: string(canonicalResult),
			},
		}}, nil)
		for _, want := range []string{
			"⚠ 过程问题（最终答案正确，不记为错题）", "**错误步骤：** 300÷2÷2=50",
			"**原因：** 连续除法计算错误", "### 家长怎么讲",
		} {
			if !strings.Contains(final, want) {
				t.Fatalf("final artifact/DingTalk source lacks %q:\n%s", want, final)
			}
		}
		for _, forbidden := range []string{"correct_with_process_issue", "```json", "Grading status"} {
			if strings.Contains(final, forbidden) {
				t.Fatalf("final artifact/DingTalk source leaked %q:\n%s", forbidden, final)
			}
		}
	})

	t.Run("final artifact summarizes fourteen correct and two process issues", func(t *testing.T) {
		entries := make([]gradingFinalEntry, 0, 16)
		for ordinal := 1; ordinal <= 16; ordinal++ {
			status := k12.GradingAssessmentCorrect
			if ordinal >= 15 {
				status = k12.GradingAssessmentProcessIssue
			}
			entries = append(entries, gradingFinalEntry{
				question: RecognizedQuestion{ProblemID: "q" + string(rune('A'+ordinal-1))},
				assessment: &k12.GradingAssessmentItem{
					Status: status, ResultJSON: `{}`,
				},
			})
		}
		final := renderCanonicalGradingFinal(entries, nil)
		for _, want := range []string{
			"14 道正确 / 2 道过程问题",
			"过程问题表示最终答案正确，但书写过程需要核对，不记为错题",
		} {
			if !strings.Contains(final, want) {
				t.Fatalf("final artifact/DingTalk summary lacks %q:\n%s", want, final)
			}
		}
		if strings.Contains(final, "2 道错题") {
			t.Fatalf("final artifact/DingTalk summary counted process issues as wrong:\n%s", final)
		}
	})
}

type processIssueVerifiedGrader struct {
	outcome GradeOutcome
}

func (g processIssueVerifiedGrader) GradeVerified(
	context.Context,
	string,
	string,
	string,
	string,
) (GradeOutcome, error) {
	return g.outcome, nil
}
