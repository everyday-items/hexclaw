package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type grading20260726PhysicalSolver struct {
	delay     time.Duration
	mu        sync.Mutex
	calls     map[k12.GradingItemOperation]int
	failFirst map[k12.GradingItemOperation]error
}

func (*grading20260726PhysicalSolver) UsesGradingPhysicalCalls() bool { return true }

func (s *grading20260726PhysicalSolver) Solve(
	ctx context.Context,
	problem, _, _ string,
) (SolveResult, error) {
	generated, err := grading20260726PhysicalCall(
		ctx, k12.GradingItemOperationSolveGenerate, problem, s.delay,
		func() (SolveResult, error) {
			if err := s.firstFailure(k12.GradingItemOperationSolveGenerate); err != nil {
				return SolveResult{}, err
			}
			return SolveResult{
				Solution: "2",
				Evidence: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec},
			}, nil
		}, s,
	)
	if err != nil {
		return SolveResult{}, err
	}
	return grading20260726PhysicalCall(
		ctx, k12.GradingItemOperationSolveVerify, problem+"\x00"+generated.Solution, s.delay,
		func() (SolveResult, error) {
			if err := s.firstFailure(k12.GradingItemOperationSolveVerify); err != nil {
				return SolveResult{}, err
			}
			return generated, nil
		}, s,
	)
}

func (s *grading20260726PhysicalSolver) record(operation k12.GradingItemOperation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[operation]++
}

func (s *grading20260726PhysicalSolver) count(operation k12.GradingItemOperation) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[operation]
}

func (s *grading20260726PhysicalSolver) firstFailure(operation k12.GradingItemOperation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls[operation] == 1 {
		return s.failFirst[operation]
	}
	return nil
}

type grading20260726PhysicalGrader struct {
	delay     time.Duration
	mu        sync.Mutex
	calls     int
	failFirst error
}

func (*grading20260726PhysicalGrader) UsesGradingPhysicalCalls() bool { return true }

func (g *grading20260726PhysicalGrader) Grade(
	ctx context.Context,
	problem, studentAnswer, solution string,
) (GradeOutcome, error) {
	return grading20260726PhysicalCall(
		ctx, k12.GradingItemOperationGrade,
		problem+"\x00"+studentAnswer+"\x00"+solution, g.delay,
		func() (GradeOutcome, error) {
			if g.count() == 1 && g.failFirst != nil {
				return GradeOutcome{}, g.failFirst
			}
			return GradeOutcome{
				Verdict: VerdictDisagree, WrongStep: "第 1 步", ErrorCause: "计算错误",
			}, nil
		},
		g,
	)
}

type grading20260726RestartGuide struct {
	mu        sync.Mutex
	calls     int
	failFirst error
}

func (g *grading20260726RestartGuide) GenerateParentTeachingGuide(
	_ context.Context,
	req ParentTeachingGuideRequest,
) (ParentTeachingGuide, error) {
	g.mu.Lock()
	g.calls++
	call := g.calls
	g.mu.Unlock()
	if call == 1 && g.failFirst != nil {
		return ParentTeachingGuide{}, g.failFirst
	}
	return ParentTeachingGuide{
		Answer:                 "2",
		FullSolutionSteps:      []string{"untrusted"},
		GradeLevelMethod:       "本年级方法",
		LikelyMistakes:         []string{"计算错误"},
		ParentTeachingSequence: []string{"先读题", "再计算"},
		FollowUpQuestions:      []string{"为什么？"},
		CheckingMethod:         "代回检查",
	}, nil
}

func (g *grading20260726RestartGuide) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func (g *grading20260726PhysicalGrader) record(k12.GradingItemOperation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
}

func (g *grading20260726PhysicalGrader) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

type grading20260726Recorder interface {
	record(k12.GradingItemOperation)
}

func grading20260726PhysicalCall[T any](
	ctx context.Context,
	operation k12.GradingItemOperation,
	request string,
	delay time.Duration,
	build func() (T, error),
	recorder grading20260726Recorder,
) (T, error) {
	var zero T
	digestRaw := sha256.Sum256([]byte(string(operation) + "\x00" + request))
	result, err := ExecuteGradingPhysicalCall(ctx, GradingPhysicalCallSpec{
		Operation:     operation,
		RequestDigest: hex.EncodeToString(digestRaw[:]),
	}, func(callCtx context.Context) (string, error) {
		recorder.record(operation)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-callCtx.Done():
			return "", callCtx.Err()
		}
		value, buildErr := build()
		if buildErr != nil {
			return "", buildErr
		}
		raw, marshalErr := json.Marshal(value)
		return string(raw), marshalErr
	})
	if err != nil {
		return zero, err
	}
	if err := json.Unmarshal([]byte(result.Payload), &zero); err != nil {
		return zero, fmt.Errorf("decode physical result: %w", err)
	}
	return zero, nil
}

func TestGrading20260726_PublicImageTaskRejectsZeroPolicyBeforeAnyModelCall(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent,
	}}, solver, grader)

	_, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo:          orchestratorPhotoRequest(),
		SourceKind:     "image_task",
		SourceKey:      "public-image-task-zero-policy",
		BudgetSnapshot: k12.GradingBudgetSnapshot{},
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero-policy public image task err=%v, want ErrInvalidInput", err)
	}
	if created {
		t.Fatal("zero-policy public image task must not create a grading job")
	}
	if got := solver.callCount("1+1="); got != 0 {
		t.Fatalf("solver calls=%d, want zero", got)
	}
	if got := grader.callCount("1+1="); got != 0 {
		t.Fatalf("grader calls=%d, want zero", got)
	}
}

func TestGrading20260726_PhysicalOperationVocabulary(t *testing.T) {
	for _, operation := range []k12.GradingItemOperation{
		k12.GradingItemOperationSolveGenerate,
		k12.GradingItemOperationSolveVerify,
		k12.GradingItemOperationGrade,
		k12.GradingItemOperationParentGuide,
	} {
		if !operation.Valid() {
			t.Errorf("physical operation %q must be valid", operation)
		}
	}
}

func TestGrading20260726_SixteenSlowQuestionsPersistThreeNPlusWWrites(t *testing.T) {
	questions := make([]RecognizedQuestion, 16)
	for i := range questions {
		questions[i] = RecognizedQuestion{
			Question:      fmt.Sprintf("q%02d", i+1),
			Subject:       "数学",
			StudentAnswer: "wrong",
			AnswerState:   AnswerStatePresent,
		}
	}
	solver := &grading20260726PhysicalSolver{
		delay: 20 * time.Millisecond,
		calls: map[k12.GradingItemOperation]int{},
	}
	grader := &grading20260726PhysicalGrader{delay: 20 * time.Millisecond}
	o := newParallelAnchorOrchestrator(
		t, &countingRecognizer{questions: questions}, nil,
		WithGradingRunDir(t.TempDir()),
	)
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	guide := &parentTeachingGuideSpy{}
	o.deps.ParentTeachingGuide = guide

	jobID := runItemResumeJobToAssessing(t, o, "physical-ledger-16-slow")
	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil {
		t.Fatalf("confirm and run: %v", err)
	}
	if completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("stage=%s, want completed", completed.Record.Status)
	}

	invocations, err := o.deps.Records.ListGradingItemInvocations(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatalf("list physical invocations: %v", err)
	}
	counts := map[k12.GradingItemOperation]int{}
	receipts := map[string]bool{}
	for _, invocation := range invocations {
		counts[invocation.Operation]++
		if invocation.Status == k12.ModelInvocationSucceeded {
			if invocation.CostReceiptID == "" {
				t.Errorf("successful invocation %s has no cost receipt", invocation.InvocationID)
			}
			if receipts[invocation.CostReceiptID] {
				t.Errorf("duplicate cost receipt %q", invocation.CostReceiptID)
			}
			receipts[invocation.CostReceiptID] = true
		}
	}
	for _, operation := range []k12.GradingItemOperation{
		k12.GradingItemOperationSolveGenerate,
		k12.GradingItemOperationSolveVerify,
		k12.GradingItemOperationGrade,
		k12.GradingItemOperationParentGuide,
	} {
		if got := counts[operation]; got != 16 {
			t.Errorf("%s ledger rows=%d, want 16; all=%v", operation, got, counts)
		}
	}
	if got := len(invocations); got != 64 {
		t.Fatalf("physical ledger rows=%d, want 3N+W=64", got)
	}
	if got := solver.count(k12.GradingItemOperationSolveGenerate); got != 16 {
		t.Errorf("solve_generate POSTs=%d, want 16", got)
	}
	if got := solver.count(k12.GradingItemOperationSolveVerify); got != 16 {
		t.Errorf("solve_verify POSTs=%d, want 16", got)
	}
	if got := grader.count(); got != 16 {
		t.Errorf("grade POSTs=%d, want 16", got)
	}
	if got := len(guide.snapshot()); got != 16 {
		t.Errorf("parent_guide POSTs=%d, want 16", got)
	}
}

func TestGrading20260726_SentCallSurvivesStageCancellationAndCommits(t *testing.T) {
	o := newItemResumeOrchestrator(
		t,
		t.TempDir(),
		[]RecognizedQuestion{{
			Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
		}},
		&itemResumeSolver{calls: map[string]int{}},
		&itemResumeGrader{calls: map[string]int{}},
	)
	jobID := runItemResumeJobToAssessing(t, o, "physical-call-independent-deadline")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	executor := newDurableGradingPhysicalCallExecutor(o, job, run.questions[0])
	stageCtx, cancelStage := context.WithCancel(context.Background())
	ctx := withGradingPhysicalCallExecutor(stageCtx, executor)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := ExecuteGradingPhysicalCall(ctx, GradingPhysicalCallSpec{
			Operation: k12.GradingItemOperationSolveGenerate, RequestDigest: "sha256:sent-call",
		}, func(callCtx context.Context) (string, error) {
			close(started)
			timer := time.NewTimer(40 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
				return `{"solution":"2"}`, nil
			case <-callCtx.Done():
				return "", callCtx.Err()
			}
		})
		done <- err
	}()
	<-started
	cancelStage()
	if err := <-done; err != nil {
		t.Fatalf("already-sent physical call was cancelled by stage budget: %v", err)
	}
	rows, err := o.deps.Records.ListGradingItemInvocations(
		context.Background(), "mingming", jobID,
	)
	if err != nil || len(rows) != 1 || rows[0].Status != k12.ModelInvocationSucceeded ||
		rows[0].CostReceiptID == "" {
		t.Fatalf("sent-call durable row=%+v err=%v", rows, err)
	}
}

func TestGrading20260726_ThreeRestartsReplayFinishedOperationsOnly(t *testing.T) {
	solver := &grading20260726PhysicalSolver{
		calls: map[k12.GradingItemOperation]int{},
		failFirst: map[k12.GradingItemOperation]error{
			k12.GradingItemOperationSolveVerify: errItemResumeProvider503,
		},
	}
	grader := &grading20260726PhysicalGrader{}
	guide := &grading20260726RestartGuide{}
	o := newParallelAnchorOrchestrator(
		t,
		&countingRecognizer{questions: []RecognizedQuestion{{
			Question: "q1", Subject: "数学", StudentAnswer: "wrong", AnswerState: AnswerStatePresent,
		}}},
		nil,
		WithGradingRunDir(t.TempDir()),
	)
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	o.deps.ParentTeachingGuide = guide
	jobID := runItemResumeJobToAssessing(t, o, "physical-three-restarts")

	view, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, errItemResumeProvider503) ||
		view.Record.Status != k12.GradingStageFailedRetryable {
		t.Fatalf("verify failure stage=%s err=%v", view.Record.Status, err)
	}
	for restart := 1; restart <= 3; restart++ {
		o.mu.Lock()
		delete(o.runs, jobID)
		o.mu.Unlock()
		if _, err := o.ensureRun(context.Background(), jobID); err != nil {
			t.Fatalf("restart %d restore runtime: %v", restart, err)
		}
	}
	view, err = o.RetryAndRun(context.Background(), jobID)
	if err != nil || view.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("retry after three restarts stage=%s err=%v", view.Record.Status, err)
	}
	if got := solver.count(k12.GradingItemOperationSolveGenerate); got != 1 {
		t.Errorf("solve_generate POSTs=%d, want 1 across three restarts", got)
	}
	if got := solver.count(k12.GradingItemOperationSolveVerify); got != 2 {
		t.Errorf("solve_verify POSTs=%d, want one definitive failure plus one retry", got)
	}
	if got := grader.count(); got != 1 {
		t.Errorf("grade POSTs=%d, want one unfinished operation", got)
	}
	if got := guide.count(); got != 1 {
		t.Errorf("parent_guide POSTs=%d, want one unfinished operation", got)
	}
}

func TestGrading20260726_UnknownPhysicalCallBlocksRepeatPOSTWithoutFakeQuery(t *testing.T) {
	o := newItemResumeOrchestrator(
		t,
		t.TempDir(),
		[]RecognizedQuestion{{
			Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
		}},
		&itemResumeSolver{calls: map[string]int{}},
		&itemResumeGrader{calls: map[string]int{}},
	)
	jobID := runItemResumeJobToAssessing(t, o, "physical-query-unavailable")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	spec := GradingPhysicalCallSpec{
		Operation: k12.GradingItemOperationSolveGenerate, RequestDigest: "sha256:unknown-call",
	}
	posts := 0
	call := func(context.Context) (string, error) {
		posts++
		return "", io.ErrUnexpectedEOF
	}
	executor := newDurableGradingPhysicalCallExecutor(o, job, run.questions[0])
	ctx := withGradingPhysicalCallExecutor(context.Background(), executor)
	if _, err := ExecuteGradingPhysicalCall(ctx, spec, call); !errors.Is(
		err, ErrGradingPhysicalCallOutcomeUnknown,
	) {
		t.Fatalf("first ambiguous call err=%v", err)
	}
	executor = newDurableGradingPhysicalCallExecutor(o, job, run.questions[0])
	ctx = withGradingPhysicalCallExecutor(context.Background(), executor)
	if _, err := ExecuteGradingPhysicalCall(ctx, spec, call); !errors.Is(
		err, ErrModelInvocationRequiresReconciliation,
	) {
		t.Fatalf("unknown recovery err=%v, want reconciliation blocker", err)
	}
	if posts != 1 {
		t.Fatalf("provider POSTs=%d, want exactly one without query capability", posts)
	}
	rows, err := o.deps.Records.ListGradingItemInvocations(
		context.Background(), "mingming", jobID,
	)
	if err != nil || len(rows) != 1 || rows[0].Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf("unknown ledger=%+v err=%v", rows, err)
	}
}

func TestGrading20260726_ReconciliationOnlyNeverCreatesOrSendsMissingPhysicalCall(
	t *testing.T,
) {
	o := newItemResumeOrchestrator(
		t,
		t.TempDir(),
		[]RecognizedQuestion{{
			Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
		}},
		&itemResumeSolver{calls: map[string]int{}},
		&itemResumeGrader{calls: map[string]int{}},
	)
	jobID := runItemResumeJobToAssessing(t, o, "physical-reconciliation-only")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	executor := newDurableGradingPhysicalCallExecutor(o, job, run.questions[0])
	ctx := withProblemSourceReconciliationOnly(context.Background())
	ctx = withGradingPhysicalCallExecutor(ctx, executor)
	posts := 0
	_, err := ExecuteGradingPhysicalCall(ctx, GradingPhysicalCallSpec{
		Operation:     k12.GradingItemOperationSolveGenerate,
		RequestDigest: "sha256:reconciliation-only-missing",
	}, func(context.Context) (string, error) {
		posts++
		return `{"solution":"must-not-send"}`, nil
	})
	if !errors.Is(err, ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("reconciliation-only missing physical call err=%v", err)
	}
	if posts != 0 {
		t.Fatalf("reconciliation-only provider POSTs=%d, want 0", posts)
	}
	rows, listErr := o.deps.Records.ListGradingItemInvocations(
		context.Background(), "mingming", jobID,
	)
	if listErr != nil || len(rows) != 0 {
		t.Fatalf("reconciliation-only created physical rows=%+v err=%v", rows, listErr)
	}
}
