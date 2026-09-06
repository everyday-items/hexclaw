package usecase

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func completeBoundaryParentGuide() *ParentTeachingGuide {
	return &ParentTeachingGuide{
		Answer:                 "2",
		FullSolutionSteps:      []string{"1 + 1 = 2"},
		GradeLevelMethod:       "用一年级加法理解两个一合成二",
		LikelyMistakes:         []string{"漏算一个加数"},
		ParentTeachingSequence: []string{"先让孩子摆两个实物，再说出算式"},
		FollowUpQuestions:      []string{"如果再加 1，结果是多少？"},
		CheckingMethod:         "用两个实物逐个数一遍",
	}
}

func boundaryAssessmentReceipt(
	t *testing.T,
	item PhotoGradeItem,
	parentGuideInvocationID string,
) k12.GradingAssessmentItem {
	t.Helper()
	status, err := gradingAssessmentStatus(item.Status)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(gradingAssessmentCanonicalResult(item))
	if err != nil {
		t.Fatal(err)
	}
	receipt := k12.GradingAssessmentItem{
		AgentName: "mingming", JobID: "job-final-boundary",
		ProblemID: item.Recognized.ProblemID, AttemptID: item.Recognized.AttemptID,
		ConfirmedVersion: item.Recognized.ConfirmedVersion,
		InputDigest:      item.Recognized.InputDigest,
		Status:           status,
		ResultJSON:       string(raw),
		ResultDigest:     modelInvocationDigest(raw),
		ProjectionStatus: k12.GradingProjectionCommitted,
		CreatedAt:        1,
		UpdatedAt:        1,
	}
	switch item.Status {
	case PhotoCorrect, PhotoWrong:
		receipt.SolveInvocationID = "solve-succeeded"
		receipt.GradeInvocationID = "grade-succeeded"
	case PhotoBlankSolved:
		receipt.SolveInvocationID = "solve-succeeded"
	}
	receipt.ParentGuideInvocationID = parentGuideInvocationID
	return receipt
}

func boundaryRecognizedQuestion() RecognizedQuestion {
	return RecognizedQuestion{
		ProblemID: "problem-1", AttemptID: "attempt-1",
		ConfirmedVersion: 1, InputDigest: "sha256:input-1",
		Question: "1 + 1 =", Subject: "数学",
	}
}

func TestFinalResultBoundaryRejectsHistoricalWrongWithoutParentGuide(t *testing.T) {
	q := boundaryRecognizedQuestion()
	item := PhotoGradeItem{
		Recognized: q,
		Status:     PhotoWrong,
		ResultKind: PhotoItemAssessment,
		Grade: GradeResult{
			Solution: "2",
			Outcome:  GradeOutcome{Verdict: VerdictDisagree},
		},
	}
	receipt := boundaryAssessmentReceipt(t, item, "")
	result := PhotoGradeResult{
		Mode: PhotoModeGrade, TaskIntent: PhotoTaskCompletedHomework,
		Items: []PhotoGradeItem{item},
	}

	if err := validateGradingAssessmentExactSet(result, []k12.GradingAssessmentItem{receipt}); !errors.Is(err, ErrGradingAssessmentExactSet) {
		t.Fatalf("historical wrong receipt without guide must not cross terminal boundary: %v", err)
	}
	if _, err := replayGradingAssessmentItem(q, receipt); !errors.Is(err, ErrGradingAssessmentExactSet) {
		t.Fatalf("historical wrong receipt without guide must not be replayed as terminal: %v", err)
	}
	run := &gradingRun{
		req:       PhotoGradeRequest{TaskIntent: PhotoTaskCompletedHomework},
		questions: []RecognizedQuestion{q},
	}
	if err := validateFrozenAssessReceiptSet(run, []k12.GradingAssessmentItem{receipt}); !errors.Is(err, ErrGradingAssessmentExactSet) {
		t.Fatalf("receipt-only recovery must remain non-terminal without guide: %v", err)
	}
}

func TestFinalResultBoundaryEnforcesExactParentGuidePolicy(t *testing.T) {
	q := boundaryRecognizedQuestion()
	tests := []struct {
		name        string
		intent      PhotoTaskIntent
		mode        PhotoMode
		status      PhotoItemStatus
		guide       *ParentTeachingGuide
		guideRef    string
		wantErr     bool
		answerState AnswerState
	}{
		{
			name:   "completed wrong complete guide",
			intent: PhotoTaskCompletedHomework, mode: PhotoModeGrade, status: PhotoWrong,
			guide: completeBoundaryParentGuide(), guideRef: "parent-guide-succeeded",
			answerState: AnswerStatePresent,
		},
		{
			name:   "completed wrong incomplete guide",
			intent: PhotoTaskCompletedHomework, mode: PhotoModeGrade, status: PhotoWrong,
			guide: &ParentTeachingGuide{Answer: "2"}, guideRef: "parent-guide-succeeded",
			wantErr: true, answerState: AnswerStatePresent,
		},
		{
			name:   "completed correct never has guide",
			intent: PhotoTaskCompletedHomework, mode: PhotoModeGrade, status: PhotoCorrect,
			guide:   completeBoundaryParentGuide(),
			wantErr: true, answerState: AnswerStatePresent,
		},
		{
			name:   "completed blank solved complete guide",
			intent: PhotoTaskCompletedHomework, mode: PhotoModeGrade, status: PhotoBlankSolved,
			guide: completeBoundaryParentGuide(), guideRef: "parent-guide-succeeded",
			answerState: AnswerStateBlank,
		},
		{
			name:   "completed unclear remains guide-free",
			intent: PhotoTaskCompletedHomework, mode: PhotoModeGrade, status: PhotoAnswerUnclear,
			answerState: AnswerStateUnclear,
		},
		{
			name:   "completed blank solved rejects a missing guide",
			intent: PhotoTaskCompletedHomework, mode: PhotoModeGrade, status: PhotoBlankSolved,
			wantErr: true, answerState: AnswerStateBlank,
		},
		{
			name:   "completed unclear rejects an attached guide",
			intent: PhotoTaskCompletedHomework, mode: PhotoModeGrade, status: PhotoAnswerUnclear,
			guide:   completeBoundaryParentGuide(),
			wantErr: true, answerState: AnswerStateUnclear,
		},
		{
			name:   "blank worksheet solved complete guide",
			intent: PhotoTaskBlankWorksheet, mode: PhotoModeSolve, status: PhotoBlankSolved,
			guide: completeBoundaryParentGuide(), guideRef: "parent-guide-succeeded",
			answerState: AnswerStateBlank,
		},
		{
			name:   "blank worksheet solved missing guide",
			intent: PhotoTaskBlankWorksheet, mode: PhotoModeSolve, status: PhotoBlankSolved,
			wantErr: true, answerState: AnswerStateBlank,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			itemQ := q
			itemQ.AnswerState = tt.answerState
			item := PhotoGradeItem{
				Recognized:  itemQ,
				Status:      tt.status,
				ResultKind:  photoItemResultKind(tt.status),
				ParentGuide: tt.guide,
			}
			receipt := boundaryAssessmentReceipt(t, item, tt.guideRef)
			result := PhotoGradeResult{
				Mode: tt.mode, TaskIntent: tt.intent,
				Items: []PhotoGradeItem{item},
			}
			err := validateGradingAssessmentExactSet(result, []k12.GradingAssessmentItem{receipt})
			if tt.wantErr && !errors.Is(err, ErrGradingAssessmentExactSet) {
				t.Fatalf("expected terminal-boundary rejection, got %v", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected terminal-boundary rejection: %v", err)
			}
		})
	}
}
