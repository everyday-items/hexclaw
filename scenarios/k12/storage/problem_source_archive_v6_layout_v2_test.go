package k12storage_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-005：私有 V2 recognition-layout
// 聚合本身就是 V6 归档事实。Store 写入终态回执后、V73 结果前发生崩溃时，必须恢复足够的
// 不可变证据以在本地收尾，且不能根据物理调用投影重建授权。
func TestREGK12RecognitionDurabilityBudget20260808005RestoresFinalizedProblemSourceLayoutBeforeV73(
	t *testing.T,
) {
	ctx := context.Background()
	sourceStore, sourceDB := setup(t)
	_ = seedProblemSourceRecognitionFixture(t, sourceStore, sourceDB, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, sourceDB)
	freezeProblemSourceArchiveReceipt(t, sourceDB, k12storage.ProblemSourceRecognitionCommit{
		CommandReceiptID:    recognitionReceipt,
		DispatchID:          recognitionDispatch,
		PathProblemID:       recognitionParent,
		Action:              "select_region",
		StructureVersion:    1,
		SourceInputRevision: 2,
		ResultInputRevision: 3,
	})
	parent, plan, finalized, pendingResult :=
		seedFinalizedProblemSourceRecognitionLayoutV2(t, ctx, sourceStore, sourceDB)

	var resultCount int
	if err := sourceDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM k12_problem_source_recognition_results WHERE work_id=?`,
		recognitionWork,
	).Scan(&resultCount); err != nil || resultCount != 0 {
		t.Fatalf("precondition V73 count=%d err=%v", resultCount, err)
	}
	archive, err := sourceStore.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatalf("export finalized pre-V73 V6 archive: %v", err)
	}

	targetStore, targetDB := setup(t)
	seedProblemSourceArchiveTargetParents(t, targetDB)
	seedProblemSourceArchiveV2TargetAttempts(t, targetDB, archive)
	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if importErr := targetStore.ImportProblemSourceArchiveV6Tx(
		ctx,
		tx,
		"mingming",
		archive,
	); importErr != nil {
		_ = tx.Rollback()
		t.Fatalf("import finalized pre-V73 V6 archive: %v", importErr)
	}
	if commitErr := tx.Commit(); commitErr != nil {
		t.Fatal(commitErr)
	}

	restored, err := targetStore.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil {
		t.Fatalf("load restored finalized layout: %v", err)
	}
	if restored.Status != "succeeded" || restored.AuthorizedPlan == nil ||
		!reflect.DeepEqual(*restored.AuthorizedPlan, plan) {
		t.Fatalf("restored layout runtime drifted: %+v", restored)
	}
	replayed, created, err := targetStore.FinalizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || created || !reflect.DeepEqual(replayed, finalized) {
		t.Fatalf(
			"restored finalization replay: created=%v result=%+v err=%v",
			created,
			replayed,
			err,
		)
	}

	lease, found, err := targetStore.ClaimProblemSourceReprocessJob(
		ctx,
		"restored-local-worker",
		time.Now().UTC().Add(time.Hour),
		5*time.Minute,
	)
	if err != nil || !found || lease.WorkID != recognitionWork {
		t.Fatalf("claim restored finalized work: lease=%+v found=%v err=%v", lease, found, err)
	}
	committed, committedNow, err := targetStore.CommitProblemSourceRecognitionResult(
		ctx,
		lease.Lease(),
		pendingResult,
	)
	if err != nil || !committedNow || committed.WorkID != recognitionWork {
		t.Fatalf(
			"commit restored V73 locally: created=%v result=%+v err=%v",
			committedNow,
			committed,
			err,
		)
	}
	beforeReplayCount := countProblemSourceArchiveRows(
		t,
		targetDB,
		"k12_model_physical_invocations",
	)
	if _, replayCreated, err := targetStore.CommitProblemSourceRecognitionResult(
		ctx,
		lease.Lease(),
		pendingResult,
	); err != nil || replayCreated {
		t.Fatalf("V73 replay created=%v err=%v", replayCreated, err)
	}
	if after := countProblemSourceArchiveRows(
		t,
		targetDB,
		"k12_model_physical_invocations",
	); after != beforeReplayCount {
		t.Fatalf("local V73 replay changed physical calls: before=%d after=%d", beforeReplayCount, after)
	}
}

func TestREGK12RecognitionDurabilityBudget20260808005RejectsPrivateLayoutTampering(
	t *testing.T,
) {
	ctx := context.Background()
	store, db := setup(t)
	_ = seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, db)
	freezeProblemSourceArchiveReceipt(t, db, k12storage.ProblemSourceRecognitionCommit{
		CommandReceiptID: recognitionReceipt, DispatchID: recognitionDispatch,
		PathProblemID: recognitionParent, Action: "select_region",
		StructureVersion: 1, SourceInputRevision: 2, ResultInputRevision: 3,
	})
	seedFinalizedProblemSourceRecognitionLayoutV2(t, ctx, store, db)
	archive, err := store.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil || len(archive.RecognitionLayoutsV2) != 1 {
		t.Fatalf("export tamper fixture: layouts=%d err=%v", len(archive.RecognitionLayoutsV2), err)
	}
	tests := []struct {
		name   string
		mutate func(*k12storage.ProblemSourceArchiveV6)
	}{
		{
			name: "plan header digest",
			mutate: func(v *k12storage.ProblemSourceArchiveV6) {
				v.RecognitionLayoutsV2[0].Plan.HeaderDigest = "sha256:" + repeatHex("1", 64)
			},
		},
		{
			name: "candidate exact-set missing row",
			mutate: func(v *k12storage.ProblemSourceArchiveV6) {
				v.RecognitionLayoutsV2[0].Candidates = v.RecognitionLayoutsV2[0].Candidates[:1]
			},
		},
		{
			name: "batch member reference",
			mutate: func(v *k12storage.ProblemSourceArchiveV6) {
				v.RecognitionLayoutsV2[0].BatchMembers[0].CandidateID = "layout_target_v2_unknown"
			},
		},
		{
			name: "batch settlement digest",
			mutate: func(v *k12storage.ProblemSourceArchiveV6) {
				v.RecognitionLayoutsV2[0].BatchSettlements[0].SettlementDigest = "sha256:" + repeatHex("2", 64)
			},
		},
		{
			name: "repair authorization source",
			mutate: func(v *k12storage.ProblemSourceArchiveV6) {
				v.RecognitionLayoutsV2[0].RepairAuthorizations[0].SourceBatchPhysicalInvocationID = "physical-foreign"
			},
		},
		{
			name: "repair settlement digest",
			mutate: func(v *k12storage.ProblemSourceArchiveV6) {
				v.RecognitionLayoutsV2[0].RepairSettlements[0].SettlementDigest = "sha256:" + repeatHex("3", 64)
			},
		},
		{
			name: "finalization physical exact-set digest",
			mutate: func(v *k12storage.ProblemSourceArchiveV6) {
				v.RecognitionLayoutsV2[0].Finalization.PhysicalResultsExactSetDigest = "sha256:" + repeatHex("4", 64)
			},
		},
		{
			name: "physical child exact-set missing row",
			mutate: func(v *k12storage.ProblemSourceArchiveV6) {
				v.ModelPhysicalInvocations = v.ModelPhysicalInvocations[1:]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneProblemSourceArchiveV6ForTest(t, archive)
			test.mutate(&candidate)
			if err := k12storage.ValidateProblemSourceArchiveV6("mingming", candidate); err == nil {
				t.Fatal("tampered private V2 layout archive passed validation")
			}
		})
	}
}

func TestREGK12RecognitionDurabilityBudget20260808005ImportConflictRollsBackWholeLayout(
	t *testing.T,
) {
	ctx := context.Background()
	sourceStore, sourceDB := setup(t)
	_ = seedProblemSourceRecognitionFixture(t, sourceStore, sourceDB, recognitionWork)
	seedProblemSourceArchiveHomeworkParent(t, sourceDB)
	freezeProblemSourceArchiveReceipt(t, sourceDB, k12storage.ProblemSourceRecognitionCommit{
		CommandReceiptID: recognitionReceipt, DispatchID: recognitionDispatch,
		PathProblemID: recognitionParent, Action: "select_region",
		StructureVersion: 1, SourceInputRevision: 2, ResultInputRevision: 3,
	})
	seedFinalizedProblemSourceRecognitionLayoutV2(t, ctx, sourceStore, sourceDB)
	archive, err := sourceStore.ExportProblemSourceArchiveV6(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}

	targetStore, targetDB := setup(t)
	seedProblemSourceArchiveTargetParents(t, targetDB)
	seedProblemSourceArchiveV2TargetAttempts(t, targetDB, archive)
	asset := archive.PageAssets[0]
	if _, execErr := targetDB.ExecContext(ctx, `INSERT INTO k12_page_assets (
		owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,
		pixel_width,pixel_height,orientation_policy,orientation_policy_version,
		transform_chain_json,storage_state,ready_at,last_error,created_at,updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, asset.OwnerScope,
		asset.PageAssetID, asset.AgentName, asset.ContentDigest, asset.MediaType,
		asset.SizeBytes+1, asset.PixelWidth, asset.PixelHeight, asset.OrientationPolicy,
		asset.OrientationPolicyVersion, asset.TransformChainJSON, asset.StorageState,
		asset.ReadyAt, asset.LastError, asset.CreatedAt, asset.UpdatedAt); execErr != nil {
		t.Fatal(execErr)
	}
	tx, err := targetDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := targetStore.ImportProblemSourceArchiveV6Tx(ctx, tx, "mingming", archive); err == nil {
		_ = tx.Rollback()
		t.Fatal("conflicting immutable PageAsset accepted archive import")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"k12_problem_source_action_receipts",
		"k12_model_invocations",
		"k12_model_physical_invocations",
		"k12_recognition_layout_plans",
		"k12_recognition_layout_finalizations",
	} {
		var count int
		if err := targetDB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("rollback left %s rows=%d", table, count)
		}
	}
}

func seedFinalizedProblemSourceRecognitionLayoutV2(
	t *testing.T,
	ctx context.Context,
	store *k12storage.Store,
	db *sql.DB,
) (
	k12.ModelInvocation,
	k12.RecognitionLayoutPlanV2,
	k12.RecognitionLayoutPlanFinalizationResultV2,
	k12storage.ProblemSourceRecognitionResult,
) {
	t.Helper()
	var routeJSON string
	var attempt int
	if err := db.QueryRowContext(ctx, `
		SELECT route_snapshot_json,attempt
		FROM k12_model_invocations WHERE invocation_id=?`,
		"recognition-parent-invocation",
	).Scan(&routeJSON, &attempt); err != nil {
		t.Fatalf("load source-recognition parent identity: %v", err)
	}
	var route k12.GradingModelSnapshot
	if err := json.Unmarshal([]byte(routeJSON), &route); err != nil {
		t.Fatal(err)
	}
	policy := k12.ApprovedRecognizingRequestPolicy()
	route.RecognizingRequestPolicy = policy
	work, err := store.GetProblemSourceReprocessJob(
		ctx,
		recognitionOwner,
		recognitionWork,
	)
	if err != nil {
		t.Fatal(err)
	}
	requestDigest, err := k12storage.ProblemSourceRecognitionParentRequestDigest(
		work,
		route,
		policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, deletePhysicalErr := db.ExecContext(
		ctx,
		`DELETE FROM k12_model_physical_invocations WHERE parent_invocation_id=?`,
		"recognition-parent-invocation",
	); deletePhysicalErr != nil {
		t.Fatal(deletePhysicalErr)
	}
	if _, deleteParentErr := db.ExecContext(
		ctx,
		`DELETE FROM k12_model_invocations WHERE invocation_id=?`,
		"recognition-parent-invocation",
	); deleteParentErr != nil {
		t.Fatal(deleteParentErr)
	}

	parent := k12.ModelInvocation{
		InvocationID:          "recognition-parent-invocation",
		AgentName:             "mingming",
		JobID:                 recognitionJob,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         requestDigest,
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               attempt,
		CreatedAt:             100,
	}
	manifestContent := `{"targets":["manifest_0001","manifest_0002"]}`
	manifestDigest := recognitionLayoutRuntimeTestDigest(manifestContent)
	plan, err := k12.BuildRecognitionLayoutPlanV2(k12.RecognitionLayoutPlanInputV2{
		PagePNG: recognitionLayoutRuntimeTestPagePNG(t),
		Manifest: k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: "problem-source-layout-manifest",
			ResultDigest: manifestDigest,
		},
		Targets: []k12.RecognitionLayoutManifestTargetV2{
			{
				ManifestRef: "manifest_0001", ManifestOrder: 1,
				DisplayLabel: "1", SourceNumberPath: []string{"1"},
				Region: k12.SourcePixelRegion{X: 0, Y: 0, Width: 20, Height: 20},
			},
			{
				ManifestRef: "manifest_0002", ManifestOrder: 2,
				DisplayLabel: "2", SourceNumberPath: []string{"2"},
				Region: k12.SourcePixelRegion{X: 0, Y: 20, Width: 20, Height: 20},
			},
		},
	})
	if err != nil {
		t.Fatalf("build source archive V2 plan: %v", err)
	}
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "problem-source-layout-plan-v2",
		ParentInvocationID:       parent.InvocationID,
		AgentName:                parent.AgentName,
		JobID:                    parent.JobID,
		PageDigest:               plan.PageDigest,
		ParentRequestDigest:      parent.RequestDigest,
		RouteSnapshot:            parent.RouteSnapshot,
		RequestPolicySnapshot:    parent.RequestPolicySnapshot,
		StageStartedAtUnixMillis: time.Now().UTC().UnixMilli(),
		PhysicalCallCapMillis:    120000,
		BudgetBuckets: k12.RecognitionLayoutBudgetBucketsV2{
			UpTo1ProblemMillis:   60000,
			UpTo8ProblemsMillis:  120000,
			UpTo16ProblemsMillis: 180000,
			UpTo32ProblemsMillis: 300000,
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
	parent, manifest, created, err := store.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
		ctx,
		parent,
		manifest,
		header,
	)
	if err != nil || !created {
		t.Fatalf("publish source archive V2 plan: created=%v err=%v", created, err)
	}
	if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		manifest.PhysicalInvocationID,
	); claimErr != nil || !claimed {
		t.Fatalf("claim source archive manifest: claimed=%v err=%v", claimed, claimErr)
	}
	if _, markErr := store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		parent.AgentName,
		manifest.PhysicalInvocationID,
		manifestContent,
		"provider-source-archive-manifest",
	); markErr != nil {
		t.Fatal(markErr)
	}
	if authorizeErr := store.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: manifest.PhysicalInvocationID,
			ResultDigest: manifestDigest,
		},
		plan,
	); authorizeErr != nil {
		t.Fatal(authorizeErr)
	}

	batch := recognitionLayoutRuntimeBatchInvocation(t, parent, plan, 0)
	preparedBatch, created, err := store.PrepareModelPhysicalInvocation(ctx, batch)
	if err != nil || !created {
		t.Fatalf("prepare source archive batch: created=%v err=%v", created, err)
	}
	if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		preparedBatch.PhysicalInvocationID,
	); claimErr != nil || !claimed {
		t.Fatalf("claim source archive batch: claimed=%v err=%v", claimed, claimErr)
	}
	succeededBatch, err := store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		parent.AgentName,
		preparedBatch.PhysicalInvocationID,
		`{"batch":"source-archive"}`,
		"provider-source-archive-batch",
	)
	if err != nil {
		t.Fatal(err)
	}
	primary, created, err := store.SettleRecognitionLayoutPrimaryBatchV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		k12.RecognitionLayoutPrimaryBatchSettlementV2{
			PlanDigest:                 plan.AuthorizedPlanDigest,
			SourcePhysicalInvocationID: succeededBatch.PhysicalInvocationID,
			SourcePhysicalUnit:         succeededBatch.PhysicalUnit,
			SourcePhysicalResultDigest: succeededBatch.ResultDigest,
			Classification:             k12.RecognitionLayoutBatchClassifiedV2,
			Candidates: []k12.RecognitionLayoutCandidateSettlementV2{
				{CandidateID: plan.Targets[0].TargetID, Classification: k12.RecognitionLayoutCandidateMissingV2},
				{CandidateID: plan.Targets[1].TargetID, Classification: k12.RecognitionLayoutCandidateInvalidV2},
			},
		},
	)
	if err != nil || !created || len(primary.RepairAuthorizations) != 2 {
		t.Fatalf("settle source archive primary: created=%v result=%+v err=%v", created, primary, err)
	}
	for index, authorization := range primary.RepairAuthorizations {
		exactSetDigest, digestErr := k12.RecognitionLayoutTargetExactSetDigestV2(
			[]string{authorization.CandidateID},
		)
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		repair := newPhysicalInvocation(
			parent,
			"problem-source-"+string(authorization.PhysicalUnit),
			authorization.PhysicalUnit,
		)
		repair.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
		repair.PlanDigest = plan.AuthorizedPlanDigest
		repair.CandidateExactSetDigest = exactSetDigest
		preparedRepair, repairCreated, prepareErr := store.PrepareModelPhysicalInvocation(ctx, repair)
		if prepareErr != nil || !repairCreated {
			t.Fatalf("prepare source archive repair %d: created=%v err=%v", index+1, repairCreated, prepareErr)
		}
		if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
			ctx,
			parent.AgentName,
			preparedRepair.PhysicalInvocationID,
		); claimErr != nil || !claimed {
			t.Fatalf("claim source archive repair %d: claimed=%v err=%v", index+1, claimed, claimErr)
		}
		succeededRepair, succeedErr := store.MarkModelPhysicalInvocationSucceededWithContent(
			ctx,
			parent.AgentName,
			preparedRepair.PhysicalInvocationID,
			`{"repair":"source-archive"}`,
			"provider-source-archive-repair",
		)
		if succeedErr != nil {
			t.Fatal(succeedErr)
		}
		if _, settleCreated, settleErr := store.SettleRecognitionLayoutRepairV2(
			ctx,
			parent.AgentName,
			parent.InvocationID,
			k12.RecognitionLayoutRepairSettlementV2{
				PlanDigest:                 plan.AuthorizedPlanDigest,
				AuthorizationID:            authorization.AuthorizationID,
				AuthorizationDigest:        authorization.AuthorizationDigest,
				CandidateID:                authorization.CandidateID,
				SourcePhysicalInvocationID: succeededRepair.PhysicalInvocationID,
				SourcePhysicalUnit:         succeededRepair.PhysicalUnit,
				SourcePhysicalResultDigest: succeededRepair.ResultDigest,
				Classification:             k12.RecognitionLayoutCandidateValidV2,
				ResultKind:                 k12.RecognitionLayoutCandidateQuestionV2,
				ResultJSON:                 json.RawMessage(`{"text":"restored-source-question"}`),
			},
		); settleErr != nil || !settleCreated {
			t.Fatalf("settle source archive repair %d: created=%v err=%v", index+1, settleCreated, settleErr)
		}
	}
	finalized, created, err := store.FinalizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || !created {
		t.Fatalf("finalize source archive layout: created=%v result=%+v err=%v", created, finalized, err)
	}
	pending := validProblemSourceRecognitionResult()
	pending.ParentInvocationID = parent.InvocationID
	pending.PhysicalResults = make(
		[]k12storage.ProblemSourceRecognitionPhysicalResultRef,
		len(finalized.PhysicalResults),
	)
	for index, evidence := range finalized.PhysicalResults {
		pending.PhysicalResults[index] = k12storage.ProblemSourceRecognitionPhysicalResultRef{
			PhysicalInvocationID:    evidence.PhysicalInvocationID,
			PhysicalUnit:            string(evidence.PhysicalUnit),
			RecognitionPlanVersion:  k12.RecognitionPlanVersionV2,
			PlanDigest:              evidence.PlanDigest,
			CandidateExactSetDigest: evidence.CandidateExactSetDigest,
			ResultDigest:            evidence.ResultDigest,
		}
	}
	typedDigest, err := k12storage.ProblemSourceRecognitionTypedResultDigest(pending)
	if err != nil {
		t.Fatal(err)
	}
	parent, err = store.MarkModelInvocationSucceeded(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		typedDigest,
		"provider-source-archive-parent",
	)
	if err != nil {
		t.Fatal(err)
	}
	return parent, plan, finalized, pending
}

func countProblemSourceArchiveRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	allowed := map[string]bool{
		"k12_model_physical_invocations": true,
	}
	if !allowed[table] {
		t.Fatalf("unsupported count table %q", table)
	}
	var count int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func seedProblemSourceArchiveV2TargetAttempts(
	t *testing.T,
	db *sql.DB,
	archive k12storage.ProblemSourceArchiveV6,
) {
	t.Helper()
	digests := map[string]string{}
	for _, input := range archive.InputRevisions {
		if input.CurrentDisposition == "current" {
			digests[input.ProblemID] = input.InputDigest
		}
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO k12_attempts (
		attempt_id,agent_name,submission_id,problem_id,answer_state,answer_raw,
		answer_markdown,confirmed_version,input_digest,bbox_json,created_at,updated_at
	) VALUES
		(?,?,?,?,?,?,?,?,?,?,?,?),
		(?,?,?,?,?,?,?,?,?,?,?,?)`,
		"restored-recognition-attempt-1", "mingming", recognitionSubmission,
		recognitionChildOne, "present", "旧答案一", "旧规范答案一", 2,
		digests[recognitionChildOne], `{"x":0.1,"y":0.2,"w":0.3,"h":0.1}`, 100, 100,
		"restored-recognition-attempt-2", "mingming", recognitionSubmission,
		recognitionChildTwo, "unclear", "旧答案二", "", 2,
		digests[recognitionChildTwo], `{"x":0.2,"y":0.3,"w":0.2,"h":0.1}`, 100, 100,
	); err != nil {
		t.Fatal(err)
	}
}
