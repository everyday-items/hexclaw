package usecase

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// blockingGradingAnchorer 让测试能把锚点调用稳定停在 context/release 边界，
// 不依赖真实模型延迟来证明确认分支与定位分支是否真正独立。
type blockingGradingAnchorer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingGradingAnchorer) AnchorAnswers(ctx context.Context, _ []byte, questions []RecognizedQuestion) ([]RecognizedQuestion, error) {
	a.once.Do(func() { close(a.started) })
	select {
	case <-a.release:
		out := append([]RecognizedQuestion(nil), questions...)
		box := BBox{X: 0.2, Y: 0.3, W: 0.1, H: 0.05}
		out[0].BBox = &box
		return out, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// maliciousGradingAnchorer 模拟一个越权的 adapter 输出：除 BBox 外还篡改识别冻结事实。
// 编排器必须只合并几何字段，不能信任或覆盖其它字段。
type maliciousGradingAnchorer struct{}

func (maliciousGradingAnchorer) AnchorAnswers(_ context.Context, _ []byte, questions []RecognizedQuestion) ([]RecognizedQuestion, error) {
	out := append([]RecognizedQuestion(nil), questions...)
	out[0].Question = "被锚点篡改的题干"
	out[0].StudentAnswer = "999"
	out[0].AnswerState = AnswerStateUnclear
	out[0].Subject = "被篡改学科"
	out[0].KnowledgePoints = []string{"被篡改知识点"}
	box := BBox{X: 0.2, Y: 0.3, W: 0.1, H: 0.05}
	out[0].BBox = &box
	return out, nil
}

type deadlineGradingAnchorer struct {
	started     chan struct{}
	hadDeadline chan bool
	once        sync.Once
}

func (a *deadlineGradingAnchorer) AnchorAnswers(ctx context.Context, _ []byte, _ []RecognizedQuestion) ([]RecognizedQuestion, error) {
	a.once.Do(func() { close(a.started) })
	_, ok := ctx.Deadline()
	a.hadDeadline <- ok
	<-ctx.Done()
	return nil, ctx.Err()
}

type cancelBlockingRecognizer struct {
	started chan struct{}
	done    chan struct{}
	once    sync.Once
}

func (r *cancelBlockingRecognizer) Recognize(ctx context.Context, _ []byte) ([]RecognizedQuestion, error) {
	r.once.Do(func() { close(r.started) })
	<-ctx.Done()
	close(r.done)
	return nil, ctx.Err()
}

type remainingBudgetAnchorer struct {
	remaining chan time.Duration
}

func (a *remainingBudgetAnchorer) AnchorAnswers(ctx context.Context, _ []byte, questions []RecognizedQuestion) ([]RecognizedQuestion, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("anchor provider context has no deadline")
	}
	a.remaining <- time.Until(deadline)
	return cloneRecognizedQuestions(questions), nil
}

func newParallelAnchorOrchestrator(t *testing.T, rec Recognizer, anchorer AnswerAnchorer, opts ...GradingOrchestratorOption) *GradingOrchestrator {
	t.Helper()
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictDisagree, WrongStep: "1+1 误算为 3", ErrorCause: "计算失误", KnowledgePoint: "整数加法"}},
		nil,
	)
	d.Recognizer = rec
	d.AnswerAnchorer = anchorer
	d.PhotoAnnotator = &photoAnnotatorFake{}
	return trackGradingOrchestrator(t, NewGradingOrchestrator(d, orchestratorSnapshot, opts...))
}

func waitGradingView(t *testing.T, o *GradingOrchestrator, jobID string, match func(GradingJobView) bool) GradingJobView {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		v, err := o.deps.GetGradingJob(ctx, "mingming", jobID)
		if err == nil && match(v) {
			return v
		}
		select {
		case <-ctx.Done():
			t.Fatalf("等待 GradingJob 状态超时: %v (last=%+v)", ctx.Err(), v)
		case <-ticker.C:
		}
	}
}

func TestGradingOrchestratorRecognitionStartsIndependentAnchorAndDoesNotBlockConfirmation(t *testing.T) {
	anchorer := &blockingGradingAnchorer{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-anchorer.release:
		default:
			close(anchorer.release)
		}
	})
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, rec, anchorer)
	jobID := startOrchestratorJob(t, o, "msg-parallel-anchor").Record.RecordID

	runDone := make(chan GradingJobView, 1)
	runErr := make(chan error, 1)
	go func() {
		v, err := o.RunGradingJob(context.Background(), jobID)
		runDone <- v
		runErr <- err
	}()

	select {
	case <-anchorer.started:
	case <-time.After(time.Second):
		t.Fatal("识别完成后未启动锚点分支")
	}

	var stopped GradingJobView
	select {
	case stopped = <-runDone:
		if err := <-runErr; err != nil {
			t.Fatalf("RunGradingJob: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		close(anchorer.release)
		<-runDone
		<-runErr
		t.Fatal("锚点分支阻塞了主链返回确认停点；识别冻结后应立即返回 awaiting_confirmation")
	}
	if stopped.Record.Status != k12.GradingStageAwaitingConfirmation || stopped.Fields.AnchorState != k12.GradingAnchorPending {
		t.Fatalf("锚点仍在途时应停在独立等待态: stage=%s anchor=%s", stopped.Record.Status, stopped.Fields.AnchorState)
	}

	confirmed, ok, err := o.ConfirmPhotoGradingJob(context.Background(), jobID, ConfirmPhotoGradingInput{})
	if err != nil || !ok {
		t.Fatalf("锚点仍在途时确认命令不得被阻塞: ok=%v err=%v", ok, err)
	}
	if confirmed.Record.Status != k12.GradingStageAwaitingConfirmation || confirmed.Fields.ConfirmationState != k12.GradingConfirmationConfirmed {
		t.Fatalf("确认先到时应持久化 confirmed 并等待锚点: stage=%s fields=%+v", confirmed.Record.Status, confirmed.Fields)
	}

	close(anchorer.release)
	final := waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record != nil && v.Record.Status == k12.GradingStageCompleted
	})
	if final.Fields.AnchorState != k12.GradingAnchorLocated {
		t.Fatalf("锚点后到应与确认汇合并完成批改: %+v", final.Fields)
	}
}

func TestGradingOrchestratorConfirmAndRunWaitsOutsideJobLockForSynchronousIMConsumer(t *testing.T) {
	anchorer := &blockingGradingAnchorer{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-anchorer.release:
		default:
			close(anchorer.release)
		}
	})
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, rec, anchorer)
	jobID := startOrchestratorJob(t, o, "msg-sync-im-anchor").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	select {
	case <-anchorer.started:
	case <-time.After(time.Second):
		t.Fatal("未启动锚点分支")
	}

	done := make(chan GradingJobView, 1)
	errCh := make(chan error, 1)
	go func() {
		v, err := o.ConfirmAndRun(context.Background(), jobID, nil)
		done <- v
		errCh <- err
	}()
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Fields.ConfirmationState == k12.GradingConfirmationConfirmed &&
			v.Fields.AnchorState == k12.GradingAnchorPending
	})
	select {
	case v := <-done:
		t.Fatalf("同步入口不应在汇合前伪装完成: stage=%s err=%v", v.Record.Status, <-errCh)
	default:
	}

	close(anchorer.release)
	select {
	case v := <-done:
		if err := <-errCh; err != nil || v.Record.Status != k12.GradingStageCompleted {
			t.Fatalf("锚点回位后同步入口应完成: stage=%s err=%v", v.Record.Status, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("锚点回位后 ConfirmAndRun 未完成")
	}
}

func TestGradingOrchestratorAnchorCanOnlyAddGeometryToFrozenRecognition(t *testing.T) {
	want := RecognizedQuestion{
		Question: "3.8×3=", KnowledgePoints: []string{"小数乘法"}, AnswerState: AnswerStatePresent,
		StudentAnswer: "10.4", Subject: "数学",
	}
	rec := &countingRecognizer{questions: []RecognizedQuestion{want}}
	o := newParallelAnchorOrchestrator(t, rec, maliciousGradingAnchorer{})
	jobID := startOrchestratorJob(t, o, "msg-anchor-geometry-only").Record.RecordID

	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Fields.AnchorState == k12.GradingAnchorLocated
	})
	got, ok := o.RecognizedQuestions(context.Background(), jobID)
	if !ok || len(got) != 1 {
		t.Fatalf("应可回读识别停点产物: ok=%v got=%#v", ok, got)
	}
	box := got[0].BBox
	got[0].BBox = nil
	jobView, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatalf("get job for expected recognition scope: %v", err)
	}
	wantNormalized, err := NormalizeRecognizedProblems(jobView.Fields.SubmissionID, []RecognizedQuestion{want})
	if err != nil {
		t.Fatalf("normalize expected recognition: %v", err)
	}
	if !reflect.DeepEqual(got[0], wantNormalized[0]) {
		t.Fatalf("锚点只能补 BBox，不得覆盖已冻结事实\n got=%#v\nwant=%#v", got[0], wantNormalized[0])
	}
	if box == nil || *box != (BBox{X: 0.2, Y: 0.3, W: 0.1, H: 0.05}) {
		t.Fatalf("锚点返回的可信几何应被合并: %#v", box)
	}
}

func TestGradingOrchestratorAnchorTimeoutPersistsDegradedAndTextGradingContinues(t *testing.T) {
	anchorer := &deadlineGradingAnchorer{
		started: make(chan struct{}), hadDeadline: make(chan bool, 1),
	}
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, rec, anchorer, WithGradingAnchorTimeout(25*time.Millisecond))
	jobID := startOrchestratorJob(t, o, "msg-anchor-timeout").Record.RecordID

	v, err := o.RunGradingJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	if v.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("识别完成应先进入确认停点: %s", v.Record.Status)
	}
	select {
	case <-anchorer.started:
	case <-time.After(time.Second):
		t.Fatal("未启动锚点分支")
	}
	if !<-anchorer.hadDeadline {
		t.Fatal("锚点调用必须使用 context.WithTimeout 派生的 deadline context")
	}

	confirmed, ok, err := o.ConfirmPhotoGradingJob(context.Background(), jobID, ConfirmPhotoGradingInput{})
	if err != nil || !ok {
		t.Fatalf("确认: ok=%v err=%v", ok, err)
	}
	if confirmed.Fields.ConfirmationState != k12.GradingConfirmationConfirmed {
		t.Fatalf("确认应独立持久化: %+v", confirmed.Fields)
	}
	final := waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Record != nil && v.Record.Status == k12.GradingStageCompleted
	})
	if final.Fields.AnchorState != k12.GradingAnchorDegraded {
		t.Fatalf("锚点超时必须持久化 degraded: %+v", final.Fields)
	}
	foundTimeout := false
	for _, cp := range final.Fields.StageCheckpoints {
		if cp.Stage == k12.GradingStageLocating && cp.Degraded && cp.ArtifactDigest == "anchor:timeout" {
			foundTimeout = true
		}
	}
	if !foundTimeout {
		t.Fatalf("锚点超时应留下 degraded + anchor:timeout 检查点: %+v", final.Fields.StageCheckpoints)
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || result.Markdown == "" {
		t.Fatalf("锚点超时不得阻断文字批改: ok=%v result=%+v", ok, result)
	}
}

func TestGradingOrchestratorWaitForIdleIncludesAnchorAndFollowupWorkers(t *testing.T) {
	anchorer := &blockingGradingAnchorer{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-anchorer.release:
		default:
			close(anchorer.release)
		}
	})
	rec := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, rec, anchorer)
	jobID := startOrchestratorJob(t, o, "msg-worker-lifecycle").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-anchorer.started:
	case <-time.After(time.Second):
		t.Fatal("未启动锚点分支")
	}
	if _, ok, err := o.ConfirmPhotoGradingJob(context.Background(), jobID, ConfirmPhotoGradingInput{}); err != nil || !ok {
		t.Fatalf("confirm: ok=%v err=%v", ok, err)
	}

	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancelBlocked()
	if err := o.WaitForIdle(blockedCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked anchor must keep orchestrator non-idle, got %v", err)
	}
	baselineGoroutines := runtime.NumGoroutine()
	for i := 0; i < 32; i++ {
		cancelledCtx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := o.WaitForIdle(cancelledCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled wait %d: %v", i, err)
		}
	}
	runtime.Gosched()
	if delta := runtime.NumGoroutine() - baselineGoroutines; delta > 4 {
		t.Fatalf("timed-out WaitForIdle leaked waiter goroutines: delta=%d", delta)
	}
	close(anchorer.release)

	drainedCtx, cancelDrained := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDrained()
	if err := o.WaitForIdle(drainedCtx); err != nil {
		t.Fatalf("wait for anchor and follow-up grading workers: %v", err)
	}
	final, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil || final.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("drained orchestrator must persist terminal state: stage=%v err=%v", final.Record.Status, err)
	}
}

func TestGradingOrchestratorShutdownTracksRecoveryAndRejectsPostSealScan(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, orchestratorSnapshot, WithGradingRunDir(t.TempDir())))

	conn, err := d.Records.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	recoveryDone := make(chan error, 1)
	go func() {
		_, recoverErr := o.RecoverGradingJobs(context.Background(), []string{"mingming"})
		recoveryDone <- recoverErr
	}()
	deadline := time.Now().Add(time.Second)
	for {
		o.mu.Lock()
		workers := o.workerCount
		o.mu.Unlock()
		if workers == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovery was not registered as a tracked worker")
		}
		runtime.Gosched()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := o.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown recovery scan: %v", err)
	}
	select {
	case err := <-recoveryDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("recovery cancellation error=%v", err)
		}
	default:
		t.Fatal("Shutdown returned before recovery exited")
	}
	if _, err := o.RecoverGradingJobs(context.Background(), []string{"mingming"}); !errors.Is(err, ErrGradingOrchestratorShutdown) {
		t.Fatalf("post-seal recovery error=%v, want ErrGradingOrchestratorShutdown", err)
	}
}

func TestGradingOrchestratorShutdownSealsCancelsAndDrainsAsyncGrading(t *testing.T) {
	recognizer := &cancelBlockingRecognizer{started: make(chan struct{}), done: make(chan struct{})}
	o := newParallelAnchorOrchestrator(t, recognizer, nil)
	jobID := startOrchestratorJob(t, o, "msg-shutdown-grading").Record.RecordID
	if accepted := o.StartAsync(jobID); !accepted {
		t.Fatal("running orchestrator must accept asynchronous grading")
	}
	select {
	case <-recognizer.started:
	case <-time.After(time.Second):
		t.Fatal("recognizer worker did not start")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := o.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown grading worker: %v", err)
	}
	select {
	case <-recognizer.done:
	default:
		t.Fatal("Shutdown returned before the canceled recognizer exited")
	}
	if accepted := o.StartAsync(jobID); accepted {
		t.Fatal("sealed orchestrator accepted work after Shutdown")
	}
	view, err := o.deps.GetGradingJob(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Record.Status != k12.GradingStageOutcomeUnknown {
		t.Fatalf("sent provider call canceled by shutdown must be outcome_unknown, got %s", view.Record.Status)
	}
	invocations, err := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if err != nil || len(invocations) != 1 {
		t.Fatalf("list canceled invocation: count=%d err=%v", len(invocations), err)
	}
	if invocations[0].Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf("canceled sent invocation status=%s, want outcome_unknown", invocations[0].Status)
	}
}

func TestGradingOrchestratorShutdownCancelsAndDrainsAnchor(t *testing.T) {
	anchorer := &blockingGradingAnchorer{started: make(chan struct{}), release: make(chan struct{})}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, recognizer, anchorer)
	jobID := startOrchestratorJob(t, o, "msg-shutdown-anchor").Record.RecordID
	if _, err := o.RunGradingJob(context.Background(), jobID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-anchorer.started:
	case <-time.After(time.Second):
		t.Fatal("anchor worker did not start")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := o.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown anchor worker: %v", err)
	}
	invocations, err := o.deps.Records.ListModelInvocations(context.Background(), "mingming", jobID)
	if err != nil {
		t.Fatal(err)
	}
	foundUnknownAnchor := false
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageLocating && invocation.Status == k12.ModelInvocationOutcomeUnknown {
			foundUnknownAnchor = true
		}
	}
	if !foundUnknownAnchor {
		t.Fatalf("shutdown-canceled anchor must retain outcome_unknown ledger evidence: %+v", invocations)
	}
}

func TestGradingOrchestratorAnchorProviderBudgetStartsAfterSlowLedger(t *testing.T) {
	const providerBudget = 200 * time.Millisecond
	const ledgerDelay = 120 * time.Millisecond
	anchorer := &remainingBudgetAnchorer{remaining: make(chan time.Duration, 1)}
	recognizer := &countingRecognizer{questions: []RecognizedQuestion{{
		Question: "1+1=", Subject: "数学", StudentAnswer: "3", AnswerState: AnswerStatePresent,
	}}}
	o := newParallelAnchorOrchestrator(t, recognizer, anchorer, WithGradingAnchorTimeout(providerBudget))
	job := startOrchestratorJob(t, o, "msg-slow-ledger-anchor")

	conn, err := o.deps.Records.DB().Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, _, _, _ = o.executeAnchor(job.Record.RecordID, "mingming", []byte("image"),
			[]RecognizedQuestion{{Question: "1+1=", StudentAnswer: "3", AnswerState: AnswerStatePresent}},
			orchestratorSnapshot())
		result <- nil
	}()
	timer := time.NewTimer(ledgerDelay)
	<-timer.C
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case remaining := <-anchorer.remaining:
		if remaining < providerBudget-ledgerDelay/3 {
			t.Fatalf("slow ledger consumed provider timeout: remaining=%s budget=%s", remaining, providerBudget)
		}
	case <-time.After(time.Second):
		t.Fatal("anchor provider was not called after releasing the ledger connection")
	}
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("executeAnchor did not return")
	}
}

func TestGradingJoinRequiresConfirmedAndTerminalAnchorState(t *testing.T) {
	for _, tc := range []struct {
		name         string
		confirmation string
		anchor       string
		want         bool
	}{
		{name: "confirmed-located", confirmation: k12.GradingConfirmationConfirmed, anchor: k12.GradingAnchorLocated, want: true},
		{name: "confirmed-degraded", confirmation: k12.GradingConfirmationConfirmed, anchor: k12.GradingAnchorDegraded, want: true},
		{name: "pending-located", confirmation: k12.GradingConfirmationPending, anchor: k12.GradingAnchorLocated},
		{name: "confirmed-pending", confirmation: k12.GradingConfirmationConfirmed, anchor: k12.GradingAnchorPending},
		{name: "confirmed-unknown", confirmation: k12.GradingConfirmationConfirmed, anchor: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gradingJoinReady(k12.GradingJobFields{
				ConfirmationState: tc.confirmation,
				AnchorState:       tc.anchor,
			})
			if got != tc.want {
				t.Fatalf("gradingJoinReady=%v want %v", got, tc.want)
			}
		})
	}
}
