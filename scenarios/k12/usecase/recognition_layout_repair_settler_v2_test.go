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

// REG-K12-RECOGNITION-BATCH-REPAIR-20260808-001：解析器只能通过绑定到准确的
// 成功修复子项及其不可变首轮授权的持久化执行器，结算单例修复。
func TestREGK12RecognitionBatchRepair20260808001SettlesRepairThroughDurableExecutor(
	t *testing.T,
) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/repair-settler-v2.db"
	db, store := openRecognitionPhysicalExecutorV2Store(t, dbPath)
	dbOpen := true
	defer func() {
		if dbOpen {
			_ = db.Close()
		}
	}()

	const agentName = "repair-settlement-owner"
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO agents(name) VALUES(?)`,
		agentName,
	); err != nil {
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
		"repair-settlement-session",
		k12.GradingJobFields{
			SubmissionID:      "repair-settlement-submission",
			SourceKind:        "test",
			IdempotencyKey:    k12.BuildGradingIdempotencyKey("test", "repair-settlement", 0),
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
		InvocationID:          "modelinv-repair-settler-v2",
		AgentName:             agentName,
		JobID:                 job.RecordID,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         recognitionPhysicalExecutorV2Digest("repair-settlement-parent"),
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               1,
		CreatedAt:             time.Now().Unix(),
	}
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-repair-settler-v2",
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
	storedParent, _, created, err :=
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
	primaryCall := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  plan.AuthorizedPlanDigest,
		Unit:        batch.Unit,
		TargetIDs:   append([]string(nil), batch.TargetIDs...),
		Image:       []byte("repairable-primary-contact-sheet"),
	}
	primarySource, err := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		primaryCall,
		func(context.Context) (string, error) {
			return `{"results":"missing"}`, nil
		},
	)
	if err != nil {
		t.Fatalf("execute primary batch: %v", err)
	}
	primaryProjection, primaryCreated, err := k12.SettleRecognitionLayoutPrimaryBatchV2(
		durableCtx,
		primarySource,
		k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			SourcePhysicalInvocationID: primarySource.InvocationID,
			SourcePhysicalUnit:         batch.Unit,
			SourcePhysicalResultDigest: primarySource.ResultDigest,
			Classification:             k12.RecognitionLayoutBatchClassifiedV2,
			Candidates: []k12.RecognitionLayoutCandidateSettlementV2{
				{
					CandidateID:    batch.TargetIDs[0],
					Classification: k12.RecognitionLayoutCandidateMissingV2,
				},
			},
		},
	)
	if err != nil || !primaryCreated || len(primaryProjection.RepairAuthorizations) != 1 {
		t.Fatalf(
			"authorize singleton repair: created=%v result=%+v err=%v",
			primaryCreated,
			primaryProjection,
			err,
		)
	}
	authorization := primaryProjection.RepairAuthorizations[0]
	repairCall := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		// 修复子项仍绑定到已授权方案，其修复授权摘要是独立的结算事实。
		PlanDigest: plan.AuthorizedPlanDigest,
		Unit:       authorization.PhysicalUnit,
		TargetIDs:  []string{authorization.CandidateID},
		Image:      []byte("singleton-repair-crop"),
	}
	var providerSends atomic.Int32
	repairSource, err := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		repairCall,
		func(context.Context) (string, error) {
			providerSends.Add(1)
			return `{"results":"repaired"}`, nil
		},
	)
	if err != nil || providerSends.Load() != 1 {
		t.Fatalf(
			"execute singleton repair: source=%+v sends=%d err=%v",
			repairSource,
			providerSends.Load(),
			err,
		)
	}
	settlement := k12.RecognitionLayoutRepairSettlementV2{
		PlanDigest:                 plan.AuthorizedPlanDigest,
		AuthorizationID:            authorization.AuthorizationID,
		AuthorizationDigest:        authorization.AuthorizationDigest,
		CandidateID:                authorization.CandidateID,
		SourcePhysicalInvocationID: repairSource.InvocationID,
		SourcePhysicalUnit:         authorization.PhysicalUnit,
		SourcePhysicalResultDigest: repairSource.ResultDigest,
		Classification:             k12.RecognitionLayoutCandidateValidV2,
		ResultKind:                 k12.RecognitionLayoutCandidateQuestionV2,
		ResultJSON:                 json.RawMessage(`{"student_answer":"4","text":"2+2"}`),
	}
	first, repairCreated, err := k12.SettleRecognitionLayoutRepairV2(
		durableCtx,
		repairSource,
		settlement,
	)
	if err != nil || !repairCreated || first.FrozenResult == nil ||
		first.FrozenResult.CandidateID != authorization.CandidateID ||
		first.UnresolvedCandidateID != "" {
		t.Fatalf(
			"settle singleton repair: created=%v result=%+v err=%v",
			repairCreated,
			first,
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
	var restartSends atomic.Int32
	replayedSource, err := restartedExecutor.ExecuteRecognitionPhysicalCall(
		ctx,
		repairCall,
		func(context.Context) (string, error) {
			restartSends.Add(1)
			return "must-not-send", nil
		},
	)
	if err != nil || restartSends.Load() != 0 || replayedSource != repairSource {
		t.Fatalf(
			"replay succeeded repair source: source=%+v sends=%d err=%v",
			replayedSource,
			restartSends.Load(),
			err,
		)
	}
	replayed, replayCreated, err := k12.SettleRecognitionLayoutRepairV2(
		restartedCtx,
		replayedSource,
		settlement,
	)
	if err != nil || replayCreated || !reflect.DeepEqual(replayed, first) ||
		providerSends.Load() != 1 {
		t.Fatalf(
			"exact repair settlement replay: created=%v result=%+v sends=%d err=%v",
			replayCreated,
			replayed,
			providerSends.Load(),
			err,
		)
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
	if _, _, err := k12.SettleRecognitionLayoutRepairV2(
		missingCapabilityCtx,
		repairSource,
		settlement,
	); err == nil {
		t.Fatal("approved V2 context accepted an executor without repair settlement capability")
	}

	driftedParent := restartedParent
	driftedParent.AgentName = "different-owner"
	driftedOwnerCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(ctx, headerDigest),
		newDurableRecognitionPhysicalCallExecutor(
			&GradingOrchestrator{deps: Deps{Records: restartedStore}},
			driftedParent,
		),
	)
	if _, _, err := k12.SettleRecognitionLayoutRepairV2(
		driftedOwnerCtx,
		repairSource,
		settlement,
	); err == nil {
		t.Fatal("repair settlement accepted a drifted parent owner")
	}

	driftTests := []struct {
		name       string
		callCtx    context.Context
		source     k12.RecognitionPhysicalCallResult
		settlement k12.RecognitionLayoutRepairSettlementV2
	}{
		{
			name:       "header",
			callCtx:    k12.WithRecognitionPhysicalCallExecutor(k12.WithRecognitionLayoutPlanV2(ctx, recognitionPhysicalExecutorV2Digest("other-header")), restartedExecutor),
			source:     repairSource,
			settlement: settlement,
		},
		{
			name:    "plan",
			callCtx: restartedCtx,
			source:  repairSource,
			settlement: func() k12.RecognitionLayoutRepairSettlementV2 {
				changed := settlement
				changed.PlanDigest = recognitionPhysicalExecutorV2Digest("other-plan")
				return changed
			}(),
		},
		{
			name:    "authorization_id",
			callCtx: restartedCtx,
			source:  repairSource,
			settlement: func() k12.RecognitionLayoutRepairSettlementV2 {
				changed := settlement
				changed.AuthorizationID = "repair-auth-v2-different"
				return changed
			}(),
		},
		{
			name:    "authorization_digest",
			callCtx: restartedCtx,
			source:  repairSource,
			settlement: func() k12.RecognitionLayoutRepairSettlementV2 {
				changed := settlement
				changed.AuthorizationDigest = recognitionPhysicalExecutorV2Digest("other-authorization")
				return changed
			}(),
		},
		{
			name:    "candidate",
			callCtx: restartedCtx,
			source:  repairSource,
			settlement: func() k12.RecognitionLayoutRepairSettlementV2 {
				changed := settlement
				changed.CandidateID = "different-candidate"
				return changed
			}(),
		},
		{
			name:    "source_argument",
			callCtx: restartedCtx,
			source: func() k12.RecognitionPhysicalCallResult {
				changed := repairSource
				changed.InvocationID = "different-source"
				return changed
			}(),
			settlement: settlement,
		},
		{
			name:    "source_result_argument",
			callCtx: restartedCtx,
			source: func() k12.RecognitionPhysicalCallResult {
				changed := repairSource
				changed.ResultDigest = recognitionPhysicalExecutorV2Digest("different-source-result")
				return changed
			}(),
			settlement: settlement,
		},
		{
			name:    "source_invocation",
			callCtx: restartedCtx,
			source:  repairSource,
			settlement: func() k12.RecognitionLayoutRepairSettlementV2 {
				changed := settlement
				changed.SourcePhysicalInvocationID = "different-source"
				return changed
			}(),
		},
		{
			name:    "source_unit",
			callCtx: restartedCtx,
			source:  repairSource,
			settlement: func() k12.RecognitionLayoutRepairSettlementV2 {
				changed := settlement
				changed.SourcePhysicalUnit = k12.RecognitionPhysicalUnit("layout_repair_0002")
				return changed
			}(),
		},
		{
			name:    "source_result",
			callCtx: restartedCtx,
			source:  repairSource,
			settlement: func() k12.RecognitionLayoutRepairSettlementV2 {
				changed := settlement
				changed.SourcePhysicalResultDigest = recognitionPhysicalExecutorV2Digest("different-source-result")
				return changed
			}(),
		},
	}
	for _, test := range driftTests {
		t.Run(test.name+"_drift", func(t *testing.T) {
			if _, _, err := k12.SettleRecognitionLayoutRepairV2(
				test.callCtx,
				test.source,
				test.settlement,
			); err == nil {
				t.Fatal("drifted repair settlement was accepted")
			}
		})
	}
}
