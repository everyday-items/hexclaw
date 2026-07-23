package usecase

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func frozenWiringBudget() k12.GradingBudgetSnapshot {
	return k12.GradingBudgetSnapshot{
		PolicyVersion: 7,
		StageSeconds: k12.GradingStageBudgets{
			Queued: 11, Normalizing: 22, Recognizing: 33,
			Locating: 44, Rendering: 55, Projecting: 66,
		},
		AssessingBuckets: []k12.GradingAssessingBudgetBucket{
			{MaxProblems: 1, Seconds: 70},
			{MaxProblems: 8, Seconds: 71},
			{MaxProblems: 16, Seconds: 72},
			{MaxProblems: 32, Seconds: 73},
		},
		ItemConcurrency: 2,
	}
}

func TestCreateGradingJobFreezesConfiguredBudgetAndUsesItForEveryDeadline(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Now = func() int64 { return 1_000 }
	d.GradingBudgetSnapshot = frozenWiringBudget()
	ctx := context.Background()
	v, created, err := d.CreateGradingJob(ctx, "mingming", "session", CreateGradingJobInput{
		SubmissionID: "budget-submission", SourceKind: "desktop", SourceKey: "budget-job",
		ModelSnapshot: orchestratorSnapshot(), MaterializesProblemAttempts: true,
	})
	if err != nil || !created {
		t.Fatalf("create frozen job: created=%v err=%v", created, err)
	}
	if v.Fields.BudgetSnapshot.PolicyVersion != 7 || v.Fields.Deadline != 1_011 {
		t.Fatalf("creation did not freeze/use configured budget: %+v", v.Fields)
	}

	advance := func(wantStage string, wantDeadline int64) {
		t.Helper()
		v, err = d.AdvanceGradingStage(ctx, "mingming", v.Record.RecordID, AdvanceGradingInput{
			Outcome: GradingOutcomeOK, ArtifactDigest: "ok",
		})
		if err != nil || v.Record.Status != wantStage || v.Fields.Deadline != wantDeadline {
			t.Fatalf("advance stage=%s deadline=%d, want %s/%d err=%v",
				v.Record.Status, v.Fields.Deadline, wantStage, wantDeadline, err)
		}
	}
	advance(k12.GradingStageNormalizing, 1_022)
	advance(k12.GradingStageRecognizing, 1_033)
	advance(k12.GradingStageAwaitingConfirmation, 0)

	questions := make([]RecognizedQuestion, 9)
	for i := range questions {
		questions[i] = RecognizedQuestion{
			Question: fmt.Sprintf("q%d", i+1), Subject: "数学",
			AnswerState: AnswerStatePresent, StudentAnswer: "1",
		}
	}
	questions, err = NormalizeRecognizedProblems(v.Fields.SubmissionID, questions)
	if err != nil {
		t.Fatal(err)
	}
	for i := range questions {
		questions[i].ConfirmedVersion = 1
	}
	questions = FreezeRecognizedQuestionInputDigests(questions, "五年级上")
	typed, err := RecognizedQuestionsProblemAttemptSnapshot("mingming", v.Fields.SubmissionID, questions, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Records.PutProblemAttemptSnapshot(ctx, typed); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AdvanceGradingStage(ctx, "mingming", v.Record.RecordID, AdvanceGradingInput{
		Outcome: GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated, ArtifactDigest: "located",
	}); err != nil {
		t.Fatal(err)
	}
	v, err = d.ConfirmGradingJob(ctx, "mingming", v.Record.RecordID, nil)
	if err != nil || v.Record.Status != k12.GradingStageAssessing || v.Fields.Deadline != 1_072 {
		t.Fatalf("assessing bucket not derived from exact item count: stage=%s deadline=%d err=%v",
			v.Record.Status, v.Fields.Deadline, err)
	}
	advance(k12.GradingStageRendering, 1_055)
	advance(k12.GradingStageProjecting, 1_066)
}

func TestCreateGradingJobKeepsStrictLegacyDeadlineWhenBudgetIsUnfrozen(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Now = func() int64 { return 2_000 }
	v, _, err := d.CreateGradingJob(context.Background(), "mingming", "session", CreateGradingJobInput{
		SubmissionID: "legacy-budget-submission", SourceKind: "desktop", SourceKey: "legacy-budget-job",
		ModelSnapshot: orchestratorSnapshot(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Fields.BudgetSnapshot.IsFrozen() || v.Fields.Deadline != 2_000+k12.GradingStageBudgetSeconds(k12.GradingStageQueued) {
		t.Fatalf("legacy release gate drifted: %+v", v.Fields)
	}
}

func TestFrozenGenericJobFailsClosedAtCreationWithoutProblemAttemptMaterializer(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil,
	)
	d.Now = func() int64 { return 3_000 }
	d.GradingBudgetSnapshot = frozenWiringBudget()
	ctx := context.Background()
	_, _, err := d.CreateGradingJob(ctx, "mingming", "session", CreateGradingJobInput{
		SubmissionID: "unmaterialized-submission", SourceKind: "desktop", SourceKey: "generic-no-attempts",
		ModelSnapshot: orchestratorSnapshot(),
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unmaterialized frozen Job err=%v, want invalid input", err)
	}
}
