package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type recognitionLayoutFinalizedReplayMarkerProbe struct {
	calls      int
	marked     bool
	contextErr error
}

type recognitionLayoutFinalizedProjectionProbe struct {
	questions []RecognizedQuestion
	calls     int
	marked    bool
	replayed  bool
}

func (p *recognitionLayoutFinalizedProjectionProbe) Recognize(
	ctx context.Context,
	_ []byte,
) ([]RecognizedQuestion, error) {
	p.calls++
	p.marked = k12.RecognitionLayoutFinalizationReplayV2Enabled(ctx)
	if !p.marked {
		return nil, errors.New("finalized projection entered without replay marker")
	}
	_, replayed, err := k12.ReplayFinalizedRecognitionLayoutPlanV2(ctx)
	p.replayed = replayed
	if err != nil || !replayed {
		return nil, errors.Join(
			errors.New("finalized projection did not replay its durable receipt"),
			err,
		)
	}
	return cloneRecognizedQuestions(p.questions), nil
}

func (p *recognitionLayoutFinalizedReplayMarkerProbe) Recognize(
	ctx context.Context,
	_ []byte,
) ([]RecognizedQuestion, error) {
	p.calls++
	p.marked = k12.RecognitionLayoutFinalizationReplayV2Enabled(ctx)
	p.contextErr = ctx.Err()
	return nil, ErrRecognitionPhysicalCallObservedInFlight
}

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-001：Store 提交 V2 最终化回执后，
// 即使原阶段截止时间已经过去，重启识别阶段也必须进入显式只读重放路径。
// 标记应在 Recognizer 边界前设置；解码由适配器负责，且不得再次发送 Provider 请求。
func TestREGK12RecognitionDurabilityBudget20260808001OrchestratorMarksFinalizedV2Replay(
	t *testing.T,
) {
	ctx := context.Background()
	currentUnix := time.Now().Unix()
	policy := k12.ApprovedRecognizingRequestPolicy()
	route := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision",
		RecognizingRequestPolicy: policy,
	}
	page := recognitionLayoutInitialV2PagePNG(t)
	deps, _ := newPipeline(
		t,
		fakeSolver{
			solution: "4",
			ev: SolveEvidence{
				Verdict:      VerdictAgree,
				EvidenceType: EvidenceNumericExec,
			},
		},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		nil,
	)
	deps.Now = func() int64 { return currentUnix }
	budget := recognitionLayoutInitialV2Budget()
	budget.RecognizingBuckets = k12.RecognitionLayoutBudgetBucketsV2{
		UpTo1ProblemMillis:   5_000,
		UpTo8ProblemsMillis:  5_000,
		UpTo16ProblemsMillis: 5_000,
		UpTo32ProblemsMillis: 5_000,
	}
	budget.StageSeconds.Recognizing = 5
	deps.GradingBudgetSnapshot = budget
	probe := &recognitionLayoutFinalizedReplayMarkerProbe{}
	deps.Recognizer = probe
	runDir := t.TempDir()
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return route, nil
		},
		WithGradingRunDir(runDir),
	))
	job, created, err := orchestrator.StartPhotoGradingJob(
		ctx,
		StartPhotoGradingInput{
			Photo: PhotoGradeRequest{
				AgentName:     "mingming",
				Grade:         "五年级上",
				SourceSession: "layout-v2-finalized-replay-session",
				Image:         page,
			},
			SourceKind: "desktop",
			SourceKey:  "layout-v2-finalized-replay",
		},
	)
	if err != nil || !created {
		t.Fatalf("start finalized replay fixture: created=%v err=%v", created, err)
	}
	job, err = deps.AdvanceGradingStage(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
		AdvanceGradingInput{Outcome: GradingOutcomeOK},
	)
	if err != nil || job.Record.Status != k12.GradingStageNormalizing {
		t.Fatalf("advance to normalizing: job=%+v err=%v", job, err)
	}
	job, err = deps.AdvanceGradingStage(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
		AdvanceGradingInput{
			Outcome:        GradingOutcomeOK,
			ArtifactDigest: "normalized:v2-finalized-replay",
		},
	)
	if err != nil || job.Record.Status != k12.GradingStageRecognizing {
		t.Fatalf("advance to recognizing: job=%+v err=%v", job, err)
	}
	requestDigest := recognizingInvocationDigest(
		page,
		job.Fields.ModelSnapshot,
		policy,
	)
	parent, err := orchestrator.beginRecognizingModelInvocationWithPolicy(
		ctx,
		job,
		page,
		requestDigest,
		policy,
	)
	if err != nil {
		t.Fatalf("publish initial V2 manifest: %v", err)
	}
	runtime := finalizeRecognitionLayoutOrchestratorReplayFixtureV2(
		t,
		ctx,
		orchestrator,
		parent,
		page,
	)
	if runtime.Status != "succeeded" {
		t.Fatalf("layout runtime status=%q, want succeeded", runtime.Status)
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(ctx, 5*time.Second)
	if shutdownErr := orchestrator.Shutdown(shutdownCtx); shutdownErr != nil {
		cancelShutdown()
		t.Fatalf("shutdown pre-crash orchestrator: %v", shutdownErr)
	}
	cancelShutdown()
	restarted := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return route, nil
		},
		WithGradingRunDir(runDir),
	))
	run, err := restarted.ensureRun(ctx, job.Record.RecordID)
	if err != nil {
		t.Fatalf("recover finalized replay run after restart: %v", err)
	}

	// 持久化阶段现已过期。它必须阻止新的物理发送，但不能阻止只读最终化重放。
	if wait := time.Until(time.Unix(job.Fields.Deadline, 0)); wait > 0 {
		time.Sleep(wait + 50*time.Millisecond)
	}
	_, runErr := restarted.runRecognize(ctx, run, job.Record.RecordID)
	if !errors.Is(runErr, ErrRecognitionPhysicalCallObservedInFlight) {
		t.Fatalf("focused replay probe error=%v", runErr)
	}
	if probe.calls != 1 || !probe.marked ||
		!errors.Is(probe.contextErr, context.DeadlineExceeded) {
		t.Fatalf(
			"replay entry calls=%d marked=%v ctxErr=%v, want one marked expired read-only entry",
			probe.calls,
			probe.marked,
			probe.contextErr,
		)
	}
	afterParent, err := deps.Records.GetModelInvocation(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || afterParent.Status != k12.ModelInvocationSent {
		t.Fatalf("focused replay probe mutated parent=%+v err=%v", afterParent, err)
	}

	confidence := 0.99
	projection := &recognitionLayoutFinalizedProjectionProbe{
		questions: []RecognizedQuestion{
			{
				ProblemKind:           ProblemKindStandalone,
				SourceNumberPath:      []string{"1"},
				DisplayLabel:          "1",
				Question:              "2+2=?",
				CanonicalMarkdown:     "2+2=?",
				Subject:               "数学",
				KnowledgePoints:       []string{"加法"},
				AnswerState:           AnswerStatePresent,
				StudentAnswer:         "4",
				RecognitionConfidence: &confidence,
			},
		},
	}
	restarted.deps.Recognizer = projection
	view, runErr := restarted.runRecognize(ctx, run, job.Record.RecordID)
	if runErr != nil || view.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf(
			"project finalized replay: stage=%s err=%v",
			view.Record.Status,
			runErr,
		)
	}
	if projection.calls != 1 || !projection.marked || !projection.replayed {
		t.Fatalf("finalized projection probe=%+v", projection)
	}
	if len(run.questions) != 1 || run.questions[0].ProblemID == "" ||
		run.questions[0].Question != "2+2=?" {
		t.Fatalf("projected questions=%+v", run.questions)
	}
	finalParent, err := deps.Records.GetModelInvocation(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || finalParent.Status != k12.ModelInvocationSucceeded {
		t.Fatalf("finalized replay parent=%+v err=%v", finalParent, err)
	}
	receipt, ok := restarted.readRecognitionReceipt(job.Record.RecordID)
	if !ok || receipt.InvocationID != parent.InvocationID ||
		len(receipt.Questions) != 1 || len(receipt.PhysicalInvocations) != 2 {
		t.Fatalf("finalized replay receipt=%+v ok=%v", receipt, ok)
	}
	snapshot, err := deps.Records.GetProblemAttemptSnapshot(
		ctx,
		parent.AgentName,
		job.Fields.SubmissionID,
	)
	if err != nil || len(snapshot.Problems) != 1 || len(snapshot.Attempts) != 1 {
		t.Fatalf("finalized replay typed facts=%+v err=%v", snapshot, err)
	}
}

func finalizeRecognitionLayoutOrchestratorReplayFixtureV2(
	t *testing.T,
	ctx context.Context,
	orchestrator *GradingOrchestrator,
	parent k12.ModelInvocation,
	page []byte,
) k12.RecognitionLayoutPlanRuntimeV2 {
	t.Helper()
	runtime, err := orchestrator.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil {
		t.Fatalf("load initial layout runtime: %v", err)
	}
	canonicalPage, err := k12.CanonicalizeRecognitionPageV2(page)
	if err != nil {
		t.Fatalf("canonicalize finalized replay page: %v", err)
	}
	executor := newDurableRecognitionPhysicalCallExecutor(orchestrator, parent)
	durableCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(ctx, runtime.HeaderDigest),
		executor,
	)
	manifestCall := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  runtime.HeaderDigest,
		Unit:        k12.RecognitionPhysicalUnitWholePage,
		Image:       canonicalPage.PNG,
	}
	manifest, err := executor.ExecuteRecognitionPhysicalCall(
		durableCtx,
		manifestCall,
		func(context.Context) (string, error) {
			return `{"targets":"one"}`, nil
		},
	)
	if err != nil {
		t.Fatalf("execute finalized replay manifest: %v", err)
	}
	plan, err := k12.BuildRecognitionLayoutPlanV2(
		k12.RecognitionLayoutPlanInputV2{
			PagePNG: canonicalPage.PNG,
			Manifest: k12.RecognitionLayoutManifestSuccessV2{
				InvocationID: manifest.InvocationID,
				ResultDigest: manifest.ResultDigest,
			},
			Targets: []k12.RecognitionLayoutManifestTargetV2{
				{
					ManifestRef:      "manifest_0001",
					ManifestOrder:    1,
					SourceNumberPath: []string{"1"},
					DisplayLabel:     "1",
					Region: k12.SourcePixelRegion{
						X: 0, Y: 0, Width: 8, Height: 12,
					},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("build finalized replay plan: %v", err)
	}
	if authorizeErr := k12.AuthorizeRecognitionLayoutPlanV2(
		durableCtx,
		manifest,
		plan,
	); authorizeErr != nil {
		t.Fatalf("authorize finalized replay plan: %v", authorizeErr)
	}
	batch := plan.Batches[0]
	batchImage, err := k12.BuildRecognitionLayoutBatchImageV2(
		canonicalPage.PNG,
		plan,
		batch.Unit,
	)
	if err != nil {
		t.Fatalf("build finalized replay batch image: %v", err)
	}
	batchCall := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  plan.AuthorizedPlanDigest,
		Unit:        batch.Unit,
		TargetIDs:   append([]string(nil), batch.TargetIDs...),
		Image:       batchImage,
	}
	batchResult, err := executor.ExecuteRecognitionPhysicalCall(
		durableCtx,
		batchCall,
		func(context.Context) (string, error) {
			return `{"results":"one"}`, nil
		},
	)
	if err != nil {
		t.Fatalf("execute finalized replay batch: %v", err)
	}
	resultJSON := json.RawMessage(`{"display_label":"1","problem_kind":"standalone","question":"2+2=?","source_number_path":["1"]}`)
	settled, settlementCreated, err := k12.SettleRecognitionLayoutPrimaryBatchV2(
		durableCtx,
		batchResult,
		k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			SourcePhysicalInvocationID: batchResult.InvocationID,
			SourcePhysicalUnit:         batch.Unit,
			SourcePhysicalResultDigest: batchResult.ResultDigest,
			Classification:             k12.RecognitionLayoutBatchClassifiedV2,
			Candidates: []k12.RecognitionLayoutCandidateSettlementV2{
				{
					CandidateID:    batch.TargetIDs[0],
					Classification: k12.RecognitionLayoutCandidateValidV2,
					ResultKind:     k12.RecognitionLayoutCandidateQuestionV2,
					ResultJSON:     resultJSON,
				},
			},
		},
	)
	if err != nil || !settlementCreated || len(settled.FrozenResults) != 1 {
		t.Fatalf(
			"settle finalized replay batch: created=%v result=%+v err=%v",
			settlementCreated,
			settled,
			err,
		)
	}
	_, finalizationCreated, err := k12.FinalizeRecognitionLayoutPlanV2(durableCtx)
	if err != nil || !finalizationCreated {
		t.Fatalf("finalize replay fixture: created=%v err=%v", finalizationCreated, err)
	}
	finalized, err := orchestrator.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil {
		t.Fatalf("reload finalized layout runtime: %v", err)
	}
	return finalized
}
