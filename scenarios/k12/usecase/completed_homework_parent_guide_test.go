package usecase

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type completedHomeworkParentGuideSpy struct {
	mu     sync.Mutex
	calls  []ParentTeachingGuideRequest
	errFor map[string]error
	answer map[string]string
	onCall func(ParentTeachingGuideRequest)
}

func (s *completedHomeworkParentGuideSpy) GenerateParentTeachingGuide(
	_ context.Context,
	req ParentTeachingGuideRequest,
) (ParentTeachingGuide, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.onCall != nil {
		s.onCall(req)
	}
	if err := s.errFor[req.Problem]; err != nil {
		return ParentTeachingGuide{}, err
	}
	answer := "2"
	if value := s.answer[req.Problem]; value != "" {
		answer = value
	}
	return ParentTeachingGuide{
		Answer:                 answer,
		FullSolutionSteps:      []string{"模型不得覆盖已验算步骤"},
		GradeLevelMethod:       "按本年级方法分析 " + req.Problem,
		LikelyMistakes:         []string{"结合本题错步：" + req.WrongStep},
		ParentTeachingSequence: []string{"先让孩子复述题意", "再定位第一个错步", "最后独立重算"},
		FollowUpQuestions:      []string{"为什么这里要这样算？"},
		CheckingMethod:         "把结果代回原题独立验算",
	}, nil
}

func (s *completedHomeworkParentGuideSpy) snapshot() []ParentTeachingGuideRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ParentTeachingGuideRequest(nil), s.calls...)
}

func TestCompletedHomeworkGeneratesSevenItemGuideOnlyForVerifiedWrongItem(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}, evidenceType: EvidenceHeterogeneousModel}
	grader := &itemResumeGrader{
		calls: map[string]int{},
		outcomes: map[string]GradeOutcome{
			"q-wrong": {
				Verdict: VerdictDisagree, WrongStep: "把 1+1 算成 3",
				ErrorCause: "基础加法计算失误", KnowledgePoint: "整数加法",
			},
			"q-correct": {Verdict: VerdictAgree, KnowledgePoint: "整数加法"},
		},
	}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "q-wrong", Subject: "数学", StudentAnswer: "3",
			AnswerState: AnswerStatePresent, KnowledgePoints: []string{"整数加法"},
		},
		{
			Question: "q-correct", Subject: "数学", StudentAnswer: "2",
			AnswerState: AnswerStatePresent, KnowledgePoints: []string{"整数加法"},
		},
	}, solver, grader)
	generator := &completedHomeworkParentGuideSpy{}
	o.deps.ParentTeachingGuide = generator

	jobID := runItemResumeJobToAssessing(t, o, "completed-homework-parent-guide")
	preparedBeforeProvider := make(chan bool, 1)
	generator.mu.Lock()
	generator.onCall = func(req ParentTeachingGuideRequest) {
		invocations, callErr := o.deps.Records.ListGradingItemInvocations(
			context.Background(),
			"mingming",
			jobID,
		)
		if callErr != nil {
			preparedBeforeProvider <- false
			return
		}
		for _, invocation := range invocations {
			if invocation.Operation == k12.GradingItemOperationParentGuide &&
				invocation.Status == k12.ModelInvocationSent &&
				invocation.ResultJSON == "" {
				preparedBeforeProvider <- true
				return
			}
		}
		preparedBeforeProvider <- false
	}
	generator.mu.Unlock()
	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil || completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("complete homework: stage=%s err=%v", completed.Record.Status, err)
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || len(result.Items) != 2 {
		t.Fatalf("result exact set missing: ok=%v result=%#v", ok, result)
	}

	calls := generator.snapshot()
	if len(calls) != 1 || calls[0].Problem != "q-wrong" {
		t.Fatalf("parent guide calls=%#v, want wrong item only", calls)
	}
	select {
	case prepared := <-preparedBeforeProvider:
		if !prepared {
			t.Fatal("parent guide provider ran before a durable sent invocation existed")
		}
	default:
		t.Fatal("parent guide provider preparation was not observed")
	}
	if calls[0].VerifiedSolution != "2" ||
		calls[0].StudentAnswer != "3" ||
		calls[0].WrongStep != "把 1+1 算成 3" ||
		calls[0].ErrorCause != "基础加法计算失误" {
		t.Fatalf("wrong-item guide was not anchored to solved/graded facts: %#v", calls[0])
	}

	wrong, correct := result.Items[0], result.Items[1]
	if wrong.Status != PhotoWrong || wrong.ParentGuide == nil {
		t.Fatalf("verified wrong item lacks parent guide: %#v", wrong)
	}
	if wrong.Grade.Outcome.WrongStep != "把 1+1 算成 3" ||
		wrong.Grade.Outcome.ErrorCause != "基础加法计算失误" {
		t.Fatalf("existing wrong_step/error_cause were lost: %#v", wrong.Grade.Outcome)
	}
	wantSteps := []string{"2"}
	if wrong.ParentGuide.Answer != "2" ||
		!reflect.DeepEqual(wrong.ParentGuide.FullSolutionSteps, wantSteps) ||
		validateParentTeachingGuide(*wrong.ParentGuide) != nil {
		t.Fatalf("wrong guide is incomplete or changed verified solution: %#v", wrong.ParentGuide)
	}
	if correct.Status != PhotoCorrect || correct.ParentGuide != nil {
		t.Fatalf("correct item must remain folded with zero guide: %#v", correct)
	}

	invocations, err := o.deps.Records.ListGradingItemInvocations(
		context.Background(), "mingming", jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	operations := make(map[string][]k12.GradingItemOperation)
	for _, invocation := range invocations {
		operations[invocation.ProblemID] = append(operations[invocation.ProblemID], invocation.Operation)
		if invocation.RouteSnapshot != completed.Fields.ModelSnapshot {
			t.Fatalf("item route drifted from frozen job route: item=%+v job=%+v",
				invocation.RouteSnapshot, completed.Fields.ModelSnapshot)
		}
	}
	assertExactGradingItemOperations(t, operations[wrong.Recognized.ProblemID],
		k12.GradingItemOperationSolve,
		k12.GradingItemOperationGrade,
		k12.GradingItemOperationParentGuide,
	)
	assertExactGradingItemOperations(t, operations[correct.Recognized.ProblemID],
		k12.GradingItemOperationSolve,
		k12.GradingItemOperationGrade,
	)
	for _, want := range []string{
		"**第一个错步：** 把 1+1 算成 3",
		"**错因：** 基础加法计算失误",
		"**答案：** 2",
		"**必要步骤：**",
		"**本年级方法：**",
		"**易错点：**",
		"**家长怎么讲：**",
		"**可以追问：**",
		"**怎么检查：**",
	} {
		if !strings.Contains(result.Markdown, want) {
			t.Fatalf("completed-homework markdown missing %q:\n%s", want, result.Markdown)
		}
	}
}

func assertExactGradingItemOperations(
	t *testing.T,
	got []k12.GradingItemOperation,
	want ...k12.GradingItemOperation,
) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("item operations=%v, want exact set %v", got, want)
	}
	counts := make(map[k12.GradingItemOperation]int, len(got))
	for _, operation := range got {
		counts[operation]++
	}
	for _, operation := range want {
		if counts[operation] != 1 {
			t.Fatalf("item operations=%v, want exact set %v", got, want)
		}
	}
}

func TestCompletedHomeworkCorrectOnlyPageMakesZeroParentGuideCalls(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}}
	grader := &itemResumeGrader{
		calls: map[string]int{},
		outcomes: map[string]GradeOutcome{
			"q1": {Verdict: VerdictAgree},
			"q2": {Verdict: VerdictAgree},
		},
	}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{
		{
			Question: "q1", Subject: "数学", StudentAnswer: "2",
			AnswerState: AnswerStatePresent,
		},
		{
			Question: "q2", Subject: "数学", StudentAnswer: "2",
			AnswerState: AnswerStatePresent,
		},
	}, solver, grader)
	generator := &completedHomeworkParentGuideSpy{}
	o.deps.ParentTeachingGuide = generator

	jobID := runItemResumeJobToAssessing(t, o, "completed-homework-correct-only")
	completed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err != nil || completed.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("correct-only completion: stage=%s err=%v", completed.Record.Status, err)
	}
	if calls := generator.snapshot(); len(calls) != 0 {
		t.Fatalf("correct-only page made parent guide calls: %#v", calls)
	}
	for _, question := range []string{"q1", "q2"} {
		if solver.callCount(question) != 1 || grader.callCount(question) != 1 {
			t.Fatalf("correct-only %s provider calls: solve=%d grade=%d, want one each",
				question, solver.callCount(question), grader.callCount(question))
		}
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || len(result.Items) != 2 {
		t.Fatalf("correct-only result missing: ok=%v result=%#v", ok, result)
	}
	for _, item := range result.Items {
		if item.Status != PhotoCorrect || item.ParentGuide != nil {
			t.Fatalf("correct item expanded a parent guide: %#v", item)
		}
	}
	invocations, err := o.deps.Records.ListGradingItemInvocations(
		context.Background(),
		"mingming",
		jobID,
	)
	if err != nil {
		t.Fatal(err)
	}
	operations := make(map[string][]k12.GradingItemOperation)
	for _, invocation := range invocations {
		if invocation.Operation == k12.GradingItemOperationParentGuide {
			t.Fatalf("correct-only page wrote a fake parent_guide ledger: %+v", invocation)
		}
		operations[invocation.ProblemID] = append(operations[invocation.ProblemID], invocation.Operation)
	}
	for _, item := range result.Items {
		assertExactGradingItemOperations(t, operations[item.Recognized.ProblemID],
			k12.GradingItemOperationSolve,
			k12.GradingItemOperationGrade,
		)
	}
}

func TestCompletedHomeworkRejectsGuideAnswerOutsideVerifiedSolutionWithoutChangingWrongFacts(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}, evidenceType: EvidenceHeterogeneousModel}
	grader := &itemResumeGrader{
		calls: map[string]int{},
		outcomes: map[string]GradeOutcome{
			"q-wrong": {
				Verdict: VerdictDisagree, WrongStep: "错步", ErrorCause: "错因",
			},
		},
	}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q-wrong", Subject: "数学", StudentAnswer: "3",
		AnswerState: AnswerStatePresent,
	}}, solver, grader)
	generator := &completedHomeworkParentGuideSpy{answer: map[string]string{"q-wrong": "999"}}
	o.deps.ParentTeachingGuide = generator

	jobID := runItemResumeJobToAssessing(t, o, "completed-homework-answer-anchor")
	failed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err == nil || failed.Record.Status == k12.GradingStageCompleted {
		t.Fatalf("invented answer must fail visibly: stage=%s err=%v", failed.Record.Status, err)
	}
	if !strings.Contains(err.Error(), "answer is not anchored") {
		t.Fatalf("anchor failure not identified: %v", err)
	}
	if calls := generator.snapshot(); len(calls) != 1 {
		t.Fatalf("anchor validation retried provider unexpectedly: %#v", calls)
	}
	if _, retryErr := o.RetryAndRun(context.Background(), jobID); retryErr == nil {
		t.Fatal("deterministic anchor failure must remain visible on retry")
	}
	if calls := generator.snapshot(); len(calls) != 1 {
		t.Fatalf("durably succeeded guide provider was resent after local anchor failure: %#v", calls)
	}
	invocations, listErr := o.deps.Records.ListGradingItemInvocations(
		context.Background(), "mingming", jobID,
	)
	if listErr != nil {
		t.Fatal(listErr)
	}
	for _, invocation := range invocations {
		if invocation.Operation == k12.GradingItemOperationParentGuide &&
			invocation.Status != k12.ModelInvocationSucceeded {
			t.Fatalf("complete provider result must be durable before local anchor validation: %+v", invocation)
		}
	}
}

func TestCompletedHomeworkRejectsIntermediateValueAsGuideAnswer(t *testing.T) {
	solver := &itemResumeSolver{
		calls:        map[string]int{},
		evidenceType: EvidenceHeterogeneousModel,
		solutions: map[string]string{
			"q-wrong": "## 过程\n\n1+1=2\n\n继续计算得到 3\n\n## 答案\n\n3",
		},
	}
	grader := &itemResumeGrader{
		calls: map[string]int{},
		outcomes: map[string]GradeOutcome{
			"q-wrong": {Verdict: VerdictDisagree, WrongStep: "错步", ErrorCause: "错因"},
		},
	}
	o := newItemResumeOrchestrator(t, t.TempDir(), []RecognizedQuestion{{
		Question: "q-wrong", Subject: "数学", StudentAnswer: "4",
		AnswerState: AnswerStatePresent,
	}}, solver, grader)
	generator := &completedHomeworkParentGuideSpy{
		answer: map[string]string{"q-wrong": "2"},
	}
	o.deps.ParentTeachingGuide = generator

	jobID := runItemResumeJobToAssessing(t, o, "completed-homework-intermediate-answer")
	failed, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if err == nil || failed.Record.Status == k12.GradingStageCompleted {
		t.Fatalf("intermediate value accepted as final answer: stage=%s err=%v", failed.Record.Status, err)
	}
	if !strings.Contains(err.Error(), "answer is not anchored") {
		t.Fatalf("intermediate-value rejection was not explicit: %v", err)
	}
	if calls := generator.snapshot(); len(calls) != 1 ||
		calls[0].VerifiedSolution != solver.solutions["q-wrong"] {
		t.Fatalf("guide did not use the immutable verified solution: %#v", calls)
	}
}

func TestCompletedHomeworkParentGuideOutcomeUnknownIsNeverResent(t *testing.T) {
	solver := &itemResumeSolver{calls: map[string]int{}, evidenceType: EvidenceHeterogeneousModel}
	grader := &itemResumeGrader{
		calls: map[string]int{},
		outcomes: map[string]GradeOutcome{
			"q-wrong": {Verdict: VerdictDisagree, WrongStep: "错步", ErrorCause: "错因"},
		},
	}
	runDir := t.TempDir()
	o := newItemResumeOrchestrator(t, runDir, []RecognizedQuestion{{
		Question: "q-wrong", Subject: "数学", StudentAnswer: "3",
		AnswerState: AnswerStatePresent,
	}}, solver, grader)
	generator := &completedHomeworkParentGuideSpy{
		errFor: map[string]error{"q-wrong": context.DeadlineExceeded},
	}
	o.deps.ParentTeachingGuide = generator

	jobID := runItemResumeJobToAssessing(t, o, "completed-homework-guide-unknown")
	unknown, err := o.ConfirmAndRun(context.Background(), jobID, nil)
	if !errors.Is(err, context.DeadlineExceeded) ||
		unknown.Record.Status != k12.GradingStageOutcomeUnknown ||
		unknown.Fields.Retryable {
		t.Fatalf("ambiguous guide call: stage=%s fields=%+v err=%v",
			unknown.Record.Status, unknown.Fields, err)
	}
	if _, err := o.RetryAndRun(context.Background(), jobID); err == nil {
		t.Fatal("ordinary retry must not resend an outcome_unknown parent guide")
	}
	recovered := trackGradingOrchestrator(t, NewGradingOrchestrator(
		o.deps,
		orchestratorSnapshotResolver,
		WithGradingRunDir(runDir),
	))
	if _, recoverErr := recovered.RecoverGradingJobs(
		context.Background(),
		[]string{"mingming"},
	); recoverErr != nil {
		t.Fatalf("recover outcome_unknown job: %v", recoverErr)
	}
	idleCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recovered.WaitForIdle(idleCtx); err != nil {
		t.Fatalf("wait recovered outcome_unknown job: %v", err)
	}
	if calls := generator.snapshot(); len(calls) != 1 {
		t.Fatalf("outcome_unknown parent guide resent: %#v", calls)
	}
	if solver.callCount("q-wrong") != 1 || grader.callCount("q-wrong") != 1 {
		t.Fatalf("prior durable operations resent: solve=%d grade=%d",
			solver.callCount("q-wrong"), grader.callCount("q-wrong"))
	}
}
