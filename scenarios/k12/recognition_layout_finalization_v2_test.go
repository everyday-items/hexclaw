package k12

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"testing"
)

func TestREGK12RecognitionDurabilityBudget20260808001FinalizationDomainGate(
	t *testing.T,
) {
	runtime, want := recognitionLayoutFinalizationDomainFixture(t, "running")
	executor := &recognitionLayoutFinalizationDomainExecutor{
		runtime: runtime,
		result:  want,
		created: true,
	}
	ctx := WithRecognitionPhysicalCallExecutor(
		WithRecognitionLayoutPlanV2(context.Background(), runtime.HeaderDigest),
		executor,
	)
	got, created, err := FinalizeRecognitionLayoutPlanV2(ctx)
	if err != nil || !created || !reflect.DeepEqual(got, want) ||
		executor.finalizeCalls != 1 {
		t.Fatalf(
			"finalize through bound capability: created=%v calls=%d result=%+v err=%v",
			created,
			executor.finalizeCalls,
			got,
			err,
		)
	}

	missingCapability := recognitionLayoutFinalizationRuntimeOnly{runtime: runtime}
	missingCtx := WithRecognitionPhysicalCallExecutor(
		WithRecognitionLayoutPlanV2(context.Background(), runtime.HeaderDigest),
		missingCapability,
	)
	if _, _, err := FinalizeRecognitionLayoutPlanV2(missingCtx); err == nil {
		t.Fatal("V2 finalization accepted an executor without finalizer capability")
	}
	if _, _, err := FinalizeRecognitionLayoutPlanV2(context.Background()); err == nil {
		t.Fatal("V2 finalization accepted a context without a durable executor")
	}
	runtimeDrifts := []struct {
		name         string
		headerDigest string
		mutate       func(*RecognitionLayoutPlanRuntimeV2)
	}{
		{
			name:         "header",
			headerDigest: recognitionLayoutFinalizationTestDigest("different-header"),
			mutate:       func(*RecognitionLayoutPlanRuntimeV2) {},
		},
		{
			name:         "authorized_plan",
			headerDigest: runtime.HeaderDigest,
			mutate: func(changed *RecognitionLayoutPlanRuntimeV2) {
				changed.AuthorizedPlan = nil
			},
		},
		{
			name:         "status",
			headerDigest: runtime.HeaderDigest,
			mutate: func(changed *RecognitionLayoutPlanRuntimeV2) {
				changed.Status = "failed"
			},
		},
		{
			name:         "runtime_candidate_exact_set",
			headerDigest: runtime.HeaderDigest,
			mutate: func(changed *RecognitionLayoutPlanRuntimeV2) {
				changed.CandidateExactSetDigest =
					recognitionLayoutFinalizationTestDigest("different-runtime-set")
			},
		},
	}
	for _, test := range runtimeDrifts {
		t.Run(test.name+"_runtime_drift", func(t *testing.T) {
			changed := runtime
			test.mutate(&changed)
			drifted := &recognitionLayoutFinalizationDomainExecutor{
				runtime: changed,
				result:  want,
			}
			driftCtx := WithRecognitionPhysicalCallExecutor(
				WithRecognitionLayoutPlanV2(context.Background(), test.headerDigest),
				drifted,
			)
			if _, _, err := FinalizeRecognitionLayoutPlanV2(driftCtx); err == nil {
				t.Fatal("drifted finalization runtime was accepted")
			}
		})
	}

	driftCases := []struct {
		name   string
		mutate func(*RecognitionLayoutPlanFinalizationResultV2)
	}{
		{
			name: "plan_id",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.PlanID = "layout-plan-drifted"
			},
		},
		{
			name: "plan_digest",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.PlanDigest = recognitionLayoutFinalizationTestDigest("other-plan")
			},
		},
		{
			name: "candidate_exact_set",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.CandidateExactSetDigest = recognitionLayoutFinalizationTestDigest("other-set")
			},
		},
		{
			name: "candidate_results_exact_set_valid_sha",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.CandidateResultsExactSetDigest = recognitionLayoutFinalizationTestDigest("other-candidate-results")
			},
		},
		{
			name: "physical_results_exact_set_valid_sha",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.PhysicalResultsExactSetDigest = recognitionLayoutFinalizationTestDigest("other-physical-results")
			},
		},
		{
			name: "finalization_valid_sha",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.FinalizationDigest = recognitionLayoutFinalizationTestDigest("other-finalization")
			},
		},
		{
			name: "candidate_order",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.CandidateResults[0], result.CandidateResults[1] =
					result.CandidateResults[1], result.CandidateResults[0]
			},
		},
		{
			name: "candidate_kind",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.CandidateResults[0].ResultKind = "invented"
			},
		},
		{
			name: "candidate_result_digest",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.CandidateResults[0].ResultDigest = "sha256:invalid"
			},
		},
		{
			name: "candidate_source",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.CandidateResults[0].SourcePhysicalInvocationID =
					"modelphysical-00000000000000000000000000000000"
			},
		},
		{
			name: "physical_duplicate",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.PhysicalResults[2].PhysicalInvocationID =
					result.PhysicalResults[1].PhysicalInvocationID
			},
		},
		{
			name: "physical_order",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.PhysicalResults[0], result.PhysicalResults[1] =
					result.PhysicalResults[1], result.PhysicalResults[0]
			},
		},
		{
			name: "physical_attempt",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.PhysicalResults[1].Attempt = 2
			},
		},
		{
			name: "physical_extra",
			mutate: func(result *RecognitionLayoutPlanFinalizationResultV2) {
				result.PhysicalResults = append(
					result.PhysicalResults,
					result.PhysicalResults[1],
				)
				result.PhysicalResultCount++
			},
		},
	}
	for _, test := range driftCases {
		t.Run(test.name+"_drift", func(t *testing.T) {
			changed := cloneRecognitionLayoutFinalizationTestResult(t, want)
			test.mutate(&changed)
			drifted := &recognitionLayoutFinalizationDomainExecutor{
				runtime: runtime,
				result:  changed,
			}
			driftCtx := WithRecognitionPhysicalCallExecutor(
				WithRecognitionLayoutPlanV2(context.Background(), runtime.HeaderDigest),
				drifted,
			)
			if _, _, err := FinalizeRecognitionLayoutPlanV2(driftCtx); err == nil {
				t.Fatal("drifted finalization projection was accepted")
			}
		})
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001FinalizationReplayGate(
	t *testing.T,
) {
	t.Run("non_succeeded_is_not_replayed", func(t *testing.T) {
		runtime, result := recognitionLayoutFinalizationDomainFixture(t, "running")
		executor := &recognitionLayoutFinalizationDomainExecutor{
			runtime: runtime,
			result:  result,
		}
		ctx := WithRecognitionPhysicalCallExecutor(
			WithRecognitionLayoutPlanV2(context.Background(), runtime.HeaderDigest),
			executor,
		)
		got, replayed, err := ReplayFinalizedRecognitionLayoutPlanV2(ctx)
		if err != nil || replayed || !reflect.DeepEqual(got, RecognitionLayoutPlanFinalizationResultV2{}) ||
			executor.finalizeCalls != 0 {
			t.Fatalf(
				"non-succeeded replay: replayed=%v calls=%d result=%+v err=%v",
				replayed,
				executor.finalizeCalls,
				got,
				err,
			)
		}
	})

	t.Run("succeeded_is_read_only_replayed", func(t *testing.T) {
		runtime, result := recognitionLayoutFinalizationDomainFixture(t, "succeeded")
		executor := &recognitionLayoutFinalizationDomainExecutor{
			runtime: runtime,
			result:  result,
			created: false,
		}
		ctx := WithRecognitionPhysicalCallExecutor(
			WithRecognitionLayoutPlanV2(context.Background(), runtime.HeaderDigest),
			executor,
		)
		got, replayed, err := ReplayFinalizedRecognitionLayoutPlanV2(ctx)
		if err != nil || !replayed || !reflect.DeepEqual(got, result) ||
			executor.finalizeCalls != 1 {
			t.Fatalf(
				"succeeded replay: replayed=%v calls=%d result=%+v err=%v",
				replayed,
				executor.finalizeCalls,
				got,
				err,
			)
		}
	})

	t.Run("succeeded_cannot_create_a_new_receipt", func(t *testing.T) {
		runtime, result := recognitionLayoutFinalizationDomainFixture(t, "succeeded")
		executor := &recognitionLayoutFinalizationDomainExecutor{
			runtime: runtime,
			result:  result,
			created: true,
		}
		ctx := WithRecognitionPhysicalCallExecutor(
			WithRecognitionLayoutPlanV2(context.Background(), runtime.HeaderDigest),
			executor,
		)
		if _, _, err := ReplayFinalizedRecognitionLayoutPlanV2(ctx); err == nil {
			t.Fatal("succeeded runtime created a new finalization receipt during replay")
		}
	})
}

func TestREGK12RecognitionDurabilityBudget20260808001FinalizationReplayContextIsBoundToHeader(
	t *testing.T,
) {
	headerDigest := recognitionLayoutFinalizationTestDigest("replay-header")
	base := context.Background()
	if RecognitionLayoutFinalizationReplayV2Enabled(base) {
		t.Fatal("plain context unexpectedly enabled finalized-plan replay")
	}
	withoutHeader := WithRecognitionLayoutFinalizationReplayV2(base)
	if RecognitionLayoutFinalizationReplayV2Enabled(withoutHeader) {
		t.Fatal("replay marker was enabled without an immutable V2 header")
	}
	withHeader := WithRecognitionLayoutPlanV2(base, headerDigest)
	marked := WithRecognitionLayoutFinalizationReplayV2(withHeader)
	if !RecognitionLayoutFinalizationReplayV2Enabled(marked) {
		t.Fatal("header-bound finalized-plan replay marker was not enabled")
	}
	drifted := WithRecognitionLayoutPlanV2(
		marked,
		recognitionLayoutFinalizationTestDigest("different-header"),
	)
	if RecognitionLayoutFinalizationReplayV2Enabled(drifted) {
		t.Fatal("replay marker survived an immutable header drift")
	}
}

type recognitionLayoutFinalizationRuntimeOnly struct {
	runtime RecognitionLayoutPlanRuntimeV2
}

func (recognitionLayoutFinalizationRuntimeOnly) ExecuteRecognitionPhysicalCall(
	context.Context,
	RecognitionPhysicalCall,
	func(context.Context) (string, error),
) (RecognitionPhysicalCallResult, error) {
	return RecognitionPhysicalCallResult{}, nil
}

func (f recognitionLayoutFinalizationRuntimeOnly) LoadRecognitionLayoutPlanV2Runtime(
	context.Context,
) (RecognitionLayoutPlanRuntimeV2, error) {
	return f.runtime, nil
}

type recognitionLayoutFinalizationDomainExecutor struct {
	recognitionLayoutFinalizationRuntimeOnly
	runtime       RecognitionLayoutPlanRuntimeV2
	result        RecognitionLayoutPlanFinalizationResultV2
	created       bool
	finalizeCalls int
}

func (f *recognitionLayoutFinalizationDomainExecutor) LoadRecognitionLayoutPlanV2Runtime(
	context.Context,
) (RecognitionLayoutPlanRuntimeV2, error) {
	return f.runtime, nil
}

func (f *recognitionLayoutFinalizationDomainExecutor) FinalizeRecognitionLayoutPlanV2(
	context.Context,
) (RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	f.finalizeCalls++
	return f.result, f.created, nil
}

func recognitionLayoutFinalizationDomainFixture(
	t *testing.T,
	status string,
) (RecognitionLayoutPlanRuntimeV2, RecognitionLayoutPlanFinalizationResultV2) {
	t.Helper()
	var page bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 20; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 1, A: 255})
		}
	}
	if err := png.Encode(&page, img); err != nil {
		t.Fatal(err)
	}
	manifest := RecognitionLayoutManifestSuccessV2{
		InvocationID: "modelphysical-11111111111111111111111111111111",
		ResultDigest: recognitionLayoutFinalizationTestDigest("manifest-result"),
	}
	plan, err := BuildRecognitionLayoutPlanV2(RecognitionLayoutPlanInputV2{
		PagePNG:  page.Bytes(),
		Manifest: manifest,
		Targets: []RecognitionLayoutManifestTargetV2{
			{
				ManifestRef:      "manifest_0001",
				ManifestOrder:    1,
				SourceNumberPath: []string{"1"},
				DisplayLabel:     "1",
				Region:           SourcePixelRegion{X: 0, Y: 0, Width: 20, Height: 10},
			},
			{
				ManifestRef:      "manifest_0002",
				ManifestOrder:    2,
				SourceNumberPath: []string{"2"},
				DisplayLabel:     "2",
				Region:           SourcePixelRegion{X: 0, Y: 10, Width: 20, Height: 10},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := ApprovedRecognizingRequestPolicy()
	route := GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + RecognizingPolicyModel,
		Capability:               "vision",
		RecognizingRequestPolicy: policy,
	}
	header := RecognitionLayoutPlanHeaderV2{
		PlanID:                   "layout-plan-domain-finalization-v2",
		ParentInvocationID:       "modelinv-domain-finalization-v2",
		AgentName:                "domain-finalization-owner",
		JobID:                    "gradingjob-domain-finalization-v2",
		PageDigest:               plan.PageDigest,
		ParentRequestDigest:      recognitionLayoutFinalizationTestDigest("parent-request"),
		RouteSnapshot:            route,
		RequestPolicySnapshot:    policy,
		StageStartedAtUnixMillis: 1_000,
		PhysicalCallCapMillis:    120000,
		BudgetBuckets: RecognitionLayoutBudgetBucketsV2{
			UpTo1ProblemMillis:   120000,
			UpTo8ProblemsMillis:  600000,
			UpTo16ProblemsMillis: 600000,
			UpTo32ProblemsMillis: 600000,
		},
		AdapterWorkerHardCap: 2,
		EffectiveConcurrency: 1,
	}
	headerDigest, err := RecognitionLayoutPlanHeaderDigestV2(header)
	if err != nil {
		t.Fatal(err)
	}
	targetIDs := []string{plan.Targets[0].TargetID, plan.Targets[1].TargetID}
	candidateExactSetDigest, err := RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	if err != nil {
		t.Fatal(err)
	}
	batchExactSetDigest, err := RecognitionLayoutTargetExactSetDigestV2(
		plan.Batches[0].TargetIDs,
	)
	if err != nil {
		t.Fatal(err)
	}
	repairUnit, err := RecognitionLayoutRepairUnitV2(2)
	if err != nil {
		t.Fatal(err)
	}
	repairExactSetDigest, err := RecognitionLayoutTargetExactSetDigestV2(
		[]string{plan.Targets[1].TargetID},
	)
	if err != nil {
		t.Fatal(err)
	}
	batchID := "modelphysical-22222222222222222222222222222222"
	repairID := "modelphysical-33333333333333333333333333333333"
	batchResultDigest := recognitionLayoutFinalizationTestDigest("batch-result")
	repairResultDigest := recognitionLayoutFinalizationTestDigest("repair-result")
	result := RecognitionLayoutPlanFinalizationResultV2{
		PlanID:                  header.PlanID,
		PlanDigest:              plan.AuthorizedPlanDigest,
		CandidateExactSetDigest: candidateExactSetDigest,
		CandidateResultCount:    2,
		PhysicalResultCount:     3,
		CandidateResults: []RecognitionLayoutCandidateFinalResultV2{
			{
				CandidateID:                plan.Targets[0].TargetID,
				ResultKind:                 RecognitionLayoutCandidateQuestionV2,
				ResultDigest:               recognitionLayoutFinalizationTestDigest("candidate-1"),
				ResultJSON:                 []byte(`{"text":"2+2"}`),
				SourcePhysicalInvocationID: batchID,
				SourcePhysicalUnit:         plan.Batches[0].Unit,
				SourcePhysicalResultDigest: batchResultDigest,
			},
			{
				CandidateID:                plan.Targets[1].TargetID,
				ResultKind:                 RecognitionLayoutCandidateNonQuestionV2,
				ResultDigest:               recognitionLayoutFinalizationTestDigest("candidate-2"),
				ResultJSON:                 []byte(`{"reason":"column_title"}`),
				SourcePhysicalInvocationID: repairID,
				SourcePhysicalUnit:         repairUnit,
				SourcePhysicalResultDigest: repairResultDigest,
			},
		},
		PhysicalResults: []RecognitionLayoutPhysicalResultEvidenceV2{
			{
				PhysicalInvocationID: manifest.InvocationID,
				PhysicalUnit:         RecognitionPhysicalUnitWholePage,
				ResultDigest:         manifest.ResultDigest,
				PlanDigest:           headerDigest,
				Attempt:              1,
			},
			{
				PhysicalInvocationID:    batchID,
				PhysicalUnit:            plan.Batches[0].Unit,
				ResultDigest:            batchResultDigest,
				PlanDigest:              plan.AuthorizedPlanDigest,
				CandidateExactSetDigest: batchExactSetDigest,
				Attempt:                 1,
			},
			{
				PhysicalInvocationID:    repairID,
				PhysicalUnit:            repairUnit,
				ResultDigest:            repairResultDigest,
				PlanDigest:              plan.AuthorizedPlanDigest,
				CandidateExactSetDigest: repairExactSetDigest,
				Attempt:                 1,
			},
		},
	}
	result.CandidateResultsExactSetDigest, err =
		RecognitionLayoutCandidateResultsExactSetDigestV2(result.CandidateResults)
	if err != nil {
		t.Fatal(err)
	}
	result.PhysicalResultsExactSetDigest, err =
		RecognitionLayoutPhysicalResultsExactSetDigestV2(result.PhysicalResults)
	if err != nil {
		t.Fatal(err)
	}
	_, result.FinalizationDigest, err = CanonicalRecognitionLayoutPlanFinalizationV2(
		header.ParentInvocationID,
		result,
	)
	if err != nil {
		t.Fatal(err)
	}
	return RecognitionLayoutPlanRuntimeV2{
		Header:                       header,
		HeaderDigest:                 headerDigest,
		ManifestPhysicalInvocationID: manifest.InvocationID,
		ManifestResultDigest:         manifest.ResultDigest,
		CandidateExactSetDigest:      candidateExactSetDigest,
		SelectedBucketMaxProblems:    8,
		StageDeadlineAtUnixMillis:    601_000,
		Status:                       status,
		AuthorizedPlan:               &plan,
	}, result
}

func recognitionLayoutFinalizationTestDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneRecognitionLayoutFinalizationTestResult(
	t *testing.T,
	result RecognitionLayoutPlanFinalizationResultV2,
) RecognitionLayoutPlanFinalizationResultV2 {
	t.Helper()
	clone := result
	clone.CandidateResults = append(
		[]RecognitionLayoutCandidateFinalResultV2(nil),
		result.CandidateResults...,
	)
	for index := range clone.CandidateResults {
		clone.CandidateResults[index].ResultJSON = append(
			[]byte(nil),
			result.CandidateResults[index].ResultJSON...,
		)
	}
	clone.PhysicalResults = append(
		[]RecognitionLayoutPhysicalResultEvidenceV2(nil),
		result.PhysicalResults...,
	)
	return clone
}
