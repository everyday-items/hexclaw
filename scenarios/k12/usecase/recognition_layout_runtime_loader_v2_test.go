package usecase

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestREGK12RecognitionDurabilityBudget20260808001LoadsAuthorizedRuntimeThroughExecutor(
	t *testing.T,
) {
	ctx := context.Background()
	db, store := openRecognitionPhysicalExecutorV2Store(
		t,
		t.TempDir()+"/layout-runtime-loader-v2.db",
	)
	defer db.Close()

	const agentName = "runtime-loader-owner"
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
		"runtime-loader-session",
		k12.GradingJobFields{
			SubmissionID:      "runtime-loader-submission",
			SourceKind:        "test",
			IdempotencyKey:    k12.BuildGradingIdempotencyKey("test", "runtime-loader", 0),
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

	pagePNG := recognitionPhysicalExecutorV2PagePNG(t, 40, 20)
	parent := k12.ModelInvocation{
		InvocationID:          "modelinv-runtime-loader-v2",
		AgentName:             agentName,
		JobID:                 job.RecordID,
		Stage:                 k12.GradingStageRecognizing,
		RequestDigest:         recognitionPhysicalExecutorV2Digest("runtime-loader-parent"),
		RouteSnapshot:         route,
		RequestPolicySnapshot: policy,
		Attempt:               1,
		CreatedAt:             time.Now().Unix(),
	}
	header := k12.RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-runtime-loader-v2",
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
		t.Fatalf("publish runtime fixture: created=%v parent=%+v manifest=%+v err=%v", created, storedParent, storedManifest, err)
	}
	if _, claimed, claimErr := store.ClaimModelPhysicalInvocationSent(
		ctx,
		agentName,
		manifestID,
	); claimErr != nil || !claimed {
		t.Fatalf("claim manifest: claimed=%v err=%v", claimed, claimErr)
	}
	const manifestPayload = `{"targets":"one"}`
	storedManifest, err = store.MarkModelPhysicalInvocationSucceededWithContent(
		ctx,
		agentName,
		manifestID,
		manifestPayload,
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
		Targets: []k12.RecognitionLayoutManifestTargetV2{{
			ManifestRef:      "manifest_0001",
			ManifestOrder:    1,
			SourceNumberPath: []string{"1"},
			DisplayLabel:     "1",
			Region:           k12.SourcePixelRegion{X: 0, Y: 0, Width: 40, Height: 20},
		}},
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
		t.Fatalf("authorize runtime fixture: %v", authorizeErr)
	}

	executor := newDurableRecognitionPhysicalCallExecutor(
		&GradingOrchestrator{deps: Deps{Records: store}},
		storedParent,
	)
	loadCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(
			k12.WithGradingModelRequestPolicy(ctx, policy),
			headerDigest,
		),
		executor,
	)
	runtime, err := k12.LoadRecognitionLayoutPlanV2Runtime(loadCtx)
	if err != nil {
		t.Fatalf("load authorized runtime through executor: %v", err)
	}
	if runtime.HeaderDigest != headerDigest ||
		runtime.Header.PhysicalCallCapMillis != 120000 ||
		runtime.Header.EffectiveConcurrency != 1 ||
		runtime.SelectedBucketMaxProblems != 1 ||
		runtime.StageDeadlineAtUnixMillis !=
			header.StageStartedAtUnixMillis+header.BudgetBuckets.UpTo1ProblemMillis ||
		runtime.AuthorizedPlan == nil ||
		runtime.AuthorizedPlan.AuthorizedPlanDigest != plan.AuthorizedPlanDigest {
		t.Fatalf("loaded runtime drifted: %+v", runtime)
	}

	missingLoaderCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(
			k12.WithGradingModelRequestPolicy(ctx, policy),
			headerDigest,
		),
		recognitionLayoutRuntimeMissingLoader{},
	)
	if _, err := k12.LoadRecognitionLayoutPlanV2Runtime(missingLoaderCtx); err == nil {
		t.Fatal("approved V2 context accepted an executor without a runtime loader")
	}

	driftedParent := storedParent
	driftedParent.AgentName = "different-owner"
	driftedExecutor := newDurableRecognitionPhysicalCallExecutor(
		&GradingOrchestrator{deps: Deps{Records: store}},
		driftedParent,
	)
	driftedCtx := k12.WithRecognitionPhysicalCallExecutor(
		k12.WithRecognitionLayoutPlanV2(ctx, headerDigest),
		driftedExecutor,
	)
	if _, err := k12.LoadRecognitionLayoutPlanV2Runtime(driftedCtx); err == nil {
		t.Fatal("runtime loader accepted a drifted parent owner")
	}

	if _, err := db.ExecContext(
		ctx,
		`DROP TRIGGER k12_recognition_layout_plan_deadline_once`,
	); err != nil {
		t.Fatalf("open deadline tamper fixture: %v", err)
	}
	if _, err := db.ExecContext(
		ctx,
		`UPDATE k12_recognition_layout_plans
		    SET stage_deadline_at=stage_deadline_at+1
		  WHERE plan_id=?`,
		header.PlanID,
	); err != nil {
		t.Fatalf("tamper selected deadline: %v", err)
	}
	if _, err := k12.LoadRecognitionLayoutPlanV2Runtime(loadCtx); err == nil {
		t.Fatal("runtime loader accepted a tampered selected deadline")
	}
}

type recognitionLayoutRuntimeMissingLoader struct{}

func (recognitionLayoutRuntimeMissingLoader) ExecuteRecognitionPhysicalCall(
	_ context.Context,
	_ k12.RecognitionPhysicalCall,
	_ func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	return k12.RecognitionPhysicalCallResult{}, fmt.Errorf("not used")
}
