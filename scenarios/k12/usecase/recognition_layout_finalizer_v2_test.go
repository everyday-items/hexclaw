package usecase

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-001：最终化只能通过绑定到
// 不可变 V2 父项的执行器跨越真实 Store。重启后重放成功方案时不会调用 Provider。
func TestREGK12RecognitionDurabilityBudget20260808001FinalizesAndReplaysThroughDurableExecutor(
	t *testing.T,
) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/layout-finalizer-v2.db"
	db, store := openRecognitionPhysicalExecutorV2Store(t, dbPath)
	dbOpen := true
	defer func() {
		if dbOpen {
			_ = db.Close()
		}
	}()

	const agentName = "layout-finalizer-owner"
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES(?)`, agentName); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	policy := k12.ApprovedRecognizingRequestPolicy()
	route := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		Capability:               "vision",
		RecognizingRequestPolicy: policy,
	}
	job, err := k12.NewGradingJobRecord(
		agentName,
		"layout-finalizer-session",
		k12.GradingJobFields{
			SubmissionID:      "layout-finalizer-submission",
			SourceKind:        "test",
			IdempotencyKey:    k12.BuildGradingIdempotencyKey("test", "layout-finalizer", 0),
			ModelSnapshot:     route,
			ConfirmationState: k12.GradingConfirmationPending,
			AnchorState:       k12.GradingAnchorPending,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, putErr := store.Put(ctx, job); putErr != nil {
		t.Fatalf("persist job: %v", putErr)
	}

	pagePNG := recognitionPhysicalExecutorV2PagePNG(t, 40, 40)
	parent := k12.ModelInvocation{
		InvocationID:          "modelinv-layout-finalizer-v2",
		AgentName:             agentName,
		JobID:                 job.RecordID,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         recognitionPhysicalExecutorV2Digest("layout-finalizer-parent"),
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               1,
		CreatedAt:             time.Now().Unix(),
	}
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-finalizer-v2",
		ParentInvocationID:       parent.InvocationID,
		AgentName:                parent.AgentName,
		JobID:                    parent.JobID,
		PageDigest:               recognitionPhysicalExecutorV2BytesDigest(pagePNG),
		ParentRequestDigest:      parent.RequestDigest,
		RouteSnapshot:            route,
		RequestPolicySnapshot:    policy,
		StageStartedAtUnixMillis: time.Now().UnixMilli(),
		PhysicalCallCapMillis:    120000,
		BudgetBuckets: k12.RecognitionLayoutBudgetBucketsV2{
			UpTo1ProblemMillis:   120000,
			UpTo8ProblemsMillis:  600000,
			UpTo16ProblemsMillis: 600000,
			UpTo32ProblemsMillis: 600000,
		},
		AdapterWorkerHardCap: 2,
		EffectiveConcurrency: 1,
	}
	headerDigest, err := k12.RecognitionLayoutPlanHeaderDigestV2(header)
	if err != nil {
		t.Fatal(err)
	}
	manifestCall := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  headerDigest,
		Unit:        k12.RecognitionPhysicalUnitWholePage,
		Image:       pagePNG,
	}
	manifestID, err := stableRecognitionPhysicalInvocationIDForCall(
		parent.InvocationID,
		manifestCall,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifestRequestDigest, err := recognizingPhysicalInvocationDigest(parent, manifestCall)
	if err != nil {
		t.Fatal(err)
	}
	storedParent, _, created, err := store.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
		ctx,
		parent,
		k12.ModelPhysicalInvocation{
			PhysicalInvocationID:   manifestID,
			ParentInvocationID:     parent.InvocationID,
			AgentName:              parent.AgentName,
			JobID:                  parent.JobID,
			Stage:                  parent.Stage,
			PhysicalUnit:           manifestCall.Unit,
			RecognitionPlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:             headerDigest,
			RequestDigest:          manifestRequestDigest,
			RouteSnapshot:          parent.RouteSnapshot,
			RequestPolicySnapshot:  parent.RequestPolicySnapshot,
			Attempt:                1,
			CreatedAt:              parent.CreatedAt,
		},
		header,
	)
	if err != nil || !created {
		t.Fatalf("publish V2 fixture: created=%v err=%v", created, err)
	}
	if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
		ctx,
		agentName,
		manifestID,
	); claimErr != nil || !claimed {
		t.Fatalf("claim manifest: claimed=%v err=%v", claimed, claimErr)
	}
	storedManifest, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		agentName,
		manifestID,
		`{"targets":"one"}`,
		"",
	)
	if err != nil {
		t.Fatalf("succeed manifest: %v", err)
	}
	plan, err := k12.BuildRecognitionLayoutPlanV2(k12.RecognitionLayoutPlanInputV2{
		PagePNG: pagePNG,
		Manifest: k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: manifestID,
			ResultDigest: storedManifest.ResultDigest,
		},
		Targets: []k12.RecognitionLayoutManifestTargetV2{
			{
				ManifestRef:      "manifest_0001",
				ManifestOrder:    1,
				SourceNumberPath: []string{"1"},
				DisplayLabel:     "1",
				Region:           k12.SourcePixelRegion{X: 0, Y: 0, Width: 40, Height: 40},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if authorizeErr := store.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		agentName,
		parent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: manifestID,
			ResultDigest: storedManifest.ResultDigest,
		},
		plan,
	); authorizeErr != nil {
		t.Fatalf("authorize plan: %v", authorizeErr)
	}

	executor := newDurableRecognitionPhysicalCallExecutor(
		&GradingOrchestrator{deps: Deps{Records: store, Now: time.Now().Unix}},
		storedParent,
	)
	durableCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(
			k12.WithGradingModelRequestPolicy(ctx, policy),
			headerDigest,
		),
		executor,
	)
	batch := plan.Batches[0]
	batchCall := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  plan.AuthorizedPlanDigest,
		Unit:        batch.Unit,
		TargetIDs:   append([]string(nil), batch.TargetIDs...),
		Image:       []byte("finalizable-primary-contact-sheet"),
	}
	var providerSends atomic.Int32
	source, err := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		batchCall,
		func(context.Context) (string, error) {
			providerSends.Add(1)
			return `{"results":"valid"}`, nil
		},
	)
	if err != nil || providerSends.Load() != 1 {
		t.Fatalf("execute primary batch: sends=%d err=%v", providerSends.Load(), err)
	}
	if projection, settlementCreated, settlementErr := k12.SettleRecognitionLayoutPrimaryBatchV2(
		durableCtx,
		source,
		k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			SourcePhysicalInvocationID: source.InvocationID,
			SourcePhysicalUnit:         batch.Unit,
			SourcePhysicalResultDigest: source.ResultDigest,
			Classification:             k12.RecognitionLayoutBatchClassifiedV2,
			Candidates: []k12.RecognitionLayoutCandidateSettlementV2{
				{
					CandidateID:    batch.TargetIDs[0],
					Classification: k12.RecognitionLayoutCandidateValidV2,
					ResultKind:     k12.RecognitionLayoutCandidateQuestionV2,
					ResultJSON:     json.RawMessage(`{"student_answer":"4","text":"2+2"}`),
				},
			},
		},
	); settlementErr != nil || !settlementCreated || len(projection.FrozenResults) != 1 {
		t.Fatalf(
			"settle finalizable primary: created=%v result=%+v err=%v",
			settlementCreated,
			projection,
			settlementErr,
		)
	}

	want, finalizationCreated, err := k12.FinalizeRecognitionLayoutPlanV2(durableCtx)
	if err != nil || !finalizationCreated || want.PlanID != header.PlanID ||
		want.PlanDigest != plan.AuthorizedPlanDigest ||
		len(want.CandidateResults) != 1 || len(want.PhysicalResults) != 2 ||
		providerSends.Load() != 1 || executor.localCallEntries.Load() != 1 {
		t.Fatalf(
			"finalize through durable executor: created=%v sends=%d entries=%d result=%+v err=%v",
			finalizationCreated,
			providerSends.Load(),
			executor.localCallEntries.Load(),
			want,
			err,
		)
	}
	replayedSameProcess, replayCreated, err := k12.FinalizeRecognitionLayoutPlanV2(
		durableCtx,
	)
	if err != nil || replayCreated || !reflect.DeepEqual(replayedSameProcess, want) ||
		executor.localCallEntries.Load() != 1 {
		t.Fatalf(
			"same-process finalization replay: created=%v entries=%d result=%+v err=%v",
			replayCreated,
			executor.localCallEntries.Load(),
			replayedSameProcess,
			err,
		)
	}

	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close first process: %v", closeErr)
	}
	dbOpen = false
	restartedDB, restartedStore := openRecognitionPhysicalExecutorV2Store(t, dbPath)
	defer restartedDB.Close()
	restartedParent, err := restartedStore.GetModelInvocation(
		ctx,
		agentName,
		parent.InvocationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	restartedExecutor := newDurableRecognitionPhysicalCallExecutor(
		&GradingOrchestrator{deps: Deps{Records: restartedStore, Now: time.Now().Unix}},
		restartedParent,
	)
	restartedCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(
			k12.WithGradingModelRequestPolicy(ctx, policy),
			headerDigest,
		),
		restartedExecutor,
	)
	restartedResult, replayed, err := k12.ReplayFinalizedRecognitionLayoutPlanV2(
		restartedCtx,
	)
	if err != nil || !replayed || !reflect.DeepEqual(restartedResult, want) ||
		restartedExecutor.localCallEntries.Load() != 0 || providerSends.Load() != 1 {
		t.Fatalf(
			"restart finalization replay: replayed=%v sends=%d entries=%d result=%+v err=%v",
			replayed,
			providerSends.Load(),
			restartedExecutor.localCallEntries.Load(),
			restartedResult,
			err,
		)
	}

	driftCases := []struct {
		name         string
		headerDigest string
		mutate       func(*k12.ModelInvocation)
	}{
		{
			name:         "owner",
			headerDigest: headerDigest,
			mutate: func(parent *k12.ModelInvocation) {
				parent.AgentName = "different-owner"
			},
		},
		{
			name:         "job",
			headerDigest: headerDigest,
			mutate: func(parent *k12.ModelInvocation) {
				parent.JobID = "different-job"
			},
		},
		{
			name:         "request",
			headerDigest: headerDigest,
			mutate: func(parent *k12.ModelInvocation) {
				parent.RequestDigest = recognitionPhysicalExecutorV2Digest("drifted-parent")
			},
		},
		{
			name:         "route",
			headerDigest: headerDigest,
			mutate: func(parent *k12.ModelInvocation) {
				parent.RouteSnapshot.Provider = "different-provider"
			},
		},
		{
			name:         "policy",
			headerDigest: headerDigest,
			mutate: func(parent *k12.ModelInvocation) {
				parent.RequestPolicySnapshot = k12.ModelRequestPolicySnapshot{}
			},
		},
		{
			name:         "header",
			headerDigest: recognitionPhysicalExecutorV2Digest("different-header"),
			mutate:       func(*k12.ModelInvocation) {},
		},
	}
	for _, test := range driftCases {
		t.Run(test.name+"_drift", func(t *testing.T) {
			driftedParent := restartedParent
			test.mutate(&driftedParent)
			driftedCtx := k12.WithRecognitionPhysicalCallExecutor(
				k12.WithRecognitionLayoutPlanV2(ctx, test.headerDigest),
				newDurableRecognitionPhysicalCallExecutor(
					&GradingOrchestrator{deps: Deps{Records: restartedStore}},
					driftedParent,
				),
			)
			if _, _, err := k12.FinalizeRecognitionLayoutPlanV2(driftedCtx); err == nil {
				t.Fatal("finalization accepted a drifted durable parent binding")
			}
		})
	}
}
