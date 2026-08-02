package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// K12-ASSESSING-DEADLINE-001: the 120-second DD-036 recognizing limit must
// never become a second, shorter deadline inside a frozen assessing stage.
func TestBUG20260802FrozenAssessingDeadlineOutranksRecognizingCallCap(t *testing.T) {
	stageDeadline := time.Now().Add(10 * time.Minute).Round(0)
	stageCtx, cancelStage := context.WithDeadline(context.Background(), stageDeadline)
	defer cancelStage()

	callCtx, cancelCall := gradingIndependentCallContext(stageCtx, 120_000)
	defer cancelCall()

	gotDeadline, ok := callCtx.Deadline()
	if !ok || !gotDeadline.Equal(stageDeadline) {
		t.Fatalf("assessing call deadline=%v ok=%v, want frozen stage deadline=%v; recognizing 120s must not leak", gotDeadline, ok, stageDeadline)
	}
}

func TestBUG20260802NoStageDeadlineKeepsExplicitModelCallCap(t *testing.T) {
	before := time.Now()
	callCtx, cancelCall := gradingIndependentCallContext(context.Background(), 120_000)
	defer cancelCall()

	gotDeadline, ok := callCtx.Deadline()
	if !ok {
		t.Fatal("explicit model call cap must remain bounded without a stage deadline")
	}
	if remaining := time.Until(gotDeadline); remaining < 119*time.Second || remaining > 121*time.Second {
		t.Fatalf("explicit model call cap remaining=%s, want approximately 120s (started %v)", remaining, before)
	}
}

type assessingDeadlineRecorder struct {
	mu        sync.Mutex
	deadlines map[k12.GradingItemOperation]time.Time
}

func (r *assessingDeadlineRecorder) capture(
	operation k12.GradingItemOperation,
	ctx context.Context,
) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		return fmt.Errorf("%s call lost its deadline", operation)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deadlines[operation] = deadline
	return nil
}

func (r *assessingDeadlineRecorder) snapshot() map[k12.GradingItemOperation]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[k12.GradingItemOperation]time.Time, len(r.deadlines))
	for operation, deadline := range r.deadlines {
		result[operation] = deadline
	}
	return result
}

type assessingDeadlineSolver struct{ recorder *assessingDeadlineRecorder }

func (*assessingDeadlineSolver) UsesGradingPhysicalCalls() bool { return true }

func (s *assessingDeadlineSolver) Solve(
	ctx context.Context, _, _, _ string,
) (SolveResult, error) {
	want := SolveResult{
		Solution: "2",
		Evidence: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return SolveResult{}, err
	}
	result, err := ExecuteGradingPhysicalCall(ctx, GradingPhysicalCallSpec{
		Operation:     k12.GradingItemOperationSolveVerify,
		RequestDigest: "deadline-solve-verify",
	}, func(callCtx context.Context) (string, error) {
		if captureErr := s.recorder.capture(k12.GradingItemOperationSolveVerify, callCtx); captureErr != nil {
			return "", captureErr
		}
		return string(raw), nil
	})
	if err != nil {
		return SolveResult{}, err
	}
	var got SolveResult
	if err := json.Unmarshal([]byte(result.Payload), &got); err != nil {
		return SolveResult{}, err
	}
	return got, nil
}

type assessingDeadlineGrader struct{ recorder *assessingDeadlineRecorder }

func (*assessingDeadlineGrader) UsesGradingPhysicalCalls() bool { return true }

func (g *assessingDeadlineGrader) Grade(
	ctx context.Context, _, _, _ string,
) (GradeOutcome, error) {
	want := GradeOutcome{Verdict: VerdictDisagree, WrongStep: "第 1 步", ErrorCause: "计算错误"}
	raw, err := json.Marshal(want)
	if err != nil {
		return GradeOutcome{}, err
	}
	result, err := ExecuteGradingPhysicalCall(ctx, GradingPhysicalCallSpec{
		Operation:     k12.GradingItemOperationGrade,
		RequestDigest: "deadline-grade",
	}, func(callCtx context.Context) (string, error) {
		if captureErr := g.recorder.capture(k12.GradingItemOperationGrade, callCtx); captureErr != nil {
			return "", captureErr
		}
		return string(raw), nil
	})
	if err != nil {
		return GradeOutcome{}, err
	}
	var got GradeOutcome
	if err := json.Unmarshal([]byte(result.Payload), &got); err != nil {
		return GradeOutcome{}, err
	}
	return got, nil
}

type assessingDeadlineGuide struct{ recorder *assessingDeadlineRecorder }

func (g *assessingDeadlineGuide) GenerateParentTeachingGuide(
	ctx context.Context,
	_ ParentTeachingGuideRequest,
) (ParentTeachingGuide, error) {
	if err := g.recorder.capture(k12.GradingItemOperationParentGuide, ctx); err != nil {
		return ParentTeachingGuide{}, err
	}
	return ParentTeachingGuide{
		Answer:                 "2",
		FullSolutionSteps:      []string{"计算得到 2"},
		GradeLevelMethod:       "按题意计算",
		LikelyMistakes:         []string{"计算顺序错误"},
		ParentTeachingSequence: []string{"先读题"},
		FollowUpQuestions:      []string{"为什么这样算？"},
		CheckingMethod:         "代回检查",
	}, nil
}

func TestBUG20260802FrozenAssessingNestedOperationsUseDurableStageDeadline(t *testing.T) {
	recorder := &assessingDeadlineRecorder{deadlines: map[k12.GradingItemOperation]time.Time{}}
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent,
	}}}, nil, WithGradingRunDir(t.TempDir()))
	o.deps.Solver = &assessingDeadlineSolver{recorder: recorder}
	o.deps.Grader = &assessingDeadlineGrader{recorder: recorder}
	o.deps.VerifiedGrader = nil
	o.deps.ParentTeachingGuide = &assessingDeadlineGuide{recorder: recorder}

	jobID := runItemResumeJobToAssessing(t, o, "assessing-stage-deadline")
	beforeConfirm, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfirm.Fields.BudgetSnapshot.AssessingBuckets[0].Seconds = 600
	raw, err := json.Marshal(beforeConfirm.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.deps.Records.UpdateStatusFields(context.Background(), jobID, beforeConfirm.Record.Status,
		beforeConfirm.Record.DueAt, string(raw), beforeConfirm.Record.Version); err != nil {
		t.Fatal(err)
	}

	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	wantDeadline := time.Unix(job.Fields.Deadline, 0)
	if remaining := time.Until(wantDeadline); remaining < 599*time.Second || remaining > 601*time.Second {
		t.Fatalf("frozen assessing deadline remaining=%s, want approximately 600s", remaining)
	}
	view, err := o.runAssessItems(context.Background(), run, job)
	if err != nil || view.Record.Status != k12.GradingStageRendering {
		t.Fatalf("run frozen assessment: stage=%s err=%v", view.Record.Status, err)
	}

	got := recorder.snapshot()
	for _, operation := range []k12.GradingItemOperation{
		k12.GradingItemOperationSolveVerify,
		k12.GradingItemOperationGrade,
		k12.GradingItemOperationParentGuide,
	} {
		if deadline, ok := got[operation]; !ok || !deadline.Equal(wantDeadline) {
			t.Fatalf("%s deadline=%v present=%v, want frozen assessing deadline=%v", operation, deadline, ok, wantDeadline)
		}
	}
}
