package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type itemResumeProviderResponseError struct {
	statusCode int
	cause      error
}

func (e *itemResumeProviderResponseError) Error() string { return e.cause.Error() }
func (e *itemResumeProviderResponseError) Unwrap() error { return e.cause }
func (e *itemResumeProviderResponseError) ProviderResponseStatusCode() int {
	return e.statusCode
}

var errItemResumeProvider503 = &itemResumeProviderResponseError{
	statusCode: 503, cause: errors.New("test provider: 503 Service Unavailable"),
}

type itemResumeSolver struct {
	mu        sync.Mutex
	calls     map[string]int
	solutions map[string]string
}

func (s *itemResumeSolver) Solve(_ context.Context, problem, _, _ string) (SolveResult, error) {
	s.mu.Lock()
	s.calls[problem]++
	solution := s.solutions[problem]
	s.mu.Unlock()
	if solution == "" {
		solution = "2"
	}
	return SolveResult{
		Solution: solution,
		Evidence: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec},
	}, nil
}

func (s *itemResumeSolver) callCount(problem string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[problem]
}

type itemResumeGrader struct {
	mu             sync.Mutex
	calls          map[string]int
	failFirst      map[string]error
	outcomeUnknown map[string]error
	outcomes       map[string]GradeOutcome
}

func (g *itemResumeGrader) Grade(_ context.Context, problem, _, _ string) (GradeOutcome, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls[problem]++
	if err := g.outcomeUnknown[problem]; err != nil {
		return GradeOutcome{}, err
	}
	if err := g.failFirst[problem]; err != nil && g.calls[problem] == 1 {
		return GradeOutcome{}, err
	}
	if outcome, ok := g.outcomes[problem]; ok {
		return outcome, nil
	}
	return GradeOutcome{Verdict: VerdictAgree}, nil
}

func (g *itemResumeGrader) callCount(problem string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls[problem]
}

func newItemResumeOrchestrator(
	t *testing.T,
	runDir string,
	questions []RecognizedQuestion,
	solver *itemResumeSolver,
	grader *itemResumeGrader,
) *GradingOrchestrator {
	t.Helper()
	o := newParallelAnchorOrchestrator(t, &countingRecognizer{questions: questions}, nil,
		WithGradingRunDir(runDir))
	o.deps.Solver = solver
	o.deps.Grader = grader
	o.deps.VerifiedGrader = nil
	// Durable V33 completed-homework wrong items require their own parent-guide
	// provider operation. Keep this shared fixture production-shaped; tests that
	// need a failure/spy replace it explicitly.
	o.deps.ParentTeachingGuide = &parentTeachingGuideSpy{}
	return o
}

func runItemResumeJobToAssessing(t *testing.T, o *GradingOrchestrator, sourceKey string) string {
	t.Helper()
	jobID := startOrchestratorJob(t, o, sourceKey).Record.RecordID
	freezeItemResumeBudget(t, o, jobID)
	view, err := o.RunGradingJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("run to confirmation: %v", err)
	}
	if view.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("run stopped at %s, want awaiting_confirmation", view.Record.Status)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Fields.AnchorState == k12.GradingAnchorLocated ||
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})
	return jobID
}

func confirmItemResumeJobWithoutRun(t *testing.T, o *GradingOrchestrator, jobID string) (*gradingRun, GradingJobView) {
	t.Helper()
	run := o.lookup(jobID)
	if run == nil {
		t.Fatal("missing grading runtime before confirmation")
	}
	candidate := *run
	candidate.questions = cloneRecognizedQuestions(run.questions)
	candidate.anchored = cloneRecognizedQuestions(run.anchored)
	if err := applyAndValidateGradingConfirmation(&candidate, ConfirmPhotoGradingInput{}); err != nil {
		t.Fatalf("apply grading confirmation: %v", err)
	}
	job, err := o.deps.GetGradingJob(context.Background(), run.agentName, jobID)
	if err != nil {
		t.Fatalf("get job before confirmation: %v", err)
	}
	confirmedFacts := candidate.questions
	if candidate.anchored != nil {
		confirmedFacts = candidate.anchored
	}
	if err := o.persistProblemAttemptFacts(context.Background(), run.agentName,
		job.Fields.SubmissionID, confirmedFacts); err != nil {
		t.Fatalf("persist confirmed Problem/Attempt facts: %v", err)
	}
	if err := o.persistRun(jobID, &candidate); err != nil {
		t.Fatalf("persist confirmed runtime: %v", err)
	}
	if _, err := o.deps.ConfirmGradingJob(context.Background(), run.agentName, jobID,
		[]string{"canonical-recognition:" + CanonicalRecognizedQuestionsDigest(candidate.questions)}); err != nil {
		t.Fatalf("confirm grading job: %v", err)
	}
	run.questions = candidate.questions
	run.anchored = candidate.anchored
	job, err = o.deps.GetGradingJob(context.Background(), run.agentName, jobID)
	if err != nil {
		t.Fatalf("get assessing job: %v", err)
	}
	if job.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("confirmed job stage=%s, want assessing", job.Record.Status)
	}
	return run, job
}

func assertAssessStageInvocationStatuses(t *testing.T, o *GradingOrchestrator, jobID string,
	want ...k12.ModelInvocationStatus,
) {
	t.Helper()
	invocations, err := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatalf("list stage invocations: %v", err)
	}
	got := make([]k12.ModelInvocationStatus, 0, len(invocations))
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageAssessing {
			got = append(got, invocation.Status)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("assessing stage invocation statuses=%v, want %v", got, want)
	}
}

func freezeItemResumeBudget(t *testing.T, o *GradingOrchestrator, jobID string) {
	t.Helper()
	view, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatalf("get job before freezing budget: %v", err)
	}
	view.Fields.BudgetSnapshot = k12.GradingBudgetSnapshot{
		PolicyVersion: 1,
		StageSeconds: k12.GradingStageBudgets{
			Queued: 60, Normalizing: 60, Recognizing: 120,
			Locating: 60, Rendering: 60, Projecting: 60,
		},
		AssessingBuckets: []k12.GradingAssessingBudgetBucket{
			{MaxProblems: 1, Seconds: 90},
			{MaxProblems: 8, Seconds: 180},
			{MaxProblems: 16, Seconds: 300},
			{MaxProblems: 32, Seconds: 540},
		},
		ItemConcurrency: 2,
	}
	raw, err := json.Marshal(view.Fields)
	if err != nil {
		t.Fatalf("marshal frozen budget: %v", err)
	}
	if err := o.deps.Records.UpdateStatusFields(context.Background(), jobID, view.Record.Status,
		view.Record.DueAt, string(raw), view.Record.Version); err != nil {
		t.Fatalf("persist frozen budget: %v", err)
	}
}

func TestGradingOrchestratorItemResume_One503DoesNotCompleteAndRetriesOnlyMissingItem(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{
		calls:     map[string]int{},
		failFirst: map[string]error{"q3": errItemResumeProvider503},
	}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent},
		{Question: "q2", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent},
		{Question: "q3", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent},
	}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "item-resume-n-minus-one")

	failed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, errItemResumeProvider503) {
		t.Fatalf("one 503 err=%v, want provider error", err)
	}
	if failed.Record.Status != k12.GradingStageFailedRetryable {
		t.Fatalf("one missing receipt must not complete the page: stage=%s", failed.Record.Status)
	}
	assertAssessStageInvocationStatuses(t, o, jobID, k12.ModelInvocationFailed)

	completed, err := o.RetryAndRun(context.Background(), jobID)
	if err != nil {
		t.Fatalf("retry missing item: %v", err)
	}
	if completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("exact receipt set should complete: stage=%s", completed.Record.Status)
	}
	assertAssessStageInvocationStatuses(t, o, jobID,
		k12.ModelInvocationFailed, k12.ModelInvocationSucceeded)
	for _, problem := range []string{"q1", "q2", "q3"} {
		if got := solver.callCount(problem); got != 1 {
			t.Errorf("solver calls for %s=%d, want one durable solve", problem, got)
		}
	}
	for _, problem := range []string{"q1", "q2"} {
		if got := grader.callCount(problem); got != 1 {
			t.Errorf("grader calls for committed %s=%d, want no resend", problem, got)
		}
	}
	if got := grader.callCount("q3"); got != 2 {
		t.Errorf("grader calls for failed q3=%d, want first failure plus one safe retry", got)
	}
}

func TestGradingOrchestratorItemResume_SolveSucceededRetryRunsOnlyGrade(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{
		calls:     map[string]int{},
		failFirst: map[string]error{"q1": errItemResumeProvider503},
	}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "item-resume-grade-only")

	failed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, errItemResumeProvider503) || failed.Record.Status != k12.GradingStageFailedRetryable {
		t.Fatalf("first grade failure: stage=%s err=%v", failed.Record.Status, err)
	}
	completed, err := o.RetryAndRun(context.Background(), jobID)
	if err != nil || completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("grade-only retry: stage=%s err=%v", completed.Record.Status, err)
	}
	if got := solver.callCount("q1"); got != 1 {
		t.Fatalf("durable solve was resent %d times, want exactly one", got)
	}
	if got := grader.callCount("q1"); got != 2 {
		t.Fatalf("grader calls=%d, want failure plus one safe retry", got)
	}
}

func TestGradingOrchestratorItemResume_OutcomeUnknownDoesNotResendAfterRecovery(t *testing.T) {
	runDir := t.TempDir()
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{
		calls:          map[string]int{},
		outcomeUnknown: map[string]error{"q1": context.DeadlineExceeded},
	}
	o1 := newItemResumeOrchestrator(t, runDir, []RecognizedQuestion{{
		Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o1, "item-resume-unknown")

	unknown, err := o1.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unknown provider call err=%v, want deadline exceeded", err)
	}
	if unknown.Record.Status != k12.GradingStageOutcomeUnknown || unknown.Fields.Retryable {
		t.Fatalf("unknown call must stop without retry: stage=%s fields=%+v", unknown.Record.Status, unknown.Fields)
	}
	assertAssessStageInvocationStatuses(t, o1, jobID, k12.ModelInvocationOutcomeUnknown)

	o2 := trackGradingOrchestrator(t, NewGradingOrchestrator(o1.deps, orchestratorSnapshotResolver,
		WithGradingRunDir(runDir)))
	if _, err := o2.RecoverGradingJobs(context.Background(), []string{"mingming"}); err != nil {
		t.Fatalf("recover unknown job: %v", err)
	}
	idleCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := o2.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("wait recovery: %v", err)
	}
	if got := solver.callCount("q1"); got != 1 {
		t.Fatalf("unknown solve was resent after recovery: %d", got)
	}
	if got := grader.callCount("q1"); got != 1 {
		t.Fatalf("unknown grade was resent after recovery: %d", got)
	}
}

func TestGradingOrchestratorItemResume_TransportErrorAfterSendIsUnknownAndNeverResent(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	transportErr := fmt.Errorf("test provider request failed: %w", io.ErrUnexpectedEOF)
	grader := &itemResumeGrader{
		calls:          map[string]int{},
		outcomeUnknown: map[string]error{"q1": transportErr},
	}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "item-resume-transport-unknown")
	unknown, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) || unknown.Record.Status != k12.GradingStageOutcomeUnknown {
		t.Fatalf("ambiguous transport result: stage=%s err=%v", unknown.Record.Status, err)
	}
	if _, err := o.RetryAndRun(context.Background(), jobID); err == nil {
		t.Fatal("ordinary retry must reject an ambiguous sent invocation")
	}
	if solver.callCount("q1") != 1 || grader.callCount("q1") != 1 {
		t.Fatalf("ambiguous invocation was resent: solver=%d grader=%d",
			solver.callCount("q1"), grader.callCount("q1"))
	}
}

func TestGradingOrchestratorItemResume_ExactSetCompletes(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent},
		{Question: "q2", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent},
		{Question: "q3", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent},
	}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "item-resume-exact-set")

	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil || completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("complete exact set: stage=%s err=%v", completed.Record.Status, err)
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || len(result.Items) != 3 {
		t.Fatalf("completed result exact set: ok=%v items=%d", ok, len(result.Items))
	}
	assertAssessStageInvocationStatuses(t, o, jobID, k12.ModelInvocationSucceeded)
	for _, problem := range []string{"q1", "q2", "q3"} {
		if got := solver.callCount(problem); got != 1 {
			t.Errorf("solver calls for %s=%d, want one", problem, got)
		}
		if got := grader.callCount(problem); got != 1 {
			t.Errorf("grader calls for %s=%d, want one", problem, got)
		}
	}
}

func TestGradingOrchestratorItemResume_SentAggregateReplaysCommittedItemWithoutResend(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "item-resume-sent-aggregate")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)

	invocation, err := o.beginFrozenAssessInvocation(context.Background(), run, job)
	if err != nil || invocation.Status != k12.ModelInvocationSent {
		t.Fatalf("prepare aggregate sent boundary: status=%s err=%v", invocation.Status, err)
	}
	if _, err := o.assessDurablePhotoItem(context.Background(), o.deps, job, run.req,
		PhotoModeGrade, run.questions[0]); err != nil {
		t.Fatalf("commit item before simulated crash: %v", err)
	}
	if solver.callCount("q1") != 1 || grader.callCount("q1") != 1 {
		t.Fatalf("seed calls: solver=%d grader=%d, want one each",
			solver.callCount("q1"), grader.callCount("q1"))
	}

	view, err := o.runAssessItems(context.Background(), run, job)
	if err != nil || view.Record.Status != k12.GradingStageRendering {
		t.Fatalf("resume sent aggregate: stage=%s err=%v", view.Record.Status, err)
	}
	if solver.callCount("q1") != 1 || grader.callCount("q1") != 1 {
		t.Fatalf("committed item was resent: solver=%d grader=%d",
			solver.callCount("q1"), grader.callCount("q1"))
	}
	assertAssessStageInvocationStatuses(t, o, jobID, k12.ModelInvocationSucceeded)
}

func TestGradingOrchestratorItemResume_RejectsUnconfirmedAttemptBeforeAnyModelCall(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "item-resume-unconfirmed-preflight")
	job, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	run := o.lookup(jobID)
	if run == nil || len(run.questions) != 1 {
		t.Fatal("missing pre-confirmation runtime question")
	}
	q := run.questions[0]
	q.ConfirmedVersion = 0
	q.InputDigest = "sha256:must-not-authorize-unconfirmed-input"
	_, err = o.assessDurablePhotoItem(context.Background(), o.deps, job, run.req, PhotoModeGrade, q)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unconfirmed attempt err=%v, want invalid input", err)
	}
	if solver.callCount("q1") != 0 || grader.callCount("q1") != 0 {
		t.Fatalf("unconfirmed attempt reached model: solver=%d grader=%d",
			solver.callCount("q1"), grader.callCount("q1"))
	}
}

func TestGradingItemInvocationStartsNewAttemptForNewImmutableInputWithoutResendingExactRequest(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "item-invocation-new-input")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	question := run.questions[0]
	if question.ProblemID == "" || question.AttemptID == "" {
		t.Fatalf("missing durable item identity: %+v", question)
	}

	physicalCalls := 0
	firstRequest := map[string]any{"input_digest": question.InputDigest, "value": "first"}
	first, _, err := executeGradingItemOperation(
		context.Background(), o, job, question,
		k12.GradingItemOperationSolve, firstRequest,
		func(context.Context) (string, error) {
			physicalCalls++
			return "first-result", nil
		},
	)
	if err != nil || first != "first-result" || physicalCalls != 1 {
		t.Fatalf("first immutable request: result=%q calls=%d err=%v", first, physicalCalls, err)
	}

	changed := question
	changed.InputDigest = "sha256:new-current-input-revision"
	secondRequest := map[string]any{"input_digest": changed.InputDigest, "value": "second"}
	second, secondInvocationID, err := executeGradingItemOperation(
		context.Background(), o, job, changed,
		k12.GradingItemOperationSolve, secondRequest,
		func(context.Context) (string, error) {
			physicalCalls++
			return "second-result", nil
		},
	)
	if err != nil || second != "second-result" || physicalCalls != 2 {
		t.Fatalf("new input request: result=%q invocation=%q calls=%d err=%v",
			second, secondInvocationID, physicalCalls, err)
	}

	replayed, replayInvocationID, err := executeGradingItemOperation(
		context.Background(), o, job, changed,
		k12.GradingItemOperationSolve, secondRequest,
		func(context.Context) (string, error) {
			physicalCalls++
			return "must-not-run", nil
		},
	)
	if err != nil || replayed != "second-result" ||
		replayInvocationID != secondInvocationID || physicalCalls != 2 {
		t.Fatalf("exact request replay: result=%q invocation=%q calls=%d err=%v",
			replayed, replayInvocationID, physicalCalls, err)
	}
	invocations, err := o.deps.Records.ListGradingItemInvocations(
		context.Background(), job.Record.AgentName, job.Record.RecordID,
	)
	if err != nil || len(invocations) != 2 ||
		invocations[0].OperationAttempt != 1 || invocations[1].OperationAttempt != 2 {
		t.Fatalf("immutable invocation attempts=%+v err=%v", invocations, err)
	}
}

// REG-P0: reconciliation may consume a previously succeeded invocation, but
// it must never prepare, claim, or send a missing generic grading operation.
// This protects compositions whose provider adapter cannot expose the finer
// per-subagent physical-call interceptor.
func TestGradingItemInvocationReconciliationOnlyNeverCreatesOrSendsMissingOperation(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o, "item-invocation-reconcile-no-send")
	run, job := confirmItemResumeJobWithoutRun(t, o, jobID)
	question := run.questions[0]

	physicalCalls := 0
	_, invocationID, err := executeGradingItemOperation(
		withProblemSourceReconciliationOnly(context.Background()),
		o,
		job,
		question,
		k12.GradingItemOperationSolve,
		map[string]any{"input_digest": question.InputDigest, "value": "missing"},
		func(context.Context) (string, error) {
			physicalCalls++
			return "must-not-send", nil
		},
	)
	if !errors.Is(err, ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("reconciliation-only err=%v, want reconciliation required", err)
	}
	if invocationID != "" || physicalCalls != 0 {
		t.Fatalf("reconciliation-only invocation=%q physical_calls=%d, want no durable creation/send", invocationID, physicalCalls)
	}
	invocations, listErr := o.deps.Records.ListGradingItemInvocations(
		context.Background(), job.Record.AgentName, job.Record.RecordID,
	)
	if listErr != nil || len(invocations) != 0 {
		t.Fatalf("reconciliation-only durable invocations=%+v err=%v, want none", invocations, listErr)
	}
}

func TestGradingOrchestratorItemResume_WrongProjectionFactsSurviveCrashAndDedupe(t *testing.T) {
	runDir := t.TempDir()
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{
		calls:     map[string]int{},
		failFirst: map[string]error{"q2": errItemResumeProvider503},
		outcomes: map[string]GradeOutcome{
			"q1": {Verdict: VerdictDisagree, ErrorCause: "计算失误", WrongStep: "1+1=3"},
			"q2": {Verdict: VerdictAgree},
		},
	}
	o1 := newItemResumeOrchestrator(t, runDir, []RecognizedQuestion{
		{Question: "q1", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent},
		{Question: "q2", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent},
	}, solver, grader)
	jobID := runItemResumeJobToAssessing(t, o1, "item-resume-wrong-projection-crash")
	failed, err := o1.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, errItemResumeProvider503) || failed.Record.Status != k12.GradingStageFailedRetryable {
		t.Fatalf("seed partial receipts: stage=%s err=%v", failed.Record.Status, err)
	}

	// Rebuild the page with a new orchestrator. q1 must come exclusively from
	// its durable receipt; the pre-crash page result was never persisted.
	o2 := trackGradingOrchestrator(t, NewGradingOrchestrator(o1.deps, orchestratorSnapshotResolver,
		WithGradingRunDir(runDir)))
	completed, err := o2.RetryAndRun(context.Background(), jobID)
	if err != nil || completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("resume after crash: stage=%s err=%v", completed.Record.Status, err)
	}
	first, ok := o2.PhotoResult(jobID)
	if !ok || len(first.Items) != 2 {
		t.Fatalf("recovered result missing: ok=%v items=%d", ok, len(first.Items))
	}
	if !first.Items[0].Grade.RecordCreated || first.Items[0].Grade.RecordID == "" {
		t.Fatalf("first wrong projection facts lost across receipt replay: %+v", first.Items[0].Grade)
	}
	firstRecordID := first.Items[0].Grade.RecordID
	if got := grader.callCount("q1"); got != 1 {
		t.Fatalf("receipt replay resent q1 grader: %d", got)
	}

	// A different Job for the same canonical mistake must retain the existing
	// record identity while honestly reporting that it did not create it.
	o3 := trackGradingOrchestrator(t, NewGradingOrchestrator(o1.deps, orchestratorSnapshotResolver,
		WithGradingRunDir(t.TempDir())))
	job2View, created, err := o3.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: PhotoGradeRequest{
			AgentName: "mingming", Grade: "五年级上", SourceSession: "dt-1",
			Image: []byte("a distinct page containing the same canonical mistake"),
		},
		SourceKind: "im", SourceKey: "item-resume-wrong-projection-dedupe",
	})
	if err != nil || !created {
		t.Fatalf("start dedupe job: created=%v err=%v", created, err)
	}
	job2 := job2View.Record.RecordID
	freezeItemResumeBudget(t, o3, job2)
	view2, err := o3.RunGradingJob(context.Background(), job2)
	if err != nil || view2.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("dedupe job to confirmation: stage=%s err=%v", view2.Record.Status, err)
	}
	waitGradingView(t, o3, job2, func(v GradingJobView) bool {
		return v.Fields.AnchorState == k12.GradingAnchorLocated || v.Fields.AnchorState == k12.GradingAnchorDegraded
	})
	completed2, err := o3.ConfirmAndRun(context.Background(), job2, nil)
	if err != nil || completed2.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("deduped wrong job: stage=%s err=%v", completed2.Record.Status, err)
	}
	second, ok := o3.PhotoResult(job2)
	if !ok || len(second.Items) != 2 {
		t.Fatalf("deduped result missing: ok=%v items=%d", ok, len(second.Items))
	}
	if second.Items[0].Grade.RecordCreated || second.Items[0].Grade.RecordID != firstRecordID {
		t.Fatalf("dedupe projection facts=%+v, want created=false record_id=%s",
			second.Items[0].Grade, firstRecordID)
	}
}

func TestGradingOrchestratorItemResume_V51PolicyWritesCanonicalItemReceipts(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{calls: map[string]int{}}
	// ADR-K12-024/V51 has one production path. Completion is authorized by
	// per-problem invocation and assessment receipts rather than a page result.
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q1", Subject: "数学", StudentAnswer: "1", AnswerState: AnswerStatePresent,
	}}, solver, grader)
	o.deps.GradingBudgetSnapshot = orchestratorTestBudget()
	jobID := startOrchestratorJob(t, o, "item-resume-v51-canonical-receipts").Record.RecordID
	view, err := o.RunGradingJob(context.Background(), jobID)
	if err != nil || view.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("V51 run to confirmation: stage=%s err=%v", view.Record.Status, err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Fields.AnchorState == k12.GradingAnchorLocated ||
			v.Fields.AnchorState == k12.GradingAnchorDegraded
	})

	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil || completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("V51 grading: stage=%s err=%v", completed.Record.Status, err)
	}
	wantRows := map[string]int{
		"k12_grading_item_invocations": 2,
		"k12_grading_assessment_items": 1,
	}
	for table, want := range wantRows {
		var rows int
		if err := o.deps.Records.DB().QueryRow("SELECT COUNT(*) FROM "+table+" WHERE job_id=?", jobID).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if rows != want {
			t.Fatalf("V51 canonical receipts in %s=%d, want %d", table, rows, want)
		}
	}
}
