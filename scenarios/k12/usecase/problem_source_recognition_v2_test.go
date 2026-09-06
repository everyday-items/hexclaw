package usecase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

var errProblemSourceRecognitionV2EntryProbe = errors.New(
	"problem source recognition v2 entry probe complete",
)

type problemSourceRecognitionV2EntryProbe struct {
	records *k12storage.Store
	work    k12storage.ProblemSourceReprocessJob

	calls         int
	policy        k12.ModelRequestPolicySnapshot
	policyEnabled bool
	headerDigest  string
	headerEnabled bool
	parent        k12.ModelInvocation
	runtime       k12.RecognitionLayoutPlanRuntimeV2
	entryErr      error
}

func (p *problemSourceRecognitionV2EntryProbe) Recognize(
	ctx context.Context,
	_ []byte,
) ([]RecognizedQuestion, error) {
	p.calls++
	p.policy, p.policyEnabled = k12.GradingModelRequestPolicyFromContext(ctx)
	p.headerDigest, p.headerEnabled =
		k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	p.parent, p.entryErr = p.records.GetModelInvocation(
		context.Background(),
		p.work.AgentName,
		stableProblemSourceRecognitionParentID(
			p.work.WorkID,
			p.work.AttemptCount,
		),
	)
	if p.entryErr == nil {
		p.runtime, p.entryErr = p.records.LoadRecognitionLayoutPlanRuntimeV2(
			context.Background(),
			p.parent.AgentName,
			p.parent.InvocationID,
		)
	}
	return nil, errProblemSourceRecognitionV2EntryProbe
}

type problemSourceRecognitionHarness struct {
	orchestrator *GradingOrchestrator
	job          GradingJobView
	work         k12storage.ProblemSourceReprocessJob
	page         []byte
}

type recognitionV2AuthorizedBatchSeed struct {
	executor   *durableRecognitionPhysicalCallExecutor
	durableCtx context.Context
	batchCall  k12.RecognitionPhysicalCall
}

func seedRecognitionV2AuthorizedBatch(
	t *testing.T,
	orchestrator *GradingOrchestrator,
	parent k12.ModelInvocation,
	page []byte,
) recognitionV2AuthorizedBatchSeed {
	t.Helper()
	ctx := context.Background()
	runtime, err := orchestrator.deps.Records.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil {
		t.Fatalf("load V2 exact-set seed runtime: %v", err)
	}
	canonicalPage, err := k12.CanonicalizeRecognitionPageV2(page)
	if err != nil {
		t.Fatalf("canonicalize V2 exact-set seed page: %v", err)
	}
	executor := newDurableRecognitionPhysicalCallExecutor(orchestrator, parent)
	durableCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(ctx, runtime.HeaderDigest),
		executor,
	)
	manifest, err := executor.ExecuteRecognitionPhysicalCall(
		durableCtx,
		k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  runtime.HeaderDigest,
			Unit:        k12.RecognitionPhysicalUnitWholePage,
			Image:       canonicalPage.PNG,
		},
		func(context.Context) (string, error) {
			return `{"targets":"one"}`, nil
		},
	)
	if err != nil {
		t.Fatalf("succeed V2 exact-set seed manifest: %v", err)
	}
	plan, err := k12.BuildRecognitionLayoutPlanV2(
		k12.RecognitionLayoutPlanInputV2{
			PagePNG: canonicalPage.PNG,
			Manifest: k12.RecognitionLayoutManifestSuccessV2{
				InvocationID: manifest.InvocationID,
				ResultDigest: manifest.ResultDigest,
			},
			Targets: []k12.RecognitionLayoutManifestTargetV2{{
				ManifestRef:      "manifest_0001",
				ManifestOrder:    1,
				SourceNumberPath: []string{"1"},
				DisplayLabel:     "1",
				Region: k12.SourcePixelRegion{
					X: 0, Y: 0, Width: 8, Height: 12,
				},
			}},
		},
	)
	if err != nil {
		t.Fatalf("build V2 exact-set seed plan: %v", err)
	}
	if authorizeErr := k12.AuthorizeRecognitionLayoutPlanV2(
		durableCtx,
		manifest,
		plan,
	); authorizeErr != nil {
		t.Fatalf("authorize V2 exact-set seed plan: %v", authorizeErr)
	}
	batch := plan.Batches[0]
	batchImage, err := k12.BuildRecognitionLayoutBatchImageV2(
		canonicalPage.PNG,
		plan,
		batch.Unit,
	)
	if err != nil {
		t.Fatalf("build V2 exact-set seed batch image: %v", err)
	}
	return recognitionV2AuthorizedBatchSeed{
		executor:   executor,
		durableCtx: durableCtx,
		batchCall: k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  plan.AuthorizedPlanDigest,
			Unit:        batch.Unit,
			TargetIDs:   append([]string(nil), batch.TargetIDs...),
			Image:       batchImage,
		},
	}
}

func seedRecognitionV2BatchTerminalState(
	t *testing.T,
	seed recognitionV2AuthorizedBatchSeed,
	status k12.ModelInvocationStatus,
) k12.ModelPhysicalInvocation {
	t.Helper()
	ctx := context.Background()
	parent := seed.executor.parent
	physicalID, err := stableRecognitionPhysicalInvocationIDForCall(
		parent.InvocationID,
		seed.batchCall,
	)
	if err != nil {
		t.Fatalf("identify V2 exact-set seed batch: %v", err)
	}
	requestDigest, err := recognizingPhysicalInvocationDigest(
		parent,
		seed.batchCall,
	)
	if err != nil {
		t.Fatalf("digest V2 exact-set seed batch: %v", err)
	}
	planVersion, candidateExactSetDigest, err :=
		recognitionPhysicalInvocationPlanProjection(seed.batchCall)
	if err != nil {
		t.Fatalf("project V2 exact-set seed batch: %v", err)
	}
	prepared, created, err := seed.executor.o.deps.Records.
		PrepareModelPhysicalInvocation(
			ctx,
			k12.ModelPhysicalInvocation{
				PhysicalInvocationID:    physicalID,
				ParentInvocationID:      parent.InvocationID,
				AgentName:               parent.AgentName,
				JobID:                   parent.JobID,
				Stage:                   parent.Stage,
				PhysicalUnit:            seed.batchCall.Unit,
				RecognitionPlanVersion:  planVersion,
				PlanDigest:              seed.batchCall.PlanDigest,
				CandidateExactSetDigest: candidateExactSetDigest,
				RequestDigest:           requestDigest,
				RouteSnapshot:           parent.RouteSnapshot,
				RequestPolicySnapshot:   parent.RequestPolicySnapshot,
				Attempt:                 1,
				CreatedAt:               seed.executor.o.deps.now(),
				UpdatedAt:               seed.executor.o.deps.now(),
			},
		)
	if err != nil || !created || prepared.Status != k12.ModelInvocationPrepared {
		t.Fatalf(
			"prepare V2 exact-set seed batch: created=%v child=%+v err=%v",
			created,
			prepared,
			err,
		)
	}
	sent, claimed, err := seed.executor.o.deps.Records.
		ClaimModelPhysicalInvocationSent(ctx, parent.AgentName, physicalID)
	if err != nil || !claimed || sent.Status != k12.ModelInvocationSent {
		t.Fatalf(
			"claim V2 exact-set seed batch: claimed=%v child=%+v err=%v",
			claimed,
			sent,
			err,
		)
	}
	switch status {
	case k12.ModelInvocationSent:
		return sent
	case k12.ModelInvocationOutcomeUnknown:
		unknown, markErr := seed.executor.o.deps.Records.
			MarkModelPhysicalInvocationOutcomeUnknown(
				ctx,
				parent.AgentName,
				physicalID,
				"provider_transport_result_unknown",
			)
		if markErr != nil {
			t.Fatalf("mark V2 exact-set seed outcome unknown: %v", markErr)
		}
		return unknown
	default:
		t.Fatalf("unsupported V2 exact-set seed status %s", status)
		return k12.ModelPhysicalInvocation{}
	}
}

func newProblemSourceRecognitionHarness(
	t *testing.T,
	planVersion int,
	recognizer Recognizer,
	suffix string,
	models ...string,
) problemSourceRecognitionHarness {
	t.Helper()
	const nowUnix int64 = 2_000_000_000
	model := k12.RecognizingPolicyModel
	if len(models) > 0 {
		model = models[0]
	}
	policy := k12.ModelRequestPolicySnapshot{}
	if model == k12.RecognizingPolicyModel {
		policy = k12.ApprovedRecognizingRequestPolicy()
	}
	route := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    model,
		Route:                    "hexclaw-gpt/" + model,
		Capability:               "vision",
		RecognizingRequestPolicy: policy,
	}
	budget := orchestratorTestBudget()
	if planVersion == k12.RecognitionPlanVersionV2 {
		budget = recognitionLayoutInitialV2Budget()
	}
	// 使用真正的纵向密集页面，使迁移期间即使适配器仍保留旧密集页面启发式逻辑，
	// 此夹具也会保持为 V2。
	page := planSelectorPagePNG(t, 800, 1200)
	deps, _ := newPipeline(
		t,
		fakeSolver{
			solution: "4",
			ev: SolveEvidence{
				Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec,
			},
		},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		nil,
	)
	deps.Now = func() int64 { return nowUnix }
	deps.GradingBudgetSnapshot = budget
	deps.Recognizer = recognizer
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return route, nil
		},
	))
	job, created, err := orchestrator.StartPhotoGradingJob(
		context.Background(),
		StartPhotoGradingInput{
			Photo: PhotoGradeRequest{
				AgentName:     "mingming",
				Grade:         "五年级上",
				SourceSession: "problem-source-recognition-" + suffix,
				Image:         page,
			},
			SourceKind:     "desktop",
			SourceKey:      "problem-source-recognition-" + suffix,
			BudgetSnapshot: budget,
		},
	)
	if err != nil || !created {
		t.Fatalf("start problem-source fixture: created=%v err=%v", created, err)
	}
	work := k12storage.ProblemSourceReprocessJob{
		WorkID:             "source-work-" + suffix,
		CommandReceiptID:   "source-command-" + suffix,
		OwnerScope:         "owner-source-v2",
		AgentName:          job.Record.AgentName,
		DispatchID:         "source-dispatch-" + suffix,
		JobID:              job.Record.RecordID,
		ProblemID:          "problem-1",
		Action:             "retake",
		StructureVersion:   1,
		InputRevision:      2,
		InputDigest:        "sha256:" + strings.Repeat("1", 64),
		AffectedProblemIDs: []string{"problem-1"},
		RequestJSON:        []byte(`{"source":"retake"}`),
		AttemptCount:       1,
	}
	return problemSourceRecognitionHarness{
		orchestrator: orchestrator,
		job:          job,
		work:         work,
		page:         page,
	}
}

// 区域选择与重拍继承 Job 的 V2 契约；非 Sol 模型有持久头部和清单授权，不携带 Sol 参数。
func TestREGK12RecognitionDurabilityBudget20260808001ProblemSourcePublishesV2BeforeRecognizer(
	t *testing.T,
) {
	probe := &problemSourceRecognitionV2EntryProbe{}
	fixture := newProblemSourceRecognitionHarness(
		t,
		k12.RecognitionPlanVersionV2,
		probe,
		"v2-entry",
		"gpt-5.6-luna",
	)
	probe.records = fixture.orchestrator.deps.Records
	probe.work = fixture.work

	_, err := fixture.orchestrator.executeProblemSourceRecognition(
		context.Background(),
		fixture.work,
		fixture.job,
		fixture.page,
	)
	if !errors.Is(err, errProblemSourceRecognitionV2EntryProbe) {
		t.Fatalf("execute source V2 error=%v, want focused entry stop", err)
	}
	if probe.entryErr != nil {
		t.Fatalf("source V2 recognizer-entry evidence: %v", probe.entryErr)
	}
	wantPolicy := k12.NormalizeModelRequestPolicySnapshot(
		fixture.job.Fields.ModelSnapshot.RecognizingRequestPolicy,
	)
	if probe.calls != 1 || probe.policyEnabled != !wantPolicy.IsZero() || probe.policy != wantPolicy ||
		!probe.headerEnabled || probe.headerDigest == "" {
		t.Fatalf(
			"source V2 entry calls=%d policy_enabled=%v policy=%+v header_enabled=%v header=%q",
			probe.calls,
			probe.policyEnabled,
			probe.policy,
			probe.headerEnabled,
			probe.headerDigest,
		)
	}
	if probe.parent.Status != k12.ModelInvocationSent ||
		probe.runtime.Status != "prepared_manifest" ||
		probe.runtime.HeaderDigest != probe.headerDigest {
		t.Fatalf(
			"source V2 parent=%+v runtime=%+v context_header=%q",
			probe.parent,
			probe.runtime,
			probe.headerDigest,
		)
	}
	header := probe.runtime.Header
	if header.ParentInvocationID != probe.parent.InvocationID ||
		header.JobID != fixture.job.Record.RecordID ||
		header.StageStartedAtUnixMillis != probe.parent.CreatedAt*1000 ||
		header.PhysicalCallCapMillis !=
			fixture.job.Fields.BudgetSnapshot.PhysicalCallCapMillis ||
		header.BudgetBuckets != fixture.job.Fields.BudgetSnapshot.RecognizingBuckets {
		t.Fatalf("source V2 header=%+v parent=%+v", header, probe.parent)
	}
}

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-001：已经最终化的来源 V2
// 精确集合是 sent 和 succeeded 父项共同的恢复权威。重启后投影该集合时不会发送
// Provider 请求，并会保留 ProblemSource 结果引用中的全部 V2 标识字段。
func TestREGK12RecognitionDurabilityBudget20260808001ProblemSourceReplaysFinalizedV2ExactSet(
	t *testing.T,
) {
	for _, parentStatus := range []k12.ModelInvocationStatus{
		k12.ModelInvocationSent,
		k12.ModelInvocationSucceeded,
	} {
		t.Run(string(parentStatus), func(t *testing.T) {
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
			fixture := newProblemSourceRecognitionHarness(
				t,
				k12.RecognitionPlanVersionV2,
				projection,
				"v2-finalized-"+string(parentStatus),
			)
			parent := publishProblemSourceRecognitionV2Fixture(
				t,
				fixture,
			)
			runtime := finalizeRecognitionLayoutOrchestratorReplayFixtureV2(
				t,
				context.Background(),
				fixture.orchestrator,
				parent,
				fixture.page,
			)
			if runtime.Status != "succeeded" {
				t.Fatalf("source V2 runtime=%+v, want finalized", runtime)
			}
			if parentStatus == k12.ModelInvocationSucceeded {
				var err error
				parent, err = fixture.orchestrator.deps.Records.
					MarkModelInvocationSucceeded(
						context.Background(),
						parent.AgentName,
						parent.InvocationID,
						"sha256:"+strings.Repeat("a", 64),
						"",
					)
				if err != nil {
					t.Fatalf("seed succeeded source V2 parent: %v", err)
				}
			}
			shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
			if err := fixture.orchestrator.Shutdown(shutdownCtx); err != nil {
				cancelShutdown()
				t.Fatalf("shutdown source V2 fixture before restart: %v", err)
			}
			cancelShutdown()
			fixture.orchestrator = trackGradingOrchestrator(
				t,
				NewGradingOrchestrator(
					fixture.orchestrator.deps,
					fixture.orchestrator.snapshotFn,
				),
			)
			wantPhysical, err := fixture.orchestrator.recognitionPhysicalSuccessSet(
				context.Background(),
				parent,
				fixture.page,
			)
			if err != nil {
				t.Fatalf("load source V2 canonical exact set: %v", err)
			}

			execution, err := fixture.orchestrator.executeProblemSourceRecognition(
				context.Background(),
				fixture.work,
				fixture.job,
				fixture.page,
			)
			if err != nil {
				t.Fatalf("replay finalized source V2 parent=%s: %v", parentStatus, err)
			}
			if projection.calls != 1 || !projection.marked || !projection.replayed {
				t.Fatalf(
					"source V2 replay probe=%+v, want one read-only replay and zero Provider sends",
					projection,
				)
			}
			if len(execution.PhysicalResults) != len(wantPhysical) {
				t.Fatalf(
					"source V2 refs=%+v want physical=%+v",
					execution.PhysicalResults,
					wantPhysical,
				)
			}
			for index, child := range wantPhysical {
				ref := execution.PhysicalResults[index]
				if ref.PhysicalInvocationID != child.PhysicalInvocationID ||
					ref.PhysicalUnit != string(child.PhysicalUnit) ||
					ref.ResultDigest != child.ResultDigest ||
					ref.RecognitionPlanVersion != child.RecognitionPlanVersion ||
					ref.PlanDigest != child.PlanDigest ||
					ref.CandidateExactSetDigest != child.CandidateExactSetDigest {
					t.Fatalf(
						"source V2 ref[%d]=%+v want child=%+v",
						index,
						ref,
						child,
					)
				}
			}
		})
	}
}

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-002：最终化 V2 父项标记为
// succeeded 后、V73 结果提交前发生崩溃，不得使下一次队列租约变成新的识别尝试。
// 新租约会复用旧精确集合，并且是提交 V73 的唯一权威。
func TestREGK12RecognitionDurabilityBudget20260808002ProblemSourceReclaimedLeaseCommitsFinalizedPriorAttempt(
	t *testing.T,
) {
	ctx := context.Background()
	confidence := 0.99
	recognized := []RecognizedQuestion{{
		ProblemKind:           ProblemKindStandalone,
		SourceNumberPath:      []string{"1"},
		DisplayLabel:          "1",
		Question:              "recovered finalized source",
		CanonicalMarkdown:     "recovered finalized source",
		Subject:               "数学",
		AnswerState:           AnswerStatePresent,
		StudentAnswer:         "4",
		RecognitionConfidence: &confidence,
		EvidenceTranscriptions: []string{
			"recovered finalized source",
		},
		AnswerEvidenceTranscriptions: []string{"4"},
	}}
	fixture := newSourceReprocessIntegrationFixture(t, recognized)
	densePage := planSelectorPagePNG(t, 800, 1200)
	ready, err := fixture.repository.Persist(
		ctx,
		"guardian-1",
		"mingming",
		densePage,
	)
	if err != nil {
		t.Fatalf("persist dense retake source: %v", err)
	}
	affected := fixture.run.questions[0]
	command, err := fixture.coordinator.CommitProblemSourceAction(
		ctx,
		ProblemSourceActionCommand{
			OwnerScope:            "guardian-1",
			TrustedAgentName:      "mingming",
			DispatchID:            fixture.dispatchID,
			ProblemID:             affected.ProblemID,
			IdempotencyKey:        "source-v2-finalized-pre-v73-crash",
			Action:                "retake",
			StructureVersion:      1,
			ExpectedInputRevision: 1,
			Payload: []byte(`{"page_asset_id":"` +
				ready.Metadata.PageAssetID + `"}`),
		},
	)
	if err != nil {
		t.Fatalf("commit dense retake source action: %v", err)
	}
	claimedAt := time.Now().UTC()
	firstLease, claimed, err := fixture.coordinator.Records.
		ClaimProblemSourceReprocessJob(
			ctx,
			"source-v2-crash-worker-1",
			claimedAt,
			time.Minute,
		)
	if err != nil || !claimed ||
		firstLease.CommandReceiptID != command.CommandReceiptID ||
		firstLease.AttemptCount != 1 {
		t.Fatalf(
			"claim first source lease: claimed=%v work=%+v err=%v",
			claimed,
			firstLease,
			err,
		)
	}

	v2Job := fixture.job
	v2Job.Fields.BudgetSnapshot = recognitionLayoutInitialV2Budget()
	projection := &recognitionLayoutFinalizedProjectionProbe{questions: recognized}
	fixture.orchestrator.deps.Recognizer = projection
	v2Fixture := problemSourceRecognitionHarness{
		orchestrator: fixture.orchestrator,
		job:          v2Job,
		work:         firstLease,
		page:         densePage,
	}
	parent := publishProblemSourceRecognitionV2Fixture(t, v2Fixture)
	finalized := finalizeRecognitionLayoutOrchestratorReplayFixtureV2(
		t,
		ctx,
		fixture.orchestrator,
		parent,
		densePage,
	)
	if finalized.Status != "succeeded" {
		t.Fatalf("pre-crash source plan status=%q, want succeeded", finalized.Status)
	}
	firstExecution, err := fixture.orchestrator.executeProblemSourceRecognition(
		ctx,
		firstLease,
		v2Job,
		densePage,
	)
	if err != nil {
		t.Fatalf("project finalized source before crash: %v", err)
	}
	snapshot, err := fixture.coordinator.Records.GetProblemAttemptSnapshot(
		ctx,
		firstLease.AgentName,
		v2Job.Fields.SubmissionID,
	)
	if err != nil {
		t.Fatalf("load stable source structure: %v", err)
	}
	structureQuestions, err := RecognizedQuestionsFromProblemAttemptSnapshot(snapshot)
	if err != nil {
		t.Fatalf("decode stable source structure: %v", err)
	}
	items, err := mapProblemSourceRecognitionExactSet(
		firstLease,
		structureQuestions,
		firstExecution.Recognized,
	)
	if err != nil {
		t.Fatalf("map pre-crash source exact set: %v", err)
	}
	typedResult := k12storage.ProblemSourceRecognitionResult{
		MappingState:       k12storage.ProblemSourceRecognitionMappingStableExactSet,
		ParentInvocationID: firstExecution.Parent.InvocationID,
		PhysicalResults:    firstExecution.PhysicalResults,
		Items:              items,
	}
	typedDigest, err := k12storage.ProblemSourceRecognitionTypedResultDigest(
		typedResult,
	)
	if err != nil {
		t.Fatalf("digest pre-crash typed source result: %v", err)
	}
	if _, markErr := fixture.coordinator.Records.MarkModelInvocationSucceeded(
		ctx,
		firstExecution.Parent.AgentName,
		firstExecution.Parent.InvocationID,
		typedDigest,
		"",
	); markErr != nil {
		t.Fatalf("mark source parent succeeded before crash: %v", markErr)
	}
	if _, lookupErr := fixture.coordinator.Records.GetProblemSourceRecognitionResultByWork(
		ctx,
		firstLease.OwnerScope,
		firstLease.WorkID,
	); !errors.Is(lookupErr, k12storage.ErrProblemSourceRecognitionNotFound) {
		t.Fatalf("V73 result exists before crash: %v", lookupErr)
	}

	if shutdownErr := fixture.orchestrator.Shutdown(ctx); shutdownErr != nil {
		t.Fatalf("shutdown first source process: %v", shutdownErr)
	}
	restartedDeps := fixture.orchestrator.deps
	restartedProjection := &recognitionLayoutFinalizedProjectionProbe{
		questions: recognized,
	}
	restartedDeps.Recognizer = restartedProjection
	restarted := trackGradingOrchestrator(
		t,
		NewGradingOrchestrator(
			restartedDeps,
			fixture.orchestrator.snapshotFn,
			WithGradingRunDir(fixture.orchestrator.runDir),
		),
	)
	secondLease, claimed, err := fixture.coordinator.Records.
		ClaimProblemSourceReprocessJob(
			ctx,
			"source-v2-crash-worker-2",
			claimedAt.Add(2*time.Minute),
			time.Minute,
		)
	if err != nil || !claimed || secondLease.WorkID != firstLease.WorkID ||
		secondLease.AttemptCount != firstLease.AttemptCount+1 {
		t.Fatalf(
			"reclaim expired source lease: claimed=%v work=%+v err=%v",
			claimed,
			secondLease,
			err,
		)
	}
	var physicalBefore int
	if countErr := fixture.coordinator.Records.DB().QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_model_physical_invocations WHERE parent_invocation_id=?`,
		parent.InvocationID,
	).Scan(&physicalBefore); countErr != nil {
		t.Fatalf("count pre-recovery physical exact set: %v", countErr)
	}

	recovered, err := restarted.executeProblemSourceRecognition(
		ctx,
		secondLease,
		v2Job,
		densePage,
	)
	if err != nil {
		t.Fatalf("recover finalized source on reclaimed lease: %v", err)
	}
	if recovered.Parent.InvocationID != parent.InvocationID ||
		restartedProjection.calls != 1 || !restartedProjection.marked ||
		!restartedProjection.replayed {
		t.Fatalf(
			"reclaimed recovery parent=%s probe=%+v want old finalized replay",
			recovered.Parent.InvocationID,
			restartedProjection,
		)
	}
	if nextID := stableProblemSourceRecognitionParentID(
		secondLease.WorkID,
		secondLease.AttemptCount,
	); nextID == recovered.Parent.InvocationID {
		t.Fatalf("reclaimed recovery created successor parent %s", nextID)
	}
	var physicalAfter int
	if countErr := fixture.coordinator.Records.DB().QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_model_physical_invocations WHERE parent_invocation_id=?`,
		parent.InvocationID,
	).Scan(&physicalAfter); countErr != nil {
		t.Fatalf("count post-recovery physical exact set: %v", countErr)
	}
	if physicalAfter != physicalBefore {
		t.Fatalf(
			"reclaimed recovery changed physical exact set before=%d after=%d",
			physicalBefore,
			physicalAfter,
		)
	}

	recoveredItems, err := mapProblemSourceRecognitionExactSet(
		secondLease,
		structureQuestions,
		recovered.Recognized,
	)
	if err != nil {
		t.Fatalf("map recovered source exact set: %v", err)
	}
	recoveredResult := k12storage.ProblemSourceRecognitionResult{
		MappingState:       k12storage.ProblemSourceRecognitionMappingStableExactSet,
		ParentInvocationID: recovered.Parent.InvocationID,
		PhysicalResults:    recovered.PhysicalResults,
		Items:              recoveredItems,
	}
	commit, created, err := fixture.coordinator.Records.
		CommitProblemSourceRecognitionResult(
			ctx,
			secondLease.Lease(),
			recoveredResult,
		)
	if err != nil || !created || commit.ParentInvocationID != parent.InvocationID ||
		commit.ParentInvocationAttempt != parent.Attempt {
		t.Fatalf(
			"commit recovered source with current lease: created=%v commit=%+v err=%v",
			created,
			commit,
			err,
		)
	}
	if replay, replayCreated, err := fixture.coordinator.Records.
		CommitProblemSourceRecognitionResult(
			ctx,
			secondLease.Lease(),
			recoveredResult,
		); err != nil || replayCreated || replay.ResultDigest != commit.ResultDigest {
		t.Fatalf(
			"idempotent recovered V73 replay: created=%v commit=%+v err=%v",
			replayCreated,
			replay,
			err,
		)
	}
}

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-004：成功的 V2 清单是方案证据，
// 不是成功的候选结果。一旦每个已授权后继项都确定失败，父项必须进入 failed，
// 下一次队列尝试可以无需对账而继续。
func TestREGK12RecognitionDurabilityBudget20260808004ProblemSourceConcludesFailedV2ExactSetAfterSucceededManifest(
	t *testing.T,
) {
	ctx := context.Background()
	fixture := newProblemSourceRecognitionHarness(
		t,
		k12.RecognitionPlanVersionV2,
		&problemSourceRecognitionV2EntryProbe{},
		"v2-definitive-batch-failure",
	)
	parent := publishProblemSourceRecognitionV2Fixture(t, fixture)
	runtime, err := fixture.orchestrator.deps.Records.
		LoadRecognitionLayoutPlanRuntimeV2(
			ctx,
			parent.AgentName,
			parent.InvocationID,
		)
	if err != nil {
		t.Fatalf("load source V2 failure runtime: %v", err)
	}
	canonicalPage, err := k12.CanonicalizeRecognitionPageV2(fixture.page)
	if err != nil {
		t.Fatalf("canonicalize source V2 failure page: %v", err)
	}
	executor := newDurableRecognitionPhysicalCallExecutor(
		fixture.orchestrator,
		parent,
	)
	durableCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(ctx, runtime.HeaderDigest),
		executor,
	)
	manifest, err := executor.ExecuteRecognitionPhysicalCall(
		durableCtx,
		k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  runtime.HeaderDigest,
			Unit:        k12.RecognitionPhysicalUnitWholePage,
			Image:       canonicalPage.PNG,
		},
		func(context.Context) (string, error) {
			return `{"targets":"one"}`, nil
		},
	)
	if err != nil {
		t.Fatalf("succeed source V2 manifest: %v", err)
	}
	plan, err := k12.BuildRecognitionLayoutPlanV2(
		k12.RecognitionLayoutPlanInputV2{
			PagePNG: canonicalPage.PNG,
			Manifest: k12.RecognitionLayoutManifestSuccessV2{
				InvocationID: manifest.InvocationID,
				ResultDigest: manifest.ResultDigest,
			},
			Targets: []k12.RecognitionLayoutManifestTargetV2{{
				ManifestRef:      "manifest_0001",
				ManifestOrder:    1,
				SourceNumberPath: []string{"1"},
				DisplayLabel:     "1",
				Region: k12.SourcePixelRegion{
					X: 0, Y: 0, Width: 8, Height: 12,
				},
			}},
		},
	)
	if err != nil {
		t.Fatalf("build source V2 failure plan: %v", err)
	}
	if authorizeErr := k12.AuthorizeRecognitionLayoutPlanV2(
		durableCtx,
		manifest,
		plan,
	); authorizeErr != nil {
		t.Fatalf("authorize source V2 failure plan: %v", authorizeErr)
	}
	batch := plan.Batches[0]
	batchImage, err := k12.BuildRecognitionLayoutBatchImageV2(
		canonicalPage.PNG,
		plan,
		batch.Unit,
	)
	if err != nil {
		t.Fatalf("build source V2 failure batch image: %v", err)
	}
	var providerSends int
	_, batchErr := executor.ExecuteRecognitionPhysicalCall(
		durableCtx,
		k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  plan.AuthorizedPlanDigest,
			Unit:        batch.Unit,
			TargetIDs:   append([]string(nil), batch.TargetIDs...),
			Image:       batchImage,
		},
		func(context.Context) (string, error) {
			providerSends++
			return "", recognitionPhysicalExecutorV2HTTPError{status: 400}
		},
	)
	if batchErr == nil || providerSends != 1 {
		t.Fatalf(
			"seed definitive source V2 batch failure: sends=%d err=%v",
			providerSends,
			batchErr,
		)
	}
	childrenBefore, err := fixture.orchestrator.problemSourceRecognitionChildren(
		ctx,
		parent,
	)
	if err != nil || len(childrenBefore) != 2 {
		t.Fatalf("source V2 failed exact set=%+v err=%v", childrenBefore, err)
	}

	settledErr := fixture.orchestrator.settleProblemSourceRecognitionError(
		ctx,
		parent,
		batchErr,
	)
	if errors.Is(settledErr, ErrModelInvocationRequiresReconciliation) {
		t.Fatalf(
			"definitive source V2 exact set was misclassified as reconciliation: %v",
			settledErr,
		)
	}
	storedParent, err := fixture.orchestrator.deps.Records.GetModelInvocation(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || storedParent.Status != k12.ModelInvocationFailed ||
		storedParent.FailureKind != "source_recognition_physical_failed" {
		t.Fatalf("settled source V2 failed parent=%+v err=%v", storedParent, err)
	}
	if successorErr := fixture.orchestrator.problemSourceRecognitionAttemptAllowsSuccessor(
		ctx,
		storedParent,
	); successorErr != nil {
		t.Fatalf("definitively failed source V2 parent blocks successor: %v", successorErr)
	}
	childrenAfter, err := fixture.orchestrator.problemSourceRecognitionChildren(
		ctx,
		storedParent,
	)
	if err != nil || len(childrenAfter) != len(childrenBefore) || providerSends != 1 {
		t.Fatalf(
			"settlement changed source V2 physical set: before=%d after=%d sends=%d err=%v",
			len(childrenBefore),
			len(childrenAfter),
			providerSends,
			err,
		)
	}
}

func TestREGK12RecognitionDurabilityBudget20260808004GradingJobConsumesSharedV2ExactSetClassifier(
	t *testing.T,
) {
	ctx := context.Background()
	fixture := newRecognitionInitialCallRecoveryFixture(
		t,
		k12.RecognitionPlanVersionV2,
	)
	seed := seedRecognitionV2AuthorizedBatch(
		t,
		fixture.orchestrator,
		fixture.parent,
		fixture.image,
	)
	var providerSends int
	_, batchErr := seed.executor.ExecuteRecognitionPhysicalCall(
		seed.durableCtx,
		seed.batchCall,
		func(context.Context) (string, error) {
			providerSends++
			return "", recognitionPhysicalExecutorV2HTTPError{status: 400}
		},
	)
	if batchErr == nil || providerSends != 1 {
		t.Fatalf(
			"seed GradingJob definitive V2 batch failure: sends=%d err=%v",
			providerSends,
			batchErr,
		)
	}
	classification, err := fixture.orchestrator.
		classifyRecognitionPhysicalExactSet(ctx, fixture.parent)
	if err != nil ||
		classification.State != recognitionPhysicalExactSetDefinitiveFailure {
		t.Fatalf(
			"classify GradingJob definitive V2 exact set: class=%+v err=%v",
			classification,
			err,
		)
	}
	handled, settled, err := fixture.orchestrator.
		settleConclusiveRecognitionRecovery(
			ctx,
			fixture.run,
			fixture.job,
			fixture.parent,
		)
	settledStatus := ""
	if settled.Record != nil {
		settledStatus = settled.Record.Status
	}
	if !handled || err == nil ||
		settledStatus != k12.GradingStageFailedTerminal {
		t.Fatalf(
			"GradingJob did not consume definitive V2 classification: handled=%v status=%s err=%v",
			handled,
			settledStatus,
			err,
		)
	}
	if providerSends != 1 {
		t.Fatalf("GradingJob recovery resent Provider: sends=%d", providerSends)
	}
}

func TestREGK12RecognitionDurabilityBudget20260808004SentAndOutcomeUnknownRemainReconciliationAcrossConsumers(
	t *testing.T,
) {
	for _, status := range []k12.ModelInvocationStatus{
		k12.ModelInvocationSent,
		k12.ModelInvocationOutcomeUnknown,
	} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			problemSource := newProblemSourceRecognitionHarness(
				t,
				k12.RecognitionPlanVersionV2,
				&problemSourceRecognitionV2EntryProbe{},
				"v2-ambiguous-"+string(status),
			)
			problemSourceParent := publishProblemSourceRecognitionV2Fixture(
				t,
				problemSource,
			)
			problemSourceSeed := seedRecognitionV2AuthorizedBatch(
				t,
				problemSource.orchestrator,
				problemSourceParent,
				problemSource.page,
			)
			seedRecognitionV2BatchTerminalState(t, problemSourceSeed, status)
			problemSourceClass, err := problemSource.orchestrator.
				classifyRecognitionPhysicalExactSet(ctx, problemSourceParent)
			if err != nil || problemSourceClass.State !=
				recognitionPhysicalExactSetNeedsReconciliation {
				t.Fatalf(
					"ProblemSource %s classification=%+v err=%v",
					status,
					problemSourceClass,
					err,
				)
			}
			childrenBefore, err := problemSource.orchestrator.
				problemSourceRecognitionChildren(ctx, problemSourceParent)
			if err != nil {
				t.Fatalf("list ProblemSource %s exact set: %v", status, err)
			}
			settledErr := problemSource.orchestrator.
				settleProblemSourceRecognitionError(
					ctx,
					problemSourceParent,
					errors.New("focused ambiguous exact-set recovery"),
				)
			if !errors.Is(
				settledErr,
				ErrModelInvocationRequiresReconciliation,
			) {
				t.Fatalf(
					"ProblemSource %s escaped reconciliation: %v",
					status,
					settledErr,
				)
			}
			storedProblemSourceParent, err := problemSource.orchestrator.
				deps.Records.GetModelInvocation(
				ctx,
				problemSourceParent.AgentName,
				problemSourceParent.InvocationID,
			)
			if err != nil || storedProblemSourceParent.Status !=
				k12.ModelInvocationSent {
				t.Fatalf(
					"ProblemSource %s parent=%+v err=%v",
					status,
					storedProblemSourceParent,
					err,
				)
			}
			childrenAfter, err := problemSource.orchestrator.
				problemSourceRecognitionChildren(ctx, problemSourceParent)
			if err != nil || len(childrenAfter) != len(childrenBefore) {
				t.Fatalf(
					"ProblemSource %s recovery changed physical set before=%d after=%d err=%v",
					status,
					len(childrenBefore),
					len(childrenAfter),
					err,
				)
			}

			grading := newRecognitionInitialCallRecoveryFixture(
				t,
				k12.RecognitionPlanVersionV2,
			)
			gradingSeed := seedRecognitionV2AuthorizedBatch(
				t,
				grading.orchestrator,
				grading.parent,
				grading.image,
			)
			seedRecognitionV2BatchTerminalState(t, gradingSeed, status)
			gradingClass, err := grading.orchestrator.
				classifyRecognitionPhysicalExactSet(ctx, grading.parent)
			if err != nil || gradingClass.State != problemSourceClass.State {
				t.Fatalf(
					"GradingJob %s classification=%+v ProblemSource=%+v err=%v",
					status,
					gradingClass,
					problemSourceClass,
					err,
				)
			}
			handled, _, err := grading.orchestrator.
				settleConclusiveRecognitionRecovery(
					ctx,
					grading.run,
					grading.job,
					grading.parent,
				)
			if handled || err != nil {
				t.Fatalf(
					"GradingJob %s recovery left passive reconciliation: handled=%v err=%v",
					status,
					handled,
					err,
				)
			}
			storedGradingParent, err := grading.orchestrator.deps.Records.
				GetModelInvocation(
					ctx,
					grading.parent.AgentName,
					grading.parent.InvocationID,
				)
			if err != nil || storedGradingParent.Status !=
				k12.ModelInvocationSent {
				t.Fatalf(
					"GradingJob %s parent=%+v err=%v",
					status,
					storedGradingParent,
					err,
				)
			}
		})
	}
}

func TestREGK12RecognitionDurabilityBudget20260808004OnlyFinalizationReceiptClassifiesSuccessAcrossConsumers(
	t *testing.T,
) {
	ctx := context.Background()
	problemSource := newProblemSourceRecognitionHarness(
		t,
		k12.RecognitionPlanVersionV2,
		&problemSourceRecognitionV2EntryProbe{},
		"v2-finalized-classification",
	)
	problemSourceParent := publishProblemSourceRecognitionV2Fixture(
		t,
		problemSource,
	)
	finalizeRecognitionLayoutOrchestratorReplayFixtureV2(
		t,
		ctx,
		problemSource.orchestrator,
		problemSourceParent,
		problemSource.page,
	)
	problemSourceClass, err := problemSource.orchestrator.
		classifyRecognitionPhysicalExactSet(ctx, problemSourceParent)
	if err != nil ||
		problemSourceClass.State != recognitionPhysicalExactSetFinalizedSuccess {
		t.Fatalf(
			"ProblemSource finalized classification=%+v err=%v",
			problemSourceClass,
			err,
		)
	}
	if _, replayErr := problemSource.orchestrator.recognitionPhysicalSuccessSet(
		ctx,
		problemSourceParent,
		problemSource.page,
	); replayErr != nil {
		t.Fatalf("replay ProblemSource finalized exact set: %v", replayErr)
	}

	grading := newRecognitionInitialCallRecoveryFixture(
		t,
		k12.RecognitionPlanVersionV2,
	)
	finalizeRecognitionLayoutOrchestratorReplayFixtureV2(
		t,
		ctx,
		grading.orchestrator,
		grading.parent,
		grading.image,
	)
	gradingClass, err := grading.orchestrator.
		classifyRecognitionPhysicalExactSet(ctx, grading.parent)
	if err != nil || gradingClass.State != problemSourceClass.State {
		t.Fatalf(
			"GradingJob finalized classification=%+v ProblemSource=%+v err=%v",
			gradingClass,
			problemSourceClass,
			err,
		)
	}
	handled, _, err := grading.orchestrator.settleConclusiveRecognitionRecovery(
		ctx,
		grading.run,
		grading.job,
		grading.parent,
	)
	if handled || err != nil {
		t.Fatalf(
			"GradingJob finalized recovery should resume exact-set replay: handled=%v err=%v",
			handled,
			err,
		)
	}
	if _, err := grading.orchestrator.recognitionPhysicalSuccessSet(
		ctx,
		grading.parent,
		grading.image,
	); err != nil {
		t.Fatalf("replay GradingJob finalized exact set: %v", err)
	}
}

func publishProblemSourceRecognitionV2Fixture(
	t *testing.T,
	fixture problemSourceRecognitionHarness,
) k12.ModelInvocation {
	t.Helper()
	ctx := context.Background()
	parentAttempt := 1
	invocations, err := fixture.orchestrator.deps.Records.ListModelInvocations(
		ctx,
		fixture.work.AgentName,
		fixture.work.JobID,
	)
	if err != nil {
		t.Fatalf("list source V2 fixture attempts: %v", err)
	}
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageRecognizing &&
			invocation.Attempt >= parentAttempt {
			parentAttempt = invocation.Attempt + 1
		}
	}
	route := k12.NormalizeGradingModelSnapshot(fixture.job.Fields.ModelSnapshot)
	policy := k12.NormalizeModelRequestPolicySnapshot(
		route.RecognizingRequestPolicy,
	)
	requestDigest, err := problemSourceRecognitionParentDigest(
		fixture.work,
		route,
		policy,
	)
	if err != nil {
		t.Fatalf("digest source V2 parent: %v", err)
	}
	parent := k12.ModelInvocation{
		InvocationID: stableProblemSourceRecognitionParentID(
			fixture.work.WorkID,
			fixture.work.AttemptCount,
		),
		AgentName:             fixture.work.AgentName,
		JobID:                 fixture.work.JobID,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         requestDigest,
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               parentAttempt,
		CreatedAt:             fixture.orchestrator.deps.now(),
		UpdatedAt:             fixture.orchestrator.deps.now(),
	}
	canonicalPage, err := k12.CanonicalizeRecognitionPageV2(fixture.page)
	if err != nil {
		t.Fatalf("canonicalize source V2 fixture: %v", err)
	}
	budget := fixture.job.Fields.BudgetSnapshot
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   stableRecognitionLayoutPlanIDV2(parent.InvocationID),
		ParentInvocationID:       parent.InvocationID,
		AgentName:                parent.AgentName,
		JobID:                    parent.JobID,
		PageDigest:               canonicalPage.Digest,
		ParentRequestDigest:      parent.RequestDigest,
		RouteSnapshot:            parent.RouteSnapshot,
		RequestPolicySnapshot:    parent.RequestPolicySnapshot,
		StageStartedAtUnixMillis: parent.CreatedAt * 1000,
		PhysicalCallCapMillis:    budget.PhysicalCallCapMillis,
		BudgetBuckets:            budget.RecognizingBuckets,
		AdapterWorkerHardCap:     budget.WorkerHardCap,
		EffectiveConcurrency:     budget.EffectiveConcurrency,
	}
	headerDigest, err := k12.RecognitionLayoutPlanHeaderDigestV2(header)
	if err != nil {
		t.Fatalf("digest source V2 header=%+v: %v", header, err)
	}
	call := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  headerDigest,
		Unit:        k12.RecognitionPhysicalUnitWholePage,
		Image:       canonicalPage.PNG,
	}
	manifestID, err := stableRecognitionPhysicalInvocationIDForCall(
		parent.InvocationID,
		call,
	)
	if err != nil {
		t.Fatalf("identify source V2 manifest: %v", err)
	}
	manifestDigest, err := recognizingPhysicalInvocationDigest(parent, call)
	if err != nil {
		t.Fatalf("digest source V2 manifest: %v", err)
	}
	published, _, created, err := fixture.orchestrator.deps.Records.
		PrepareRecognizingInvocationWithInitialLayoutPlanV2(
			ctx,
			parent,
			k12.ModelPhysicalInvocation{
				PhysicalInvocationID:   manifestID,
				ParentInvocationID:     parent.InvocationID,
				AgentName:              parent.AgentName,
				JobID:                  parent.JobID,
				Stage:                  parent.Stage,
				PhysicalUnit:           call.Unit,
				RecognitionPlanVersion: k12.RecognitionPlanVersionV2,
				PlanDigest:             headerDigest,
				RequestDigest:          manifestDigest,
				RouteSnapshot:          parent.RouteSnapshot,
				RequestPolicySnapshot:  parent.RequestPolicySnapshot,
				Attempt:                1,
				CreatedAt:              parent.CreatedAt,
				UpdatedAt:              parent.UpdatedAt,
			},
			header,
		)
	if err != nil || !created || published.Status != k12.ModelInvocationSent {
		t.Fatalf(
			"publish source V2 fixture: parent=%+v created=%v err=%v",
			published,
			created,
			err,
		)
	}
	return published
}

func TestREGK12RecognitionDurabilityBudget20260808001ProblemSourceKeepsV1PhysicalContract(
	t *testing.T,
) {
	recognizer := &sourceReprocessPhysicalRecognizer{
		batches: [][]RecognizedQuestion{{
			{
				ProblemKind:      ProblemKindStandalone,
				SourceNumberPath: []string{"1"},
				DisplayLabel:     "1",
				Question:         "1+1=?",
				Subject:          "数学",
				AnswerState:      AnswerStatePresent,
				StudentAnswer:    "2",
			},
		}},
	}
	fixture := newProblemSourceRecognitionHarness(
		t,
		k12.RecognitionPlanVersionV1,
		recognizer,
		"v1-guard",
	)
	execution, err := fixture.orchestrator.executeProblemSourceRecognition(
		context.Background(),
		fixture.work,
		fixture.job,
		fixture.page,
	)
	if err != nil {
		t.Fatalf("execute source V1: %v", err)
	}
	calls, sends := recognizer.counts()
	if calls != 1 || sends != 1 || len(execution.PhysicalResults) != 1 {
		t.Fatalf(
			"source V1 calls=%d sends=%d physical=%+v",
			calls,
			sends,
			execution.PhysicalResults,
		)
	}
	ref := execution.PhysicalResults[0]
	wantID := stableRecognitionPhysicalInvocationID(
		execution.Parent.InvocationID,
		k12.RecognitionPhysicalUnitWholePage,
	)
	if ref.PhysicalInvocationID != wantID ||
		ref.PhysicalUnit != string(k12.RecognitionPhysicalUnitWholePage) ||
		ref.ResultDigest == "" || ref.RecognitionPlanVersion != 0 ||
		ref.PlanDigest != "" || ref.CandidateExactSetDigest != "" {
		t.Fatalf(
			"source V1 ref=%+v want legacy id=%s with no V2 metadata",
			ref,
			wantID,
		)
	}
	if execution.Parent.Status != k12.ModelInvocationSent {
		t.Fatalf("source V1 parent=%+v", execution.Parent)
	}
	if len(execution.Recognized) != 1 {
		t.Fatalf("source V1 recognized=%+v", execution.Recognized)
	}
}
