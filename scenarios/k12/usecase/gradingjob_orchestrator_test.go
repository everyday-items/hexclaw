package usecase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 编排器契约（架构设计 §6.7 状态机推进 / §6.15 单机执行模型·二阶段）：
// RunGradingJob 按 Job 当前 stage 顺序推进真实批改流程，只调用现有用例公开入口，
// 每阶段产物摘要经 AdvanceGradingStage 写入 stage_checkpoints，失败按 retryable 语义收敛。

// --- 编排器专用替身（pipeline_test.go / photo_grade_test.go 替身的计数扩展）---

// countingRecognizer 前 failures 次调用返回已收到 HTTP 503 的确定性下游错误，
// 之后成功；calls 计数用于断言「检查点恢复不重复调用识别模型」（§6.7 规则 3）。
type countingRecognizer struct {
	questions []RecognizedQuestion
	failures  int
	calls     int
}

func (r *countingRecognizer) Recognize(context.Context, []byte) ([]RecognizedQuestion, error) {
	r.calls++
	if r.calls <= r.failures {
		return nil, &gradingProviderResponseError{status: 503}
	}
	questions := cloneRecognizedQuestions(r.questions)
	for i := range questions {
		if len(questions[i].KnowledgePoints) == 0 {
			questions[i].KnowledgePoints = []string{"整数加法"}
		}
	}
	return questions, nil
}

type failingAnnotator struct{ calls int }

func (f *failingAnnotator) Annotate(context.Context, []byte, []PhotoAnnotation) (RenderedPhoto, error) {
	f.calls++
	return RenderedPhoto{}, errors.New("coordinate compositor crashed")
}

func orchestratorSnapshot() k12.GradingModelSnapshot {
	return k12.GradingModelSnapshot{Provider: "openrouter", Model: "test-vlm", Capability: "vision"}
}

func orchestratorSnapshotResolver(requested k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
	if strings.TrimSpace(requested.Provider) != "" || strings.TrimSpace(requested.Model) != "" {
		return k12.NormalizeGradingModelSnapshot(requested), nil
	}
	return orchestratorSnapshot(), nil
}

type gradingProviderResponseError struct {
	status int
}

func (e *gradingProviderResponseError) Error() string {
	return fmt.Sprintf("provider returned HTTP %d", e.status)
}

func (e *gradingProviderResponseError) ProviderResponseStatusCode() int {
	return e.status
}

type providerErrorRecognizer struct {
	err   error
	calls int
}

func (r *providerErrorRecognizer) Recognize(context.Context, []byte) ([]RecognizedQuestion, error) {
	r.calls++
	return nil, r.err
}

func newOrchestrator(t *testing.T, rec Recognizer, anchorer AnswerAnchorer, annotator PhotoAnnotator) *GradingOrchestrator {
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
	// GradingJob deadlines are absolute Unix timestamps. This helper exercises
	// provider boundaries, so it must use a wall clock rather than newPipeline's
	// deterministic domain-record clock (1000).
	d.Now = func() int64 { return time.Now().Unix() }
	return trackGradingOrchestrator(t, NewGradingOrchestrator(d, orchestratorSnapshotResolver))
}

func orchestratorTestBudget() k12.GradingBudgetSnapshot {
	return k12.GradingBudgetSnapshot{
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
}

func trackGradingOrchestrator(t *testing.T, o *GradingOrchestrator) *GradingOrchestrator {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := o.Shutdown(ctx); err != nil {
			t.Errorf("shutdown grading orchestrator before test database cleanup: %v", err)
		}
	})
	return o
}

func orchestratorPhotoRequest() PhotoGradeRequest {
	return PhotoGradeRequest{
		AgentName: "mingming", Grade: "五年级上", SourceSession: "dt-1",
		Image: []byte("inbound jpeg bytes"),
	}
}

func startOrchestratorJob(t *testing.T, o *GradingOrchestrator, sourceKey string) GradingJobView {
	t.Helper()
	v, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "im", SourceKey: sourceKey,
	})
	if err != nil || !created {
		t.Fatalf("StartPhotoGradingJob: %v created=%v", err, created)
	}
	return v
}

func checkpointStages(v GradingJobView) map[string]int {
	stages := map[string]int{}
	for _, cp := range v.Fields.StageCheckpoints {
		stages[cp.Stage]++
	}
	return stages
}

// TestGradingOrchestratorFullChainCompletes 全链推进：Start → Run 立即停在 awaiting_confirmation
// （锚点独立回位 located）→ ConfirmAndRun → completed，检查点齐全，识别/锚点模型各只调一次，
// 最终产物（批改图 + Markdown）可取。
func TestGradingOrchestratorFullChainCompletes(t *testing.T) {
	ctx := context.Background()
	rec := &countingRecognizer{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "3"},
	}}
	anchorer := &photoAnchorerFake{boxes: map[int]BBox{0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05}}}
	annotator := &photoAnnotatorFake{}
	o := newOrchestrator(t, rec, anchorer, annotator)

	v := startOrchestratorJob(t, o, "msg-1")
	jobID := v.Record.RecordID

	v, err := o.RunGradingJob(ctx, jobID)
	if err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	if v.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("应停在 awaiting_confirmation, got %s", v.Record.Status)
	}
	if v.Fields.ConfirmationState != k12.GradingConfirmationPending {
		t.Fatalf("停点确认态应 pending: %s", v.Fields.ConfirmationState)
	}
	if v.Fields.AnchorState != k12.GradingAnchorPending {
		t.Fatalf("主链不得等待锚点分支: %s", v.Fields.AnchorState)
	}
	if _, ok := o.PhotoResult(jobID); ok {
		t.Fatalf("确认前不得有最终批改产物")
	}
	v = waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Fields.AnchorState == k12.GradingAnchorLocated
	})

	v, err = o.ConfirmAndRun(ctx, jobID, nil)
	if err != nil {
		t.Fatalf("ConfirmAndRun: %v", err)
	}
	if v.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("确认后应推进到 completed, got %s", v.Record.Status)
	}
	stages := checkpointStages(v)
	for _, want := range []string{
		k12.GradingStageNormalizing, k12.GradingStageRecognizing, k12.GradingStageLocating,
		k12.GradingStageAwaitingConfirmation, k12.GradingStageAssessing,
		k12.GradingStageRendering, k12.GradingStageProjecting,
	} {
		if stages[want] == 0 {
			t.Errorf("缺少阶段检查点 %s: %v", want, v.Fields.StageCheckpoints)
		}
	}
	if rec.calls != 1 {
		t.Fatalf("识别模型应只调 1 次（assessing 复用识别检查点产物）, got %d", rec.calls)
	}
	if anchorer.calls != 1 {
		t.Fatalf("锚点模型应只调 1 次, got %d", anchorer.calls)
	}
	if annotator.calls != 1 {
		t.Fatalf("批注渲染应调 1 次, got %d", annotator.calls)
	}
	result, ok := o.PhotoResult(jobID)
	if !ok {
		t.Fatalf("completed 后应可取批改产物")
	}
	if result.AnnotatedImage == nil || len(result.AnnotatedImage.Data) == 0 {
		t.Fatalf("有可信 bbox 时应产出批改图: %#v", result.AnnotatedImage)
	}
	if len(result.Items) != 1 || result.Items[0].Status != PhotoWrong {
		t.Fatalf("判错题应保持 PhotoWrong: %#v", result.Items)
	}
	if result.Markdown == "" {
		t.Fatalf("最终 Markdown 不可空")
	}
}

// Once the durable Job reaches the confirmation stop, its already-persisted
// recognition projection must stay readable even if the in-process runner still
// owns the execution lock for a few instructions. HTTP polling observes the
// durable stage independently and must never receive awaiting_confirmation
// without the facts required by the confirmation form.
func TestGradingOrchestratorRecognitionReadableWhileExecutionLockIsHeld(t *testing.T) {
	ctx := context.Background()
	rec := &countingRecognizer{questions: []RecognizedQuestion{
		{Question: "3.8×3=?", Subject: "数学", StudentAnswer: "10.4"},
	}}
	o := newOrchestrator(t, rec, &photoAnchorerFake{}, &photoAnnotatorFake{})

	v := startOrchestratorJob(t, o, "recognition-projection-lock")
	jobID := v.Record.RecordID
	v, err := o.RunGradingJob(ctx, jobID)
	if err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	if v.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("应停在 awaiting_confirmation, got %s", v.Record.Status)
	}

	l := o.jobLock(jobID)
	l.Lock()
	questions, ok := o.RecognizedQuestionsForOwner(ctx, "mingming", jobID)
	l.Unlock()
	if !ok || len(questions) != 1 || questions[0].Question != "3.8×3=?" {
		t.Fatalf("confirmation stop must expose durable recognition while execution lock is held: ok=%v questions=%#v", ok, questions)
	}
}

// TestGradingOrchestratorRenderFailureDegrades 规则 2：批注渲染失败 → 降级续跑 projecting
// 直至 completed（不进失败态），rendering 检查点标 degraded，文字产物仍可投递。
func TestGradingOrchestratorRenderFailureDegrades(t *testing.T) {
	ctx := context.Background()
	rec := &countingRecognizer{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "3"},
	}}
	anchorer := &photoAnchorerFake{boxes: map[int]BBox{0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05}}}
	annotator := &failingAnnotator{}
	o := newOrchestrator(t, rec, anchorer, annotator)

	v := startOrchestratorJob(t, o, "msg-render-fail")
	jobID := v.Record.RecordID
	if _, err := o.RunGradingJob(ctx, jobID); err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Fields.AnchorState == k12.GradingAnchorLocated
	})
	v, err := o.ConfirmAndRun(ctx, jobID, nil)
	if err != nil {
		t.Fatalf("渲染失败必须降级续跑而非报错: %v", err)
	}
	if v.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("渲染失败降级后应 completed, got %s", v.Record.Status)
	}
	degraded := false
	for _, cp := range v.Fields.StageCheckpoints {
		if cp.Stage == k12.GradingStageRendering && cp.Degraded {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("rendering 降级应记录在检查点: %v", v.Fields.StageCheckpoints)
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || result.Markdown == "" {
		t.Fatalf("降级后文字批改产物仍须可投递: ok=%v", ok)
	}
	if result.AnnotatedImage != nil {
		t.Fatalf("渲染失败不得伪造批改图: %#v", result.AnnotatedImage)
	}
}

// TestGradingOrchestratorRecognizeFailureRetryResumes 识别失败 → failed_retryable；
// RetryAndRun 从最近检查点续跑（不重跑 normalizing），二次识别成功后完整走完。
func TestGradingOrchestratorRecognizeFailureRetryResumes(t *testing.T) {
	ctx := context.Background()
	rec := &countingRecognizer{failures: 1, questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "3"},
	}}
	anchorer := &photoAnchorerFake{boxes: map[int]BBox{0: {X: 0.2, Y: 0.3, W: 0.1, H: 0.05}}}
	o := newOrchestrator(t, rec, anchorer, &photoAnnotatorFake{})

	v := startOrchestratorJob(t, o, "msg-flaky")
	jobID := v.Record.RecordID

	v, err := o.RunGradingJob(ctx, jobID)
	if err == nil {
		t.Fatalf("识别失败应把下游错误抛回入口（保持与旧路径一致的错误面）")
	}
	if v.Record.Status != k12.GradingStageFailedRetryable {
		t.Fatalf("识别失败应落 failed_retryable, got %s", v.Record.Status)
	}
	if !v.Fields.Retryable || v.Fields.FailedStage != k12.GradingStageRecognizing {
		t.Fatalf("失败语义应可安全重试且记录失败阶段: %+v", v.Fields)
	}

	v, err = o.RetryAndRun(ctx, jobID)
	if err != nil {
		t.Fatalf("RetryAndRun: %v", err)
	}
	if v.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("重试续跑应到 awaiting_confirmation, got %s", v.Record.Status)
	}
	waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Fields.AnchorState == k12.GradingAnchorLocated
	})
	if _, err = o.ConfirmAndRun(ctx, jobID, nil); err != nil {
		t.Fatalf("ConfirmAndRun: %v", err)
	}
	final, err := o.deps.GetGradingJob(ctx, "mingming", jobID)
	if err != nil || final.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("重试后应完整走完: %v %s", err, final.Record.Status)
	}
	if got := checkpointStages(final)[k12.GradingStageNormalizing]; got != 1 {
		t.Fatalf("重试不得重跑已成功的 normalizing（规则 3）, 检查点数=%d", got)
	}
	if rec.calls != 2 {
		t.Fatalf("识别应恰好调 2 次（1 失败 + 1 重试成功）, got %d", rec.calls)
	}
}

func TestStartPhotoGradingJobFailsBeforePersistWhenSnapshotResolverRejects(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return k12.GradingModelSnapshot{}, errors.New("no text+vision model")
		},
	))

	_, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "no-vision",
	})
	if !errors.Is(err, ErrInvalidInput) || created {
		t.Fatalf("start err=%v created=%v, want ErrInvalidInput before persistence", err, created)
	}
	jobs, listErr := d.ListGradingJobs(context.Background(), "mingming", "")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(jobs) != 0 {
		t.Fatalf("resolver failure persisted %d jobs", len(jobs))
	}
}

func TestStartPhotoGradingJobRejectsIdempotencySnapshotConflict(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d,
		func(requested k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return k12.NormalizeGradingModelSnapshot(requested), nil
		},
	))
	policy := k12.ApprovedRecognizingRequestPolicy()
	firstSnapshot := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Capability:               "vision",
		RecognizingRequestPolicy: policy,
	}
	first, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "same-key",
		ModelSnapshot: firstSnapshot,
	})
	if err != nil || !created {
		t.Fatalf("first start err=%v created=%v", err, created)
	}

	_, created, err = o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "same-key",
		ModelSnapshot: k12.GradingModelSnapshot{
			Provider: "hexclaw-gpt", Model: "another-vision", Capability: "vision",
		},
	})
	if !errors.Is(err, ErrInvalidInput) || created {
		t.Fatalf("route conflict err=%v created=%v, want ErrInvalidInput", err, created)
	}
	stored, err := d.GetGradingJob(context.Background(), "mingming", first.Record.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Fields.ModelSnapshot.Model != "gpt-5.6-sol" {
		t.Fatalf("idempotency conflict overwrote frozen route: %+v", stored.Fields.ModelSnapshot)
	}
}

func TestStartPhotoGradingJobSameRouteReplayKeepsFrozenSnapshotMetadata(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d,
		func(requested k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return k12.NormalizeGradingModelSnapshot(requested), nil
		},
	))
	policy := k12.ApprovedRecognizingRequestPolicy()
	firstSnapshot := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Capability:               "vision",
		TimeoutMS:                30_000,
		RecognizingRequestPolicy: policy,
	}
	first, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "same-route-replay",
		ModelSnapshot: firstSnapshot,
	})
	if err != nil || !created {
		t.Fatalf("first start err=%v created=%v", err, created)
	}

	replayed, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "same-route-replay",
		ModelSnapshot: k12.GradingModelSnapshot{
			Provider:                 "hexclaw-gpt",
			Model:                    k12.RecognizingPolicyModel,
			Capability:               "vision",
			TimeoutMS:                60_000,
			RecognizingRequestPolicy: policy,
		},
	})
	if err != nil || created || replayed.Record.RecordID != first.Record.RecordID {
		t.Fatalf("same route replay err=%v created=%v id=%s want=%s", err, created, replayed.Record.RecordID, first.Record.RecordID)
	}
	if replayed.Fields.ModelSnapshot.TimeoutMS != firstSnapshot.TimeoutMS {
		t.Fatalf("replay rewrote frozen snapshot metadata: got=%+v want=%+v", replayed.Fields.ModelSnapshot, firstSnapshot)
	}
}

func TestStartPhotoGradingJobReplayUsesFrozenRouteBeforeMutableResolver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		replay k12.GradingModelSnapshot
	}{
		{name: "empty request"},
		{name: "auto request", replay: k12.GradingModelSnapshot{Model: "auto"}},
		{
			name: "same explicit route after model removal",
			replay: k12.GradingModelSnapshot{
				Provider: "provider-a", Model: "vision-a", Route: "provider-a/vision-a",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newPipeline(t,
				fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
				fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
			resolverCalls := 0
			modelAvailable := true
			current := k12.GradingModelSnapshot{
				Provider: "provider-a", Model: "vision-a", Route: "provider-a/vision-a", Capability: "vision",
			}
			o := trackGradingOrchestrator(t, NewGradingOrchestrator(d,
				func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
					resolverCalls++
					if !modelAvailable {
						return k12.GradingModelSnapshot{}, errors.New("configured model was removed")
					}
					return current, nil
				},
			))
			first, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
				Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "frozen-replay",
			})
			if err != nil || !created {
				t.Fatalf("first start err=%v created=%v", err, created)
			}
			if resolverCalls != 1 {
				t.Fatalf("first start resolver calls=%d want=1", resolverCalls)
			}

			// Both a changed default and a removed/disabled catalog entry must be
			// irrelevant to an exact idempotent replay of the persisted Job.
			current = k12.GradingModelSnapshot{
				Provider: "provider-b", Model: "vision-b", Route: "provider-b/vision-b", Capability: "vision",
			}
			modelAvailable = false
			replayed, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
				Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "frozen-replay",
				ModelSnapshot: tc.replay,
			})
			if err != nil || created {
				t.Fatalf("replay err=%v created=%v", err, created)
			}
			if replayed.Record.RecordID != first.Record.RecordID {
				t.Fatalf("replay record=%s want=%s", replayed.Record.RecordID, first.Record.RecordID)
			}
			if replayed.Fields.ModelSnapshot.Route != "provider-a/vision-a" {
				t.Fatalf("replay route=%+v want frozen provider-a/vision-a", replayed.Fields.ModelSnapshot)
			}
			if resolverCalls != 1 {
				t.Fatalf("idempotent replay consulted mutable resolver: calls=%d want=1", resolverCalls)
			}
		})
	}
}

func TestStartPhotoGradingJobReplayValidatesIdentityBeforeMutableResolver(t *testing.T) {
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	resolverCalls := 0
	modelAvailable := true
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d,
		func(requested k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			resolverCalls++
			if !modelAvailable {
				return k12.GradingModelSnapshot{}, errors.New("configured model was removed")
			}
			if strings.TrimSpace(requested.Provider) == "" {
				return k12.GradingModelSnapshot{
					Provider: "provider-a", Model: "vision-a", Route: "provider-a/vision-a",
				}, nil
			}
			return k12.NormalizeGradingModelSnapshot(requested), nil
		},
	))
	if _, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "identity-before-resolver",
	}); err != nil || !created {
		t.Fatalf("first start err=%v created=%v", err, created)
	}
	modelAvailable = false

	changedPhoto := orchestratorPhotoRequest()
	changedPhoto.Image = []byte("different image bytes")
	if _, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: changedPhoto, SourceKind: "desktop", SourceKey: "identity-before-resolver",
	}); !errors.Is(err, ErrInvalidInput) || created || !strings.Contains(err.Error(), "submission") {
		t.Fatalf("submission conflict err=%v created=%v, want immutable submission rejection", err, created)
	}
	if resolverCalls != 1 {
		t.Fatalf("submission conflict consulted mutable resolver: calls=%d want=1", resolverCalls)
	}

	if _, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "identity-before-resolver",
		ModelSnapshot: k12.GradingModelSnapshot{
			Provider: "provider-b", Model: "vision-b", Route: "provider-b/vision-b",
		},
	}); !errors.Is(err, ErrInvalidInput) || created || !strings.Contains(err.Error(), "model route") {
		t.Fatalf("route conflict err=%v created=%v, want frozen route rejection", err, created)
	}
	if resolverCalls != 1 {
		t.Fatalf("route conflict consulted mutable resolver: calls=%d want=1", resolverCalls)
	}
}

func TestGradingProvider400IsTerminalWithoutRetry(t *testing.T) {
	rec := &providerErrorRecognizer{err: &gradingProviderResponseError{status: 400}}
	o := newOrchestrator(t, rec, nil, nil)
	started := startOrchestratorJob(t, o, "provider-400")

	failed, err := o.RunGradingJob(context.Background(), started.Record.RecordID)
	if err == nil {
		t.Fatal("provider 400 should be returned")
	}
	if failed.Record.Status != k12.GradingStageFailedTerminal ||
		failed.Fields.Retryable ||
		failed.Fields.AttemptCount != 1 {
		t.Fatalf("provider 400 job=%+v fields=%+v", failed.Record, failed.Fields)
	}
	if _, retryErr := o.RetryAndRun(context.Background(), started.Record.RecordID); retryErr == nil {
		t.Fatal("permanent provider 400 exposed retry")
	}
	if rec.calls != 1 {
		t.Fatalf("provider 400 called recognizer %d times, want 1", rec.calls)
	}
}

func TestGradingProvider429RemainsRetryable(t *testing.T) {
	rec := &providerErrorRecognizer{err: &gradingProviderResponseError{status: 429}}
	o := newOrchestrator(t, rec, nil, nil)
	started := startOrchestratorJob(t, o, "provider-429")

	failed, err := o.RunGradingJob(context.Background(), started.Record.RecordID)
	if err == nil {
		t.Fatal("provider 429 should be returned")
	}
	if failed.Record.Status != k12.GradingStageFailedRetryable ||
		!failed.Fields.Retryable ||
		failed.Fields.AttemptCount != 1 {
		t.Fatalf("provider 429 job=%+v fields=%+v", failed.Record, failed.Fields)
	}
}

// TestGradingOrchestratorAnchorAbsentDegrades 无锚点能力时锚点分支显式 degraded
// （§4.9 文字降级），不阻塞确认与批改，最终 completed 且只出文字结果。
func TestGradingOrchestratorAnchorAbsentDegrades(t *testing.T) {
	ctx := context.Background()
	rec := &countingRecognizer{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "3"},
	}}
	o := newOrchestrator(t, rec, nil, &photoAnnotatorFake{})

	v := startOrchestratorJob(t, o, "msg-no-anchor")
	jobID := v.Record.RecordID
	v, err := o.RunGradingJob(ctx, jobID)
	if err != nil {
		t.Fatalf("RunGradingJob: %v", err)
	}
	if v.Record.Status != k12.GradingStageAwaitingConfirmation || v.Fields.AnchorState != k12.GradingAnchorPending {
		t.Fatalf("主链应先返回确认点且不等待能力降级回位: stage=%s anchor=%s", v.Record.Status, v.Fields.AnchorState)
	}
	v = waitGradingView(t, o, jobID, func(v GradingJobView) bool {
		return v.Fields.AnchorState == k12.GradingAnchorDegraded
	})
	v, err = o.ConfirmAndRun(ctx, jobID, nil)
	if err != nil || v.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("degraded 不得阻塞批改: %v %s", err, v.Record.Status)
	}
	result, ok := o.PhotoResult(jobID)
	if !ok || result.AnnotatedImage != nil {
		t.Fatalf("无坐标降级只出文字结果，不得伪造批改图: ok=%v %#v", ok, result.AnnotatedImage)
	}
}

// TestGradingOrchestratorIdempotentStart §4.10：同一 IM message_id 重复投递只产生一个 Job，
// 且运行时登记不被覆盖。
func TestGradingOrchestratorIdempotentStart(t *testing.T) {
	ctx := context.Background()
	rec := &countingRecognizer{questions: []RecognizedQuestion{
		{Question: "1+1=", Subject: "数学", StudentAnswer: "3"},
	}}
	o := newOrchestrator(t, rec, nil, nil)

	v1 := startOrchestratorJob(t, o, "msg-dup")
	v2, created2, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "im", SourceKey: "msg-dup",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created2 || v2.Record.RecordID != v1.Record.RecordID {
		t.Fatalf("同幂等键应命中既有 Job: created=%v id=%s want %s", created2, v2.Record.RecordID, v1.Record.RecordID)
	}
}

func TestStartPhotoGradingJobSameKeyDifferentImageFailsClosed(t *testing.T) {
	ctx := context.Background()
	o := newOrchestrator(t, &countingRecognizer{}, nil, nil)

	first, firstCreated, firstErr := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "desktop-request-immutable",
	})
	if firstErr != nil || !firstCreated {
		t.Fatalf("first StartPhotoGradingJob: created=%v err=%v", firstCreated, firstErr)
	}
	changed := orchestratorPhotoRequest()
	changed.Image = []byte("different inbound jpeg bytes")
	got, created, err := o.StartPhotoGradingJob(ctx, StartPhotoGradingInput{
		Photo: changed, SourceKind: "desktop", SourceKey: "desktop-request-immutable",
	})
	if !errors.Is(err, ErrInvalidInput) || created {
		t.Fatalf("same request_id with different image must fail closed: got=%+v created=%v err=%v", got, created, err)
	}

	stored, getErr := o.deps.GetGradingJob(ctx, "mingming", first.Record.RecordID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if stored.Fields.SubmissionID != first.Fields.SubmissionID {
		t.Fatalf("identity conflict overwrote immutable submission: got=%s want=%s", stored.Fields.SubmissionID, first.Fields.SubmissionID)
	}
}
