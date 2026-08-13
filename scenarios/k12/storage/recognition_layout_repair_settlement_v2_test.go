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

// REG-K12-RECOGNITION-BATCH-REPAIR-20260808-001：只有精确成功的第一轮单候选子调用
// 才能结算其持久修复授权。有效结果只冻结一次；无效结果直接终态关闭，不产生第二轮修复授权。
func TestREGK12RecognitionBatchRepair20260808001PersistsRepairSettlement(
	t *testing.T,
) {
	ctx := context.Background()
	store, db, path, parent, plan, authorizations, repairs :=
		prepareRecognitionLayoutRepairSettlementFixture(t, ctx)

	valid := recognitionLayoutRepairSettlementInput(
		plan,
		authorizations[0],
		repairs[0],
		k12.RecognitionLayoutCandidateValidV2,
	)
	valid.ResultKind = k12.RecognitionLayoutCandidateQuestionV2
	valid.ResultJSON = json.RawMessage(`{"student_answer":"4","text":"2+2"}`)
	wantValid, created, err := store.SettleRecognitionLayoutRepairV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		valid,
	)
	if err != nil || !created ||
		wantValid.Classification != k12.RecognitionLayoutCandidateValidV2 ||
		wantValid.SettlementDigest == "" || wantValid.FrozenResult == nil ||
		wantValid.FrozenResult.CandidateID != authorizations[0].CandidateID ||
		wantValid.FrozenResult.ResultKind != k12.RecognitionLayoutCandidateQuestionV2 ||
		wantValid.FrozenResult.ResultDigest == "" ||
		wantValid.UnresolvedCandidateID != "" {
		t.Fatalf(
			"settle valid singleton repair: created=%v result=%+v err=%v",
			created,
			wantValid,
			err,
		)
	}
	if replayed, replayCreated, replayErr := store.SettleRecognitionLayoutRepairV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		valid,
	); replayErr != nil || replayCreated || !reflect.DeepEqual(replayed, wantValid) {
		t.Fatalf(
			"valid exact replay changed facts: created=%v result=%+v err=%v",
			replayCreated,
			replayed,
			replayErr,
		)
	}

	mutations := map[string]func(*k12.RecognitionLayoutRepairSettlementV2){
		"candidate JSON": func(v *k12.RecognitionLayoutRepairSettlementV2) {
			v.ResultJSON = json.RawMessage(`{"student_answer":"5","text":"2+2"}`)
		},
		"source result digest": func(v *k12.RecognitionLayoutRepairSettlementV2) {
			v.SourcePhysicalResultDigest = recognitionLayoutRuntimeTestDigest("different repair result")
		},
		"source invocation": func(v *k12.RecognitionLayoutRepairSettlementV2) {
			v.SourcePhysicalInvocationID = repairs[1].PhysicalInvocationID
		},
		"source unit": func(v *k12.RecognitionLayoutRepairSettlementV2) {
			v.SourcePhysicalUnit = repairs[1].PhysicalUnit
		},
		"candidate": func(v *k12.RecognitionLayoutRepairSettlementV2) {
			v.CandidateID = authorizations[1].CandidateID
		},
		"authorization id": func(v *k12.RecognitionLayoutRepairSettlementV2) {
			v.AuthorizationID = authorizations[1].AuthorizationID
		},
		"authorization digest": func(v *k12.RecognitionLayoutRepairSettlementV2) {
			v.AuthorizationDigest = authorizations[1].AuthorizationDigest
		},
		"plan": func(v *k12.RecognitionLayoutRepairSettlementV2) {
			v.PlanDigest = recognitionLayoutRuntimeTestDigest("different plan")
		},
		"classification": func(v *k12.RecognitionLayoutRepairSettlementV2) {
			v.Classification = k12.RecognitionLayoutCandidateInvalidV2
			v.ResultKind = ""
			v.ResultJSON = nil
		},
	}
	for name, mutate := range mutations {
		t.Run("replay_drift/"+name, func(t *testing.T) {
			changed := valid
			changed.ResultJSON = append(json.RawMessage(nil), valid.ResultJSON...)
			mutate(&changed)
			if _, _, settlementErr := store.SettleRecognitionLayoutRepairV2(
				ctx,
				parent.AgentName,
				parent.InvocationID,
				changed,
			); settlementErr == nil {
				t.Fatal("drifted repair settlement replay was accepted")
			}
		})
	}
	if _, _, ownerDriftErr := store.SettleRecognitionLayoutRepairV2(
		ctx,
		"another-agent",
		parent.InvocationID,
		valid,
	); ownerDriftErr == nil {
		t.Fatal("repair settlement accepted a drifted owner")
	}
	if _, _, parentDriftErr := store.SettleRecognitionLayoutRepairV2(
		ctx,
		parent.AgentName,
		"another-parent",
		valid,
	); parentDriftErr == nil {
		t.Fatal("repair settlement accepted a drifted parent")
	}

	invalid := recognitionLayoutRepairSettlementInput(
		plan,
		authorizations[1],
		repairs[1],
		k12.RecognitionLayoutCandidateInvalidV2,
	)
	wantInvalid, invalidCreated, err := store.SettleRecognitionLayoutRepairV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		invalid,
	)
	if err != nil || !invalidCreated ||
		wantInvalid.Classification != k12.RecognitionLayoutCandidateInvalidV2 ||
		wantInvalid.SettlementDigest == "" || wantInvalid.FrozenResult != nil ||
		wantInvalid.UnresolvedCandidateID != authorizations[1].CandidateID {
		t.Fatalf(
			"settle invalid singleton repair: created=%v result=%+v err=%v",
			invalidCreated,
			wantInvalid,
			err,
		)
	}
	if replayed, replayCreated, replayErr := store.SettleRecognitionLayoutRepairV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		invalid,
	); replayErr != nil || replayCreated || !reflect.DeepEqual(replayed, wantInvalid) {
		t.Fatalf(
			"invalid exact replay changed facts: created=%v result=%+v err=%v",
			replayCreated,
			replayed,
			replayErr,
		)
	}

	assertRecognitionLayoutRepairSettlementRows(t, ctx, db, 2, 3, 2)
	if _, err := db.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_repair_settlements
            SET classification='valid'
          WHERE source_physical_invocation_id=?`,
		repairs[1].PhysicalInvocationID,
	); err == nil {
		t.Fatal("append-only repair-settlement receipt was mutable")
	}
	if _, err := db.ExecContext(
		ctx,
		`INSERT INTO k12_recognition_layout_repair_authorizations (
            plan_id,repair_authorization_id,repair_physical_unit,candidate_id,
            source_batch_id,source_batch_physical_invocation_id,
            source_batch_result_digest,repair_round,authorization_digest,created_at
         ) SELECT plan_id,'repair-round-two',repair_physical_unit,candidate_id,
                  source_batch_id,source_batch_physical_invocation_id,
                  source_batch_result_digest,2,'round-two-digest',created_at
             FROM k12_recognition_layout_repair_authorizations
            WHERE repair_authorization_id=?`,
		authorizations[1].AuthorizationID,
	); err == nil {
		t.Fatal("invalid terminal repair generated or permitted a second repair round")
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close repair settlement database: %v", err)
	}
	store, db = openPhysicalLedgerFileStore(t, path)
	defer db.Close()
	if replayed, replayCreated, replayErr := store.SettleRecognitionLayoutRepairV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		valid,
	); replayErr != nil || replayCreated || !reflect.DeepEqual(replayed, wantValid) {
		t.Fatalf(
			"valid restart replay changed facts: created=%v result=%+v err=%v",
			replayCreated,
			replayed,
			replayErr,
		)
	}
	if replayed, replayCreated, replayErr := store.SettleRecognitionLayoutRepairV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		invalid,
	); replayErr != nil || replayCreated || !reflect.DeepEqual(replayed, wantInvalid) {
		t.Fatalf(
			"invalid restart replay changed facts: created=%v result=%+v err=%v",
			replayCreated,
			replayed,
			replayErr,
		)
	}
}

func TestREGK12RecognitionBatchRepair20260808001RejectsUnsuccessfulRepairSettlement(
	t *testing.T,
) {
	for _, status := range []k12.ModelInvocationStatus{
		k12.ModelInvocationFailed,
		k12.ModelInvocationSent,
		k12.ModelInvocationOutcomeUnknown,
	} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			store, db, _, parent, plan, authorization, repair :=
				prepareRecognitionLayoutUnsuccessfulRepairFixture(t, ctx, status)
			defer db.Close()
			settlement := recognitionLayoutRepairSettlementInput(
				plan,
				authorization,
				repair,
				k12.RecognitionLayoutCandidateInvalidV2,
			)
			settlement.SourcePhysicalResultDigest =
				recognitionLayoutRuntimeTestDigest("no successful provider result")
			if _, _, err := store.SettleRecognitionLayoutRepairV2(
				ctx,
				parent.AgentName,
				parent.InvocationID,
				settlement,
			); err == nil {
				t.Fatalf("%s repair child was settled without success", status)
			}
			assertRecognitionLayoutRepairSettlementRows(t, ctx, db, 0, 2, 2)
		})
	}
}

func TestREGK12RecognitionBatchRepair20260808001ConcurrentRepairSettlementOneWinner(
	t *testing.T,
) {
	ctx := context.Background()
	store, db, _, parent, plan, authorizations, repairs :=
		prepareRecognitionLayoutRepairSettlementFixture(t, ctx)
	defer db.Close()
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enable repair-settlement WAL: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		t.Fatalf("set repair-settlement busy timeout: %v", err)
	}
	db.SetMaxOpenConns(8)
	settlement := recognitionLayoutRepairSettlementInput(
		plan,
		authorizations[0],
		repairs[0],
		k12.RecognitionLayoutCandidateValidV2,
	)
	settlement.ResultKind = k12.RecognitionLayoutCandidateNonQuestionV2
	settlement.ResultJSON = json.RawMessage(`{"reason":"page_decoration"}`)

	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	results := make(chan k12.RecognitionLayoutRepairSettlementResultV2, workers)
	var createdCount atomic.Int32
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, created, err := store.SettleRecognitionLayoutRepairV2(
				ctx,
				parent.AgentName,
				parent.InvocationID,
				settlement,
			)
			if err != nil {
				errs <- err
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
	close(errs)
	close(results)
	for err := range errs {
		t.Errorf("concurrent repair settlement: %v", err)
	}
	if createdCount.Load() != 1 {
		t.Fatalf("concurrent repair-settlement winners=%d, want 1", createdCount.Load())
	}
	var first *k12.RecognitionLayoutRepairSettlementResultV2
	for result := range results {
		if first == nil {
			copy := result
			first = &copy
			continue
		}
		if !reflect.DeepEqual(result, *first) {
			t.Errorf("concurrent repair-settlement projection drift: %+v vs %+v", result, *first)
		}
	}
	assertRecognitionLayoutRepairSettlementRows(t, ctx, db, 1, 3, 2)
}

func prepareRecognitionLayoutRepairSettlementFixture(
	t *testing.T,
	ctx context.Context,
) (
	*k12storage.Store,
	*sql.DB,
	string,
	k12.ModelInvocation,
	k12.RecognitionLayoutPlanV2,
	[]k12.RecognitionLayoutRepairAuthorizationV2,
	[]k12.ModelPhysicalInvocation,
) {
	t.Helper()
	store, db, path, parent, plan, batches :=
		prepareRecognitionLayoutSettlementFixture(t, ctx)
	primary := recognitionLayoutRepairablePrimarySettlement(plan, batches[0])
	projection, created, err := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		primary,
	)
	if err != nil || !created || len(projection.RepairAuthorizations) != 2 {
		t.Fatalf("authorize repair settlement fixture: created=%v result=%+v err=%v", created, projection, err)
	}
	repairs := make([]k12.ModelPhysicalInvocation, len(projection.RepairAuthorizations))
	for index, authorization := range projection.RepairAuthorizations {
		exactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(
			[]string{authorization.CandidateID},
		)
		if err != nil {
			t.Fatal(err)
		}
		repair := newPhysicalInvocation(
			parent,
			"physical-settlement-"+string(authorization.PhysicalUnit),
			authorization.PhysicalUnit,
		)
		repair.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
		repair.PlanDigest = plan.AuthorizedPlanDigest
		repair.CandidateExactSetDigest = exactSetDigest
		prepared, repairCreated, err := store.PrepareModelPhysicalInvocation(ctx, repair)
		if err != nil || !repairCreated {
			t.Fatalf("prepare repair %d: created=%v row=%+v err=%v", index+1, repairCreated, prepared, err)
		}
		if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
			ctx,
			parent.AgentName,
			prepared.PhysicalInvocationID,
		); claimErr != nil || !claimed {
			t.Fatalf("claim repair %d: claimed=%v err=%v", index+1, claimed, claimErr)
		}
		content := `{"repair":` + string(rune('1'+index)) + `}`
		succeeded, err := store.MarkModelPhysicalInvocationSucceededWithContent(
			ctx,
			parent.AgentName,
			prepared.PhysicalInvocationID,
			content,
			"provider-repair-settlement",
		)
		if err != nil {
			t.Fatalf("succeed repair %d: %v", index+1, err)
		}
		repairs[index] = succeeded
	}
	return store, db, path, parent, plan, projection.RepairAuthorizations, repairs
}

func prepareRecognitionLayoutUnsuccessfulRepairFixture(
	t *testing.T,
	ctx context.Context,
	status k12.ModelInvocationStatus,
) (
	*k12storage.Store,
	*sql.DB,
	string,
	k12.ModelInvocation,
	k12.RecognitionLayoutPlanV2,
	k12.RecognitionLayoutRepairAuthorizationV2,
	k12.ModelPhysicalInvocation,
) {
	t.Helper()
	store, db, path, parent, plan, batches :=
		prepareRecognitionLayoutSettlementFixture(t, ctx)
	primary := recognitionLayoutRepairablePrimarySettlement(plan, batches[0])
	projection, created, err := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		primary,
	)
	if err != nil || !created || len(projection.RepairAuthorizations) < 1 {
		t.Fatalf("authorize unsuccessful repair fixture: created=%v result=%+v err=%v", created, projection, err)
	}
	authorization := projection.RepairAuthorizations[0]
	exactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(
		[]string{authorization.CandidateID},
	)
	if err != nil {
		t.Fatal(err)
	}
	repair := newPhysicalInvocation(
		parent,
		"physical-unsuccessful-"+string(status),
		authorization.PhysicalUnit,
	)
	repair.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
	repair.PlanDigest = plan.AuthorizedPlanDigest
	repair.CandidateExactSetDigest = exactSetDigest
	prepared, repairCreated, err := store.PrepareModelPhysicalInvocation(ctx, repair)
	if err != nil || !repairCreated {
		t.Fatalf("prepare unsuccessful repair: created=%v row=%+v err=%v", repairCreated, prepared, err)
	}
	switch status {
	case k12.ModelInvocationFailed:
		repair, err = store.MarkModelPhysicalInvocationNotSent(
			ctx,
			parent.AgentName,
			prepared.PhysicalInvocationID,
		)
	case k12.ModelInvocationSent:
		repair, _, err = store.ClaimModelPhysicalInvocationSent(
			ctx,
			parent.AgentName,
			prepared.PhysicalInvocationID,
		)
	case k12.ModelInvocationOutcomeUnknown:
		if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
			ctx,
			parent.AgentName,
			prepared.PhysicalInvocationID,
		); claimErr != nil || !claimed {
			t.Fatalf("claim outcome-unknown repair: claimed=%v err=%v", claimed, claimErr)
		}
		repair, err = store.MarkModelPhysicalInvocationOutcomeUnknown(
			ctx,
			parent.AgentName,
			prepared.PhysicalInvocationID,
			"provider_response_unknown",
		)
	default:
		t.Fatalf("unsupported unsuccessful repair status %s", status)
	}
	if err != nil || repair.Status != status {
		t.Fatalf("close unsuccessful repair as %s: row=%+v err=%v", status, repair, err)
	}
	return store, db, path, parent, plan, authorization, repair
}

func recognitionLayoutRepairablePrimarySettlement(
	plan k12.RecognitionLayoutPlanV2,
	batch k12.ModelPhysicalInvocation,
) k12.RecognitionLayoutPrimaryBatchSettlementV2 {
	firstBatch := plan.Batches[0]
	return k12.RecognitionLayoutPrimaryBatchSettlementV2{
		PlanDigest:                 plan.AuthorizedPlanDigest,
		SourcePhysicalInvocationID: batch.PhysicalInvocationID,
		SourcePhysicalUnit:         batch.PhysicalUnit,
		SourcePhysicalResultDigest: batch.ResultDigest,
		Classification:             k12.RecognitionLayoutBatchClassifiedV2,
		Candidates: []k12.RecognitionLayoutCandidateSettlementV2{
			{
				CandidateID:    firstBatch.TargetIDs[0],
				Classification: k12.RecognitionLayoutCandidateMissingV2,
			},
			{
				CandidateID:    firstBatch.TargetIDs[1],
				Classification: k12.RecognitionLayoutCandidateInvalidV2,
			},
			{
				CandidateID:    firstBatch.TargetIDs[2],
				Classification: k12.RecognitionLayoutCandidateValidV2,
				ResultKind:     k12.RecognitionLayoutCandidateQuestionV2,
				ResultJSON:     json.RawMessage(`{"text":"3+3"}`),
			},
			{
				CandidateID:    firstBatch.TargetIDs[3],
				Classification: k12.RecognitionLayoutCandidateValidV2,
				ResultKind:     k12.RecognitionLayoutCandidateNonQuestionV2,
				ResultJSON:     json.RawMessage(`{"reason":"decoration"}`),
			},
		},
	}
}

func recognitionLayoutRepairSettlementInput(
	plan k12.RecognitionLayoutPlanV2,
	authorization k12.RecognitionLayoutRepairAuthorizationV2,
	repair k12.ModelPhysicalInvocation,
	classification k12.RecognitionLayoutCandidateClassificationV2,
) k12.RecognitionLayoutRepairSettlementV2 {
	return k12.RecognitionLayoutRepairSettlementV2{
		PlanDigest:                 plan.AuthorizedPlanDigest,
		AuthorizationID:            authorization.AuthorizationID,
		AuthorizationDigest:        authorization.AuthorizationDigest,
		CandidateID:                authorization.CandidateID,
		SourcePhysicalInvocationID: repair.PhysicalInvocationID,
		SourcePhysicalUnit:         repair.PhysicalUnit,
		SourcePhysicalResultDigest: repair.ResultDigest,
		Classification:             classification,
	}
}

func assertRecognitionLayoutRepairSettlementRows(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	wantSettlements int,
	wantResults int,
	wantAuthorizations int,
) {
	t.Helper()
	var settlements, results, authorizations int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_repair_settlements`,
	).Scan(&settlements); err != nil {
		t.Fatalf("count repair settlements: %v", err)
	}
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_candidate_results`,
	).Scan(&results); err != nil {
		t.Fatalf("count candidate results: %v", err)
	}
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_repair_authorizations`,
	).Scan(&authorizations); err != nil {
		t.Fatalf("count repair authorizations: %v", err)
	}
	if settlements != wantSettlements || results != wantResults ||
		authorizations != wantAuthorizations {
		t.Fatalf(
			"repair rows settlements/results/authorizations=%d/%d/%d want %d/%d/%d",
			settlements,
			results,
			authorizations,
			wantSettlements,
			wantResults,
			wantAuthorizations,
		)
	}
}
