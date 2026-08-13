package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenario"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// REG-K12-RECOGNITION-DURABILITY-BUDGET-20260808-001：持久化执行器会把每项 V2
// 授权事实投影到物理账本，将方案授权委托给真实 Store，并在重启后准确重放成功的
// V2 结果，不会再次跨越 Provider 边界。
func TestREGK12RecognitionDurabilityBudget20260808001DurableExecutorV2ProjectionAndReplay(
	t *testing.T,
) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "durable-executor-v2.db")
	db, store := openRecognitionPhysicalExecutorV2Store(t, dbPath)
	dbOpen := true
	defer func() {
		if dbOpen {
			_ = db.Close()
		}
	}()

	const agentName = "mingming"
	if _, err := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES(?)`, agentName); err != nil {
		t.Fatalf("insert test agent: %v", err)
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
		"durable-executor-v2-session",
		k12.GradingJobFields{
			SubmissionID:      "durable-executor-v2-submission",
			SourceKind:        "test",
			IdempotencyKey:    k12.BuildGradingIdempotencyKey("test", "durable-executor-v2", 0),
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

	pagePNG := recognitionPhysicalExecutorV2PagePNG(t, 40, 260)
	parent := k12.ModelInvocation{
		InvocationID:          "modelinv-durable-executor-v2",
		AgentName:             agentName,
		JobID:                 job.RecordID,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         recognitionPhysicalExecutorV2Digest("parent-request"),
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               1,
		CreatedAt:             time.Now().Unix(),
	}
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-durable-executor-v2",
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
			UpTo8ProblemsMillis:  300000,
			UpTo16ProblemsMillis: 600000,
			UpTo32ProblemsMillis: 900000,
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
		Image:       pagePNG,
	}
	manifestRequestDigest, err := recognizingPhysicalInvocationDigest(parent, manifestCall)
	if err != nil {
		t.Fatal(err)
	}
	manifestID, err := stableRecognitionPhysicalInvocationIDForCall(
		parent.InvocationID,
		manifestCall,
	)
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
		t.Fatalf("publish V2 parent/manifest: created=%v parent=%+v manifest=%+v err=%v", created, storedParent, storedManifest, err)
	}

	executor := newDurableRecognitionPhysicalCallExecutor(
		&GradingOrchestrator{deps: Deps{
			Records: store,
			Now:     time.Now().Unix,
		}},
		storedParent,
	)
	const manifestPayload = `{"targets":"compact-manifest"}`
	var manifestSends atomic.Int32
	manifestResult, err := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		manifestCall,
		func(context.Context) (string, error) {
			manifestSends.Add(1)
			return manifestPayload, nil
		},
	)
	if err != nil {
		t.Fatalf("execute atomically published V2 manifest: %v", err)
	}
	if manifestSends.Load() != 1 ||
		manifestResult.InvocationID != manifestID ||
		manifestResult.ResultDigest != recognitionPhysicalExecutorV2Digest(manifestPayload) {
		t.Fatalf("manifest result=%+v sends=%d", manifestResult, manifestSends.Load())
	}

	targets := make([]k12.RecognitionLayoutManifestTargetV2, 13)
	for index := range targets {
		ordinal := index + 1
		targets[index] = k12.RecognitionLayoutManifestTargetV2{
			ManifestRef:      fmt.Sprintf("manifest_%04d", ordinal),
			ManifestOrder:    ordinal,
			SourceNumberPath: []string{fmt.Sprintf("%d", ordinal)},
			DisplayLabel:     fmt.Sprintf("%d", ordinal),
			Region: k12.SourcePixelRegion{
				X: 0, Y: index * 20, Width: 40, Height: 20,
			},
		}
	}
	plan, err := k12.BuildRecognitionLayoutPlanV2(k12.RecognitionLayoutPlanInputV2{
		PagePNG: pagePNG,
		Manifest: k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: manifestResult.InvocationID,
			ResultDigest: manifestResult.ResultDigest,
		},
		Targets: targets,
	})
	if err != nil {
		t.Fatalf("build V2 plan: %v", err)
	}
	if len(plan.Batches) != 4 {
		t.Fatalf("plan batches=%d want=4", len(plan.Batches))
	}
	authorizeCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(ctx, headerDigest),
		executor,
	)
	if authorizeErr := k12.AuthorizeRecognitionLayoutPlanV2(
		authorizeCtx,
		manifestResult,
		plan,
	); authorizeErr != nil {
		t.Fatalf("authorize V2 plan through durable executor: %v", authorizeErr)
	}

	batchCalls := make([]k12.RecognitionPhysicalCall, len(plan.Batches))
	for index, batch := range plan.Batches {
		batchCalls[index] = k12.RecognitionPhysicalCall{
			PlanVersion: k12.RecognitionPlanVersionV2,
			PlanDigest:  plan.AuthorizedPlanDigest,
			Unit:        batch.Unit,
			TargetIDs:   append([]string(nil), batch.TargetIDs...),
			Image:       []byte(fmt.Sprintf("deterministic-contact-sheet-%04d", index+1)),
		}
	}

	const succeededPayload = `{"results":"batch-0001"}`
	succeeded, err := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		batchCalls[0],
		func(context.Context) (string, error) { return succeededPayload, nil },
	)
	if err != nil {
		t.Fatalf("execute first V2 batch: %v", err)
	}
	storedBatch, err := store.GetModelPhysicalInvocation(
		ctx,
		agentName,
		succeeded.InvocationID,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantExactSet, err := k12.RecognitionLayoutTargetExactSetDigestV2(batchCalls[0].TargetIDs)
	if err != nil {
		t.Fatal(err)
	}
	if storedBatch.RecognitionPlanVersion != k12.RecognitionPlanVersionV2 ||
		storedBatch.PlanDigest != plan.AuthorizedPlanDigest ||
		storedBatch.CandidateExactSetDigest != wantExactSet {
		t.Fatalf("V2 child projection drifted: %+v", storedBatch)
	}

	// 已准备批次只有一个 CAS 获胜者。当获胜者位于 Provider 边界内时，
	// 重入会观察到 sent，且不得调用其发送函数。
	enteredProvider := make(chan struct{})
	releaseProvider := make(chan struct{})
	type executionResult struct {
		result k12.RecognitionPhysicalCallResult
		err    error
	}
	winner := make(chan executionResult, 1)
	go func() {
		result, executeErr := executor.ExecuteRecognitionPhysicalCall(
			ctx,
			batchCalls[1],
			func(context.Context) (string, error) {
				close(enteredProvider)
				<-releaseProvider
				return `{"results":"batch-0002"}`, nil
			},
		)
		winner <- executionResult{result: result, err: executeErr}
	}()
	<-enteredProvider
	var sentReentrySends atomic.Int32
	if _, reentryErr := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		batchCalls[1],
		func(context.Context) (string, error) {
			sentReentrySends.Add(1)
			return "must-not-send", nil
		},
	); reentryErr == nil {
		close(releaseProvider)
		t.Fatal("sent V2 child was replayed as a new Provider request")
	}
	if sentReentrySends.Load() != 0 {
		close(releaseProvider)
		t.Fatalf("sent V2 re-entry Provider sends=%d want=0", sentReentrySends.Load())
	}
	close(releaseProvider)
	if won := <-winner; won.err != nil || won.result.InvocationID == "" {
		t.Fatalf("prepared V2 CAS winner result=%+v err=%v", won.result, won.err)
	}

	// 结果未知和确定失败的子项对物理重发而言都属于终态，二次进入必须无副作用。
	if _, outcomeUnknownErr := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		batchCalls[2],
		func(context.Context) (string, error) {
			return "", errors.New("ambiguous transport reset")
		},
	); outcomeUnknownErr == nil {
		t.Fatal("ambiguous transport result did not become outcome_unknown")
	}
	if _, rejectionErr := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		batchCalls[3],
		func(context.Context) (string, error) {
			return "", recognitionPhysicalExecutorV2HTTPError{status: 400}
		},
	); rejectionErr == nil {
		t.Fatal("definitive Provider rejection did not become failed")
	}
	for index := 2; index < 4; index++ {
		var terminalReentrySends atomic.Int32
		if _, terminalErr := executor.ExecuteRecognitionPhysicalCall(
			ctx,
			batchCalls[index],
			func(context.Context) (string, error) {
				terminalReentrySends.Add(1)
				return "must-not-send", nil
			},
		); terminalErr == nil {
			t.Fatalf("terminal batch %d replay unexpectedly succeeded", index+1)
		}
		if terminalReentrySends.Load() != 0 {
			t.Fatalf("terminal batch %d Provider sends=%d want=0", index+1, terminalReentrySends.Load())
		}
	}

	if closeErr := db.Close(); closeErr != nil {
		t.Fatalf("close first SQLite process: %v", closeErr)
	}
	dbOpen = false
	restartedDB, restartedStore := openRecognitionPhysicalExecutorV2Store(t, dbPath)
	defer restartedDB.Close()
	restartedParent, err := restartedStore.GetModelInvocation(
		ctx,
		agentName,
		storedParent.InvocationID,
	)
	if err != nil {
		t.Fatalf("load parent after restart: %v", err)
	}
	restartedExecutor := newDurableRecognitionPhysicalCallExecutor(
		&GradingOrchestrator{deps: Deps{
			Records: restartedStore,
			Now:     time.Now().Unix,
		}},
		restartedParent,
	)
	var restartSends atomic.Int32
	replayed, err := restartedExecutor.ExecuteRecognitionPhysicalCall(
		ctx,
		batchCalls[0],
		func(context.Context) (string, error) {
			restartSends.Add(1)
			return "must-not-send", nil
		},
	)
	if err != nil {
		t.Fatalf("replay exact succeeded V2 child after restart: %v", err)
	}
	if restartSends.Load() != 0 || replayed != succeeded || replayed.Payload != succeededPayload {
		t.Fatalf("restart replay=%+v want=%+v sends=%d", replayed, succeeded, restartSends.Load())
	}
}

type recognitionPhysicalExecutorV2HTTPError struct {
	status int
}

func (e recognitionPhysicalExecutorV2HTTPError) Error() string {
	return fmt.Sprintf("provider HTTP %d", e.status)
}

func (e recognitionPhysicalExecutorV2HTTPError) ProviderResponseStatusCode() int {
	return e.status
}

func openRecognitionPhysicalExecutorV2Store(
	t *testing.T,
	path string,
) (*sql.DB, *k12storage.Store) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open file SQLite: %v", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		_ = db.Close()
		t.Fatalf("migrate file SQLite: %v", err)
	}
	registry := scenario.NewRegistry()
	if err := registry.Assemble(k12.Pack(k12.NewCurriculumStub())); err != nil {
		_ = db.Close()
		t.Fatalf("assemble K12 registry: %v", err)
	}
	return db, k12storage.NewStore(db, registry.Records)
}

func recognitionPhysicalExecutorV2PagePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	page := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			page.Set(x, y, color.RGBA{R: uint8(y), G: uint8(x * 3), B: 96, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, page); err != nil {
		t.Fatalf("encode page PNG: %v", err)
	}
	return encoded.Bytes()
}

func recognitionPhysicalExecutorV2Digest(value string) string {
	return recognitionPhysicalExecutorV2BytesDigest([]byte(value))
}

func recognitionPhysicalExecutorV2BytesDigest(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}
