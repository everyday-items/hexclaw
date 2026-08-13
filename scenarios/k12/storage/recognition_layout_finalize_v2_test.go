package k12storage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-001：终态化是 Store 所有的
// 精确集合证明。调用方不提供候选或物理结果；Store 按计划顺序从不可变 V2 证据中重建二者。
func TestREGK12RecognitionDurabilityBudget20260808001FinalizesExactV2Plan(
	t *testing.T,
) {
	ctx := context.Background()
	store, db, path, parent, plan := prepareRecognitionLayoutFinalizableFixture(t, ctx)

	want, created, err := store.FinalizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || !created {
		t.Fatalf("finalize exact V2 plan: created=%v result=%+v err=%v", created, want, err)
	}
	if want.PlanID == "" || want.PlanDigest != plan.AuthorizedPlanDigest ||
		want.CandidateExactSetDigest == "" ||
		want.CandidateResultsExactSetDigest == "" ||
		want.PhysicalResultsExactSetDigest == "" || want.FinalizationDigest == "" ||
		want.CandidateResultCount != len(plan.Targets) ||
		want.PhysicalResultCount != 1+len(plan.Batches)+2 ||
		len(want.CandidateResults) != len(plan.Targets) ||
		len(want.PhysicalResults) != want.PhysicalResultCount {
		t.Fatalf("incomplete finalization projection: %+v", want)
	}
	wantCandidateDigest, err := k12.RecognitionLayoutCandidateResultsExactSetDigestV2(
		want.CandidateResults,
	)
	if err != nil || wantCandidateDigest != want.CandidateResultsExactSetDigest {
		t.Fatalf("shared candidate exact-set digest drifted: digest=%s err=%v", wantCandidateDigest, err)
	}
	wantPhysicalDigest, err := k12.RecognitionLayoutPhysicalResultsExactSetDigestV2(
		want.PhysicalResults,
	)
	if err != nil || wantPhysicalDigest != want.PhysicalResultsExactSetDigest {
		t.Fatalf("shared physical exact-set digest drifted: digest=%s err=%v", wantPhysicalDigest, err)
	}
	canonicalFinalization, wantFinalizationDigest, err :=
		k12.CanonicalRecognitionLayoutPlanFinalizationV2(
			parent.InvocationID,
			want,
		)
	if err != nil || len(canonicalFinalization) == 0 ||
		wantFinalizationDigest != want.FinalizationDigest {
		t.Fatalf("shared finalization digest drifted: digest=%s err=%v", wantFinalizationDigest, err)
	}
	for index, target := range plan.Targets {
		result := want.CandidateResults[index]
		if result.CandidateID != target.TargetID || result.ResultDigest == "" ||
			(result.ResultKind != k12.RecognitionLayoutCandidateQuestionV2 &&
				result.ResultKind != k12.RecognitionLayoutCandidateNonQuestionV2) ||
			len(result.ResultJSON) == 0 ||
			result.SourcePhysicalInvocationID == "" ||
			result.SourcePhysicalResultDigest == "" {
			t.Fatalf("candidate result %d is not the plan-order typed result: %+v", index+1, result)
		}
	}
	wantUnits := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitWholePage,
		plan.Batches[0].Unit,
		plan.Batches[1].Unit,
	}
	for ordinal := 1; ordinal <= 2; ordinal++ {
		unit, unitErr := k12.RecognitionLayoutRepairUnitV2(ordinal)
		if unitErr != nil {
			t.Fatal(unitErr)
		}
		wantUnits = append(wantUnits, unit)
	}
	for index, evidence := range want.PhysicalResults {
		if evidence.PhysicalUnit != wantUnits[index] ||
			evidence.PhysicalInvocationID == "" || evidence.ResultDigest == "" ||
			evidence.Attempt != 1 {
			t.Fatalf("physical evidence %d drifted: %+v", index+1, evidence)
		}
	}
	assertRecognitionLayoutFinalizationRow(t, ctx, db, want, "succeeded")
	runtime, err := store.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || runtime.Status != "succeeded" || runtime.AuthorizedPlan == nil ||
		!reflect.DeepEqual(*runtime.AuthorizedPlan, plan) {
		t.Fatalf("succeeded runtime was not restart-readable: runtime=%+v err=%v", runtime, err)
	}
	if authorizeErr := store.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: plan.ManifestInvocationID,
			ResultDigest: plan.ManifestResultDigest,
		},
		plan,
	); authorizeErr != nil {
		t.Fatalf("succeeded exact authorization replay failed: %v", authorizeErr)
	}
	driftedPlan := plan
	driftedPlan.Targets = append([]k12.RecognitionLayoutTargetV2(nil), plan.Targets...)
	driftedPlan.Targets[0].DisplayLabel = "drifted"
	if authorizeErr := store.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: plan.ManifestInvocationID,
			ResultDigest: plan.ManifestResultDigest,
		},
		driftedPlan,
	); authorizeErr == nil {
		t.Fatal("succeeded authorization replay accepted a drifted plan")
	}
	if _, execErr := db.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_finalizations
            SET finalization_digest='sha256:mutated' WHERE plan_id=?`,
		want.PlanID,
	); execErr == nil {
		t.Fatal("immutable finalization receipt was mutable")
	}

	replayed, replayCreated, err := store.FinalizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || replayCreated || !reflect.DeepEqual(replayed, want) {
		t.Fatalf("exact finalization replay drifted: created=%v result=%+v err=%v", replayCreated, replayed, err)
	}
	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close finalization database: %v", closeErr)
	}
	store, db = openPhysicalLedgerFileStore(t, path)
	defer db.Close()
	restarted, restartCreated, err := store.FinalizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || restartCreated || !reflect.DeepEqual(restarted, want) {
		t.Fatalf("restart finalization replay drifted: created=%v result=%+v err=%v", restartCreated, restarted, err)
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001FinalizationFailsClosed(
	t *testing.T,
) {
	t.Run("missing primary settlement", func(t *testing.T) {
		ctx := context.Background()
		store, db, _, parent, _, _ := prepareRecognitionLayoutSettlementFixture(t, ctx)
		defer db.Close()
		if _, _, err := store.FinalizeRecognitionLayoutPlanV2(
			ctx,
			parent.AgentName,
			parent.InvocationID,
		); err == nil {
			t.Fatal("plan without primary settlements finalized")
		}
		assertRecognitionLayoutNoFinalization(t, ctx, db)
	})

	t.Run("terminal primary ambiguity", func(t *testing.T) {
		ctx := context.Background()
		store, db, _, parent, plan, batches := prepareRecognitionLayoutSettlementFixture(t, ctx)
		defer db.Close()
		settleRecognitionLayoutAllPrimaryValid(t, ctx, store, parent, plan, batches[1])
		ambiguous := k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			SourcePhysicalInvocationID: batches[0].PhysicalInvocationID,
			SourcePhysicalUnit:         batches[0].PhysicalUnit,
			SourcePhysicalResultDigest: batches[0].ResultDigest,
			Classification:             k12.RecognitionLayoutBatchTerminalAmbiguousV2,
			AmbiguityKind:              k12.RecognitionLayoutAmbiguitySourceConflictV2,
		}
		if _, created, err := store.SettleRecognitionLayoutPrimaryBatchV2(
			ctx,
			parent.AgentName,
			parent.InvocationID,
			ambiguous,
		); err != nil || !created {
			t.Fatalf("persist terminal ambiguity fixture: created=%v err=%v", created, err)
		}
		if _, _, err := store.FinalizeRecognitionLayoutPlanV2(
			ctx,
			parent.AgentName,
			parent.InvocationID,
		); err == nil {
			t.Fatal("terminally ambiguous primary batch finalized")
		}
		assertRecognitionLayoutNoFinalization(t, ctx, db)
	})

	t.Run("missing repair settlement", func(t *testing.T) {
		ctx := context.Background()
		store, db, _, parent, plan, batches := prepareRecognitionLayoutSettlementFixture(t, ctx)
		defer db.Close()
		if _, created, err := store.SettleRecognitionLayoutPrimaryBatchV2(
			ctx,
			parent.AgentName,
			parent.InvocationID,
			recognitionLayoutRepairablePrimarySettlement(plan, batches[0]),
		); err != nil || !created {
			t.Fatalf("persist repairable primary fixture: created=%v err=%v", created, err)
		}
		settleRecognitionLayoutAllPrimaryValid(t, ctx, store, parent, plan, batches[1])
		if _, _, err := store.FinalizeRecognitionLayoutPlanV2(
			ctx,
			parent.AgentName,
			parent.InvocationID,
		); err == nil {
			t.Fatal("authorized repairs without settlements finalized")
		}
		assertRecognitionLayoutNoFinalization(t, ctx, db)
	})

	t.Run("terminal invalid repair", func(t *testing.T) {
		ctx := context.Background()
		store, db, _, parent, plan, batches := prepareRecognitionLayoutSettlementFixture(t, ctx)
		defer db.Close()
		projection, created, err := store.SettleRecognitionLayoutPrimaryBatchV2(
			ctx,
			parent.AgentName,
			parent.InvocationID,
			recognitionLayoutRepairablePrimarySettlement(plan, batches[0]),
		)
		if err != nil || !created {
			t.Fatalf("persist repairable primary fixture: created=%v err=%v", created, err)
		}
		settleRecognitionLayoutAllPrimaryValid(t, ctx, store, parent, plan, batches[1])
		for index, authorization := range projection.RepairAuthorizations {
			classification := k12.RecognitionLayoutCandidateValidV2
			if index == 0 {
				classification = k12.RecognitionLayoutCandidateInvalidV2
			}
			settleRecognitionLayoutFinalizationRepair(
				t,
				ctx,
				store,
				parent,
				plan,
				authorization,
				classification,
			)
		}
		if _, _, err := store.FinalizeRecognitionLayoutPlanV2(
			ctx,
			parent.AgentName,
			parent.InvocationID,
		); err == nil {
			t.Fatal("terminal invalid repair finalized")
		}
		assertRecognitionLayoutNoFinalization(t, ctx, db)
	})
}

func TestREGK12RecognitionDurabilityBudget20260808001FinalizationRejectsExtraPhysicalChild(
	t *testing.T,
) {
	ctx := context.Background()
	store, db, _, parent, _ := prepareRecognitionLayoutFinalizableFixture(t, ctx)
	defer db.Close()
	if _, err := db.ExecContext(ctx, `
        INSERT INTO k12_model_physical_invocations (
            physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,
            physical_unit,request_digest,route_snapshot_json,
            request_policy_snapshot_json,status,attempt,result_digest,result_content,
            external_request_id,failure_kind,created_at,updated_at,
            recognition_plan_version,plan_digest,candidate_exact_set_digest
        )
        SELECT 'physical-layout-extra',parent_invocation_id,agent_name,job_id,stage,
               'layout_batch_9999',request_digest,route_snapshot_json,
               request_policy_snapshot_json,'succeeded',1,result_digest,result_content,
               external_request_id,'',created_at,updated_at,
               'v2',plan_digest,candidate_exact_set_digest
          FROM k12_model_physical_invocations
         WHERE parent_invocation_id=? AND physical_unit='layout_batch_0001'`,
		parent.InvocationID,
	); err != nil {
		t.Fatalf("inject extra V2 child fixture: %v", err)
	}
	if _, _, err := store.FinalizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	); err == nil {
		t.Fatal("plan with an extra V2 physical child finalized")
	}
	assertRecognitionLayoutNoFinalization(t, ctx, db)
}

func TestREGK12RecognitionDurabilityBudget20260808001FinalizationRejectsNonSucceededPhysicalChild(
	t *testing.T,
) {
	for _, status := range []k12.ModelInvocationStatus{
		k12.ModelInvocationPrepared,
		k12.ModelInvocationSent,
		k12.ModelInvocationFailed,
		k12.ModelInvocationOutcomeUnknown,
	} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			store, db, _, parent, plan := prepareRecognitionLayoutFinalizableFixture(t, ctx)
			defer db.Close()
			failureKind := ""
			if status == k12.ModelInvocationFailed ||
				status == k12.ModelInvocationOutcomeUnknown {
				failureKind = "injected_terminal_state"
			}
			if _, err := db.ExecContext(
				ctx,
				`UPDATE k12_model_physical_invocations
                    SET status=?,result_content=NULL,failure_kind=?
                  WHERE parent_invocation_id=? AND physical_unit=?`,
				status,
				failureKind,
				parent.InvocationID,
				plan.Batches[0].Unit,
			); err != nil {
				t.Fatalf("inject physical status %s: %v", status, err)
			}
			if _, _, err := store.FinalizeRecognitionLayoutPlanV2(
				ctx,
				parent.AgentName,
				parent.InvocationID,
			); err == nil {
				t.Fatalf("plan with %s authorized physical child finalized", status)
			}
			assertRecognitionLayoutNoFinalization(t, ctx, db)
		})
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001FinalizationRejectsCandidateResultTamper(
	t *testing.T,
) {
	for _, column := range []string{"result_digest", "source_physical_result_digest"} {
		t.Run(column, func(t *testing.T) {
			ctx := context.Background()
			store, db, _, parent, plan := prepareRecognitionLayoutFinalizableFixture(t, ctx)
			defer db.Close()
			finalized, created, err := store.FinalizeRecognitionLayoutPlanV2(
				ctx,
				parent.AgentName,
				parent.InvocationID,
			)
			if err != nil || !created {
				t.Fatalf("finalize before tamper: created=%v err=%v", created, err)
			}
			if _, err := db.ExecContext(
				ctx,
				`DROP TRIGGER k12_recognition_layout_candidate_result_immutable`,
			); err != nil {
				t.Fatalf("drop candidate-result immutability guard: %v", err)
			}
			query := `UPDATE k12_recognition_layout_candidate_results
                         SET ` + column + `=? WHERE candidate_id=?`
			if _, err := db.ExecContext(
				ctx,
				query,
				recognitionLayoutRuntimeTestDigest("tampered candidate result"),
				plan.Targets[0].TargetID,
			); err != nil {
				t.Fatalf("tamper candidate %s: %v", column, err)
			}
			if _, _, err := store.FinalizeRecognitionLayoutPlanV2(
				ctx,
				parent.AgentName,
				parent.InvocationID,
			); err == nil {
				t.Fatalf("succeeded replay trusted tampered candidate %s", column)
			}
			assertRecognitionLayoutFinalizationRow(t, ctx, db, finalized, "succeeded")
		})
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001FinalizationReplayRejectsMissingReceipt(
	t *testing.T,
) {
	ctx := context.Background()
	store, db, _, parent, _ := prepareRecognitionLayoutFinalizableFixture(t, ctx)
	defer db.Close()
	if _, created, err := store.FinalizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	); err != nil || !created {
		t.Fatalf("finalize missing-receipt fixture: created=%v err=%v", created, err)
	}
	if _, err := db.ExecContext(
		ctx,
		`DELETE FROM k12_recognition_layout_finalizations
          WHERE parent_invocation_id=?`,
		parent.InvocationID,
	); err != nil {
		t.Fatalf("remove finalization receipt fixture: %v", err)
	}
	if _, _, err := store.FinalizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	); err == nil {
		t.Fatal("succeeded plan silently recreated a missing finalization receipt")
	}
	var status string
	var receipts int
	if err := db.QueryRowContext(
		ctx,
		`SELECT status FROM k12_recognition_layout_plans
          WHERE parent_invocation_id=?`,
		parent.InvocationID,
	).Scan(&status); err != nil {
		t.Fatalf("read missing-receipt plan: %v", err)
	}
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_finalizations`,
	).Scan(&receipts); err != nil {
		t.Fatalf("count missing finalization receipt: %v", err)
	}
	if status != "succeeded" || receipts != 0 {
		t.Fatalf("missing receipt recovery mutated state: status=%s receipts=%d", status, receipts)
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001ConcurrentFinalizationOneWinner(
	t *testing.T,
) {
	ctx := context.Background()
	store, db, _, parent, _ := prepareRecognitionLayoutFinalizableFixture(t, ctx)
	defer db.Close()
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enable finalization WAL: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		t.Fatalf("set finalization busy timeout: %v", err)
	}
	db.SetMaxOpenConns(8)

	const workers = 8
	start := make(chan struct{})
	errCh := make(chan error, workers)
	results := make(chan k12.RecognitionLayoutPlanFinalizationResultV2, workers)
	var createdCount atomic.Int32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, created, err := store.FinalizeRecognitionLayoutPlanV2(
				ctx,
				parent.AgentName,
				parent.InvocationID,
			)
			if err != nil {
				errCh <- err
				return
			}
			if created {
				createdCount.Add(1)
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	close(results)
	for err := range errCh {
		t.Errorf("concurrent finalization: %v", err)
	}
	if createdCount.Load() != 1 {
		t.Fatalf("concurrent finalization winners=%d, want 1", createdCount.Load())
	}
	var first *k12.RecognitionLayoutPlanFinalizationResultV2
	for result := range results {
		if first == nil {
			copy := result
			first = &copy
			continue
		}
		if !reflect.DeepEqual(result, *first) {
			t.Errorf("concurrent finalization projection drift: %+v vs %+v", result, *first)
		}
	}
}

func prepareRecognitionLayoutFinalizableFixture(
	t *testing.T,
	ctx context.Context,
) (
	*k12storage.Store,
	*sql.DB,
	string,
	k12.ModelInvocation,
	k12.RecognitionLayoutPlanV2,
) {
	t.Helper()
	store, db, path, parent, plan, batches := prepareRecognitionLayoutSettlementFixture(t, ctx)
	projection, created, err := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		recognitionLayoutRepairablePrimarySettlement(plan, batches[0]),
	)
	if err != nil || !created || len(projection.RepairAuthorizations) != 2 {
		t.Fatalf("settle repairable primary: created=%v result=%+v err=%v", created, projection, err)
	}
	settleRecognitionLayoutAllPrimaryValid(t, ctx, store, parent, plan, batches[1])
	for _, authorization := range projection.RepairAuthorizations {
		settleRecognitionLayoutFinalizationRepair(
			t,
			ctx,
			store,
			parent,
			plan,
			authorization,
			k12.RecognitionLayoutCandidateValidV2,
		)
	}
	return store, db, path, parent, plan
}

func settleRecognitionLayoutAllPrimaryValid(
	t *testing.T,
	ctx context.Context,
	store *k12storage.Store,
	parent k12.ModelInvocation,
	plan k12.RecognitionLayoutPlanV2,
	batch k12.ModelPhysicalInvocation,
) {
	t.Helper()
	var authorizedBatch k12.RecognitionLayoutBatchV2
	for _, candidateBatch := range plan.Batches {
		if candidateBatch.Unit == batch.PhysicalUnit {
			authorizedBatch = candidateBatch
			break
		}
	}
	candidates := make([]k12.RecognitionLayoutCandidateSettlementV2, len(authorizedBatch.TargetIDs))
	for index, candidateID := range authorizedBatch.TargetIDs {
		candidates[index] = k12.RecognitionLayoutCandidateSettlementV2{
			CandidateID:    candidateID,
			Classification: k12.RecognitionLayoutCandidateValidV2,
			ResultKind:     k12.RecognitionLayoutCandidateQuestionV2,
			ResultJSON:     json.RawMessage(`{"text":"primary-final"}`),
		}
	}
	settlement := k12.RecognitionLayoutPrimaryBatchSettlementV2{
		PlanDigest:                 plan.AuthorizedPlanDigest,
		SourcePhysicalInvocationID: batch.PhysicalInvocationID,
		SourcePhysicalUnit:         batch.PhysicalUnit,
		SourcePhysicalResultDigest: batch.ResultDigest,
		Classification:             k12.RecognitionLayoutBatchClassifiedV2,
		Candidates:                 candidates,
	}
	if _, created, err := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		settlement,
	); err != nil || !created {
		t.Fatalf("settle all-valid primary: created=%v err=%v", created, err)
	}
}

func settleRecognitionLayoutFinalizationRepair(
	t *testing.T,
	ctx context.Context,
	store *k12storage.Store,
	parent k12.ModelInvocation,
	plan k12.RecognitionLayoutPlanV2,
	authorization k12.RecognitionLayoutRepairAuthorizationV2,
	classification k12.RecognitionLayoutCandidateClassificationV2,
) {
	t.Helper()
	exactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(
		[]string{authorization.CandidateID},
	)
	if err != nil {
		t.Fatal(err)
	}
	repair := newPhysicalInvocation(
		parent,
		"physical-finalization-"+string(authorization.PhysicalUnit),
		authorization.PhysicalUnit,
	)
	repair.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
	repair.PlanDigest = plan.AuthorizedPlanDigest
	repair.CandidateExactSetDigest = exactSetDigest
	prepared, created, err := store.PrepareModelPhysicalInvocation(ctx, repair)
	if err != nil || !created {
		t.Fatalf("prepare finalization repair: created=%v err=%v", created, err)
	}
	if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		prepared.PhysicalInvocationID,
	); claimErr != nil || !claimed {
		t.Fatalf("claim finalization repair: claimed=%v err=%v", claimed, claimErr)
	}
	succeeded, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		parent.AgentName,
		prepared.PhysicalInvocationID,
		`{"repair":"final"}`,
		"provider-finalization-repair",
	)
	if err != nil {
		t.Fatalf("succeed finalization repair: %v", err)
	}
	settlement := recognitionLayoutRepairSettlementInput(
		plan,
		authorization,
		succeeded,
		classification,
	)
	if classification == k12.RecognitionLayoutCandidateValidV2 {
		settlement.ResultKind = k12.RecognitionLayoutCandidateQuestionV2
		settlement.ResultJSON = json.RawMessage(`{"text":"repaired-final"}`)
	}
	if _, settled, err := store.SettleRecognitionLayoutRepairV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		settlement,
	); err != nil || !settled {
		t.Fatalf("settle finalization repair: created=%v err=%v", settled, err)
	}
}

func assertRecognitionLayoutFinalizationRow(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	want k12.RecognitionLayoutPlanFinalizationResultV2,
	wantPlanStatus string,
) {
	t.Helper()
	var (
		planStatus, planDigest, candidateExactSetDigest string
		candidateResultsDigest, physicalResultsDigest   string
		finalizationDigest, finalizationJSON            string
		candidateCount, physicalCount                   int
	)
	if err := db.QueryRowContext(ctx, `
        SELECT plan.status,receipt.authorized_plan_digest,
               receipt.candidate_exact_set_digest,
               receipt.candidate_results_exact_set_digest,
               receipt.physical_results_exact_set_digest,
               receipt.candidate_result_count,receipt.physical_result_count,
               receipt.finalization_digest,receipt.finalization_json
          FROM k12_recognition_layout_finalizations AS receipt
          JOIN k12_recognition_layout_plans AS plan ON plan.plan_id=receipt.plan_id
         WHERE receipt.plan_id=?`, want.PlanID).Scan(
		&planStatus,
		&planDigest,
		&candidateExactSetDigest,
		&candidateResultsDigest,
		&physicalResultsDigest,
		&candidateCount,
		&physicalCount,
		&finalizationDigest,
		&finalizationJSON,
	); err != nil {
		t.Fatalf("load finalization receipt: %v", err)
	}
	if planStatus != wantPlanStatus || planDigest != want.PlanDigest ||
		candidateExactSetDigest != want.CandidateExactSetDigest ||
		candidateResultsDigest != want.CandidateResultsExactSetDigest ||
		physicalResultsDigest != want.PhysicalResultsExactSetDigest ||
		candidateCount != want.CandidateResultCount ||
		physicalCount != want.PhysicalResultCount ||
		finalizationDigest != want.FinalizationDigest || finalizationJSON == "" {
		t.Fatalf("finalization receipt drifted: status=%s digest=%s json=%s", planStatus, finalizationDigest, finalizationJSON)
	}
}

func assertRecognitionLayoutNoFinalization(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
) {
	t.Helper()
	var receipts, succeeded int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_finalizations`,
	).Scan(&receipts); err != nil {
		t.Fatalf("count finalization receipts: %v", err)
	}
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_plans WHERE status='succeeded'`,
	).Scan(&succeeded); err != nil {
		t.Fatalf("count succeeded layout plans: %v", err)
	}
	if receipts != 0 || succeeded != 0 {
		t.Fatalf("failed finalization mutated state: receipts=%d succeeded=%d", receipts, succeeded)
	}
}
