package engineadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type recognitionLayoutFinalizingExecutorV2 struct {
	runtime             k12.RecognitionLayoutPlanRuntimeV2
	finalization        k12.RecognitionLayoutPlanFinalizationResultV2
	physical            k12.RecognitionPhysicalCallResult
	finalizationCreated bool
	finalizeCalls       int
}

type recognitionLayoutFinalizationMissingExecutorV2 struct {
	delegate *recognitionLayoutFinalizingExecutorV2
}

func (e recognitionLayoutFinalizationMissingExecutorV2) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	return e.delegate.ExecuteRecognitionPhysicalCall(ctx, call, send)
}

func (e recognitionLayoutFinalizationMissingExecutorV2) LoadRecognitionLayoutPlanV2Runtime(
	ctx context.Context,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	return e.delegate.LoadRecognitionLayoutPlanV2Runtime(ctx)
}

func (e recognitionLayoutFinalizationMissingExecutorV2) SettleRecognitionLayoutPrimaryBatchV2(
	ctx context.Context,
	source k12.RecognitionPhysicalCallResult,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	return e.delegate.SettleRecognitionLayoutPrimaryBatchV2(ctx, source, settlement)
}

func (e *recognitionLayoutFinalizingExecutorV2) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	_ k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	payload, err := send(ctx)
	if err != nil {
		return k12.RecognitionPhysicalCallResult{}, err
	}
	result := e.physical
	result.Payload = payload
	return result, nil
}

func (e *recognitionLayoutFinalizingExecutorV2) LoadRecognitionLayoutPlanV2Runtime(
	context.Context,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	return e.runtime, nil
}

func (e *recognitionLayoutFinalizingExecutorV2) SettleRecognitionLayoutPrimaryBatchV2(
	_ context.Context,
	_ k12.RecognitionPhysicalCallResult,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	if len(settlement.Candidates) != 1 ||
		settlement.Candidates[0].Classification != k12.RecognitionLayoutCandidateValidV2 {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false,
			fmt.Errorf("unexpected primary settlement: %+v", settlement)
	}
	return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{
		Classification:   k12.RecognitionLayoutBatchClassifiedV2,
		SettlementDigest: recognitionLayoutV2TestDigest("finalization-settlement"),
		FrozenResults: []k12.RecognitionLayoutCandidateResultReceiptV2{{
			CandidateID:  settlement.Candidates[0].CandidateID,
			ResultKind:   settlement.Candidates[0].ResultKind,
			ResultDigest: recognitionLayoutV2TestDigest("transient-candidate-result"),
		}},
	}, true, nil
}

func (e *recognitionLayoutFinalizingExecutorV2) FinalizeRecognitionLayoutPlanV2(
	context.Context,
) (k12.RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	e.finalizeCalls++
	return e.finalization, e.finalizationCreated, nil
}

func TestREGK12RecognitionBatchRepair20260808001FinalizesBeforeReturningDurablePlanOrder(
	t *testing.T,
) {
	pagePNG, plan, runtime, finalization, physical :=
		recognitionLayoutFinalizationAdapterFixtureV2(t, "durable-question")
	executor := &recognitionLayoutFinalizingExecutorV2{
		runtime: runtime, finalization: finalization, physical: physical,
		finalizationCreated: true,
	}
	headerDigest := runtime.HeaderDigest
	ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
	ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
	transientPayload := recognitionLayoutFinalizationBatchPayloadV2(
		t,
		plan.Targets[0],
		"transient-question",
	)

	questions, err := NewRecognizerAdapter(func(
		context.Context,
		[]byte,
		string,
	) (string, error) {
		return transientPayload, nil
	}).recognizeLayoutPrimaryBatchesV2(ctx, pagePNG, plan, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if executor.finalizeCalls != 1 {
		t.Fatalf("finalize calls=%d want=1", executor.finalizeCalls)
	}
	if len(questions) != 1 || questions[0].Question != "durable-question" {
		t.Fatalf("questions=%+v want only durable finalized projection", questions)
	}
}

func TestREGK12RecognitionBatchRepair20260808001MissingFinalizationCapabilityFailsClosed(
	t *testing.T,
) {
	pagePNG, plan, runtime, finalization, physical :=
		recognitionLayoutFinalizationAdapterFixtureV2(t, "durable-question")
	delegate := &recognitionLayoutFinalizingExecutorV2{
		runtime: runtime, finalization: finalization, physical: physical,
		finalizationCreated: true,
	}
	ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), runtime.HeaderDigest)
	ctx = k12.WithRecognitionPhysicalCallExecutor(
		ctx,
		recognitionLayoutFinalizationMissingExecutorV2{delegate: delegate},
	)
	payload := recognitionLayoutFinalizationBatchPayloadV2(
		t,
		plan.Targets[0],
		"transient-question",
	)

	questions, err := NewRecognizerAdapter(func(
		context.Context,
		[]byte,
		string,
	) (string, error) {
		return payload, nil
	}).recognizeLayoutPrimaryBatchesV2(ctx, pagePNG, plan, runtime)
	if !errors.Is(err, k12.ErrRecognitionLayoutPlanV2Unauthorized) {
		t.Fatalf("questions=%+v error=%v want unauthorized", questions, err)
	}
	if delegate.finalizeCalls != 0 {
		t.Fatalf("missing finalizer delegate calls=%d want=0", delegate.finalizeCalls)
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001ReplaysFinalizedPlanBeforeProvider(
	t *testing.T,
) {
	_, plan, runtime, finalization, physical :=
		recognitionLayoutFinalizationAdapterFixtureV2(t, "replayed-question")
	runtime.Status = "succeeded"
	executor := &recognitionLayoutFinalizingExecutorV2{
		runtime: runtime, finalization: finalization, physical: physical,
	}
	ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), runtime.HeaderDigest)
	ctx = k12.WithRecognitionLayoutFinalizationReplayV2(ctx)
	ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
	providerCalls := 0

	questions, err := NewRecognizerAdapter(func(
		context.Context,
		[]byte,
		string,
	) (string, error) {
		providerCalls++
		return "", fmt.Errorf("Provider must not run during finalized replay")
	}).Recognize(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if providerCalls != 0 {
		t.Fatalf("Provider calls=%d want=0", providerCalls)
	}
	if executor.finalizeCalls != 1 {
		t.Fatalf("finalization replay calls=%d want=1", executor.finalizeCalls)
	}
	if len(questions) != 1 || questions[0].Question != "replayed-question" ||
		finalization.CandidateResults[0].CandidateID != plan.Targets[0].TargetID {
		t.Fatalf("questions=%+v want durable replay in plan order", questions)
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001UnmarkedRunDoesNotProbeFinalizationReplay(
	t *testing.T,
) {
	_, _, runtime, finalization, physical :=
		recognitionLayoutFinalizationAdapterFixtureV2(t, "must-not-replay")
	runtime.Status = "succeeded"
	executor := &recognitionLayoutFinalizingExecutorV2{
		runtime: runtime, finalization: finalization, physical: physical,
	}
	ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), runtime.HeaderDigest)
	ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)

	_, err := NewRecognizerAdapter(func(
		context.Context,
		[]byte,
		string,
	) (string, error) {
		return "", fmt.Errorf("unexpected Provider call")
	}).Recognize(ctx, nil)
	if err == nil || err.Error() != "recognizer: 空图片" {
		t.Fatalf("unmarked recognition error=%v want ordinary empty-image guard", err)
	}
	if executor.finalizeCalls != 0 {
		t.Fatalf("unmarked finalization replay calls=%d want=0", executor.finalizeCalls)
	}
}

func TestREGK12RecognitionBatchRepair20260808001FinalizedDecoderRejectsCandidateIdentityDrift(
	t *testing.T,
) {
	_, plan, _, finalization, _ :=
		recognitionLayoutFinalizationAdapterFixtureV2(t, "durable-question")
	finalization.CandidateResults[0].CandidateID = "candidate-outside-plan"
	var err error
	finalization.CandidateResultsExactSetDigest, err =
		k12.RecognitionLayoutCandidateResultsExactSetDigestV2(
			finalization.CandidateResults,
		)
	if err != nil {
		t.Fatal(err)
	}

	questions, decodeErr := RecognizedQuestionsFromLayoutFinalizationV2(
		finalization,
		plan,
	)
	if !errors.Is(decodeErr, k12.ErrRecognitionLayoutPlanV2Unauthorized) {
		t.Fatalf("decode questions=%+v error=%v want unauthorized", questions, decodeErr)
	}
}

func TestREGK12RecognitionBatchRepair20260808001FinalizedDecoderRejectsSourceDrift(
	t *testing.T,
) {
	_, plan, _, finalization, _ :=
		recognitionLayoutFinalizationAdapterFixtureV2(t, "durable-question")
	wrongUnit, err := k12.RecognitionLayoutRepairUnitV2(2)
	if err != nil {
		t.Fatal(err)
	}
	finalization.CandidateResults[0].SourcePhysicalUnit = wrongUnit
	finalization.PhysicalResults[1].PhysicalUnit = wrongUnit
	finalization.CandidateResultsExactSetDigest, err =
		k12.RecognitionLayoutCandidateResultsExactSetDigestV2(
			finalization.CandidateResults,
		)
	if err != nil {
		t.Fatal(err)
	}
	finalization.PhysicalResultsExactSetDigest, err =
		k12.RecognitionLayoutPhysicalResultsExactSetDigestV2(
			finalization.PhysicalResults,
		)
	if err != nil {
		t.Fatal(err)
	}

	questions, decodeErr := RecognizedQuestionsFromLayoutFinalizationV2(
		finalization,
		plan,
	)
	if !errors.Is(decodeErr, k12.ErrRecognitionLayoutPlanV2Unauthorized) {
		t.Fatalf("decode questions=%+v error=%v want unauthorized", questions, decodeErr)
	}
}

func TestREGK12RecognitionBatchRepair20260808001FinalizedDecoderRejectsNonQuestionPayload(
	t *testing.T,
) {
	_, plan, _, finalization, _ :=
		recognitionLayoutFinalizationAdapterFixtureV2(t, "durable-question")
	finalization.CandidateResults[0].ResultKind =
		k12.RecognitionLayoutCandidateNonQuestionV2
	finalization.CandidateResults[0].ResultJSON = []byte(`{"reason":"heading"}`)
	var err error
	finalization.CandidateResultsExactSetDigest, err =
		k12.RecognitionLayoutCandidateResultsExactSetDigestV2(
			finalization.CandidateResults,
		)
	if err != nil {
		t.Fatal(err)
	}

	questions, decodeErr := RecognizedQuestionsFromLayoutFinalizationV2(
		finalization,
		plan,
	)
	if !errors.Is(decodeErr, k12.ErrRecognitionLayoutPlanV2Unauthorized) {
		t.Fatalf("decode questions=%+v error=%v want unauthorized", questions, decodeErr)
	}
}

func recognitionLayoutFinalizationAdapterFixtureV2(
	t *testing.T,
	durableQuestion string,
) (
	[]byte,
	k12.RecognitionLayoutPlanV2,
	k12.RecognitionLayoutPlanRuntimeV2,
	k12.RecognitionLayoutPlanFinalizationResultV2,
	k12.RecognitionPhysicalCallResult,
) {
	t.Helper()
	pagePNG, seedPlan := recognitionLayoutV2DispatchPlan(t, 1)
	manifestID := "modelphysical-11111111111111111111111111111111"
	manifestDigest := recognitionLayoutV2TestDigest("adapter-finalization-manifest")
	plan, err := k12.BuildRecognitionLayoutPlanV2(k12.RecognitionLayoutPlanInputV2{
		PagePNG: pagePNG,
		Manifest: k12.RecognitionLayoutManifestSuccessV2{
			InvocationID: manifestID,
			ResultDigest: manifestDigest,
		},
		Targets: []k12.RecognitionLayoutManifestTargetV2{{
			ManifestRef:      "manifest_0001",
			ManifestOrder:    1,
			SourceNumberPath: append([]string(nil), seedPlan.Targets[0].SourceNumberPath...),
			DisplayLabel:     seedPlan.Targets[0].DisplayLabel,
			Region:           seedPlan.Targets[0].Region,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	headerDigest := recognitionLayoutV2TestDigest("adapter-finalization-header")
	candidateExactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(
		[]string{plan.Targets[0].TargetID},
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := recognitionLayoutV2RuntimeFixture(
		headerDigest,
		plan,
		1,
		time.Now().Add(-time.Second).UnixMilli(),
		time.Now().Add(5*time.Minute).UnixMilli(),
	)
	runtime.Header.PlanID = "layout-plan-adapter-finalization-v2"
	runtime.Header.ParentInvocationID = "modelinv-adapter-finalization-v2"
	runtime.ManifestPhysicalInvocationID = manifestID
	runtime.ManifestResultDigest = manifestDigest
	runtime.CandidateExactSetDigest = candidateExactSetDigest

	resultJSON := recognitionLayoutFinalizationQuestionJSONV2(
		t,
		plan.Targets[0],
		durableQuestion,
	)
	batchID := "modelphysical-22222222222222222222222222222222"
	batchDigest := recognitionLayoutV2TestDigest("adapter-finalization-batch")
	batchExactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(
		plan.Batches[0].TargetIDs,
	)
	if err != nil {
		t.Fatal(err)
	}
	finalization := k12.RecognitionLayoutPlanFinalizationResultV2{
		PlanID:                  runtime.Header.PlanID,
		PlanDigest:              plan.AuthorizedPlanDigest,
		CandidateExactSetDigest: candidateExactSetDigest,
		CandidateResultCount:    1,
		PhysicalResultCount:     2,
		CandidateResults: []k12.RecognitionLayoutCandidateFinalResultV2{{
			CandidateID:                plan.Targets[0].TargetID,
			ResultKind:                 k12.RecognitionLayoutCandidateQuestionV2,
			ResultDigest:               recognitionLayoutV2TestDigest("durable-candidate-result"),
			ResultJSON:                 resultJSON,
			SourcePhysicalInvocationID: batchID,
			SourcePhysicalUnit:         plan.Batches[0].Unit,
			SourcePhysicalResultDigest: batchDigest,
		}},
		PhysicalResults: []k12.RecognitionLayoutPhysicalResultEvidenceV2{
			{
				PhysicalInvocationID: manifestID,
				PhysicalUnit:         k12.RecognitionPhysicalUnitWholePage,
				ResultDigest:         manifestDigest,
				PlanDigest:           headerDigest,
				Attempt:              1,
			},
			{
				PhysicalInvocationID:    batchID,
				PhysicalUnit:            plan.Batches[0].Unit,
				ResultDigest:            batchDigest,
				PlanDigest:              plan.AuthorizedPlanDigest,
				CandidateExactSetDigest: batchExactSetDigest,
				Attempt:                 1,
			},
		},
	}
	finalization.CandidateResultsExactSetDigest, err =
		k12.RecognitionLayoutCandidateResultsExactSetDigestV2(finalization.CandidateResults)
	if err != nil {
		t.Fatal(err)
	}
	finalization.PhysicalResultsExactSetDigest, err =
		k12.RecognitionLayoutPhysicalResultsExactSetDigestV2(finalization.PhysicalResults)
	if err != nil {
		t.Fatal(err)
	}
	_, finalization.FinalizationDigest, err =
		k12.CanonicalRecognitionLayoutPlanFinalizationV2(
			runtime.Header.ParentInvocationID,
			finalization,
		)
	if err != nil {
		t.Fatal(err)
	}
	physical := k12.RecognitionPhysicalCallResult{
		InvocationID: batchID,
		ResultDigest: batchDigest,
	}
	return pagePNG, plan, runtime, finalization, physical
}

func recognitionLayoutFinalizationBatchPayloadV2(
	t *testing.T,
	target k12.RecognitionLayoutTargetV2,
	question string,
) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"items": []map[string]any{{
			"target_id": target.TargetID,
			"kind":      "question",
			"recognition": map[string]any{
				"problem_kind":       "standalone",
				"parent_problem_id":  "",
				"subproblem_no":      "",
				"source_number_path": target.SourceNumberPath,
				"display_label":      target.DisplayLabel,
				"question":           question,
				"subject":            "数学",
				"answer_state":       "blank",
				"student_answer":     "",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func recognitionLayoutFinalizationQuestionJSONV2(
	t *testing.T,
	target k12.RecognitionLayoutTargetV2,
	question string,
) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"answer_state":       "blank",
		"display_label":      target.DisplayLabel,
		"parent_problem_id":  "",
		"problem_kind":       "standalone",
		"question":           question,
		"source_number_path": target.SourceNumberPath,
		"student_answer":     "",
		"subject":            "数学",
		"subproblem_no":      "",
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func recognitionLayoutV2PhysicalIDForUnit(
	unit k12.RecognitionPhysicalUnit,
) string {
	digest := recognitionLayoutV2TestDigest(string(unit))
	return "modelphysical-" + digest[len("sha256:"):len("sha256:")+32]
}

func (e *recognitionLayoutV2BatchExecutor) FinalizeRecognitionLayoutPlanV2(
	ctx context.Context,
) (k12.RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	runtime, err := e.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false, err
	}
	e.mu.Lock()
	settlements := append(
		[]k12.RecognitionLayoutPrimaryBatchSettlementV2(nil),
		e.primarySettlements...,
	)
	e.finalizeCalls++
	created := e.finalizeCalls == 1
	e.mu.Unlock()
	result, err := recognitionLayoutFinalizationFromSettlementsV2(
		runtime,
		settlements,
		nil,
	)
	return result, created, err
}

func (e *recognitionLayoutRepairWaveExecutorV2) FinalizeRecognitionLayoutPlanV2(
	context.Context,
) (k12.RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	e.mu.Lock()
	primary := append(
		[]k12.RecognitionLayoutPrimaryBatchSettlementV2(nil),
		e.primarySettlements...,
	)
	repairs := append(
		[]k12.RecognitionLayoutRepairSettlementV2(nil),
		e.repairSettlements...,
	)
	runtime := e.runtime
	e.finalizeCalls++
	created := e.finalizeCalls == 1
	e.mu.Unlock()
	result, err := recognitionLayoutFinalizationFromSettlementsV2(
		runtime,
		primary,
		repairs,
	)
	return result, created, err
}

func (e *recognitionLayoutV2ImmediateExecutor) FinalizeRecognitionLayoutPlanV2(
	ctx context.Context,
) (k12.RecognitionLayoutPlanFinalizationResultV2, bool, error) {
	runtime, err := e.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return k12.RecognitionLayoutPlanFinalizationResultV2{}, false, err
	}
	e.mu.Lock()
	settlements := append(
		[]k12.RecognitionLayoutPrimaryBatchSettlementV2(nil),
		e.primarySettlements...,
	)
	e.finalizeCalls++
	created := e.finalizeCalls == 1
	e.mu.Unlock()
	result, err := recognitionLayoutFinalizationFromSettlementsV2(
		runtime,
		settlements,
		nil,
	)
	return result, created, err
}

func recognitionLayoutFinalizationFromSettlementsV2(
	runtime k12.RecognitionLayoutPlanRuntimeV2,
	primarySettlements []k12.RecognitionLayoutPrimaryBatchSettlementV2,
	repairSettlements []k12.RecognitionLayoutRepairSettlementV2,
) (k12.RecognitionLayoutPlanFinalizationResultV2, error) {
	var zero k12.RecognitionLayoutPlanFinalizationResultV2
	if runtime.AuthorizedPlan == nil {
		return zero, fmt.Errorf("test finalizer has no authorized plan")
	}
	plan := *runtime.AuthorizedPlan
	primaryByUnit := make(
		map[k12.RecognitionPhysicalUnit]k12.RecognitionLayoutPrimaryBatchSettlementV2,
		len(plan.Batches),
	)
	candidates := make(
		map[string]k12.RecognitionLayoutCandidateFinalResultV2,
		len(plan.Targets),
	)
	for _, settlement := range primarySettlements {
		primaryByUnit[settlement.SourcePhysicalUnit] = settlement
		for _, candidate := range settlement.Candidates {
			if candidate.Classification != k12.RecognitionLayoutCandidateValidV2 {
				continue
			}
			candidates[candidate.CandidateID] =
				k12.RecognitionLayoutCandidateFinalResultV2{
					CandidateID:                candidate.CandidateID,
					ResultKind:                 candidate.ResultKind,
					ResultDigest:               recognitionLayoutV2TestDigest("final:" + candidate.CandidateID),
					ResultJSON:                 append(json.RawMessage(nil), candidate.ResultJSON...),
					SourcePhysicalInvocationID: settlement.SourcePhysicalInvocationID,
					SourcePhysicalUnit:         settlement.SourcePhysicalUnit,
					SourcePhysicalResultDigest: settlement.SourcePhysicalResultDigest,
				}
		}
	}
	repairByCandidate := make(
		map[string]k12.RecognitionLayoutRepairSettlementV2,
		len(repairSettlements),
	)
	for _, settlement := range repairSettlements {
		repairByCandidate[settlement.CandidateID] = settlement
		if settlement.Classification != k12.RecognitionLayoutCandidateValidV2 {
			continue
		}
		candidates[settlement.CandidateID] =
			k12.RecognitionLayoutCandidateFinalResultV2{
				CandidateID:                settlement.CandidateID,
				ResultKind:                 settlement.ResultKind,
				ResultDigest:               recognitionLayoutV2TestDigest("final:" + settlement.CandidateID),
				ResultJSON:                 append(json.RawMessage(nil), settlement.ResultJSON...),
				SourcePhysicalInvocationID: settlement.SourcePhysicalInvocationID,
				SourcePhysicalUnit:         settlement.SourcePhysicalUnit,
				SourcePhysicalResultDigest: settlement.SourcePhysicalResultDigest,
			}
	}
	result := k12.RecognitionLayoutPlanFinalizationResultV2{
		PlanID:                  runtime.Header.PlanID,
		PlanDigest:              plan.AuthorizedPlanDigest,
		CandidateExactSetDigest: runtime.CandidateExactSetDigest,
		PhysicalResults: []k12.RecognitionLayoutPhysicalResultEvidenceV2{{
			PhysicalInvocationID: runtime.ManifestPhysicalInvocationID,
			PhysicalUnit:         k12.RecognitionPhysicalUnitWholePage,
			ResultDigest:         runtime.ManifestResultDigest,
			PlanDigest:           runtime.HeaderDigest,
			Attempt:              1,
		}},
	}
	for _, target := range plan.Targets {
		candidate, exists := candidates[target.TargetID]
		if !exists {
			return zero, fmt.Errorf("test finalizer candidate %q is unresolved", target.TargetID)
		}
		result.CandidateResults = append(result.CandidateResults, candidate)
	}
	for _, batch := range plan.Batches {
		settlement, exists := primaryByUnit[batch.Unit]
		if !exists {
			return zero, fmt.Errorf("test finalizer batch %q is unsettled", batch.Unit)
		}
		exactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(
			batch.TargetIDs,
		)
		if err != nil {
			return zero, err
		}
		result.PhysicalResults = append(
			result.PhysicalResults,
			k12.RecognitionLayoutPhysicalResultEvidenceV2{
				PhysicalInvocationID:    settlement.SourcePhysicalInvocationID,
				PhysicalUnit:            batch.Unit,
				ResultDigest:            settlement.SourcePhysicalResultDigest,
				PlanDigest:              plan.AuthorizedPlanDigest,
				CandidateExactSetDigest: exactSetDigest,
				Attempt:                 1,
			},
		)
	}
	for _, target := range plan.Targets {
		settlement, repaired := repairByCandidate[target.TargetID]
		if !repaired ||
			settlement.Classification != k12.RecognitionLayoutCandidateValidV2 {
			continue
		}
		exactSetDigest, err := k12.RecognitionLayoutTargetExactSetDigestV2(
			[]string{target.TargetID},
		)
		if err != nil {
			return zero, err
		}
		result.PhysicalResults = append(
			result.PhysicalResults,
			k12.RecognitionLayoutPhysicalResultEvidenceV2{
				PhysicalInvocationID:    settlement.SourcePhysicalInvocationID,
				PhysicalUnit:            settlement.SourcePhysicalUnit,
				ResultDigest:            settlement.SourcePhysicalResultDigest,
				PlanDigest:              plan.AuthorizedPlanDigest,
				CandidateExactSetDigest: exactSetDigest,
				Attempt:                 1,
			},
		)
	}
	result.CandidateResultCount = len(result.CandidateResults)
	result.PhysicalResultCount = len(result.PhysicalResults)
	var err error
	result.CandidateResultsExactSetDigest, err =
		k12.RecognitionLayoutCandidateResultsExactSetDigestV2(result.CandidateResults)
	if err != nil {
		return zero, err
	}
	result.PhysicalResultsExactSetDigest, err =
		k12.RecognitionLayoutPhysicalResultsExactSetDigestV2(result.PhysicalResults)
	if err != nil {
		return zero, err
	}
	_, result.FinalizationDigest, err =
		k12.CanonicalRecognitionLayoutPlanFinalizationV2(
			runtime.Header.ParentInvocationID,
			result,
		)
	if err != nil {
		return zero, err
	}
	return result, nil
}
