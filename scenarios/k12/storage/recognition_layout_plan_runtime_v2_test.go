package k12storage_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-001：V2 密集页面识别计划与其
// parent 和 compact-manifest 子调用在同一事务中发布。只有精确成功的 manifest 才能授权
// 冻结的候选/批次精确集合，独立授权的 primary batch 不继承 V1 的串行 fallback 前驱门禁。
func TestREGK12RecognitionDurabilityBudget20260808001PersistsAndAuthorizesPrimaryPlan(
	t *testing.T,
) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recognition-layout-v2.db")
	store, db := openPhysicalLedgerFileStore(t, path)
	if err := migrate.Run(ctx, db, migrate.All); err != nil {
		t.Fatalf("migrate file db: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatalf("enable WAL: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		t.Fatalf("set busy timeout: %v", err)
	}
	db.SetMaxOpenConns(4)
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES(?)`, "mingming"); err != nil {
		t.Fatalf("insert agent: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", "recognition-layout-v2-runtime")
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}

	parent := unpreparedPhysicalInvocationParent(job.RecordID)
	pagePNG := recognitionLayoutRuntimeTestPagePNG(t)
	manifestContent := `{"targets":["manifest_0001","manifest_0002","manifest_0003","manifest_0004","manifest_0005"]}`
	manifestDigest := recognitionLayoutRuntimeTestDigest(manifestContent)
	plan, err := k12.BuildRecognitionLayoutPlanV2(k12.RecognitionLayoutPlanInputV2{
		PagePNG: pagePNG,
		Manifest: k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: "physical-layout-manifest",
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
		t.Fatalf("build V2 plan fixture: %v", err)
	}
	stageStartedAt := time.Now().UnixMilli()
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-runtime-v2",
		ParentInvocationID:       parent.InvocationID,
		AgentName:                parent.AgentName,
		JobID:                    parent.JobID,
		PageDigest:               plan.PageDigest,
		ParentRequestDigest:      parent.RequestDigest,
		RouteSnapshot:            parent.RouteSnapshot,
		RequestPolicySnapshot:    parent.RequestPolicySnapshot,
		StageStartedAtUnixMillis: stageStartedAt,
		PhysicalCallCapMillis:    120000,
		// 这些值仅用于聚焦的非发布 fixture；发布值来自冻结策略，本测试不会自行虚构。
		BudgetBuckets: k12.RecognitionLayoutBudgetBucketsV2{
			UpTo1ProblemMillis:   60000,
			UpTo8ProblemsMillis:  120000,
			UpTo16ProblemsMillis: 180000,
			UpTo32ProblemsMillis: 300000,
		},
		AdapterWorkerHardCap: 2,
		EffectiveConcurrency: 2,
	}
	headerDigest, err := k12.RecognitionLayoutPlanHeaderDigestV2(header)
	if err != nil {
		t.Fatalf("digest V2 header: %v", err)
	}
	manifestChild := newPhysicalInvocation(
		parent,
		"physical-layout-manifest",
		k12.RecognitionPhysicalUnitWholePage,
	)
	manifestChild.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
	manifestChild.PlanDigest = headerDigest

	storedParent, storedManifest, created, err :=
		store.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
			ctx,
			parent,
			manifestChild,
			header,
		)
	if err != nil || !created {
		t.Fatalf("atomic V2 publication: created=%v parent=%+v manifest=%+v err=%v", created, storedParent, storedManifest, err)
	}
	if storedParent.Status != k12.ModelInvocationSent ||
		storedManifest.Status != k12.ModelInvocationPrepared ||
		storedManifest.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		storedManifest.PlanDigest != headerDigest {
		t.Fatalf("wrong atomic V2 state: parent=%+v manifest=%+v", storedParent, storedManifest)
	}
	var headerCount int
	if headerReadErr := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM k12_recognition_layout_plans
		WHERE plan_id=? AND parent_invocation_id=? AND page_digest=?
		  AND header_digest=? AND status='prepared_manifest'
		  AND effective_concurrency=2`,
		header.PlanID, parent.InvocationID, plan.PageDigest, headerDigest,
	).Scan(&headerCount); headerReadErr != nil || headerCount != 1 {
		t.Fatalf("durable V2 header count=%d err=%v", headerCount, headerReadErr)
	}
	replayParent, replayManifest, replayCreated, err :=
		store.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
			ctx,
			parent,
			manifestChild,
			header,
		)
	if err != nil || replayCreated || replayParent != storedParent ||
		replayManifest != storedManifest {
		t.Fatalf("atomic V2 replay: created=%v parent=%+v manifest=%+v err=%v", replayCreated, replayParent, replayManifest, err)
	}

	if _, _, unauthorizedPrepareErr := store.PrepareModelPhysicalInvocation(
		ctx,
		recognitionLayoutRuntimeBatchInvocation(t, storedParent, plan, 0),
	); unauthorizedPrepareErr == nil {
		t.Fatal("unauthorized primary batch reached prepared state")
	}
	if _, claimed, manifestClaimErr := store.ClaimModelPhysicalInvocationSent(
		ctx,
		parent.AgentName,
		storedManifest.PhysicalInvocationID,
	); manifestClaimErr != nil || !claimed {
		t.Fatalf("claim manifest: claimed=%v err=%v", claimed, manifestClaimErr)
	}
	if _, manifestPersistErr := store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		parent.AgentName,
		storedManifest.PhysicalInvocationID,
		manifestContent,
		"provider-manifest-1",
	); manifestPersistErr != nil {
		t.Fatalf("persist succeeded manifest: %v", manifestPersistErr)
	}
	privateManifest, err := store.LoadSucceededModelPhysicalInvocationResultContent(
		ctx,
		parent.AgentName,
		storedManifest.PhysicalInvocationID,
		manifestDigest,
	)
	if err != nil || privateManifest != manifestContent {
		t.Fatalf("private manifest replay=%q err=%v", privateManifest, err)
	}
	if _, wrongDigestErr := store.LoadSucceededModelPhysicalInvocationResultContent(
		ctx,
		parent.AgentName,
		storedManifest.PhysicalInvocationID,
		recognitionLayoutRuntimeTestDigest("different"),
	); wrongDigestErr == nil {
		t.Fatal("private manifest replay accepted a caller-selected wrong digest")
	}
	if authorizationErr := store.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: storedManifest.PhysicalInvocationID,
			ResultDigest: manifestDigest,
		},
		plan,
	); authorizationErr != nil {
		t.Fatalf("authorize exact V2 plan: %v", authorizationErr)
	}
	if authorizationReplayErr := store.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: storedManifest.PhysicalInvocationID,
			ResultDigest: manifestDigest,
		},
		plan,
	); authorizationReplayErr != nil {
		t.Fatalf("authorize exact V2 replay: %v", authorizationReplayErr)
	}
	var selectedBucket int
	var selectedDeadline int64
	if budgetReadErr := db.QueryRowContext(ctx, `
		SELECT selected_bucket_max_problems,stage_deadline_at
		FROM k12_recognition_layout_plans WHERE plan_id=?`,
		header.PlanID,
	).Scan(&selectedBucket, &selectedDeadline); budgetReadErr != nil ||
		selectedBucket != 8 || selectedDeadline != stageStartedAt+120000 {
		t.Fatalf("selected budget bucket=%d deadline=%d want bucket=8 deadline=%d err=%v", selectedBucket, selectedDeadline, stageStartedAt+120000, budgetReadErr)
	}
	postAuthorizationParent, postAuthorizationManifest, postAuthorizationCreated, err :=
		store.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
			ctx,
			parent,
			manifestChild,
			header,
		)
	if err != nil || postAuthorizationCreated ||
		postAuthorizationParent != storedParent ||
		postAuthorizationManifest.Status != k12.ModelInvocationSucceeded {
		t.Fatalf("post-authorization initial replay: created=%v parent=%+v manifest=%+v err=%v", postAuthorizationCreated, postAuthorizationParent, postAuthorizationManifest, err)
	}

	driftedBatch := recognitionLayoutRuntimeBatchInvocation(t, storedParent, plan, 0)
	driftedBatch.PlanDigest = headerDigest
	if _, _, planDriftErr := store.PrepareModelPhysicalInvocation(ctx, driftedBatch); planDriftErr == nil {
		t.Fatal("primary batch with a drifted authorized-plan digest was prepared")
	}
	driftedBatch = recognitionLayoutRuntimeBatchInvocation(t, storedParent, plan, 0)
	driftedBatch.CandidateExactSetDigest = recognitionLayoutRuntimeTestDigest("different exact set")
	if _, _, exactSetDriftErr := store.PrepareModelPhysicalInvocation(ctx, driftedBatch); exactSetDriftErr == nil {
		t.Fatal("primary batch with a drifted target exact-set was prepared")
	}
	repair := recognitionLayoutRuntimeBatchInvocation(t, storedParent, plan, 0)
	repair.PhysicalInvocationID = "physical-layout-repair-1"
	repair.PhysicalUnit, err = k12.RecognitionLayoutRepairUnitV2(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, unauthorizedRepairErr := store.PrepareModelPhysicalInvocation(ctx, repair); unauthorizedRepairErr == nil {
		t.Fatal("repair reached prepared without an explicit repair authorization")
	}

	// 两个 primary batch 从同一起始门禁准备并认领；与旧 segment 链不同，二者都不等待前驱结果。
	start := make(chan struct{})
	errs := make(chan error, len(plan.Batches))
	var wg sync.WaitGroup
	for index := range plan.Batches {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			batch := recognitionLayoutRuntimeBatchInvocation(t, storedParent, plan, index)
			prepared, batchCreated, prepareErr := store.PrepareModelPhysicalInvocation(ctx, batch)
			if prepareErr != nil || !batchCreated {
				errs <- fmt.Errorf("prepare authorized batch %d: created=%v row=%+v err=%v", index+1, batchCreated, prepared, prepareErr)
				return
			}
			claimedRow, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
				ctx,
				parent.AgentName,
				batch.PhysicalInvocationID,
			)
			if claimErr != nil || !claimed || claimedRow.Status != k12.ModelInvocationSent {
				errs <- fmt.Errorf("claim independent batch %d: claimed=%v row=%+v err=%v", index+1, claimed, claimedRow, claimErr)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for workerErr := range errs {
		t.Error(workerErr)
	}
	if t.Failed() {
		t.FailNow()
	}

	legacyJob := newGradingJobRecord(t, "mingming", "recognition-layout-v1-restart")
	if _, legacyJobErr := store.Put(ctx, legacyJob); legacyJobErr != nil {
		t.Fatalf("create legacy job: %v", legacyJobErr)
	}
	legacyParent := preparePhysicalInvocationParent(t, store, legacyJob.RecordID)
	legacyChild, legacyCreated, err := store.PrepareModelPhysicalInvocation(
		ctx,
		newPhysicalInvocation(
			legacyParent,
			"physical-legacy-whole",
			k12.RecognitionPhysicalUnitWholePage,
		),
	)
	if err != nil || !legacyCreated ||
		legacyChild.RecognitionPlanVersion != k12.RecognitionPlanVersionV1 {
		t.Fatalf("prepare legacy V1 child: created=%v row=%+v err=%v", legacyCreated, legacyChild, err)
	}

	rollbackJob := newGradingJobRecord(t, "mingming", "recognition-layout-v2-rollback")
	if _, rollbackJobErr := store.Put(ctx, rollbackJob); rollbackJobErr != nil {
		t.Fatalf("create rollback job: %v", rollbackJobErr)
	}
	rollbackParent := unpreparedPhysicalInvocationParent(rollbackJob.RecordID)
	rollbackHeader := header
	rollbackHeader.ParentInvocationID = rollbackParent.InvocationID
	rollbackHeader.JobID = rollbackParent.JobID
	rollbackHeader.ParentRequestDigest = rollbackParent.RequestDigest
	rollbackHeader.RouteSnapshot = rollbackParent.RouteSnapshot
	rollbackHeader.RequestPolicySnapshot = rollbackParent.RequestPolicySnapshot
	rollbackDigest, err := k12.RecognitionLayoutPlanHeaderDigestV2(rollbackHeader)
	if err != nil {
		t.Fatal(err)
	}
	rollbackChild := newPhysicalInvocation(
		rollbackParent,
		"physical-layout-rollback",
		k12.RecognitionPhysicalUnitWholePage,
	)
	rollbackChild.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
	rollbackChild.PlanDigest = rollbackDigest
	if _, _, _, rollbackPrepareErr := store.PrepareRecognizingInvocationWithInitialLayoutPlanV2(
		ctx,
		rollbackParent,
		rollbackChild,
		rollbackHeader,
	); rollbackPrepareErr == nil {
		t.Fatal("duplicate plan identity did not fail the atomic V2 publication")
	}
	var leakedRows int
	if leakedRowsErr := db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM k12_model_invocations WHERE invocation_id=?) +
		  (SELECT COUNT(*) FROM k12_model_physical_invocations WHERE physical_invocation_id=?) +
		  (SELECT COUNT(*) FROM k12_recognition_layout_plans WHERE parent_invocation_id=?)`,
		rollbackParent.InvocationID,
		rollbackChild.PhysicalInvocationID,
		rollbackParent.InvocationID,
	).Scan(&leakedRows); leakedRowsErr != nil || leakedRows != 0 {
		t.Fatalf("failed V2 atomic publication leaked %d parent/child/header rows: %v", leakedRows, leakedRowsErr)
	}

	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close V2 runtime db: %v", closeErr)
	}
	restarted, restartedDB := openPhysicalLedgerFileStore(t, path)
	defer restartedDB.Close()
	loadedManifest, err := restarted.GetModelPhysicalInvocation(
		ctx,
		parent.AgentName,
		storedManifest.PhysicalInvocationID,
	)
	if err != nil || loadedManifest.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 || loadedManifest.PlanDigest != headerDigest {
		t.Fatalf("restart manifest=%+v err=%v", loadedManifest, err)
	}
	privateManifest, err = restarted.LoadSucceededModelPhysicalInvocationResultContent(
		ctx,
		parent.AgentName,
		storedManifest.PhysicalInvocationID,
		manifestDigest,
	)
	if err != nil || privateManifest != manifestContent {
		t.Fatalf("restart private manifest replay=%q err=%v", privateManifest, err)
	}
	runtime, err := restarted.LoadRecognitionLayoutPlanRuntimeV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
	)
	if err != nil || runtime.AuthorizedPlan == nil ||
		runtime.HeaderDigest != headerDigest ||
		runtime.Header.EffectiveConcurrency != 2 ||
		runtime.SelectedBucketMaxProblems != 8 ||
		runtime.StageDeadlineAtUnixMillis != stageStartedAt+120000 ||
		runtime.AuthorizedPlan.AuthorizedPlanDigest != plan.AuthorizedPlanDigest {
		t.Fatalf("restart layout runtime=%+v err=%v", runtime, err)
	}
	loadedLegacy, err := restarted.GetModelPhysicalInvocation(
		ctx,
		legacyParent.AgentName,
		legacyChild.PhysicalInvocationID,
	)
	if err != nil || loadedLegacy.RecognitionPlanVersion != k12.RecognitionPlanVersionV1 ||
		loadedLegacy.PlanDigest != "" || loadedLegacy.CandidateExactSetDigest != "" {
		t.Fatalf("restart legacy V1 child=%+v err=%v", loadedLegacy, err)
	}

	// V76 默认值会映射回独立的领域 V1 值，使旧行保持可读且不改变历史调用方契约。
	var legacyVersion string
	if err := restartedDB.QueryRowContext(ctx, `
		SELECT recognition_plan_version FROM k12_model_physical_invocations
		WHERE physical_invocation_id=?`, storedManifest.PhysicalInvocationID,
	).Scan(&legacyVersion); err != nil {
		t.Fatalf("read persisted V2 SQL version: %v", err)
	}
	if legacyVersion != "v2" {
		t.Fatalf("persisted SQL plan version=%q, want v2", legacyVersion)
	}
	if _, err := restartedDB.ExecContext(ctx, `
		DROP TRIGGER k12_recognition_layout_candidate_immutable`); err != nil {
		t.Fatalf("open tamper fixture: %v", err)
	}
	if _, err := restartedDB.ExecContext(ctx, `
		UPDATE k12_recognition_layout_candidates SET crop_digest=?
		WHERE plan_id=? AND ordinal=1`,
		recognitionLayoutRuntimeTestDigest("tampered crop"),
		header.PlanID,
	); err != nil {
		t.Fatalf("tamper one persisted candidate: %v", err)
	}
	if err := restarted.AuthorizeRecognitionLayoutPlanV2(
		ctx,
		parent.AgentName,
		parent.InvocationID,
		k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: storedManifest.PhysicalInvocationID,
			ResultDigest: manifestDigest,
		},
		plan,
	); err == nil {
		t.Fatal("restart authorization replay accepted a tampered candidate row")
	}
}

func recognitionLayoutRuntimeTestPagePNG(t *testing.T) []byte {
	t.Helper()
	page := image.NewRGBA(image.Rect(0, 0, 20, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 20; x++ {
			page.Set(x, y, color.RGBA{R: uint8(y * 3), G: uint8(x * 7), B: 64, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, page); err != nil {
		t.Fatalf("encode test page: %v", err)
	}
	return encoded.Bytes()
}

func recognitionLayoutRuntimeTestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func recognitionLayoutRuntimeBatchInvocation(
	t *testing.T,
	parent k12.ModelInvocation,
	plan k12.RecognitionLayoutPlanV2,
	index int,
) k12.ModelPhysicalInvocation {
	t.Helper()
	batch := plan.Batches[index]
	exactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(batch.TargetIDs)
	if err != nil {
		t.Fatalf("digest batch exact-set: %v", err)
	}
	invocation := newPhysicalInvocation(
		parent,
		"physical-layout-batch-"+string(rune('1'+index)),
		batch.Unit,
	)
	invocation.RecognitionPlanVersion = k12.RecognitionPlanVersionV2
	invocation.PlanDigest = plan.AuthorizedPlanDigest
	invocation.CandidateExactSetDigest = exactSetDigest
	return invocation
}
