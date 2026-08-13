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

func TestREGK12RecognitionBatchRepair20260808001SettlesPrimaryBatchThroughDurableExecutor(
	t *testing.T,
) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/primary-batch-settler-v2.db"
	db, store := openRecognitionPhysicalExecutorV2Store(t, dbPath)
	dbOpen := true
	defer func() {
		if dbOpen {
			_ = db.Close()
		}
	}()

	const agentName = "primary-batch-settlement-owner"
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
		"primary-batch-settlement-session",
		k12.GradingJobFields{
			SubmissionID:      "primary-batch-settlement-submission",
			SourceKind:        "test",
			IdempotencyKey:    k12.BuildGradingIdempotencyKey("test", "primary-batch-settlement", 0),
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
		InvocationID:          "modelinv-primary-batch-settler-v2",
		AgentName:             agentName,
		JobID:                 job.RecordID,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         recognitionPhysicalExecutorV2Digest("primary-batch-parent"),
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               1,
		CreatedAt:             time.Now().Unix(),
	}
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-primary-batch-settler-v2",
		ParentInvocationID:       parent.InvocationID,
		AgentName:                parent.AgentName,
		JobID:                    parent.JobID,
		PageDigest:               recognitionPhysicalExecutorV2BytesDigest(pagePNG),
		ParentRequestDigest:      parent.RequestDigest,
		RouteSnapshot:            parent.RouteSnapshot,
		RequestPolicySnapshot:    parent.RequestPolicySnapshot,
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
	storedParent, storedManifest, created, err :=
		store.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
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
		t.Fatalf("publish V2 fixture: created=%v parent=%+v manifest=%+v err=%v", created, storedParent, storedManifest, err)
	}
	if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
		ctx,
		agentName,
		manifestID,
	); claimErr != nil || !claimed {
		t.Fatalf("claim manifest: claimed=%v err=%v", claimed, claimErr)
	}
	storedManifest, err = store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		agentName,
		manifestID,
		`{"targets":"two"}`,
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
				Region:           k12.SourcePixelRegion{X: 0, Y: 0, Width: 40, Height: 20},
			},
			{
				ManifestRef:      "manifest_0002",
				ManifestOrder:    2,
				SourceNumberPath: []string{"2"},
				DisplayLabel:     "2",
				Region:           k12.SourcePixelRegion{X: 0, Y: 20, Width: 40, Height: 20},
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
		Image:       []byte("primary-batch-contact-sheet"),
	}
	var providerSends atomic.Int32
	source, err := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		batchCall,
		func(context.Context) (string, error) {
			providerSends.Add(1)
			return `{"results":"primary-batch"}`, nil
		},
	)
	if err != nil || providerSends.Load() != 1 {
		t.Fatalf("execute source batch: source=%+v sends=%d err=%v", source, providerSends.Load(), err)
	}
	settlement := k12.RecognitionLayoutPrimaryBatchSettlementV2{
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
			{
				CandidateID:    batch.TargetIDs[1],
				Classification: k12.RecognitionLayoutCandidateMissingV2,
			},
		},
	}
	first, created, err := k12.SettleRecognitionLayoutPrimaryBatchV2(
		durableCtx,
		source,
		settlement,
	)
	if err != nil || !created ||
		len(first.FrozenResults) != 1 ||
		len(first.RepairAuthorizations) != 1 {
		t.Fatalf("settle valid+missing batch: created=%v result=%+v err=%v", created, first, err)
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
	var restartSends atomic.Int32
	replayedSource, err := restartedExecutor.ExecuteRecognitionPhysicalCall(
		ctx,
		batchCall,
		func(context.Context) (string, error) {
			restartSends.Add(1)
			return "must-not-send", nil
		},
	)
	if err != nil || restartSends.Load() != 0 || replayedSource != source {
		t.Fatalf("replay succeeded source: source=%+v sends=%d err=%v", replayedSource, restartSends.Load(), err)
	}
	replayed, replayCreated, err := k12.SettleRecognitionLayoutPrimaryBatchV2(
		restartedCtx,
		replayedSource,
		settlement,
	)
	if err != nil || replayCreated || !reflect.DeepEqual(replayed, first) ||
		providerSends.Load() != 1 {
		t.Fatalf("exact settlement replay: created=%v result=%+v sends=%d err=%v", replayCreated, replayed, providerSends.Load(), err)
	}

	runtime, err := k12.LoadRecognitionLayoutPlanV2Runtime(restartedCtx)
	if err != nil {
		t.Fatal(err)
	}
	missingCapabilityCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(
			k12.WithGradingModelRequestPolicy(ctx, policy),
			headerDigest,
		),
		recognitionLayoutSettlementMissingCapability{runtime: runtime},
	)
	if _, _, err := k12.SettleRecognitionLayoutPrimaryBatchV2(
		missingCapabilityCtx,
		source,
		settlement,
	); err == nil {
		t.Fatal("approved V2 context accepted an executor without settlement capability")
	}

	driftedParent := restartedParent
	driftedParent.AgentName = "different-owner"
	driftedExecutor := newDurableRecognitionPhysicalCallExecutor(
		&GradingOrchestrator{deps: Deps{Records: restartedStore}},
		driftedParent,
	)
	driftedOwnerCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(ctx, headerDigest),
		driftedExecutor,
	)
	if _, _, err := k12.SettleRecognitionLayoutPrimaryBatchV2(
		driftedOwnerCtx,
		source,
		settlement,
	); err == nil {
		t.Fatal("settlement accepted a drifted parent owner")
	}

	driftTests := []struct {
		name       string
		callCtx    context.Context
		source     k12.RecognitionPhysicalCallResult
		settlement k12.RecognitionLayoutPrimaryBatchSettlementV2
	}{
		{
			name:       "header",
			callCtx:    k12.WithRecognitionPhysicalCallExecutor(k12.WithRecognitionLayoutPlanV2(ctx, recognitionPhysicalExecutorV2Digest("other-header")), restartedExecutor),
			source:     source,
			settlement: settlement,
		},
		{
			name:    "plan",
			callCtx: restartedCtx,
			source:  source,
			settlement: func() k12.RecognitionLayoutPrimaryBatchSettlementV2 {
				changed := settlement
				changed.PlanDigest = recognitionPhysicalExecutorV2Digest("other-plan")
				return changed
			}(),
		},
		{
			name:    "source",
			callCtx: restartedCtx,
			source: func() k12.RecognitionPhysicalCallResult {
				changed := source
				changed.InvocationID = "different-source"
				return changed
			}(),
			settlement: settlement,
		},
		{
			name:    "result",
			callCtx: restartedCtx,
			source: func() k12.RecognitionPhysicalCallResult {
				changed := source
				changed.ResultDigest = recognitionPhysicalExecutorV2Digest("different-result")
				return changed
			}(),
			settlement: settlement,
		},
		{
			name:    "unit",
			callCtx: restartedCtx,
			source:  source,
			settlement: func() k12.RecognitionLayoutPrimaryBatchSettlementV2 {
				changed := settlement
				changed.SourcePhysicalUnit = k12.RecognitionPhysicalUnit("layout_batch_0002")
				return changed
			}(),
		},
	}
	for _, test := range driftTests {
		t.Run(test.name+"_drift", func(t *testing.T) {
			if _, _, err := k12.SettleRecognitionLayoutPrimaryBatchV2(
				test.callCtx,
				test.source,
				test.settlement,
			); err == nil {
				t.Fatal("drifted settlement was accepted")
			}
		})
	}
}

type recognitionLayoutSettlementMissingCapability struct {
	runtime k12.RecognitionLayoutPlanRuntimeV2
}

func (recognitionLayoutSettlementMissingCapability) ExecuteRecognitionPhysicalCall(
	_ context.Context,
	_ k12.RecognitionPhysicalCall,
	_ func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	return k12.RecognitionPhysicalCallResult{}, nil
}

func (f recognitionLayoutSettlementMissingCapability) LoadRecognitionLayoutPlanV2Runtime(
	context.Context,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	return f.runtime, nil
}
