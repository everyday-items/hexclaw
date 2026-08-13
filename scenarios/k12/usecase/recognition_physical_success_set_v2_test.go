package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-001：V2 识别成功集合只能根据
// 已经最终化的持久回执重建。物理顺序依次为清单、按方案顺序排列的主批次，
// 以及按候选顺序排列的实际授权单例修复。
func TestREGK12RecognitionDurabilityBudget20260808001PhysicalSuccessSetReplaysFinalizedV2ExactSet(
	t *testing.T,
) {
	ctx := context.Background()
	db, store := openRecognitionPhysicalExecutorV2Store(
		t,
		t.TempDir()+"/physical-success-set-v2.db",
	)
	defer db.Close()

	const agentName = "physical-success-set-v2-owner"
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
		"physical-success-set-v2-session",
		k12.GradingJobFields{
			SubmissionID: "physical-success-set-v2-submission",
			SourceKind:   "test",
			IdempotencyKey: k12.BuildGradingIdempotencyKey(
				"test",
				"physical-success-set-v2",
				0,
			),
			ModelSnapshot:     route,
			ConfirmationState: k12.GradingConfirmationPending,
			AnchorState:       k12.GradingAnchorPending,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, putErr := store.Put(ctx, job); putErr != nil {
		t.Fatalf("persist grading job: %v", putErr)
	}

	sourceImage := recognitionPhysicalExecutorV2PagePNG(t, 120, 500)
	canonicalPage, err := k12.CanonicalizeRecognitionPageV2(sourceImage)
	if err != nil {
		t.Fatalf("canonicalize source page: %v", err)
	}
	parent := k12.ModelInvocation{
		InvocationID:          "modelinv-physical-success-set-v2",
		AgentName:             agentName,
		JobID:                 job.RecordID,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         recognitionPhysicalExecutorV2Digest("physical-success-set-v2-parent"),
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               1,
		CreatedAt:             time.Now().Unix(),
	}
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-physical-success-set-v2",
		ParentInvocationID:       parent.InvocationID,
		AgentName:                parent.AgentName,
		JobID:                    parent.JobID,
		PageDigest:               canonicalPage.Digest,
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
		t.Fatalf("digest layout header: %v", err)
	}
	manifestCall := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  headerDigest,
		Unit:        k12.RecognitionPhysicalUnitWholePage,
		Image:       canonicalPage.PNG,
	}
	manifestID, err := stableRecognitionPhysicalInvocationIDForCall(
		parent.InvocationID,
		manifestCall,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifestRequestDigest, err := recognizingPhysicalInvocationDigest(
		parent,
		manifestCall,
	)
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
	manifest, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		agentName,
		manifestID,
		`{"targets":"five"}`,
		"",
	)
	if err != nil {
		t.Fatalf("succeed manifest: %v", err)
	}

	targets := make([]k12.RecognitionLayoutManifestTargetV2, 0, 5)
	for index := 0; index < 5; index++ {
		ordinal := index + 1
		targets = append(targets, k12.RecognitionLayoutManifestTargetV2{
			ManifestRef:      fmt.Sprintf("manifest_%04d", ordinal),
			ManifestOrder:    ordinal,
			SourceNumberPath: []string{fmt.Sprintf("%d", ordinal)},
			DisplayLabel:     fmt.Sprintf("%d", ordinal),
			Region: k12.SourcePixelRegion{
				X:      10,
				Y:      10 + index*95,
				Width:  100,
				Height: 70,
			},
		})
	}
	plan, err := k12.BuildRecognitionLayoutPlanV2(
		k12.RecognitionLayoutPlanInputV2{
			PagePNG: canonicalPage.PNG,
			Manifest: k12.RecognitionLayoutManifestSuccessV2{
				InvocationID: manifestID,
				ResultDigest: manifest.ResultDigest,
			},
			Targets: targets,
		},
	)
	if err != nil {
		t.Fatalf("build V2 layout plan: %v", err)
	}
	if len(plan.Batches) != 2 {
		t.Fatalf("plan batches=%d want=2", len(plan.Batches))
	}
	if authorizeErr := store.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		agentName,
		parent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: manifestID,
			ResultDigest: manifest.ResultDigest,
		},
		plan,
	); authorizeErr != nil {
		t.Fatalf("authorize plan: %v", authorizeErr)
	}

	orchestrator := &GradingOrchestrator{
		deps: Deps{Records: store, Now: time.Now().Unix},
	}
	executor := newDurableRecognitionPhysicalCallExecutor(
		orchestrator,
		storedParent,
	)
	durableCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(
			k12.WithGradingModelRequestPolicy(ctx, policy),
			headerDigest,
		),
		executor,
	)
	wantRepairs := map[string]bool{
		plan.Targets[0].TargetID: true,
		plan.Targets[4].TargetID: true,
	}
	repairAuthorizations := make(
		[]k12.RecognitionLayoutRepairAuthorizationV2,
		0,
		len(wantRepairs),
	)
	for batchIndex, batch := range plan.Batches {
		batchImage, batchImageErr := k12.BuildRecognitionLayoutBatchImageV2(
			canonicalPage.PNG,
			plan,
			batch.Unit,
		)
		if batchImageErr != nil {
			t.Fatalf("rebuild batch %d input: %v", batchIndex+1, batchImageErr)
		}
		batchCall := k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  plan.AuthorizedPlanDigest,
			Unit:        batch.Unit,
			TargetIDs:   append([]string(nil), batch.TargetIDs...),
			Image:       batchImage,
		}
		source, executeErr := executor.ExecuteRecognitionPhysicalCall(
			ctx,
			batchCall,
			func(context.Context) (string, error) {
				return fmt.Sprintf(`{"batch":%d}`, batchIndex+1), nil
			},
		)
		if executeErr != nil {
			t.Fatalf("execute batch %d: %v", batchIndex+1, executeErr)
		}
		settlement := k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			SourcePhysicalInvocationID: source.InvocationID,
			SourcePhysicalUnit:         batch.Unit,
			SourcePhysicalResultDigest: source.ResultDigest,
			Classification:             k12.RecognitionLayoutBatchClassifiedV2,
			Candidates: make(
				[]k12.RecognitionLayoutCandidateSettlementV2,
				0,
				len(batch.TargetIDs),
			),
		}
		for _, candidateID := range batch.TargetIDs {
			candidate := k12.RecognitionLayoutCandidateSettlementV2{
				CandidateID: candidateID,
			}
			if wantRepairs[candidateID] {
				candidate.Classification = k12.RecognitionLayoutCandidateMissingV2
			} else {
				candidate.Classification = k12.RecognitionLayoutCandidateValidV2
				candidate.ResultKind = k12.RecognitionLayoutCandidateQuestionV2
				candidate.ResultJSON = json.RawMessage(
					`{"student_answer":"4","text":"2+2"}`,
				)
			}
			settlement.Candidates = append(settlement.Candidates, candidate)
		}
		projection, settlementCreated, settleErr :=
			k12.SettleRecognitionLayoutPrimaryBatchV2(
				durableCtx,
				source,
				settlement,
			)
		if settleErr != nil || !settlementCreated {
			t.Fatalf(
				"settle batch %d: created=%v projection=%+v err=%v",
				batchIndex+1,
				settlementCreated,
				projection,
				settleErr,
			)
		}
		repairAuthorizations = append(
			repairAuthorizations,
			projection.RepairAuthorizations...,
		)
	}
	if len(repairAuthorizations) != 2 {
		t.Fatalf("repair authorizations=%d want=2", len(repairAuthorizations))
	}
	// 按逆序结算。最终化仍必须按已授权候选顺序投影修复，绝不能按完成顺序投影。
	for index := len(repairAuthorizations) - 1; index >= 0; index-- {
		authorization := repairAuthorizations[index]
		repairImage, repairImageErr := k12.BuildRecognitionLayoutRepairImageV2(
			canonicalPage.PNG,
			plan,
			authorization.CandidateID,
		)
		if repairImageErr != nil {
			t.Fatalf("rebuild repair input: %v", repairImageErr)
		}
		repairCall := k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  plan.AuthorizedPlanDigest,
			Unit:        authorization.PhysicalUnit,
			TargetIDs:   []string{authorization.CandidateID},
			Image:       repairImage,
		}
		source, executeErr := executor.ExecuteRecognitionPhysicalCall(
			ctx,
			repairCall,
			func(context.Context) (string, error) {
				return `{"repair":"valid"}`, nil
			},
		)
		if executeErr != nil {
			t.Fatalf("execute repair %s: %v", authorization.PhysicalUnit, executeErr)
		}
		projection, settlementCreated, settleErr := k12.SettleRecognitionLayoutRepairV2(
			durableCtx,
			source,
			k12.RecognitionLayoutRepairSettlementV2{
				PlanDigest:                 plan.AuthorizedPlanDigest,
				AuthorizationID:            authorization.AuthorizationID,
				AuthorizationDigest:        authorization.AuthorizationDigest,
				CandidateID:                authorization.CandidateID,
				SourcePhysicalInvocationID: source.InvocationID,
				SourcePhysicalUnit:         authorization.PhysicalUnit,
				SourcePhysicalResultDigest: source.ResultDigest,
				Classification:             k12.RecognitionLayoutCandidateValidV2,
				ResultKind:                 k12.RecognitionLayoutCandidateQuestionV2,
				ResultJSON: json.RawMessage(
					`{"student_answer":"4","text":"2+2"}`,
				),
			},
		)
		if settleErr != nil || !settlementCreated || projection.FrozenResult == nil {
			t.Fatalf(
				"settle repair %s: created=%v projection=%+v err=%v",
				authorization.PhysicalUnit,
				settlementCreated,
				projection,
				settleErr,
			)
		}
	}

	finalized, finalizationCreated, err :=
		k12.FinalizeRecognitionLayoutPlanV2(durableCtx)
	if err != nil || !finalizationCreated {
		t.Fatalf(
			"create finalization receipt: created=%v result=%+v err=%v",
			finalizationCreated,
			finalized,
			err,
		)
	}
	if len(finalized.PhysicalResults) != 5 {
		t.Fatalf("finalized physical results=%d want=5", len(finalized.PhysicalResults))
	}

	got, err := orchestrator.recognitionPhysicalSuccessSet(
		ctx,
		storedParent,
		sourceImage,
	)
	if err != nil {
		t.Fatalf("replay finalized V2 success-set: %v", err)
	}
	if len(got) != len(finalized.PhysicalResults) {
		t.Fatalf("physical children=%d want=%d", len(got), len(finalized.PhysicalResults))
	}
	for index := range got {
		want := finalized.PhysicalResults[index]
		if got[index].PhysicalInvocationID != want.PhysicalInvocationID ||
			got[index].PhysicalUnit != want.PhysicalUnit ||
			got[index].ResultDigest != want.ResultDigest ||
			got[index].PlanDigest != want.PlanDigest ||
			got[index].CandidateExactSetDigest != want.CandidateExactSetDigest ||
			got[index].RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
			got[index].Status != k12.ModelInvocationSucceeded ||
			got[index].Attempt != 1 {
			t.Fatalf("physical child %d=%+v want evidence=%+v", index, got[index], want)
		}
	}
	wantUnits := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
		plan.Batches[0].Unit,
		plan.Batches[1].Unit,
		repairAuthorizations[0].PhysicalUnit,
		repairAuthorizations[1].PhysicalUnit,
	}
	gotUnits := make([]k12.RecognitionPhysicalUnit, len(got))
	for index := range got {
		gotUnits[index] = got[index].PhysicalUnit
	}
	if !reflect.DeepEqual(gotUnits, wantUnits) {
		t.Fatalf("physical order=%v want=%v", gotUnits, wantUnits)
	}

	replayed, replayCreated, err := k12.FinalizeRecognitionLayoutPlanV2(durableCtx)
	if err != nil || replayCreated || !reflect.DeepEqual(replayed, finalized) {
		t.Fatalf(
			"success-set mutated finalization receipt: created=%v result=%+v err=%v",
			replayCreated,
			replayed,
			err,
		)
	}

	beforeSucceededReplay, err := store.ListModelPhysicalInvocations(
		ctx,
		agentName,
		parent.JobID,
	)
	if err != nil {
		t.Fatalf("list physical receipts before succeeded-parent replay: %v", err)
	}
	succeededParent, err := store.MarkModelInvocationSucceeded(
		ctx,
		agentName,
		parent.InvocationID,
		recognitionPhysicalExecutorV2Digest("recognized-parent-result"),
		"",
	)
	if err != nil {
		t.Fatalf("mark recognizing parent succeeded: %v", err)
	}
	recovered, err := orchestrator.recognitionPhysicalSuccessSet(
		ctx,
		succeededParent,
		sourceImage,
	)
	if err != nil || !reflect.DeepEqual(recovered, got) {
		t.Fatalf("replay succeeded parent without Provider: result=%+v err=%v", recovered, err)
	}
	afterSucceededReplay, err := store.ListModelPhysicalInvocations(
		ctx,
		agentName,
		parent.JobID,
	)
	if err != nil || !reflect.DeepEqual(afterSucceededReplay, beforeSucceededReplay) {
		t.Fatalf(
			"succeeded-parent replay changed physical calls: before=%+v after=%+v err=%v",
			beforeSucceededReplay,
			afterSucceededReplay,
			err,
		)
	}

	differentPage := recognitionPhysicalExecutorV2PagePNG(t, 121, 500)
	if _, err := orchestrator.recognitionPhysicalSuccessSet(
		ctx,
		succeededParent,
		differentPage,
	); err == nil {
		t.Fatal("V2 success-set accepted source pixels detached from the canonical page digest")
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001SucceededParentFinalizationIsReplayOnly(
	t *testing.T,
) {
	if err := validateRecognitionLayoutFinalizationModeV2(
		k12.ModelInvocationSucceeded,
		"running",
		false,
	); err == nil {
		t.Fatal("succeeded parent accepted a running layout plan")
	}
	if err := validateRecognitionLayoutFinalizationModeV2(
		k12.ModelInvocationSucceeded,
		"succeeded",
		true,
	); err == nil {
		t.Fatal("succeeded parent accepted a newly-created finalization receipt")
	}
	if err := validateRecognitionLayoutFinalizationModeV2(
		k12.ModelInvocationSucceeded,
		"succeeded",
		false,
	); err != nil {
		t.Fatalf("succeeded parent rejected read-only finalization replay: %v", err)
	}
}
