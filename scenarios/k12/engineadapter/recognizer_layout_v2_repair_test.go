package engineadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type recognitionLayoutRepairWaveExecutorV2 struct {
	mu sync.Mutex

	runtime               k12.RecognitionLayoutPlanRuntimeV2
	payloadByUnit         map[k12.RecognitionPhysicalUnit]string
	transportErrByUnit    map[k12.RecognitionPhysicalUnit]error
	releaseByUnit         map[k12.RecognitionPhysicalUnit]<-chan struct{}
	started               chan k12.RecognitionPhysicalUnit
	repairSettled         chan k12.RecognitionPhysicalUnit
	repairProjectionDrift bool
	deadlineByUnit        map[k12.RecognitionPhysicalUnit]time.Time
	cached                map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult
	providerSends         map[k12.RecognitionPhysicalUnit]int
	calls                 []k12.RecognitionPhysicalCall
	primarySettlements    []k12.RecognitionLayoutPrimaryBatchSettlementV2
	repairSettlements     []k12.RecognitionLayoutRepairSettlementV2
	authorizationByID     map[string]k12.RecognitionLayoutRepairAuthorizationV2
	settledRepairByAuth   map[string]k12.RecognitionLayoutRepairSettlementResultV2
	finalizeCalls         int
}

func (e *recognitionLayoutRepairWaveExecutorV2) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	e.mu.Lock()
	call.TargetIDs = append([]string(nil), call.TargetIDs...)
	call.Image = append([]byte(nil), call.Image...)
	e.calls = append(e.calls, call)
	if deadline, ok := ctx.Deadline(); ok && e.deadlineByUnit != nil {
		e.deadlineByUnit[call.Unit] = deadline
	}
	if cached, exists := e.cached[call.Unit]; exists {
		e.mu.Unlock()
		return cached, nil
	}
	transportErr := e.transportErrByUnit[call.Unit]
	release := e.releaseByUnit[call.Unit]
	started := e.started
	e.mu.Unlock()
	if strings.HasPrefix(string(call.Unit), "layout_repair_") && started != nil {
		started <- call.Unit
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return k12.RecognitionPhysicalCallResult{}, ctx.Err()
		}
	}
	if transportErr != nil {
		return k12.RecognitionPhysicalCallResult{}, transportErr
	}
	e.mu.Lock()
	e.providerSends[call.Unit]++
	e.mu.Unlock()

	payload, err := send(context.WithValue(
		ctx,
		recognitionLayoutV2VisionCallContextKey{},
		call,
	))
	if err != nil {
		return k12.RecognitionPhysicalCallResult{}, err
	}
	result := k12.RecognitionPhysicalCallResult{
		Payload:      payload,
		InvocationID: recognitionLayoutV2PhysicalIDForUnit(call.Unit),
		ResultDigest: recognitionLayoutV2TestDigest(payload),
	}
	e.mu.Lock()
	e.cached[call.Unit] = result
	e.mu.Unlock()
	return result, nil
}

func (e *recognitionLayoutRepairWaveExecutorV2) LoadRecognitionLayoutPlanV2Runtime(
	context.Context,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runtime, nil
}

func (e *recognitionLayoutRepairWaveExecutorV2) SettleRecognitionLayoutPrimaryBatchV2(
	_ context.Context,
	_ k12.RecognitionPhysicalCallResult,
	settlement k12.RecognitionLayoutPrimaryBatchSettlementV2,
) (k12.RecognitionLayoutPrimaryBatchSettlementResultV2, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	projection := recognitionLayoutPrimarySettlementProjectionV2(e.runtime, settlement)
	e.primarySettlements = append(
		e.primarySettlements,
		cloneRecognitionLayoutPrimarySettlementV2(settlement),
	)
	for _, authorization := range projection.RepairAuthorizations {
		e.authorizationByID[authorization.AuthorizationID] = authorization
	}
	return projection, len(e.primarySettlements) == 1, nil
}

func (e *recognitionLayoutRepairWaveExecutorV2) SettleRecognitionLayoutRepairV2(
	_ context.Context,
	_ k12.RecognitionPhysicalCallResult,
	settlement k12.RecognitionLayoutRepairSettlementV2,
) (k12.RecognitionLayoutRepairSettlementResultV2, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	authorization, exists := e.authorizationByID[settlement.AuthorizationID]
	if !exists || authorization.AuthorizationDigest != settlement.AuthorizationDigest ||
		authorization.CandidateID != settlement.CandidateID ||
		authorization.PhysicalUnit != settlement.SourcePhysicalUnit {
		return k12.RecognitionLayoutRepairSettlementResultV2{}, false, fmt.Errorf(
			"repair settlement is detached from primary authorization",
		)
	}
	e.repairSettlements = append(
		e.repairSettlements,
		cloneRecognitionLayoutRepairSettlementV2(settlement),
	)
	if e.repairSettled != nil {
		e.repairSettled <- settlement.SourcePhysicalUnit
	}
	if replay, ok := e.settledRepairByAuth[settlement.AuthorizationID]; ok {
		return replay, false, nil
	}
	projection := k12.RecognitionLayoutRepairSettlementResultV2{
		Classification:   settlement.Classification,
		SettlementDigest: recognitionLayoutV2TestDigest("repair-settlement:" + settlement.AuthorizationID),
	}
	if settlement.Classification == k12.RecognitionLayoutCandidateValidV2 {
		projection.FrozenResult = &k12.RecognitionLayoutCandidateResultReceiptV2{
			CandidateID:  settlement.CandidateID,
			ResultKind:   settlement.ResultKind,
			ResultDigest: recognitionLayoutV2TestDigest("repair-result:" + settlement.CandidateID),
		}
	} else {
		projection.UnresolvedCandidateID = settlement.CandidateID
	}
	if e.repairProjectionDrift && projection.FrozenResult != nil {
		projection.FrozenResult.CandidateID = "drifted-candidate"
	}
	e.settledRepairByAuth[settlement.AuthorizationID] = projection
	return projection, true, nil
}

func TestREGK12RecognitionBatchRepair20260808001ExecutesOneAuthorizedSingletonWave(
	t *testing.T,
) {
	pagePNG, plan := recognitionLayoutV2DispatchPlan(t, 4)
	headerDigest := recognitionLayoutV2TestDigest("singleton-repair-wave-header")
	runtime := recognitionLayoutV2RuntimeFixture(
		headerDigest,
		plan,
		1,
		time.Now().Add(-time.Second).UnixMilli(),
		time.Now().Add(5*time.Minute).UnixMilli(),
	)
	primaryPayload := recognitionLayoutRepairPrimaryPayloadV2(t, plan)
	repairTwo := recognitionLayoutRepairQuestionPayloadV2(t, plan.Targets[1], "repair-2")
	repairThree, err := json.Marshal(map[string]any{
		"items": []map[string]any{{
			"target_id":   plan.Targets[2].TargetID,
			"kind":        "non_question",
			"recognition": nil,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	executor := &recognitionLayoutRepairWaveExecutorV2{
		runtime: runtime,
		payloadByUnit: map[k12.RecognitionPhysicalUnit]string{
			plan.Batches[0].Unit: primaryPayload,
			"layout_repair_0002": repairTwo,
			"layout_repair_0003": string(repairThree),
		},
		cached:              make(map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult),
		providerSends:       make(map[k12.RecognitionPhysicalUnit]int),
		authorizationByID:   make(map[string]k12.RecognitionLayoutRepairAuthorizationV2),
		settledRepairByAuth: make(map[string]k12.RecognitionLayoutRepairSettlementResultV2),
	}
	ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
	ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
	adapter := NewRecognizerAdapter(func(
		visionCtx context.Context,
		_ []byte,
		prompt string,
	) (string, error) {
		call, _ := visionCtx.Value(
			recognitionLayoutV2VisionCallContextKey{},
		).(k12.RecognitionPhysicalCall)
		payload, exists := executor.payloadByUnit[call.Unit]
		if !exists {
			return "", fmt.Errorf("unexpected Provider unit %q", call.Unit)
		}
		if strings.HasPrefix(string(call.Unit), "layout_repair_") {
			if len(call.TargetIDs) != 1 || !strings.Contains(prompt, call.TargetIDs[0]) {
				return "", fmt.Errorf("repair %q prompt is not singleton", call.Unit)
			}
			for _, target := range plan.Targets {
				if target.TargetID != call.TargetIDs[0] && strings.Contains(prompt, target.TargetID) {
					return "", fmt.Errorf("repair %q prompt leaked target %q", call.Unit, target.TargetID)
				}
			}
		}
		return payload, nil
	})

	wantQuestions := []string{"primary-1", "repair-2", "primary-4"}
	for attempt := 1; attempt <= 2; attempt++ {
		questions, recognizeErr := adapter.recognizeLayoutPrimaryBatchesV2(
			ctx,
			pagePNG,
			plan,
			runtime,
		)
		if recognizeErr != nil {
			t.Fatalf("attempt %d: %v", attempt, recognizeErr)
		}
		gotQuestions := make([]string, 0, len(questions))
		for _, question := range questions {
			gotQuestions = append(gotQuestions, question.Question)
		}
		if !reflect.DeepEqual(gotQuestions, wantQuestions) {
			t.Fatalf("attempt %d questions=%v want plan order=%v", attempt, gotQuestions, wantQuestions)
		}
	}

	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.calls) != 6 {
		t.Fatalf("physical execution attempts=%d want 3 first-run + 3 replay", len(executor.calls))
	}
	for unit, want := range map[k12.RecognitionPhysicalUnit]int{
		plan.Batches[0].Unit: 1,
		"layout_repair_0002": 1,
		"layout_repair_0003": 1,
	} {
		if got := executor.providerSends[unit]; got != want {
			t.Fatalf("Provider sends for %q=%d want=%d", unit, got, want)
		}
	}
	if len(executor.repairSettlements) != 4 {
		t.Fatalf("repair settlements=%d want 2 first-run + 2 replay", len(executor.repairSettlements))
	}
	firstRepairCalls := executor.calls[1:3]
	for index, call := range firstRepairCalls {
		target := plan.Targets[index+1]
		wantUnit := k12.RecognitionPhysicalUnit(fmt.Sprintf("layout_repair_%04d", index+2))
		if call.Unit != wantUnit || call.PlanVersion != k12.RecognitionPlanVersionV2 ||
			call.PlanDigest != plan.AuthorizedPlanDigest ||
			len(call.TargetIDs) != 1 || call.TargetIDs[0] != target.TargetID ||
			recognitionLayoutV2TestDigest(string(call.Image)) != target.CropDigest {
			t.Fatalf("repair call %d lost singleton plan/crop identity: %+v", index, call)
		}
		settlement := executor.repairSettlements[index]
		authorization := executor.authorizationByID[settlement.AuthorizationID]
		if settlement.PlanDigest != plan.AuthorizedPlanDigest ||
			settlement.AuthorizationDigest != authorization.AuthorizationDigest ||
			settlement.SourcePhysicalUnit != call.Unit ||
			settlement.CandidateID != call.TargetIDs[0] {
			t.Fatalf("repair settlement %d lost independent plan/auth identity: %+v", index, settlement)
		}
	}
}

func TestREGK12RecognitionBatchRepair20260808001RepairProtocolFailureIsDurablyTerminal(
	t *testing.T,
) {
	pagePNG, plan := recognitionLayoutV2DispatchPlan(t, 1)
	target := plan.Targets[0]
	validQuestion := recognitionLayoutRepairQuestionItemV2(target, "repair-valid-shape")
	conflictingQuestion := recognitionLayoutRepairQuestionItemV2(target, "repair-source-conflict")
	conflictingQuestion["recognition"].(map[string]any)["source_number_path"] = []string{"99"}
	conflictingQuestion["recognition"].(map[string]any)["display_label"] = "99."
	encode := func(value any) string {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	tests := map[string]string{
		"missing": `{"items":[]}`,
		"identifiable invalid": encode(map[string]any{
			"items": []map[string]any{{
				"target_id": target.TargetID,
				"kind":      "question",
				"recognition": map[string]any{
					"problem_kind":       "standalone",
					"source_number_path": target.SourceNumberPath,
					"display_label":      target.DisplayLabel,
					"question":           "",
				},
			}},
		}),
		"extra": encode(map[string]any{
			"items": []map[string]any{{
				"target_id": "target-extra", "kind": "non_question", "recognition": nil,
			}},
		}),
		"duplicate": encode(map[string]any{
			"items": []map[string]any{validQuestion, validQuestion},
		}),
		"source conflict": encode(map[string]any{
			"items": []map[string]any{conflictingQuestion},
		}),
		"unattributable": `not-json`,
	}
	for name, repairPayload := range tests {
		t.Run(name, func(t *testing.T) {
			headerDigest := recognitionLayoutV2TestDigest("repair-terminal-" + name)
			runtime := recognitionLayoutV2RuntimeFixture(
				headerDigest,
				plan,
				1,
				time.Now().Add(-time.Second).UnixMilli(),
				time.Now().Add(5*time.Minute).UnixMilli(),
			)
			executor := &recognitionLayoutRepairWaveExecutorV2{
				runtime: runtime,
				payloadByUnit: map[k12.RecognitionPhysicalUnit]string{
					plan.Batches[0].Unit: `{"items":[]}`,
					"layout_repair_0001": repairPayload,
				},
				cached:              make(map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult),
				providerSends:       make(map[k12.RecognitionPhysicalUnit]int),
				authorizationByID:   make(map[string]k12.RecognitionLayoutRepairAuthorizationV2),
				settledRepairByAuth: make(map[string]k12.RecognitionLayoutRepairSettlementResultV2),
			}
			ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
			ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
			adapter := NewRecognizerAdapter(func(
				visionCtx context.Context,
				_ []byte,
				_ string,
			) (string, error) {
				call, _ := visionCtx.Value(
					recognitionLayoutV2VisionCallContextKey{},
				).(k12.RecognitionPhysicalCall)
				return executor.payloadByUnit[call.Unit], nil
			})
			for attempt := 1; attempt <= 2; attempt++ {
				_, err := adapter.recognizeLayoutPrimaryBatchesV2(
					ctx,
					pagePNG,
					plan,
					runtime,
				)
				if !errors.Is(err, k12.ErrRecognitionProtocolInvalid) {
					t.Fatalf("attempt %d error=%v want terminal protocol-invalid", attempt, err)
				}
			}

			executor.mu.Lock()
			defer executor.mu.Unlock()
			if executor.providerSends[plan.Batches[0].Unit] != 1 ||
				executor.providerSends["layout_repair_0001"] != 1 ||
				len(executor.providerSends) != 2 {
				t.Fatalf("terminal result caused a second wave: sends=%v", executor.providerSends)
			}
			if len(executor.repairSettlements) != 2 {
				t.Fatalf("repair settlement attempts=%d want first + exact replay", len(executor.repairSettlements))
			}
			for _, settlement := range executor.repairSettlements {
				if settlement.Classification != k12.RecognitionLayoutCandidateInvalidV2 ||
					settlement.ResultKind != "" || len(settlement.ResultJSON) != 0 {
					t.Fatalf("non-unique repair was not durably terminal invalid: %+v", settlement)
				}
			}
			if !reflect.DeepEqual(
				executor.repairSettlements[0],
				executor.repairSettlements[1],
			) {
				t.Fatal("terminal repair replay changed settlement identity")
			}
		})
	}
}

func TestREGK12RecognitionBatchRepair20260808001RepairTransportStopsUndispatched(
	t *testing.T,
) {
	pagePNG, plan := recognitionLayoutV2DispatchPlan(t, 4)
	failures := map[string]error{
		"timeout":         context.DeadlineExceeded,
		"cancel":          context.Canceled,
		"transport":       io.ErrUnexpectedEOF,
		"outcome unknown": errors.New("repair provider outcome unknown"),
	}
	for name, transportErr := range failures {
		t.Run(name, func(t *testing.T) {
			headerDigest := recognitionLayoutV2TestDigest("repair-transport-" + name)
			runtime := recognitionLayoutV2RuntimeFixture(
				headerDigest,
				plan,
				1,
				time.Now().Add(-time.Second).UnixMilli(),
				time.Now().Add(5*time.Minute).UnixMilli(),
			)
			executor := newRecognitionLayoutRepairWaveExecutorV2(
				runtime,
				plan,
				`{"items":[]}`,
			)
			executor.transportErrByUnit = map[k12.RecognitionPhysicalUnit]error{
				"layout_repair_0001": transportErr,
			}
			ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
			ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
			_, err := NewRecognizerAdapter(
				recognitionLayoutRepairWaveVisionV2(executor),
			).recognizeLayoutPrimaryBatchesV2(ctx, pagePNG, plan, runtime)
			if !errors.Is(err, transportErr) {
				t.Fatalf("repair error=%v want errors.Is(%v)", err, transportErr)
			}

			executor.mu.Lock()
			defer executor.mu.Unlock()
			if len(executor.repairSettlements) != 0 {
				t.Fatalf("failed repair crossed settlement boundary: %+v", executor.repairSettlements)
			}
			if len(executor.calls) != 2 ||
				executor.calls[0].Unit != plan.Batches[0].Unit ||
				executor.calls[1].Unit != "layout_repair_0001" {
				t.Fatalf("transport failure dispatched another repair wave: %+v", executor.calls)
			}
			if _, sent := executor.providerSends["layout_repair_0001"]; sent {
				t.Fatalf("transport failure invoked Provider send: %v", executor.providerSends)
			}
		})
	}
}

type recognitionLayoutObservedTransportErrorV2 struct {
	cause    error
	observed chan struct{}
	allow    chan struct{}
	once     sync.Once
}

func (e *recognitionLayoutObservedTransportErrorV2) Error() string {
	return e.cause.Error()
}

func (e *recognitionLayoutObservedTransportErrorV2) Unwrap() error {
	return e.cause
}

func (e *recognitionLayoutObservedTransportErrorV2) Is(error) bool {
	e.once.Do(func() { close(e.observed) })
	<-e.allow
	return false
}

func TestREGK12RecognitionBatchRepair20260808001RepairTransportWaitsSentSibling(
	t *testing.T,
) {
	pagePNG, plan := recognitionLayoutV2DispatchPlan(t, 4)
	headerDigest := recognitionLayoutV2TestDigest("repair-transport-sibling-header")
	runtime := recognitionLayoutV2RuntimeFixture(
		headerDigest,
		plan,
		2,
		time.Now().Add(-time.Second).UnixMilli(),
		time.Now().Add(5*time.Minute).UnixMilli(),
	)
	failRelease := make(chan struct{})
	siblingRelease := make(chan struct{})
	observed := make(chan struct{})
	allowClassification := make(chan struct{})
	transportCause := io.ErrUnexpectedEOF
	transportErr := &recognitionLayoutObservedTransportErrorV2{
		cause: transportCause, observed: observed, allow: allowClassification,
	}
	executor := newRecognitionLayoutRepairWaveExecutorV2(
		runtime,
		plan,
		`{"items":[]}`,
	)
	executor.transportErrByUnit = map[k12.RecognitionPhysicalUnit]error{
		"layout_repair_0001": transportErr,
	}
	executor.releaseByUnit = map[k12.RecognitionPhysicalUnit]<-chan struct{}{
		"layout_repair_0001": failRelease,
		"layout_repair_0002": siblingRelease,
	}
	executor.started = make(chan k12.RecognitionPhysicalUnit, 4)
	executor.repairSettled = make(chan k12.RecognitionPhysicalUnit, 4)
	executor.payloadByUnit["layout_repair_0002"] =
		recognitionLayoutRepairQuestionPayloadV2(t, plan.Targets[1], "sent-sibling")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = k12.WithRecognitionLayoutPlanV2(ctx, headerDigest)
	ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
	done := make(chan error, 1)
	go func() {
		_, err := NewRecognizerAdapter(
			recognitionLayoutRepairWaveVisionV2(executor),
		).recognizeLayoutPrimaryBatchesV2(ctx, pagePNG, plan, runtime)
		done <- err
	}()

	started := map[k12.RecognitionPhysicalUnit]bool{}
	for len(started) < 2 {
		select {
		case unit := <-executor.started:
			started[unit] = true
		case <-ctx.Done():
			t.Fatalf("repair workers did not start: %v", ctx.Err())
		}
	}
	if !started["layout_repair_0001"] || !started["layout_repair_0002"] || len(started) != 2 {
		t.Fatalf("initial sent repair exact-set=%v want 0001/0002", started)
	}
	close(failRelease)
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatalf("scheduler did not observe transport failure: %v", ctx.Err())
	}
	close(siblingRelease)
	select {
	case unit := <-executor.repairSettled:
		if unit != "layout_repair_0002" {
			t.Fatalf("settled sibling=%q want layout_repair_0002", unit)
		}
	case <-ctx.Done():
		t.Fatalf("already-sent sibling did not settle: %v", ctx.Err())
	}
	select {
	case err := <-done:
		t.Fatalf("repair returned before transport classification completed: %v", err)
	default:
	}
	select {
	case unit := <-executor.started:
		t.Fatalf("transport failure dispatched forbidden unit %q", unit)
	default:
	}
	close(allowClassification)
	select {
	case err := <-done:
		if !errors.Is(err, transportCause) {
			t.Fatalf("repair error=%v want transport cause", err)
		}
	case <-ctx.Done():
		t.Fatalf("repair scheduler did not wait then exit: %v", ctx.Err())
	}

	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.repairSettlements) != 1 ||
		executor.repairSettlements[0].SourcePhysicalUnit != "layout_repair_0002" {
		t.Fatalf("failed call settled or sent sibling was lost: %+v", executor.repairSettlements)
	}
	for _, call := range executor.calls {
		if call.Unit == "layout_repair_0003" || call.Unit == "layout_repair_0004" {
			t.Fatalf("failure dispatched previously-undispatched repair: %+v", executor.calls)
		}
	}
}

func TestREGK12RecognitionBatchRepair20260808001RepairProjectionDriftFailsClosed(
	t *testing.T,
) {
	pagePNG, plan := recognitionLayoutV2DispatchPlan(t, 1)
	headerDigest := recognitionLayoutV2TestDigest("repair-projection-drift-header")
	runtime := recognitionLayoutV2RuntimeFixture(
		headerDigest,
		plan,
		1,
		time.Now().Add(-time.Second).UnixMilli(),
		time.Now().Add(5*time.Minute).UnixMilli(),
	)
	executor := newRecognitionLayoutRepairWaveExecutorV2(
		runtime,
		plan,
		`{"items":[]}`,
	)
	executor.payloadByUnit["layout_repair_0001"] =
		recognitionLayoutRepairQuestionPayloadV2(t, plan.Targets[0], "repair-valid")
	executor.repairProjectionDrift = true
	ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), headerDigest)
	ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
	_, err := NewRecognizerAdapter(
		recognitionLayoutRepairWaveVisionV2(executor),
	).recognizeLayoutPrimaryBatchesV2(ctx, pagePNG, plan, runtime)
	if !errors.Is(err, k12.ErrRecognitionLayoutPlanV2Unauthorized) {
		t.Fatalf("repair projection drift error=%v want unauthorized", err)
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.repairSettlements) != 1 ||
		executor.providerSends["layout_repair_0001"] != 1 {
		t.Fatalf("projection drift changed physical/settlement cardinality: sends=%v settlements=%+v", executor.providerSends, executor.repairSettlements)
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001RepairUsesPersistedDeadline(
	t *testing.T,
) {
	pagePNG, plan := recognitionLayoutV2DispatchPlan(t, 1)
	tests := []struct {
		name          string
		stageOffset   time.Duration
		wantStageTime bool
	}{
		{name: "stage deadline wins", stageOffset: 30 * time.Second, wantStageTime: true},
		{name: "physical 120 second cap wins", stageOffset: 5 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			headerDigest := recognitionLayoutV2TestDigest("repair-deadline-" + test.name)
			stageDeadlineMillis := time.Now().Add(test.stageOffset).UnixMilli()
			runtime := recognitionLayoutV2RuntimeFixture(
				headerDigest,
				plan,
				1,
				time.Now().Add(-time.Second).UnixMilli(),
				stageDeadlineMillis,
			)
			executor := newRecognitionLayoutRepairWaveExecutorV2(
				runtime,
				plan,
				`{"items":[]}`,
			)
			executor.payloadByUnit["layout_repair_0001"] =
				recognitionLayoutRepairQuestionPayloadV2(t, plan.Targets[0], "deadline-repair")
			executor.deadlineByUnit = make(map[k12.RecognitionPhysicalUnit]time.Time)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			ctx = k12.WithRecognitionLayoutPlanV2(ctx, headerDigest)
			ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
			if _, err := NewRecognizerAdapter(
				recognitionLayoutRepairWaveVisionV2(executor),
			).recognizeLayoutPrimaryBatchesV2(ctx, pagePNG, plan, runtime); err != nil {
				t.Fatal(err)
			}
			executor.mu.Lock()
			deadline, exists := executor.deadlineByUnit["layout_repair_0001"]
			executor.mu.Unlock()
			if !exists {
				t.Fatal("repair Provider context had no deadline")
			}
			if test.wantStageTime {
				if !deadline.Equal(time.UnixMilli(stageDeadlineMillis)) {
					t.Fatalf("repair deadline=%v want persisted stage=%v", deadline, time.UnixMilli(stageDeadlineMillis))
				}
				return
			}
			remaining := time.Until(deadline)
			if remaining <= 0 || remaining > 120*time.Second {
				t.Fatalf("repair deadline remaining=%v want positive <=120s", remaining)
			}
		})
	}
}

func newRecognitionLayoutRepairWaveExecutorV2(
	runtime k12.RecognitionLayoutPlanRuntimeV2,
	plan k12.RecognitionLayoutPlanV2,
	primaryPayload string,
) *recognitionLayoutRepairWaveExecutorV2 {
	return &recognitionLayoutRepairWaveExecutorV2{
		runtime: runtime,
		payloadByUnit: map[k12.RecognitionPhysicalUnit]string{
			plan.Batches[0].Unit: primaryPayload,
		},
		cached:              make(map[k12.RecognitionPhysicalUnit]k12.RecognitionPhysicalCallResult),
		providerSends:       make(map[k12.RecognitionPhysicalUnit]int),
		authorizationByID:   make(map[string]k12.RecognitionLayoutRepairAuthorizationV2),
		settledRepairByAuth: make(map[string]k12.RecognitionLayoutRepairSettlementResultV2),
	}
}

func recognitionLayoutRepairWaveVisionV2(
	executor *recognitionLayoutRepairWaveExecutorV2,
) VisionFunc {
	return func(
		visionCtx context.Context,
		_ []byte,
		_ string,
	) (string, error) {
		call, _ := visionCtx.Value(
			recognitionLayoutV2VisionCallContextKey{},
		).(k12.RecognitionPhysicalCall)
		executor.mu.Lock()
		payload, exists := executor.payloadByUnit[call.Unit]
		executor.mu.Unlock()
		if !exists {
			return "", fmt.Errorf("unexpected Provider unit %q", call.Unit)
		}
		return payload, nil
	}
}

func recognitionLayoutRepairPrimaryPayloadV2(
	t *testing.T,
	plan k12.RecognitionLayoutPlanV2,
) string {
	t.Helper()
	items := []map[string]any{
		recognitionLayoutRepairQuestionItemV2(plan.Targets[0], "primary-1"),
		{
			"target_id": plan.Targets[2].TargetID,
			"kind":      "question",
			"recognition": map[string]any{
				"problem_kind":       "standalone",
				"source_number_path": plan.Targets[2].SourceNumberPath,
				"display_label":      plan.Targets[2].DisplayLabel,
				"question":           "",
			},
		},
		recognitionLayoutRepairQuestionItemV2(plan.Targets[3], "primary-4"),
	}
	encoded, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func recognitionLayoutRepairQuestionPayloadV2(
	t *testing.T,
	target k12.RecognitionLayoutTargetV2,
	question string,
) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"items": []map[string]any{
			recognitionLayoutRepairQuestionItemV2(target, question),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func recognitionLayoutRepairQuestionItemV2(
	target k12.RecognitionLayoutTargetV2,
	question string,
) map[string]any {
	return map[string]any{
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
	}
}

func cloneRecognitionLayoutRepairSettlementV2(
	input k12.RecognitionLayoutRepairSettlementV2,
) k12.RecognitionLayoutRepairSettlementV2 {
	cloned := input
	cloned.ResultJSON = append([]byte(nil), input.ResultJSON...)
	return cloned
}
