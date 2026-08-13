package k12storage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

// REG-K12-RECOGNITION-BATCH-REPAIR-20260808-001：只有精确成功的 primary-batch
// 回执才能冻结其唯一有效候选子集，并为 missing/invalid 补集授权一轮单候选修复。
func TestREGK12RecognitionBatchRepair20260808001PersistsPrimarySettlement(
	t *testing.T,
) {
	ctx := context.Background()
	store, db, path, parent, plan, batches :=
		prepareRecognitionLayoutSettlementFixture(t, ctx)

	firstBatch := plan.Batches[0]
	firstSource := batches[0]
	settlement := k12.RecognitionLayoutPrimaryBatchSettlementV2{
		PlanDigest:                 plan.AuthorizedPlanDigest,
		SourcePhysicalInvocationID: firstSource.PhysicalInvocationID,
		SourcePhysicalUnit:         firstSource.PhysicalUnit,
		SourcePhysicalResultDigest: firstSource.ResultDigest,
		Classification:             k12.RecognitionLayoutBatchClassifiedV2,
		Candidates: []k12.RecognitionLayoutCandidateSettlementV2{
			{
				CandidateID:    firstBatch.TargetIDs[0],
				Classification: k12.RecognitionLayoutCandidateValidV2,
				ResultKind:     k12.RecognitionLayoutCandidateQuestionV2,
				ResultJSON:     json.RawMessage(`{"student_answer":"4","text":"2+2"}`),
			},
			{
				CandidateID:    firstBatch.TargetIDs[1],
				Classification: k12.RecognitionLayoutCandidateMissingV2,
			},
			{
				CandidateID:    firstBatch.TargetIDs[2],
				Classification: k12.RecognitionLayoutCandidateInvalidV2,
			},
			{
				CandidateID:    firstBatch.TargetIDs[3],
				Classification: k12.RecognitionLayoutCandidateValidV2,
				ResultKind:     k12.RecognitionLayoutCandidateNonQuestionV2,
				ResultJSON:     json.RawMessage(`{"reason":"page_decoration"}`),
			},
		},
	}

	want, created, err := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		settlement,
	)
	if err != nil || !created {
		t.Fatalf("settle exact primary batch: created=%v result=%+v err=%v", created, want, err)
	}
	if want.Classification != k12.RecognitionLayoutBatchClassifiedV2 ||
		want.SettlementDigest == "" ||
		len(want.FrozenResults) != 2 || len(want.RepairAuthorizations) != 2 {
		t.Fatalf("wrong settlement projection: %+v", want)
	}
	if want.FrozenResults[0].CandidateID != firstBatch.TargetIDs[0] ||
		want.FrozenResults[0].ResultKind != k12.RecognitionLayoutCandidateQuestionV2 ||
		want.FrozenResults[0].ResultDigest == "" ||
		want.FrozenResults[1].CandidateID != firstBatch.TargetIDs[3] ||
		want.FrozenResults[1].ResultKind != k12.RecognitionLayoutCandidateNonQuestionV2 ||
		want.FrozenResults[1].ResultDigest == "" {
		t.Fatalf("valid subset was not frozen in plan order: %+v", want.FrozenResults)
	}
	for index, authorization := range want.RepairAuthorizations {
		candidateOrdinal := index + 2
		wantUnit, unitErr := k12.RecognitionLayoutRepairUnitV2(candidateOrdinal)
		if unitErr != nil {
			t.Fatal(unitErr)
		}
		if authorization.CandidateID != firstBatch.TargetIDs[index+1] ||
			authorization.PhysicalUnit != wantUnit ||
			authorization.RepairRound != 1 ||
			authorization.AuthorizationID == "" ||
			authorization.AuthorizationDigest == "" {
			t.Fatalf("repair authorization %d drifted: %+v", index, authorization)
		}
	}

	assertRecognitionLayoutSettlementRows(t, ctx, db, 2, 2)
	assertRecognitionLayoutBatchSettlementReceiptCount(t, ctx, db, 1)
	assertRecognitionLayoutBatchSettlementReceipt(
		t,
		ctx,
		db,
		firstSource.PhysicalInvocationID,
		k12.RecognitionLayoutBatchClassifiedV2,
		"",
		want.SettlementDigest,
	)
	if _, execErr := db.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_candidate_results
            SET result_json='{"text":"rewritten"}'
          WHERE candidate_id=?`,
		firstBatch.TargetIDs[0],
	); execErr == nil {
		t.Fatal("append-only candidate result was mutable")
	}
	if _, execErr := db.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_batch_settlements
            SET ambiguity_kind='source_conflict'
          WHERE source_physical_invocation_id=?`,
		firstSource.PhysicalInvocationID,
	); execErr == nil {
		t.Fatal("append-only batch-settlement receipt was mutable")
	}
	replayed, replayCreated, err := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		settlement,
	)
	if err != nil || replayCreated || !reflect.DeepEqual(replayed, want) {
		t.Fatalf("exact replay changed facts: created=%v result=%+v err=%v", replayCreated, replayed, err)
	}

	mutations := map[string]func(*k12.RecognitionLayoutPrimaryBatchSettlementV2){
		"candidate JSON": func(v *k12.RecognitionLayoutPrimaryBatchSettlementV2) {
			v.Candidates[0].ResultJSON = json.RawMessage(`{"student_answer":"5","text":"2+2"}`)
		},
		"source result digest": func(v *k12.RecognitionLayoutPrimaryBatchSettlementV2) {
			v.SourcePhysicalResultDigest = recognitionLayoutRuntimeTestDigest("different source")
		},
		"candidate order": func(v *k12.RecognitionLayoutPrimaryBatchSettlementV2) {
			v.Candidates[0], v.Candidates[1] = v.Candidates[1], v.Candidates[0]
		},
		"successful candidate becomes repairable": func(v *k12.RecognitionLayoutPrimaryBatchSettlementV2) {
			v.Candidates[0].Classification = k12.RecognitionLayoutCandidateMissingV2
			v.Candidates[0].ResultKind = ""
			v.Candidates[0].ResultJSON = nil
		},
	}
	for name, mutate := range mutations {
		t.Run("replay_drift/"+name, func(t *testing.T) {
			changed := cloneRecognitionLayoutSettlement(settlement)
			mutate(&changed)
			if _, _, settleErr := store.SettleRecognitionLayoutPrimaryBatchV2(
				ctx,
				parent.AgentName,
				parent.InvocationID,
				changed,
			); settleErr == nil {
				t.Fatal("drifted settlement replay was accepted")
			}
			assertRecognitionLayoutSettlementRows(t, ctx, db, 2, 2)
		})
	}

	// 终态歧义不会降级为逐候选修复猜测：整个来源批次保持未解决且不写入候选行。
	secondBatch := plan.Batches[1]
	secondSource := batches[1]
	ambiguousSettlement := k12.RecognitionLayoutPrimaryBatchSettlementV2{
		PlanDigest:                 plan.AuthorizedPlanDigest,
		SourcePhysicalInvocationID: secondSource.PhysicalInvocationID,
		SourcePhysicalUnit:         secondSource.PhysicalUnit,
		SourcePhysicalResultDigest: secondSource.ResultDigest,
		Classification:             k12.RecognitionLayoutBatchTerminalAmbiguousV2,
		AmbiguityKind:              k12.RecognitionLayoutAmbiguityExtraCandidateV2,
	}
	ambiguous, ambiguousCreated, err := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		ambiguousSettlement,
	)
	if err != nil || !ambiguousCreated ||
		ambiguous.Classification != k12.RecognitionLayoutBatchTerminalAmbiguousV2 ||
		ambiguous.SettlementDigest == "" ||
		!reflect.DeepEqual(ambiguous.UnresolvedCandidateIDs, secondBatch.TargetIDs) ||
		len(ambiguous.FrozenResults) != 0 || len(ambiguous.RepairAuthorizations) != 0 {
		t.Fatalf("terminal ambiguity was not fail-closed: created=%v result=%+v err=%v", ambiguousCreated, ambiguous, err)
	}
	assertRecognitionLayoutSettlementRows(t, ctx, db, 2, 2)
	assertRecognitionLayoutBatchSettlementReceiptCount(t, ctx, db, 2)
	assertRecognitionLayoutBatchSettlementReceipt(
		t,
		ctx,
		db,
		secondSource.PhysicalInvocationID,
		k12.RecognitionLayoutBatchTerminalAmbiguousV2,
		k12.RecognitionLayoutAmbiguityExtraCandidateV2,
		ambiguous.SettlementDigest,
	)
	if replayedAmbiguous, created, replayErr := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		ambiguousSettlement,
	); replayErr != nil || created || !reflect.DeepEqual(replayedAmbiguous, ambiguous) {
		t.Fatalf("ambiguous exact replay changed facts: created=%v result=%+v err=%v", created, replayedAmbiguous, replayErr)
	}
	driftedAmbiguity := ambiguousSettlement
	driftedAmbiguity.AmbiguityKind = k12.RecognitionLayoutAmbiguitySourceConflictV2
	if _, _, settleErr := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		driftedAmbiguity,
	); settleErr == nil {
		t.Fatal("durable terminal ambiguity accepted a changed ambiguity reason")
	}
	reclassified := ambiguousSettlement
	reclassified.Classification = k12.RecognitionLayoutBatchClassifiedV2
	reclassified.AmbiguityKind = ""
	reclassified.Candidates = []k12.RecognitionLayoutCandidateSettlementV2{
		{
			CandidateID:    secondBatch.TargetIDs[0],
			Classification: k12.RecognitionLayoutCandidateValidV2,
			ResultKind:     k12.RecognitionLayoutCandidateQuestionV2,
			ResultJSON:     json.RawMessage(`{"text":"late guess"}`),
		},
	}
	if _, _, settleErr := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		reclassified,
	); settleErr == nil {
		t.Fatal("durable terminal ambiguity was reclassified after restart boundary")
	}

	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close settlement db before restart: %v", closeErr)
	}
	store, db = openPhysicalLedgerFileStore(t, path)
	defer db.Close()
	if replayed, created, replayErr := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		settlement,
	); replayErr != nil || created || !reflect.DeepEqual(replayed, want) {
		t.Fatalf("classified restart replay changed facts: created=%v result=%+v err=%v", created, replayed, replayErr)
	}
	if replayed, created, replayErr := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		ambiguousSettlement,
	); replayErr != nil || created || !reflect.DeepEqual(replayed, ambiguous) {
		t.Fatalf("ambiguous restart replay changed facts: created=%v result=%+v err=%v", created, replayed, replayErr)
	}

	// 这两个修复授权是仅有的修复物理调用身份。
	for _, authorization := range want.RepairAuthorizations {
		exactSetDigest, digestErr := k12.RecognitionLayoutTargetExactSetDigestV2(
			[]string{authorization.CandidateID},
		)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		repair := newPhysicalInvocation(
			parent,
			"physical-"+string(authorization.PhysicalUnit),
			authorization.PhysicalUnit,
		)
		repair.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
		repair.PlanDigest = plan.AuthorizedPlanDigest
		repair.CandidateExactSetDigest = exactSetDigest
		prepared, repairCreated, prepareErr := store.PrepareModelPhysicalInvocation(ctx, repair)
		if prepareErr != nil || !repairCreated || prepared.PhysicalUnit != authorization.PhysicalUnit {
			t.Fatalf("prepare authorized singleton repair: created=%v row=%+v err=%v", repairCreated, prepared, prepareErr)
		}
		if _, replayCreated, replayErr := store.PrepareModelPhysicalInvocation(ctx, repair); replayErr != nil || replayCreated {
			t.Fatalf("repair replay must not create a second round: created=%v err=%v", replayCreated, replayErr)
		}
	}

	successfulRepairUnit, err := k12.RecognitionLayoutRepairUnitV2(1)
	if err != nil {
		t.Fatal(err)
	}
	successfulExactSet, err := k12.RecognitionLayoutTargetExactSetDigestV2(
		[]string{firstBatch.TargetIDs[0]},
	)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := newPhysicalInvocation(
		parent,
		"physical-successful-candidate-repair",
		successfulRepairUnit,
	)
	unauthorized.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
	unauthorized.PlanDigest = plan.AuthorizedPlanDigest
	unauthorized.CandidateExactSetDigest = successfulExactSet
	if _, _, err := store.PrepareModelPhysicalInvocation(ctx, unauthorized); err == nil {
		t.Fatal("already-frozen candidate obtained a repair authorization")
	}
}

func prepareRecognitionLayoutSettlementFixture(
	t *testing.T,
	ctx context.Context,
) (
	*k12storage.Store,
	*sql.DB,
	string,
	k12.ModelInvocation,
	k12.RecognitionLayoutPlanV2,
	[]k12.ModelPhysicalInvocation,
) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recognition-layout-settlement-v2.db")
	store, db := openPhysicalLedgerFileStore(t, path)
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatalf("migrate settlement db: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES(?)`, "mingming"); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", "recognition-layout-settlement-v2")
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}
	parent := unpreparedPhysicalInvocationParent(job.RecordID)
	manifestContent := `{"targets":["manifest_0001","manifest_0002","manifest_0003","manifest_0004","manifest_0005"]}`
	manifestDigest := recognitionLayoutRuntimeTestDigest(manifestContent)
	plan, err := k12.BuildRecognitionLayoutPlanV2(k12.RecognitionLayoutPlanInputV2{
		PagePNG: recognitionLayoutRuntimeTestPagePNG(t),
		Manifest: k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: "physical-settlement-manifest",
			ResultDigest: manifestDigest,
		},
		Targets: []k12.RecognitionLayoutManifestTargetV2{
			{ManifestRef: "manifest_0001", ManifestOrder: 1, DisplayLabel: "1", SourceNumberPath: []string{"1"}, Region: k12.SourcePixelRegion{X: 0, Y: 0, Width: 20, Height: 10}},
			{ManifestRef: "manifest_0002", ManifestOrder: 2, DisplayLabel: "2", SourceNumberPath: []string{"2"}, Region: k12.SourcePixelRegion{X: 0, Y: 10, Width: 20, Height: 10}},
			{ManifestRef: "manifest_0003", ManifestOrder: 3, DisplayLabel: "3", SourceNumberPath: []string{"3"}, Region: k12.SourcePixelRegion{X: 0, Y: 20, Width: 20, Height: 10}},
			{ManifestRef: "manifest_0004", ManifestOrder: 4, DisplayLabel: "4", SourceNumberPath: []string{"4"}, Region: k12.SourcePixelRegion{X: 0, Y: 30, Width: 20, Height: 10}},
			{ManifestRef: "manifest_0005", ManifestOrder: 5, DisplayLabel: "5", SourceNumberPath: []string{"5"}, Region: k12.SourcePixelRegion{X: 0, Y: 40, Width: 20, Height: 10}},
		},
	})
	if err != nil {
		t.Fatalf("build settlement plan: %v", err)
	}
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-settlement-v2",
		ParentInvocationID:       parent.InvocationID,
		AgentName:                parent.AgentName,
		JobID:                    parent.JobID,
		PageDigest:               plan.PageDigest,
		ParentRequestDigest:      parent.RequestDigest,
		RouteSnapshot:            parent.RouteSnapshot,
		RequestPolicySnapshot:    parent.RequestPolicySnapshot,
		StageStartedAtUnixMillis: time.Now().UnixMilli(),
		PhysicalCallCapMillis:    120000,
		BudgetBuckets: k12.RecognitionLayoutBudgetBucketsV2{
			UpTo1ProblemMillis:   600000,
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
	manifest := newPhysicalInvocation(
		parent,
		plan.ManifestInvocationID,
		k12.RecognitionPhysicalUnitWholePage,
	)
	manifest.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
	manifest.PlanDigest = headerDigest
	storedParent, storedManifest, created, err :=
		store.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
			ctx,
			parent,
			manifest,
			header,
		)
	if err != nil || !created {
		t.Fatalf("publish settlement plan: created=%v err=%v", created, err)
	}
	if _, claimed, err := store.ClaimModelPhysicalInvocationSent(
		ctx,
		storedParent.AgentName,
		storedManifest.PhysicalInvocationID,
	); err != nil || !claimed {
		t.Fatalf("claim settlement manifest: claimed=%v err=%v", claimed, err)
	}
	if _, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		storedParent.AgentName,
		storedManifest.PhysicalInvocationID,
		manifestContent,
		"provider-settlement-manifest",
	); err != nil {
		t.Fatalf("succeed settlement manifest: %v", err)
	}
	if err := store.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		storedParent.AgentName,
		storedParent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: storedManifest.PhysicalInvocationID,
			ResultDigest: manifestDigest,
		},
		plan,
	); err != nil {
		t.Fatalf("authorize settlement plan: %v", err)
	}

	batches := make([]k12.ModelPhysicalInvocation, len(plan.Batches))
	for index := range plan.Batches {
		batch := recognitionLayoutRuntimeBatchInvocation(t, storedParent, plan, index)
		prepared, batchCreated, err := store.PrepareModelPhysicalInvocation(ctx, batch)
		if err != nil || !batchCreated {
			t.Fatalf("prepare source batch %d: created=%v err=%v", index+1, batchCreated, err)
		}
		if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
			ctx,
			storedParent.AgentName,
			prepared.PhysicalInvocationID,
		); claimErr != nil || !claimed {
			t.Fatalf("claim source batch %d: claimed=%v err=%v", index+1, claimed, claimErr)
		}
		content := `{"batch":` + string(rune('1'+index)) + `}`
		succeeded, err := store.MarkModelPhysicalInvocationSucceededWithContent(
			ctx,
			storedParent.AgentName,
			prepared.PhysicalInvocationID,
			content,
			"provider-settlement-batch",
		)
		if err != nil {
			t.Fatalf("succeed source batch %d: %v", index+1, err)
		}
		batches[index] = succeeded
	}
	return store, db, path, storedParent, plan, batches
}

func cloneRecognitionLayoutSettlement(
	input k12.RecognitionLayoutPrimaryBatchSettlementV2,
) k12.RecognitionLayoutPrimaryBatchSettlementV2 {
	cloned := input
	cloned.Candidates = append(
		[]k12.RecognitionLayoutCandidateSettlementV2(nil),
		input.Candidates...,
	)
	for index := range cloned.Candidates {
		cloned.Candidates[index].ResultJSON = append(
			json.RawMessage(nil),
			input.Candidates[index].ResultJSON...,
		)
	}
	return cloned
}

func assertRecognitionLayoutSettlementRows(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	wantResults int,
	wantRepairs int,
) {
	t.Helper()
	var results, repairs int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_candidate_results`,
	).Scan(&results); err != nil {
		t.Fatalf("count candidate results: %v", err)
	}
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_repair_authorizations`,
	).Scan(&repairs); err != nil {
		t.Fatalf("count repair authorizations: %v", err)
	}
	if results != wantResults || repairs != wantRepairs {
		t.Fatalf("settlement rows results=%d repairs=%d, want %d/%d", results, repairs, wantResults, wantRepairs)
	}
}

func assertRecognitionLayoutBatchSettlementReceiptCount(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	want int,
) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM k12_recognition_layout_batch_settlements`,
	).Scan(&count); err != nil {
		t.Fatalf("count batch-settlement receipts: %v", err)
	}
	if count != want {
		t.Fatalf("batch-settlement receipts=%d, want %d", count, want)
	}
}

func assertRecognitionLayoutBatchSettlementReceipt(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	sourcePhysicalInvocationID string,
	wantClassification k12.RecognitionLayoutBatchClassificationV2,
	wantAmbiguity k12.RecognitionLayoutBatchAmbiguityKindV2,
	wantDigest string,
) {
	t.Helper()
	var classification, ambiguity, digest string
	if err := db.QueryRowContext(
		ctx,
		`SELECT classification,ambiguity_kind,settlement_digest
           FROM k12_recognition_layout_batch_settlements
          WHERE source_physical_invocation_id=?`,
		sourcePhysicalInvocationID,
	).Scan(&classification, &ambiguity, &digest); err != nil {
		t.Fatalf("load batch-settlement receipt: %v", err)
	}
	if classification != string(wantClassification) ||
		ambiguity != string(wantAmbiguity) || digest != wantDigest {
		t.Fatalf(
			"batch-settlement receipt=%q/%q/%q, want %q/%q/%q",
			classification,
			ambiguity,
			digest,
			wantClassification,
			wantAmbiguity,
			wantDigest,
		)
	}
}
