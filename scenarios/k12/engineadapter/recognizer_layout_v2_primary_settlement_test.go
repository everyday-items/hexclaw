package engineadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestREGK12RecognitionBatchRepair20260808001ClassifiesSucceededPrimaryBatch(
	t *testing.T,
) {
	targets := recognitionLayoutPrimaryClassificationTargetsV2()

	t.Run("valid question and non-question are canonical in plan order", func(t *testing.T) {
		raw := fmt.Sprintf(
			`{"items":[{"recognition":null,"kind":"non_question","target_id":%q},{"kind":"question","target_id":%q,"recognition":{"student_answer":"","question":"2+2=","source_number_path":["1"],"answer_state":"blank","display_label":"1.","problem_kind":"standalone","subproblem_no":"","parent_problem_id":"","subject":"数学"}}]}`,
			targets[1].TargetID,
			targets[0].TargetID,
		)

		got := classifyRecognitionLayoutBatchV2(raw, targets)
		if got.classification != k12.RecognitionLayoutBatchClassifiedV2 ||
			got.ambiguityKind != "" {
			t.Fatalf("classification=%q ambiguity=%q", got.classification, got.ambiguityKind)
		}
		want := []k12.RecognitionLayoutCandidateSettlementV2{
			{
				CandidateID:    targets[0].TargetID,
				Classification: k12.RecognitionLayoutCandidateValidV2,
				ResultKind:     k12.RecognitionLayoutCandidateQuestionV2,
				ResultJSON: []byte(
					`{"answer_state":"blank","display_label":"1.","parent_problem_id":"","problem_kind":"standalone","question":"2+2=","source_number_path":["1"],"student_answer":"","subject":"数学","subproblem_no":""}`,
				),
			},
			{
				CandidateID:    targets[1].TargetID,
				Classification: k12.RecognitionLayoutCandidateValidV2,
				ResultKind:     k12.RecognitionLayoutCandidateNonQuestionV2,
				ResultJSON:     []byte(`{}`),
			},
		}
		if !reflect.DeepEqual(got.candidates, want) {
			t.Fatalf("candidate settlement drifted\ngot=%s\nwant=%s", got.candidates, want)
		}
		if len(got.outcomes) != 2 || got.outcomes[0].targetID != targets[0].TargetID ||
			got.outcomes[0].question == nil ||
			got.outcomes[1].targetID != targets[1].TargetID ||
			got.outcomes[1].question != nil {
			t.Fatalf("valid outcomes are not in plan order: %+v", got.outcomes)
		}
	})

	t.Run("authorized omissions are missing", func(t *testing.T) {
		raw := fmt.Sprintf(
			`{"items":[{"target_id":%q,"kind":"non_question","recognition":null}]}`,
			targets[1].TargetID,
		)
		got := classifyRecognitionLayoutBatchV2(raw, targets)
		if got.classification != k12.RecognitionLayoutBatchClassifiedV2 ||
			len(got.candidates) != 2 ||
			got.candidates[0].CandidateID != targets[0].TargetID ||
			got.candidates[0].Classification != k12.RecognitionLayoutCandidateMissingV2 ||
			got.candidates[1].Classification != k12.RecognitionLayoutCandidateValidV2 {
			t.Fatalf("missing classification drifted: %+v", got)
		}
	})

	invalidPayloads := map[string]string{
		"known target with extra item field": fmt.Sprintf(
			`{"items":[{"target_id":%q,"kind":"non_question","recognition":null,"extra":true}]}`,
			targets[0].TargetID,
		),
		"known target with invalid kind type": fmt.Sprintf(
			`{"items":[{"target_id":%q,"kind":7,"recognition":null}]}`,
			targets[0].TargetID,
		),
		"known target with invalid recognition": fmt.Sprintf(
			`{"items":[{"target_id":%q,"kind":"question","recognition":{"problem_kind":"standalone","source_number_path":["1"],"display_label":"1.","question":""}}]}`,
			targets[0].TargetID,
		),
		"known target with malformed source type": fmt.Sprintf(
			`{"items":[{"target_id":%q,"kind":"question","recognition":{"problem_kind":"standalone","source_number_path":7,"display_label":"1.","question":"2+2="}}]}`,
			targets[0].TargetID,
		),
		"known target with null source type": fmt.Sprintf(
			`{"items":[{"target_id":%q,"kind":"question","recognition":{"problem_kind":"standalone","source_number_path":null,"display_label":"1.","question":"2+2="}}]}`,
			targets[0].TargetID,
		),
	}
	for name, raw := range invalidPayloads {
		t.Run(name+" is candidate invalid", func(t *testing.T) {
			got := classifyRecognitionLayoutBatchV2(raw, targets[:1])
			if got.classification != k12.RecognitionLayoutBatchClassifiedV2 ||
				len(got.candidates) != 1 ||
				got.candidates[0].CandidateID != targets[0].TargetID ||
				got.candidates[0].Classification != k12.RecognitionLayoutCandidateInvalidV2 ||
				len(got.candidates[0].ResultJSON) != 0 {
				t.Fatalf("invalid candidate classification drifted: %+v", got)
			}
		})
	}

	ambiguousPayloads := map[string]struct {
		raw  string
		kind k12.RecognitionLayoutBatchAmbiguityKindV2
	}{
		"unknown extra target": {
			raw:  `{"items":[{"target_id":"target-extra","kind":"non_question","recognition":null}]}`,
			kind: k12.RecognitionLayoutAmbiguityExtraCandidateV2,
		},
		"duplicate target": {
			raw: fmt.Sprintf(
				`{"items":[{"target_id":%q,"kind":"non_question","recognition":null},{"target_id":%q,"kind":"non_question","recognition":null}]}`,
				targets[0].TargetID,
				targets[0].TargetID,
			),
			kind: k12.RecognitionLayoutAmbiguityDuplicateCandidateV2,
		},
		"entry cannot be attributed": {
			raw:  `{"items":[{"target_id":7,"kind":"non_question","recognition":null}]}`,
			kind: k12.RecognitionLayoutAmbiguityUnattributableV2,
		},
		"top level cannot be attributed": {
			raw:  `{"items":[],"explanation":"guess"}`,
			kind: k12.RecognitionLayoutAmbiguityUnattributableV2,
		},
		"malformed top level": {
			raw:  `not-json`,
			kind: k12.RecognitionLayoutAmbiguityUnattributableV2,
		},
		"source numbering conflicts with manifest": {
			raw: fmt.Sprintf(
				`{"items":[{"target_id":%q,"kind":"question","recognition":{"problem_kind":"standalone","parent_problem_id":"","subproblem_no":"","source_number_path":["9"],"display_label":"9.","question":"2+2=","answer_state":"blank","student_answer":""}}]}`,
				targets[0].TargetID,
			),
			kind: k12.RecognitionLayoutAmbiguitySourceConflictV2,
		},
		"explicit source conflict dominates other item invalidity": {
			raw: fmt.Sprintf(
				`{"items":[{"target_id":%q,"kind":"question","recognition":{"problem_kind":"standalone","source_number_path":["9"],"display_label":"9.","question":"2+2=","unexpected":true}}]}`,
				targets[0].TargetID,
			),
			kind: k12.RecognitionLayoutAmbiguitySourceConflictV2,
		},
	}
	for name, test := range ambiguousPayloads {
		t.Run(name+" is terminal for the full batch", func(t *testing.T) {
			got := classifyRecognitionLayoutBatchV2(test.raw, targets)
			if got.classification != k12.RecognitionLayoutBatchTerminalAmbiguousV2 ||
				got.ambiguityKind != test.kind || len(got.candidates) != 0 ||
				len(got.outcomes) != 0 {
				t.Fatalf("terminal ambiguity drifted: %+v", got)
			}
		})
	}
}

type recognitionLayoutPrimarySettlementExecutorV2 struct {
	mu sync.Mutex

	runtime         k12.RecognitionLayoutPlanRuntimeV2
	payload         string
	transportErr    error
	projectionDrift bool
	providerSends   int
	cached          map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult
	sources         []k12.RecognitionPhysicalCallResult
	settlements     []k12.RecognitionLayoutPrimaryBatchSettlementV2
}

func (e *recognitionLayoutPrimarySettlementExecutorV2) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	e.mu.Lock()
	if cached, exists := e.cached[call.Unit]; exists {
		e.mu.Unlock()
		return cached, nil
	}
	e.providerSends++
	transportErr := e.transportErr
	payload := e.payload
	e.mu.Unlock()
	if transportErr != nil {
		return k12.RecognitionPhysicalCallResult{}, transportErr
	}
	providerPayload, err := send(ctx)
	if err != nil {
		return k12.RecognitionPhysicalCallResult{}, err
	}
	if providerPayload != payload {
		return k12.RecognitionPhysicalCallResult{}, fmt.Errorf(
			"provider payload=%q want configured=%q",
			providerPayload,
			payload,
		)
	}
	result := k12.RecognitionPhysicalCallResult{
		Payload:      payload,
		InvocationID: "physical-" + string(call.Unit),
		ResultDigest: recognitionLayoutV2TestDigest(payload),
	}
	e.mu.Lock()
	if e.cached == nil {
		e.cached = make(map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult)
	}
	e.cached[call.Unit] = result
	e.mu.Unlock()
	return result, nil
}

func (e *recognitionLayoutPrimarySettlementExecutorV2) LoadRecognitionLayoutPlanV2Runtime(
	context.Context,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runtime, nil
}

func (e *recognitionLayoutPrimarySettlementExecutorV2) SettleRecognitionLayoutPrimaryBatchV2(
	_ context.Context,
	source k12.RecognitionPhysicalCallResult,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sources = append(e.sources, source)
	e.settlements = append(
		e.settlements,
		cloneRecognitionLayoutPrimarySettlementV2(settlement),
	)
	result := recognitionLayoutPrimarySettlementProjectionV2(e.runtime, settlement)
	if e.projectionDrift && len(result.FrozenResults) > 0 {
		result.FrozenResults[0].CandidateID = "drifted-candidate"
	}
	return result, len(e.settlements) == 1, nil
}

func TestREGK12RecognitionBatchRepair20260808001SettlesSucceededPrimaryBatch(
	t *testing.T,
) {
	pagePNG, plan := recognitionLayoutV2DispatchPlan(t, 2)
	headerDigest := recognitionLayoutV2TestDigest("primary-settlement-header")
	runtime := recognitionLayoutV2RuntimeFixture(
		headerDigest,
		plan,
		1,
		time.Now().Add(-time.Second).UnixMilli(),
		time.Now().Add(5*time.Minute).UnixMilli(),
	)

	t.Run("valid exact-set settles by physical identity and replay does not resend", func(t *testing.T) {
		payload := recognitionLayoutPrimaryValidPayloadV2(t, plan, true)
		executor := &recognitionLayoutPrimarySettlementExecutorV2{
			runtime: runtime,
			payload: payload,
			cached:  make(map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult),
		}
		ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
		ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
		adapter := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) {
			return payload, nil
		})

		first := adapter.recognizeLayoutPrimaryBatchV2(ctx, pagePNG, plan, runtime, 0)
		if first.err != nil || len(first.outcomes) != 2 {
			t.Fatalf("first settled result outcomes=%d err=%v", len(first.outcomes), first.err)
		}
		second := adapter.recognizeLayoutPrimaryBatchV2(ctx, pagePNG, plan, runtime, 0)
		if second.err != nil || len(second.outcomes) != 2 {
			t.Fatalf("replayed settled result outcomes=%d err=%v", len(second.outcomes), second.err)
		}

		executor.mu.Lock()
		defer executor.mu.Unlock()
		if executor.providerSends != 1 || len(executor.settlements) != 2 ||
			len(executor.sources) != 2 {
			t.Fatalf(
				"provider sends/settlements/sources=%d/%d/%d want=1/2/2",
				executor.providerSends,
				len(executor.settlements),
				len(executor.sources),
			)
		}
		if !reflect.DeepEqual(executor.settlements[0], executor.settlements[1]) {
			t.Fatalf("exact replay changed settlement facts")
		}
		settlement := executor.settlements[0]
		source := executor.sources[0]
		if settlement.PlanDigest != plan.AuthorizedPlanDigest ||
			settlement.SourcePhysicalInvocationID != source.InvocationID ||
			settlement.SourcePhysicalUnit != plan.Batches[0].Unit ||
			settlement.SourcePhysicalResultDigest != source.ResultDigest ||
			settlement.Classification != k12.RecognitionLayoutBatchClassifiedV2 ||
			len(settlement.Candidates) != 2 {
			t.Fatalf("settlement lost physical/plan identity: %+v source=%+v", settlement, source)
		}
		for index, candidate := range settlement.Candidates {
			if candidate.CandidateID != plan.Batches[0].TargetIDs[index] ||
				candidate.Classification != k12.RecognitionLayoutCandidateValidV2 {
				t.Fatalf("candidate %d is not valid in plan order: %+v", index, candidate)
			}
		}
	})

	t.Run("missing and identifiable invalid expose durable repair authorizations", func(t *testing.T) {
		payload := fmt.Sprintf(
			`{"items":[{"target_id":%q,"kind":"question","recognition":{"problem_kind":"standalone","source_number_path":["1"],"display_label":"1.","question":""}}]}`,
			plan.Batches[0].TargetIDs[0],
		)
		executor := &recognitionLayoutPrimarySettlementExecutorV2{
			runtime: runtime,
			payload: payload,
			cached:  make(map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult),
		}
		ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
		ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
		got := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) {
			return payload, nil
		}).recognizeLayoutPrimaryBatchV2(ctx, pagePNG, plan, runtime, 0)
		if got.err != nil {
			t.Fatalf("repairable primary batch must expose its durable authorizations: %v", got.err)
		}
		authorizations := reflect.ValueOf(got).FieldByName("repairAuthorizations")
		if !authorizations.IsValid() {
			t.Fatal("primary batch result did not expose durable repair authorizations")
		}
		if authorizations.Len() != 2 {
			t.Fatalf("repair authorization count=%d want=2", authorizations.Len())
		}
		executor.mu.Lock()
		defer executor.mu.Unlock()
		if len(executor.settlements) != 1 ||
			len(executor.settlements[0].Candidates) != 2 ||
			executor.settlements[0].Candidates[0].Classification != k12.RecognitionLayoutCandidateInvalidV2 ||
			executor.settlements[0].Candidates[1].Classification != k12.RecognitionLayoutCandidateMissingV2 {
			t.Fatalf("repairable exact complement was not settled: %+v", executor.settlements)
		}
	})

	t.Run("terminal ambiguity settles the full batch with zero repair", func(t *testing.T) {
		payload := `{"items":[{"target_id":"target-extra","kind":"non_question","recognition":null}]}`
		executor := &recognitionLayoutPrimarySettlementExecutorV2{
			runtime: runtime,
			payload: payload,
			cached:  make(map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult),
		}
		ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
		ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
		got := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) {
			return payload, nil
		}).recognizeLayoutPrimaryBatchV2(ctx, pagePNG, plan, runtime, 0)
		if !errors.Is(got.err, k12.ErrRecognitionProtocolInvalid) {
			t.Fatalf("terminal ambiguity error=%v want protocol-invalid", got.err)
		}
		executor.mu.Lock()
		defer executor.mu.Unlock()
		if len(executor.settlements) != 1 ||
			executor.settlements[0].Classification != k12.RecognitionLayoutBatchTerminalAmbiguousV2 ||
			executor.settlements[0].AmbiguityKind != k12.RecognitionLayoutAmbiguityExtraCandidateV2 ||
			len(executor.settlements[0].Candidates) != 0 {
			t.Fatalf("terminal batch settlement drifted: %+v", executor.settlements)
		}
		projection := recognitionLayoutPrimarySettlementProjectionV2(
			runtime,
			executor.settlements[0],
		)
		if len(projection.FrozenResults) != 0 ||
			len(projection.RepairAuthorizations) != 0 ||
			!reflect.DeepEqual(projection.UnresolvedCandidateIDs, plan.Batches[0].TargetIDs) {
			t.Fatalf("terminal projection granted partial result/repair: %+v", projection)
		}
	})

	t.Run("durable settlement projection drift fails closed", func(t *testing.T) {
		payload := recognitionLayoutPrimaryValidPayloadV2(t, plan, false)
		executor := &recognitionLayoutPrimarySettlementExecutorV2{
			runtime:         runtime,
			payload:         payload,
			projectionDrift: true,
			cached:          make(map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult),
		}
		ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
		ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
		got := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) {
			return payload, nil
		}).recognizeLayoutPrimaryBatchV2(ctx, pagePNG, plan, runtime, 0)
		if !errors.Is(got.err, k12.ErrRecognitionLayoutPlanV2Unauthorized) {
			t.Fatalf("settlement projection drift error=%v want unauthorized", got.err)
		}
	})
}

func TestREGK12RecognitionBatchRepair20260808001TransportNeverSettlesPrimaryBatch(
	t *testing.T,
) {
	pagePNG, plan := recognitionLayoutV2DispatchPlan(t, 1)
	headerDigest := recognitionLayoutV2TestDigest("primary-transport-header")
	runtime := recognitionLayoutV2RuntimeFixture(
		headerDigest,
		plan,
		1,
		time.Now().Add(-time.Second).UnixMilli(),
		time.Now().Add(5*time.Minute).UnixMilli(),
	)
	tests := map[string]error{
		"timeout":         context.DeadlineExceeded,
		"cancel":          context.Canceled,
		"transport":       io.ErrUnexpectedEOF,
		"outcome unknown": errors.New("provider outcome unknown"),
	}
	for name, transportErr := range tests {
		t.Run(name, func(t *testing.T) {
			executor := &recognitionLayoutPrimarySettlementExecutorV2{
				runtime:      runtime,
				transportErr: transportErr,
				cached:       make(map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult),
			}
			ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
			ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
			got := NewRecognizerAdapter(func(context.Context, []byte, string) (string, error) {
				t.Fatal("transport failure must not produce a model payload")
				return "", nil
			}).recognizeLayoutPrimaryBatchV2(ctx, pagePNG, plan, runtime, 0)
			if !errors.Is(got.err, transportErr) {
				t.Fatalf("primary error=%v want errors.Is(%v)", got.err, transportErr)
			}
			executor.mu.Lock()
			defer executor.mu.Unlock()
			if len(executor.settlements) != 0 || len(executor.sources) != 0 {
				t.Fatalf("transport failure crossed settlement boundary: %+v", executor.settlements)
			}
		})
	}
}

func recognitionLayoutPrimaryValidPayloadV2(
	t *testing.T,
	plan k12.RecognitionLayoutPlanV2,
	secondNonQuestion bool,
) string {
	t.Helper()
	items := make([]map[string]any, 0, len(plan.Batches[0].TargetIDs))
	targetByID := make(map[string]k12.RecognitionLayoutTargetV2, len(plan.Targets))
	for _, target := range plan.Targets {
		targetByID[target.TargetID] = target
	}
	for index, targetID := range plan.Batches[0].TargetIDs {
		target := targetByID[targetID]
		if secondNonQuestion && index == 1 {
			items = append(items, map[string]any{
				"target_id": targetID, "kind": "non_question", "recognition": nil,
			})
			continue
		}
		items = append(items, map[string]any{
			"target_id": targetID,
			"kind":      "question",
			"recognition": map[string]any{
				"problem_kind":       "standalone",
				"parent_problem_id":  "",
				"subproblem_no":      "",
				"source_number_path": target.SourceNumberPath,
				"display_label":      target.DisplayLabel,
				"question":           fmt.Sprintf("题目-%d", index+1),
				"subject":            "数学",
				"answer_state":       "blank",
				"student_answer":     "",
			},
		})
	}
	encoded, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func recognitionLayoutPrimarySettlementProjectionV2(
	runtime k12.RecognitionLayoutPlanRuntimeV2,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) k12.RecognitionLayoutPrimaryBatchSettlementResultV2 {
	result := k12.RecognitionLayoutPrimaryBatchSettlementResultV2{
		Classification:   settlement.Classification,
		SettlementDigest: recognitionLayoutV2TestDigest("settlement:" + settlement.SourcePhysicalInvocationID),
	}
	if settlement.Classification == k12.RecognitionLayoutBatchTerminalAmbiguousV2 {
		for _, batch := range runtime.AuthorizedPlan.Batches {
			if batch.Unit == settlement.SourcePhysicalUnit {
				result.UnresolvedCandidateIDs = append(
					[]string(nil),
					batch.TargetIDs...,
				)
				break
			}
		}
		return result
	}
	for index, candidate := range settlement.Candidates {
		switch candidate.Classification {
		case k12.RecognitionLayoutCandidateValidV2:
			result.FrozenResults = append(
				result.FrozenResults,
				k12.RecognitionLayoutCandidateResultReceiptV2{
					CandidateID:  candidate.CandidateID,
					ResultKind:   candidate.ResultKind,
					ResultDigest: recognitionLayoutV2TestDigest("candidate:" + candidate.CandidateID),
				},
			)
		case k12.RecognitionLayoutCandidateMissingV2,
			k12.RecognitionLayoutCandidateInvalidV2:
			unit, _ := k12.RecognitionLayoutRepairUnitV2(index + 1)
			result.RepairAuthorizations = append(
				result.RepairAuthorizations,
				k12.RecognitionLayoutRepairAuthorizationV2{
					AuthorizationID:     "authorization-" + candidate.CandidateID,
					AuthorizationDigest: recognitionLayoutV2TestDigest("authorization:" + candidate.CandidateID),
					CandidateID:         candidate.CandidateID,
					PhysicalUnit:        unit,
					RepairRound:         1,
				},
			)
			result.UnresolvedCandidateIDs = append(
				result.UnresolvedCandidateIDs,
				candidate.CandidateID,
			)
		}
	}
	return result
}

func cloneRecognitionLayoutPrimarySettlementV2(
	input k12.RecognitionLayoutPrimaryBatchSettlementV2,
) k12.RecognitionLayoutPrimaryBatchSettlementV2 {
	cloned := input
	cloned.Candidates = append(
		[]k12.RecognitionLayoutCandidateSettlementV2(nil),
		input.Candidates...,
	)
	for index := range cloned.Candidates {
		cloned.Candidates[index].ResultJSON = append(
			[]byte(nil),
			input.Candidates[index].ResultJSON...,
		)
	}
	return cloned
}

// 现有 v2 adapter fixture 用于模拟持久化 executor 边界。在此扩展它们，可以在每个成功的
// primary 子调用都必须经过结算能力后，继续保持原有调度断言不变。
func (e *recognitionLayoutV2BatchExecutor) SettleRecognitionLayoutPrimaryBatchV2(
	ctx context.Context,
	_ k12.RecognitionPhysicalCallResult,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	runtime, err := e.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, err
	}
	e.mu.Lock()
	e.primarySettlements = append(
		e.primarySettlements,
		cloneRecognitionLayoutPrimarySettlementV2(settlement),
	)
	e.mu.Unlock()
	return recognitionLayoutPrimarySettlementProjectionV2(runtime, settlement), true, nil
}

func (e *recognitionLayoutV2ImmediateExecutor) SettleRecognitionLayoutPrimaryBatchV2(
	ctx context.Context,
	_ k12.RecognitionPhysicalCallResult,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	runtime, err := e.LoadRecognitionLayoutPlanV2Runtime(ctx)
	if err != nil {
		return k12.RecognitionLayoutPrimaryBatchSettlementResultV2{}, false, err
	}
	e.mu.Lock()
	e.primarySettlements = append(
		e.primarySettlements,
		cloneRecognitionLayoutPrimarySettlementV2(settlement),
	)
	e.mu.Unlock()
	return recognitionLayoutPrimarySettlementProjectionV2(runtime, settlement), true, nil
}

func recognitionLayoutPrimaryClassificationTargetsV2() []k12.RecognitionLayoutTargetV2 {
	return []k12.RecognitionLayoutTargetV2{
		{
			TargetID:         "target-plan-0001",
			SourceNumberPath: []string{"1"},
			DisplayLabel:     "1.",
		},
		{
			TargetID:         "target-plan-0002",
			SourceNumberPath: []string{"2"},
			DisplayLabel:     "2.",
		},
	}
}
