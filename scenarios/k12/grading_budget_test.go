package k12

import "testing"

func frozenBudgetFixture() GradingBudgetSnapshot {
	return GradingBudgetSnapshot{
		PolicyVersion: 1,
		StageSeconds: GradingStageBudgets{
			Queued: 60, Normalizing: 60, Recognizing: 120,
			Locating: 60, Rendering: 60, Projecting: 60,
		},
		AssessingBuckets: []GradingAssessingBudgetBucket{
			{MaxProblems: 1, Seconds: 90},
			{MaxProblems: 8, Seconds: 180},
			{MaxProblems: 16, Seconds: 300},
			{MaxProblems: 32, Seconds: 540},
		},
		ItemConcurrency: 2,
	}
}

func TestGradingBudgetSnapshotZeroIsStrictlyLegacyAndUnfrozen(t *testing.T) {
	var snapshot GradingBudgetSnapshot
	if snapshot.IsFrozen() {
		t.Fatal("policy_version=0 must remain legacy/unfrozen")
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("zero legacy snapshot must remain readable: %v", err)
	}
	if _, ok := snapshot.StageBudgetSeconds(GradingStageAssessing, 1); ok {
		t.Fatal("unfrozen snapshot must not manufacture an assessing budget")
	}
}

func TestGradingBudgetSnapshotRequiresMeasuredOneEightSixteenThirtyTwoBuckets(t *testing.T) {
	valid := frozenBudgetFixture()
	if !valid.IsFrozen() {
		t.Fatal("positive policy version should be frozen")
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid frozen snapshot: %v", err)
	}

	missing32 := frozenBudgetFixture()
	missing32.AssessingBuckets = missing32.AssessingBuckets[:3]
	if err := missing32.Validate(); err == nil {
		t.Fatal("frozen policy missing the real 32-problem bucket must fail closed")
	}
	badConcurrency := frozenBudgetFixture()
	badConcurrency.ItemConcurrency = 0
	if err := badConcurrency.Validate(); err == nil {
		t.Fatal("frozen policy with zero item_concurrency must be rejected")
	}
	badStage := frozenBudgetFixture()
	badStage.StageSeconds.Recognizing = 0
	if err := badStage.Validate(); err == nil {
		t.Fatal("frozen policy with a zero automatic-stage budget must be rejected")
	}
}

func TestGradingBudgetSnapshotSelectsFrozenStageAndQuestionBucket(t *testing.T) {
	snapshot := frozenBudgetFixture()
	for _, tc := range []struct {
		stage    string
		problems int
		want     int64
	}{
		{GradingStageQueued, 0, 60},
		{GradingStageRecognizing, 0, 120},
		{GradingStageAssessing, 1, 90},
		{GradingStageAssessing, 2, 180},
		{GradingStageAssessing, 8, 180},
		{GradingStageAssessing, 9, 300},
		{GradingStageAssessing, 17, 540},
	} {
		got, ok := snapshot.StageBudgetSeconds(tc.stage, tc.problems)
		if !ok || got != tc.want {
			t.Errorf("stage=%s problems=%d got=(%d,%v), want=(%d,true)",
				tc.stage, tc.problems, got, ok, tc.want)
		}
	}
	if _, ok := snapshot.StageBudgetSeconds(GradingStageAssessing, 33); ok {
		t.Fatal("unmeasured >32 problem count must fail closed")
	}
	if _, ok := snapshot.StageBudgetSeconds(GradingStageAwaitingConfirmation, 0); ok {
		t.Fatal("human wait state must have no automatic budget")
	}
}
