package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// GradingJob 崩溃恢复 + 异步编排契约（架构设计 §6.15 崩溃恢复 / §6.7 规则 3 / K12-INV-021）：
//
//  1. 进程启动扫描非终态 Job：自动阶段（queued/中间态）按检查点重新入列续跑；
//     awaiting_confirmation 保持等待（等家长），恢复后 Confirm 仍可续跑到终态；
//     failed_retryable（retryable=true）重新入列（回 queued 从检查点恢复）。
//  2. 恢复幂等：不重复调用识别/锚点模型（检查点产物落盘回放），不产生重复 Submission。
//  3. 异步执行模型：StartAsync 与请求 context 解耦、有界并发、panic 不逃逸；
//     推进到 awaiting_confirmation 停点；确认后异步续跑到终态。

// waitForStage 轮询 Job 直到达到期望 stage（异步编排的测试同步点）。
func waitForStage(t *testing.T, d Deps, agent, jobID, want string) GradingJobView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		v, err := d.GetGradingJob(context.Background(), agent, jobID)
		if err != nil {
			t.Fatalf("GetGradingJob: %v", err)
		}
		if v.Record.Status == want {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 stage=%s 超时，当前 %s（failure=%s）", want, v.Record.Status, v.Fields.FailureKind)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newRecoverableOrchestrator 与 newOrchestrator 相同，但启用运行时落盘目录（崩溃恢复载体）。
func newRecoverableOrchestrator(t *testing.T, d Deps, dir string) *GradingOrchestrator {
	t.Helper()
	return trackGradingOrchestrator(t, NewGradingOrchestrator(d, orchestratorSnapshotResolver, WithGradingRunDir(dir)))
}

func recoveryDeps(t *testing.T, rec Recognizer, anchorer AnswerAnchorer, annotator PhotoAnnotator) Deps {
	t.Helper()
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictDisagree, WrongStep: "1+1 误算为 3", ErrorCause: "计算失误", KnowledgePoint: "小数乘法"}},
		nil,
	)
	d.Recognizer = rec
	d.AnswerAnchorer = anchorer
	d.PhotoAnnotator = annotator
	d.ParentTeachingGuide = &parentTeachingGuideSpy{}
	d.Profiles = newMemProfiles()
	d.Profiles.(*memProfiles).m["mingming"] = k12.ChildProfile{
		ChildName: "小明", GradeTerm: "五年级上",
	}
	d.GradingBudgetSnapshot = orchestratorTestBudget()
	d.Now = func() int64 { return time.Now().Unix() }
	return d
}

type recoveryOutcomeUnknownRecognizer struct{ calls int }

func (r *recoveryOutcomeUnknownRecognizer) Recognize(context.Context, []byte) ([]RecognizedQuestion, error) {
	r.calls++
	return nil, context.DeadlineExceeded
}

// 崩溃点①：Job 创建后（queued）进程崩溃——重启扫描应从头续跑到确认停点。
func TestGradingRecovery_QueuedJobResumesToConfirmationStop(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &countingRecognizer{questions: []RecognizedQuestion{{Question: "1+1=", Subject: "数学", StudentAnswer: "3"}}}
	d := recoveryDeps(t, rec, &photoAnchorerFake{boxes: map[int]BBox{0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05}}}, &photoAnnotatorFake{})

	o1 := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o1.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "req-crash-1",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	// 崩溃模拟：o1 内存 run 丢弃，新进程 = 新编排器实例（同库同落盘目录）。
	o2 := newRecoverableOrchestrator(t, d, dir)
	n, err := o2.RecoverGradingJobs(ctx, []string{"mingming"})
	if err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	if n != 1 {
		t.Fatalf("应恢复 1 个非终态 Job, got %d", n)
	}
	v = waitForStage(t, d, "mingming", jobID, k12.GradingStageAwaitingConfirmation)
	if v.Fields.ConfirmationState != k12.GradingConfirmationPending {
		t.Fatalf("恢复续跑到确认停点应保持 pending, got %s", v.Fields.ConfirmationState)
	}
	if rec.calls != 1 {
		t.Fatalf("识别模型应只调 1 次, got %d", rec.calls)
	}
	// 停点后确认可正常续跑到终态（恢复后的 run 完整可用）。
	if _, err := o2.ConfirmAndRun(ctx, jobID, nil); err != nil {
		t.Fatalf("恢复后 ConfirmAndRun: %v", err)
	}
	waitForStage(t, d, "mingming", jobID, k12.GradingStageCompleted)
}

func TestGradingRecovery_ClearImageTaskAtConfirmationCheckpointAutoFreezes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "4÷0.5=", Subject: "数学",
		AnswerState: AnswerStatePresent, StudentAnswer: "8",
		RecognitionConfidence: float64Ptr(0.99), OCRSignals: []string{"decimal_point"},
	}}}
	d := recoveryDeps(t, rec, nil, nil)
	d.ParentTeachingGuide = &parentTeachingGuideSpy{}
	o1 := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o1.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "image_task", SourceKey: "recover-auto-freeze",
		BudgetSnapshot:            frozenWiringBudget(),
		ParentAutomaticAttemptID:  "recover-auto-freeze:1",
		ParentAutomaticDeadlineAt: d.now() + 300,
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	run := o1.lookup(jobID)
	if run == nil {
		t.Fatal("missing grading runtime")
	}
	if _, err = o1.advanceOK(ctx, run, jobID, ""); err != nil {
		t.Fatalf("advance queued: %v", err)
	}
	if _, err = o1.advanceOK(ctx, run, jobID, "image:test"); err != nil {
		t.Fatalf("advance normalizing: %v", err)
	}
	v, err = o1.runRecognize(ctx, run, jobID)
	if err != nil || v.Record.Status != k12.GradingStageAwaitingConfirmation ||
		v.Fields.ConfirmationState != k12.GradingConfirmationPending {
		t.Fatalf("prepare confirmation checkpoint: stage=%s confirmation=%s err=%v",
			v.Record.Status, v.Fields.ConfirmationState, err)
	}
	v, err = d.AdvanceGradingStage(ctx, "mingming", jobID, AdvanceGradingInput{
		Outcome: GradingOutcomeAnchor, AnchorState: k12.GradingAnchorLocated, ArtifactDigest: "anchor:test",
	})
	if err != nil || v.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("prepare located checkpoint: stage=%s err=%v", v.Record.Status, err)
	}

	// Simulate a run checkpoint written by the previous policy, where a clear
	// decimal was persisted as if it were uncertainty. Recovery must re-evaluate
	// this historical fact rather than only fixing newly recognized jobs.
	candidate := *run
	candidate.questions = cloneRecognizedQuestions(run.questions)
	candidate.questions[0].ConfirmationRequired = true
	candidate.questions[0].ConfirmationReasons = []OCRRiskReason{OCRRiskDecimalPoint}
	if err = o1.persistRun(jobID, &candidate); err != nil {
		t.Fatalf("persist historical run checkpoint: %v", err)
	}

	o2 := newRecoverableOrchestrator(t, d, dir)
	if _, err = o2.RecoverGradingJobs(ctx, []string{"mingming"}); err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	v = waitForStage(t, d, "mingming", jobID, k12.GradingStageCompleted)
	if v.Fields.ConfirmationState != k12.GradingConfirmationConfirmed {
		t.Fatalf("clear recovered image task was not auto-frozen: %#v", v.Fields)
	}
	snapshot, err := d.Records.GetProblemAttemptSnapshot(ctx, run.agentName, v.Fields.SubmissionID)
	if err != nil {
		t.Fatalf("load re-evaluated Problem/Attempt checkpoint: %v", err)
	}
	if snapshot.Problems[0].ConfirmationRequired || len(snapshot.Problems[0].ConfirmationReasons) != 0 {
		t.Fatalf("stale format reason survived recovery: %#v", snapshot.Problems[0])
	}
}

// 崩溃点②：停在 awaiting_confirmation 时崩溃——重启后保持等待（不自动确认、不重复调模型），
// 家长确认后续跑到终态（K12-INV-021：无重复模型调用）。
func TestGradingRecovery_AwaitingConfirmationStaysWaiting(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &countingRecognizer{questions: []RecognizedQuestion{{Question: "1+1=", Subject: "数学", StudentAnswer: "3"}}}
	anchorer := &photoAnchorerFake{boxes: map[int]BBox{0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05}}}
	d := recoveryDeps(t, rec, anchorer, &photoAnnotatorFake{})

	o1 := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o1.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "req-crash-2",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	if v, err = o1.RunGradingJob(ctx, jobID); err != nil || v.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("RunGradingJob 应停在确认点: %v stage=%s", err, v.Record.Status)
	}
	// 本用例模拟“确认停点已完整落盘后进程退出”；先等独立锚点分支回位，避免把仍存活的
	// o1 goroutine 与新进程 o2 并跑误当成崩溃恢复（真实崩溃时旧 goroutine 已不存在）。
	waitGradingView(t, o1, jobID, func(v GradingJobView) bool {
		return v.Fields.AnchorState == k12.GradingAnchorLocated
	})

	o2 := newRecoverableOrchestrator(t, d, dir)
	if _, err := o2.RecoverGradingJobs(ctx, []string{"mingming"}); err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	// 等待态不动：扫描后仍是 awaiting_confirmation + pending。
	v, err = d.GetGradingJob(ctx, "mingming", jobID)
	if err != nil || v.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("awaiting_confirmation 恢复后应保持等待: %v stage=%s", err, v.Record.Status)
	}
	if rec.calls != 1 || anchorer.calls != 1 {
		t.Fatalf("恢复不得重复调模型（K12-INV-021）: recognize=%d anchor=%d", rec.calls, anchorer.calls)
	}
	if _, err := o2.ConfirmAndRun(ctx, jobID, nil); err != nil {
		t.Fatalf("恢复后确认续跑: %v", err)
	}
	waitForStage(t, d, "mingming", jobID, k12.GradingStageCompleted)
	if rec.calls != 1 || anchorer.calls != 1 {
		t.Fatalf("确认续跑应回放检查点产物而非重调模型: recognize=%d anchor=%d", rec.calls, anchorer.calls)
	}
}

// 崩溃点③：failed_retryable（可重试）——重启扫描重新入列，从检查点续跑。
func TestGradingRecovery_FailedRetryableReenqueued(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &countingRecognizer{failures: 1, questions: []RecognizedQuestion{{Question: "1+1=", Subject: "数学", StudentAnswer: "3"}}}
	d := recoveryDeps(t, rec, &photoAnchorerFake{boxes: map[int]BBox{0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05}}}, &photoAnnotatorFake{})

	o1 := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o1.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "req-crash-3",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	if _, err = o1.RunGradingJob(ctx, jobID); err == nil {
		t.Fatalf("首次识别应失败（下游超时）")
	}
	if v, _ = d.GetGradingJob(ctx, "mingming", jobID); v.Record.Status != k12.GradingStageFailedRetryable {
		t.Fatalf("应落 failed_retryable, got %s", v.Record.Status)
	}

	o2 := newRecoverableOrchestrator(t, d, dir)
	if _, err := o2.RecoverGradingJobs(ctx, []string{"mingming"}); err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	waitForStage(t, d, "mingming", jobID, k12.GradingStageAwaitingConfirmation)
	if rec.calls != 2 {
		t.Fatalf("重试应恰好再调一次识别（首次失败+重试成功）, got %d", rec.calls)
	}
}

// REG-BUG-20260724-015: if recognition facts were committed but the local
// run.json write or invocation-ledger acknowledgement was lost, restart must
// reconcile that exact durable result and continue the same Job. It must not
// call the recognition provider a second time.
func TestGradingRecovery_OutcomeUnknownWithDurableRecognitionReconcilesWithoutResend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}}
	d := recoveryDeps(t, rec, nil, &photoAnnotatorFake{})

	o1 := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o1.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "req-reconcile-durable-recognition",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	jobDir := o1.runPath(jobID, "")
	if err := os.Chmod(jobDir, 0o500); err != nil {
		t.Fatalf("make runtime directory read-only: %v", err)
	}
	_, runErr := o1.RunGradingJob(ctx, jobID)
	if err := os.Chmod(jobDir, 0o700); err != nil {
		t.Fatalf("restore runtime directory permissions: %v", err)
	}
	if runErr == nil {
		t.Fatal("recognition run must expose the injected run.json durability failure")
	}
	parked, err := d.GetGradingJob(ctx, "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Record.Status != k12.GradingStageOutcomeUnknown {
		t.Fatalf("durability ambiguity stage=%s, want outcome_unknown", parked.Record.Status)
	}
	if rec.calls != 1 {
		t.Fatalf("initial recognition provider calls=%d, want 1", rec.calls)
	}

	o2 := newRecoverableOrchestrator(t, d, dir)
	if _, err := o2.RecoverGradingJobs(ctx, []string{"mingming"}); err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	waitForStage(t, d, "mingming", jobID, k12.GradingStageAwaitingConfirmation)
	if rec.calls != 1 {
		t.Fatalf("durable-result recovery resent recognition: calls=%d", rec.calls)
	}
	invocations, err := d.Records.ListModelInvocations(ctx, "mingming", jobID)
	if err != nil || len(invocations) != 1 {
		t.Fatalf("list model invocations: count=%d err=%v", len(invocations), err)
	}
	if invocations[0].Status != k12.ModelInvocationReconciled ||
		invocations[0].FailureKind != "reconciled_succeeded" {
		t.Fatalf("recognition invocation not reconciled from durable evidence: %+v", invocations[0])
	}
}

func TestGradingRecovery_OutcomeUnknownWithoutDurableResultStaysParkedWithoutResend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &recoveryOutcomeUnknownRecognizer{}
	d := recoveryDeps(t, rec, nil, &photoAnnotatorFake{})

	o1 := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o1.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "req-unknown-no-result",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	if _, err := o1.RunGradingJob(ctx, jobID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recognition outcome=%v, want deadline ambiguity", err)
	}
	if rec.calls != 1 {
		t.Fatalf("initial recognition calls=%d, want 1", rec.calls)
	}

	o2 := newRecoverableOrchestrator(t, d, dir)
	if _, err := o2.RecoverGradingJobs(ctx, []string{"mingming"}); err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	parked, err := d.GetGradingJob(ctx, "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Record.Status != k12.GradingStageOutcomeUnknown {
		t.Fatalf("unproven provider outcome stage=%s, want outcome_unknown", parked.Record.Status)
	}
	if rec.calls != 1 {
		t.Fatalf("recovery blindly resent recognition: calls=%d", rec.calls)
	}
}

// A Problem/Attempt snapshot can predate the latest invocation of the same Job. It
// is not proof that this Job's ambiguous invocation succeeded. Recovery may only
// reconcile recognition from a Job-scoped immutable receipt.
func TestGradingRecovery_OutcomeUnknownRejectsStaleSnapshotWithoutInvocationReceipt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &recoveryOutcomeUnknownRecognizer{}
	d := recoveryDeps(t, rec, nil, &photoAnnotatorFake{})

	o1 := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o1.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "req-unknown-stale-snapshot",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	if _, err := o1.RunGradingJob(ctx, jobID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recognition outcome=%v, want deadline ambiguity", err)
	}
	parked, err := d.GetGradingJob(ctx, "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	staleQuestions, err := NormalizeRecognizedProblems(parked.Fields.SubmissionID, []RecognizedQuestion{{
		Question: "9+9=", Subject: "数学", StudentAnswer: "18", AnswerState: AnswerStatePresent,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range staleQuestions {
		staleQuestions[i].PageAssetID = "page-stale-other-job"
	}
	stale, err := RecognizedQuestionsProblemAttemptSnapshot(
		"mingming", parked.Fields.SubmissionID, staleQuestions, d.now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Records.PutProblemAttemptSnapshot(ctx, stale); err != nil {
		t.Fatalf("seed stale snapshot: %v", err)
	}

	o2 := newRecoverableOrchestrator(t, d, dir)
	if _, err := o2.RecoverGradingJobs(ctx, []string{"mingming"}); err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	got, err := d.GetGradingJob(ctx, "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Record.Status != k12.GradingStageOutcomeUnknown {
		t.Fatalf("stale snapshot reconciled without invocation receipt: stage=%s", got.Record.Status)
	}
	if rec.calls != 1 {
		t.Fatalf("recovery resent ambiguous recognition: calls=%d", rec.calls)
	}
}

func TestGradingRecovery_OutcomeUnknownWithProcessLocalAssessmentStaysUnknownWithoutResend(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "2", AnswerState: AnswerStatePresent,
	}}}
	d := recoveryDeps(t, rec, nil, &photoAnnotatorFake{})
	o1 := newRecoverableOrchestrator(t, d, dir)
	v, _, err := o1.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "req-reconcile-durable-assessment",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID := v.Record.RecordID
	if _, err = o1.RunGradingJob(ctx, jobID); err != nil {
		t.Fatalf("recognition: %v", err)
	}
	waitGradingView(t, o1, jobID, func(view GradingJobView) bool {
		return view.Record.Status == k12.GradingStageAwaitingConfirmation &&
			view.Fields.AnchorState != k12.GradingAnchorPending
	})
	run := o1.lookup(jobID)
	if run == nil {
		t.Fatal("grading runtime missing")
	}
	candidate := *run
	candidate.questions = cloneRecognizedQuestions(run.questions)
	candidate.anchored = cloneRecognizedQuestions(run.anchored)
	if err := applyAndValidateGradingConfirmation(&candidate, ConfirmPhotoGradingInput{}); err != nil {
		t.Fatalf("freeze confirmation: %v", err)
	}
	job, err := d.GetGradingJob(ctx, run.agentName, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := o1.persistProblemAttemptFacts(ctx, run.agentName, job.Fields.SubmissionID, candidate.questions); err != nil {
		t.Fatal(err)
	}
	if err := o1.persistRun(jobID, &candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ConfirmGradingJob(ctx, run.agentName, jobID, []string{
		"canonical-recognition:" + CanonicalRecognizedQuestionsDigest(candidate.questions),
	}); err != nil {
		t.Fatalf("confirm job: %v", err)
	}
	run.questions = candidate.questions
	run.anchored = candidate.anchored

	assessmentQuestions := RecognizedQuestionsForAssessment(cloneRecognizedQuestions(run.questions))
	result := PhotoGradeResult{
		Mode: PhotoModeGrade, TaskIntent: PhotoTaskCompletedHomework,
		ResultSurface: PhotoSurfaceAnnotatedHomework,
		Items: []PhotoGradeItem{{
			Recognized: assessmentQuestions[0],
			Status:     PhotoCorrect,
			ResultKind: PhotoItemAssessment,
			Grade: GradeResult{
				Solution: "2",
				Evidence: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec},
				Outcome:  GradeOutcome{Verdict: VerdictAgree},
			},
		}},
		Markdown: "批改完成",
	}
	run.result = &result
	if err := o1.persistRun(jobID, run); err != nil {
		t.Fatalf("persist complete assessment: %v", err)
	}
	job, err = d.GetGradingJob(ctx, run.agentName, jobID)
	if err != nil || job.Record.Status != k12.GradingStageAssessing {
		t.Fatalf("assessment job: stage=%s err=%v", job.Record.Status, err)
	}
	requestRaw, _ := json.Marshal(struct {
		Request   PhotoGradeRequest    `json:"request"`
		Questions []RecognizedQuestion `json:"questions"`
		Anchored  []RecognizedQuestion `json:"anchored,omitempty"`
	}{run.req, run.questions, run.anchored})
	invocation, _, err := d.Records.PrepareModelInvocation(ctx, k12.ModelInvocation{
		InvocationID: "modelinv-durable-assessment", AgentName: run.agentName,
		JobID: jobID, Stage: k12.GradingStageAssessing,
		RequestDigest: modelInvocationDigest([]byte(k12.GradingStageAssessing), requestRaw),
		RouteSnapshot: job.Fields.ModelSnapshot, Attempt: job.Fields.AttemptCount + 1,
		CreatedAt: d.now(), UpdatedAt: d.now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocation, err = d.Records.MarkModelInvocationSent(ctx, run.agentName, invocation.InvocationID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Records.MarkModelInvocationOutcomeUnknown(ctx, run.agentName, invocation.InvocationID, "ack_lost"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.AdvanceGradingStage(ctx, run.agentName, jobID, AdvanceGradingInput{
		Outcome: GradingOutcomeUnknown, FailureKind: "ack_lost",
	}); err != nil {
		t.Fatal(err)
	}

	o2 := newRecoverableOrchestrator(t, d, dir)
	if _, err := o2.RecoverGradingJobs(ctx, []string{"mingming"}); err != nil {
		t.Fatalf("RecoverGradingJobs: %v", err)
	}
	parked, err := d.GetGradingJob(ctx, "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if parked.Record.Status != k12.GradingStageOutcomeUnknown {
		t.Fatalf("process-local assessment advanced recovery: stage=%s", parked.Record.Status)
	}
	invocations, err := d.Records.ListModelInvocations(ctx, "mingming", jobID)
	if err != nil || len(invocations) != 2 {
		t.Fatalf("model invocations count=%d err=%v", len(invocations), err)
	}
	var assessmentInvocation *k12.ModelInvocation
	for i := range invocations {
		if invocations[i].Stage == k12.GradingStageAssessing {
			assessmentInvocation = &invocations[i]
		}
	}
	if assessmentInvocation == nil ||
		assessmentInvocation.Status != k12.ModelInvocationOutcomeUnknown ||
		assessmentInvocation.FailureKind != "ack_lost" {
		t.Fatalf("assessment invocation must remain unknown without canonical receipts: %+v", assessmentInvocation)
	}
}

// 异步执行模型（§6.15）：StartAsync 推进到停点；ConfirmPhotoGradingJob 应用家长修正
// （修正后的作答进入批改输入）并异步续跑到终态。
func TestGradingAsync_StartAndConfirmWithCorrections(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rec := &countingRecognizer{questions: []RecognizedQuestion{{Question: "1+1=", Subject: "数学", StudentAnswer: "3"}}}
	d := recoveryDeps(t, rec, &photoAnchorerFake{boxes: map[int]BBox{0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05}}}, &photoAnnotatorFake{})
	o := newRecoverableOrchestrator(t, d, dir)

	v, _, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "req-async-1",
	})
	if err != nil {
		t.Fatalf("StartPhotoGradingJob: %v", err)
	}
	jobID := v.Record.RecordID
	o.StartAsync(jobID)
	waitForStage(t, d, "mingming", jobID, k12.GradingStageAwaitingConfirmation)

	// 家长修正第 0 题作答（3 → 2），确认后批改应使用修正后的 canonical 输入（§6.7 确认冻结）。
	v, ok, err := o.ConfirmPhotoGradingJob(ctx, jobID, ConfirmPhotoGradingInput{
		Subject: "数学",
		Corrections: []GradingQuestionCorrection{
			{Index: 0, Question: "1+1=", StudentAnswer: "2", AnswerState: AnswerStatePresent},
		},
	})
	if err != nil || !ok {
		t.Fatalf("ConfirmPhotoGradingJob: ok=%v err=%v", ok, err)
	}
	if v.Fields.ConfirmationState != k12.GradingConfirmationConfirmed {
		t.Fatalf("确认后 confirmation_state 应 confirmed, got %s", v.Fields.ConfirmationState)
	}
	waitForStage(t, d, "mingming", jobID, k12.GradingStageCompleted)
	result, ok := o.PhotoResult(jobID)
	if !ok || len(result.Items) != 1 {
		t.Fatalf("completed 后应可取批改产物: ok=%v items=%d", ok, len(result.Items))
	}
	if result.Items[0].Recognized.StudentAnswer != "2" {
		t.Fatalf("批改输入应为家长修正后的作答（2）, got %q", result.Items[0].Recognized.StudentAnswer)
	}
}
