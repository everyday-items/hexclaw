package engineadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

var errRecognitionLayoutV2DeadlineObserved = errors.New("v2 deadline observed")

type recognitionLayoutV2DeadlineExecutor struct {
	mu       sync.Mutex
	executes int
}

func (e *recognitionLayoutV2DeadlineExecutor) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	_ k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	e.mu.Lock()
	e.executes++
	e.mu.Unlock()
	payload, err := send(ctx)
	return k12.RecognitionPhysicalCallResult{Payload: payload}, err
}

func (e *recognitionLayoutV2DeadlineExecutor) executeCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.executes
}

func TestREGK12RecognitionDurabilityBudget20260808001ManifestPhysicalCallDeadline(
	t *testing.T,
) {
	pagePNG := recognitionLayoutV2DensePagePNG(t, 1000, 1800)
	headerDigest := recognitionLayoutV2TestDigest("manifest-deadline-header")

	t.Run("physical cap wins over a longer parent deadline", func(t *testing.T) {
		executor := &recognitionLayoutV2DeadlineExecutor{}
		parent, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		ctx := k12.WithRecognitionLayoutPlanV2(parent, headerDigest)
		ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
		var gotDeadline time.Time
		startedAt := time.Now()
		_, err := NewRecognizerAdapter(func(
			visionCtx context.Context,
			_ []byte,
			_ string,
		) (string, error) {
			var ok bool
			gotDeadline, ok = visionCtx.Deadline()
			if !ok {
				t.Fatal("manifest Provider context has no deadline")
			}
			return "", errRecognitionLayoutV2DeadlineObserved
		}).Recognize(ctx, pagePNG)
		if !errors.Is(err, errRecognitionLayoutV2DeadlineObserved) {
			t.Fatalf("recognition error=%v want deadline observation sentinel", err)
		}
		remaining := gotDeadline.Sub(startedAt)
		if remaining < 119*time.Second || remaining > 121*time.Second {
			t.Fatalf("manifest deadline remaining=%v want 120s physical cap", remaining)
		}
		if executor.executeCount() != 1 {
			t.Fatalf("manifest physical executes=%d want=1", executor.executeCount())
		}
	})

	t.Run("shorter parent deadline wins", func(t *testing.T) {
		executor := &recognitionLayoutV2DeadlineExecutor{}
		parent, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		parentDeadline, _ := parent.Deadline()
		ctx := k12.WithRecognitionLayoutPlanV2(parent, headerDigest)
		ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
		var gotDeadline time.Time
		_, err := NewRecognizerAdapter(func(
			visionCtx context.Context,
			_ []byte,
			_ string,
		) (string, error) {
			gotDeadline, _ = visionCtx.Deadline()
			return "", errRecognitionLayoutV2DeadlineObserved
		}).Recognize(ctx, pagePNG)
		if !errors.Is(err, errRecognitionLayoutV2DeadlineObserved) {
			t.Fatalf("recognition error=%v want deadline observation sentinel", err)
		}
		if gotDeadline != parentDeadline {
			t.Fatalf("manifest deadline=%v want exact parent deadline=%v", gotDeadline, parentDeadline)
		}
	})

	t.Run("expired parent performs zero Provider sends", func(t *testing.T) {
		executor := &recognitionLayoutV2DeadlineExecutor{}
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		ctx := k12.WithRecognitionLayoutPlanV2(parent, headerDigest)
		ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
		providerSends := 0
		_, err := NewRecognizerAdapter(func(
			context.Context,
			[]byte,
			string,
		) (string, error) {
			providerSends++
			return "", nil
		}).Recognize(ctx, pagePNG)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("recognition error=%v want context canceled", err)
		}
		if providerSends != 0 {
			t.Fatalf("expired manifest Provider sends=%d want=0", providerSends)
		}
	})
}

func (e *recognitionLayoutV2BatchExecutor) LoadRecognitionLayoutPlanV2Runtime(
	ctx context.Context,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	e.mu.Lock()
	plan := e.authorizedPlan
	effectiveConcurrency := e.runtimeEffectiveConcurrency
	stageStartedAt := e.runtimeStageStartedAtMillis
	stageDeadlineAt := e.runtimeStageDeadlineAtMillis
	e.mu.Unlock()
	encoded, err := json.Marshal(plan)
	if err != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, err
	}
	var durablePlan k12.RecognitionLayoutPlanV2
	if err := json.Unmarshal(encoded, &durablePlan); err != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, err
	}
	headerDigest, _ := k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	return recognitionLayoutV2RuntimeFixture(
		headerDigest,
		durablePlan,
		effectiveConcurrency,
		stageStartedAt,
		stageDeadlineAt,
	), nil
}

func recognitionLayoutV2NonQuestionPayload(
	call k12.RecognitionPhysicalCall,
) (string, error) {
	items := make([]map[string]any, 0, len(call.TargetIDs))
	for _, targetID := range call.TargetIDs {
		items = append(items, map[string]any{
			"target_id":   targetID,
			"kind":        "non_question",
			"recognition": nil,
		})
	}
	encoded, err := json.Marshal(map[string]any{"items": items})
	return string(encoded), err
}

type recognitionLayoutV2ImmediateExecutor struct {
	mu sync.Mutex

	manifestPayload       string
	manifestDigest        string
	manifestIdentity      string
	providerSends         map[k12.RecognitionPhysicalUnit]int
	authorizedPlan        k12.RecognitionLayoutPlanV2
	effectiveConcurrency  int
	stageStartedAtMillis  int64
	stageDeadlineAtMillis int64
	headerDigestOverride  string
	authorizedPlanDrift   bool
	primarySettlements    []k12.RecognitionLayoutPrimaryBatchSettlementV2
	finalizeCalls         int
}

func newRecognitionLayoutV2ImmediateExecutor(
	manifestPayload string,
) *recognitionLayoutV2ImmediateExecutor {
	return &recognitionLayoutV2ImmediateExecutor{
		manifestPayload:       manifestPayload,
		manifestDigest:        recognitionLayoutV2TestDigest(manifestPayload),
		manifestIdentity:      "modelphysical-11111111111111111111111111111111",
		providerSends:         make(map[k12.RecognitionPhysicalUnit]int),
		effectiveConcurrency:  1,
		stageStartedAtMillis:  time.Now().Add(-time.Second).UnixMilli(),
		stageDeadlineAtMillis: time.Now().Add(5 * time.Minute).UnixMilli(),
	}
}

func (e *recognitionLayoutV2ImmediateExecutor) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	e.mu.Lock()
	e.providerSends[call.Unit]++
	e.mu.Unlock()
	payload, err := send(context.WithValue(
		ctx,
		recognitionLayoutV2VisionCallContextKey{},
		call,
	))
	var invocationID string
	resultDigest := recognitionLayoutV2TestDigest(payload)
	if call.Unit == k12.RecognitionPhysicalUnitWholePage {
		invocationID = e.manifestIdentity
		resultDigest = e.manifestDigest
	} else {
		invocationID = recognitionLayoutV2PhysicalIDForUnit(call.Unit)
	}
	return k12.RecognitionPhysicalCallResult{
		Payload:      payload,
		InvocationID: invocationID,
		ResultDigest: resultDigest,
	}, err
}

func (e *recognitionLayoutV2ImmediateExecutor) AuthorizeRecognitionLayoutPlanV2(
	_ context.Context,
	manifest k12.RecognitionPhysicalCallResult,
	plan k12.RecognitionLayoutPlanV2,
) error {
	if manifest.InvocationID != e.manifestIdentity ||
		manifest.ResultDigest != e.manifestDigest {
		return fmt.Errorf("manifest authority drift: %#v", manifest)
	}
	e.mu.Lock()
	e.authorizedPlan = plan
	e.mu.Unlock()
	return nil
}

func (e *recognitionLayoutV2ImmediateExecutor) LoadRecognitionLayoutPlanV2Runtime(
	ctx context.Context,
) (k12.RecognitionLayoutPlanRuntimeV2, error) {
	e.mu.Lock()
	plan := e.authorizedPlan
	effectiveConcurrency := e.effectiveConcurrency
	stageStartedAt := e.stageStartedAtMillis
	stageDeadlineAt := e.stageDeadlineAtMillis
	headerDigestOverride := e.headerDigestOverride
	authorizedPlanDrift := e.authorizedPlanDrift
	e.mu.Unlock()
	encoded, err := json.Marshal(plan)
	if err != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, err
	}
	var durablePlan k12.RecognitionLayoutPlanV2
	if err := json.Unmarshal(encoded, &durablePlan); err != nil {
		return k12.RecognitionLayoutPlanRuntimeV2{}, err
	}
	if authorizedPlanDrift && len(durablePlan.Targets) > 0 {
		durablePlan.Targets[0].DisplayLabel += "-drift"
	}
	headerDigest, _ := k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx)
	if headerDigestOverride != "" {
		headerDigest = headerDigestOverride
	}
	return recognitionLayoutV2RuntimeFixture(
		headerDigest,
		durablePlan,
		effectiveConcurrency,
		stageStartedAt,
		stageDeadlineAt,
	), nil
}

func (e *recognitionLayoutV2ImmediateExecutor) batchProviderSendCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	total := 0
	for unit, count := range e.providerSends {
		if strings.HasPrefix(string(unit), "layout_batch_") {
			total += count
		}
	}
	return total
}

type recognitionLayoutV2ImmediateNoLoader struct {
	delegate *recognitionLayoutV2ImmediateExecutor
}

func (e recognitionLayoutV2ImmediateNoLoader) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	call k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	return e.delegate.ExecuteRecognitionPhysicalCall(ctx, call, send)
}

func (e recognitionLayoutV2ImmediateNoLoader) AuthorizeRecognitionLayoutPlanV2(
	ctx context.Context,
	manifest k12.RecognitionPhysicalCallResult,
	plan k12.RecognitionLayoutPlanV2,
) error {
	return e.delegate.AuthorizeRecognitionLayoutPlanV2(ctx, manifest, plan)
}

func recognitionLayoutV2RuntimeFixture(
	headerDigest string,
	plan k12.RecognitionLayoutPlanV2,
	effectiveConcurrency int,
	stageStartedAtMillis int64,
	stageDeadlineAtMillis int64,
) k12.RecognitionLayoutPlanRuntimeV2 {
	buckets := k12.RecognitionLayoutBudgetBucketsV2{
		UpTo1ProblemMillis:   120000,
		UpTo8ProblemsMillis:  240000,
		UpTo16ProblemsMillis: 360000,
		UpTo32ProblemsMillis: 480000,
	}
	selectedBucket, _, _ := buckets.Select(len(plan.Targets))
	targetIDs := make([]string, len(plan.Targets))
	for index, target := range plan.Targets {
		targetIDs[index] = target.TargetID
	}
	candidateExactSetDigest, _ := k12.RecognitionLayoutTargetExactSetDigestV2(targetIDs)
	return k12.RecognitionLayoutPlanRuntimeV2{
		Header: k12.RecognitionLayoutPlanHeaderV2{
			PlanID:                   "layout-plan-adapter-fixture-v2",
			ParentInvocationID:       "modelinv-adapter-fixture-v2",
			PageDigest:               plan.PageDigest,
			StageStartedAtUnixMillis: stageStartedAtMillis,
			PhysicalCallCapMillis:    120000,
			BudgetBuckets:            buckets,
			AdapterWorkerHardCap:     2,
			EffectiveConcurrency:     effectiveConcurrency,
		},
		HeaderDigest:                 headerDigest,
		ManifestPhysicalInvocationID: plan.ManifestInvocationID,
		ManifestResultDigest:         plan.ManifestResultDigest,
		CandidateExactSetDigest:      candidateExactSetDigest,
		SelectedBucketMaxProblems:    selectedBucket,
		StageDeadlineAtUnixMillis:    stageDeadlineAtMillis,
		Status:                       "authorized",
		AuthorizedPlan:               &plan,
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001AuthorizedRuntimeGuardsPrimaryProvider(
	t *testing.T,
) {
	pagePNG := recognitionLayoutV2DensePagePNG(t, 1000, 1800)
	manifestPayload := recognitionLayoutV2ManifestPayload(t, 9)
	headerDigest := recognitionLayoutV2TestDigest("runtime-guard-header")

	tests := []struct {
		name          string
		configure     func(*recognitionLayoutV2ImmediateExecutor)
		withoutLoader bool
		wantError     error
	}{
		{
			name:          "missing durable runtime loader",
			withoutLoader: true,
			wantError:     k12.ErrRecognitionLayoutPlanV2Unauthorized,
		},
		{
			name: "durable header digest drift",
			configure: func(executor *recognitionLayoutV2ImmediateExecutor) {
				executor.headerDigestOverride = recognitionLayoutV2TestDigest("other-header")
			},
			wantError: k12.ErrRecognitionLayoutPlanV2Unauthorized,
		},
		{
			name: "durable authorized plan drift",
			configure: func(executor *recognitionLayoutV2ImmediateExecutor) {
				executor.authorizedPlanDrift = true
			},
			wantError: k12.ErrRecognitionLayoutPlanV2Unauthorized,
		},
		{
			name: "selected stage deadline already expired",
			configure: func(executor *recognitionLayoutV2ImmediateExecutor) {
				executor.stageStartedAtMillis = time.Now().Add(-time.Minute).UnixMilli()
				executor.stageDeadlineAtMillis = time.Now().Add(-time.Second).UnixMilli()
			},
			wantError: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := newRecognitionLayoutV2ImmediateExecutor(manifestPayload)
			if test.configure != nil {
				test.configure(executor)
			}
			var physicalExecutor k12.RecognitionPhysicalCallExecutor = executor
			if test.withoutLoader {
				physicalExecutor = recognitionLayoutV2ImmediateNoLoader{delegate: executor}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx = k12.WithRecognitionLayoutPlanV2(ctx, headerDigest)
			ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, physicalExecutor)
			_, err := NewRecognizerAdapter(func(
				visionCtx context.Context,
				_ []byte,
				_ string,
			) (string, error) {
				call, _ := visionCtx.Value(
					recognitionLayoutV2VisionCallContextKey{},
				).(k12.RecognitionPhysicalCall)
				if call.Unit == k12.RecognitionPhysicalUnitWholePage {
					return manifestPayload, nil
				}
				return recognitionLayoutV2NonQuestionPayload(call)
			}).Recognize(ctx, pagePNG)
			if !errors.Is(err, test.wantError) {
				t.Fatalf("recognition error=%v want errors.Is(%v)", err, test.wantError)
			}
			if got := executor.batchProviderSendCount(); got != 0 {
				t.Fatalf("batch Provider sends=%d want=0", got)
			}
		})
	}
}

func TestREGK12RecognitionDurabilityBudget20260808001RuntimeConcurrencyAndBatchDeadline(
	t *testing.T,
) {
	tests := []struct {
		name                 string
		effectiveConcurrency int
		stageDeadlineOffset  time.Duration
		wantStageDeadline    bool
	}{
		{
			name:                 "release effective one is strictly serial and uses selected deadline",
			effectiveConcurrency: 1,
			stageDeadlineOffset:  30 * time.Second,
			wantStageDeadline:    true,
		},
		{
			name:                 "effective two never exceeds hard cap and each call uses physical cap",
			effectiveConcurrency: 2,
			stageDeadlineOffset:  10 * time.Minute,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pagePNG := recognitionLayoutV2DensePagePNG(t, 1000, 1800)
			manifestPayload := recognitionLayoutV2ManifestPayload(t, 9)
			headerDigest := recognitionLayoutV2TestDigest("runtime-concurrency-" + test.name)
			executor := newRecognitionLayoutV2BatchExecutor(manifestPayload)
			executor.runtimeEffectiveConcurrency = test.effectiveConcurrency
			executor.runtimeStageStartedAtMillis = time.Now().Add(-time.Second).UnixMilli()
			executor.runtimeStageDeadlineAtMillis = time.Now().Add(test.stageDeadlineOffset).UnixMilli()
			stageDeadline := time.UnixMilli(executor.runtimeStageDeadlineAtMillis)

			var deadlineMu sync.Mutex
			providerDeadlines := make(map[k12.RecognitionPhysicalUnit]time.Time)
			providerRemaining := make(map[k12.RecognitionPhysicalUnit]time.Duration)
			vision := func(
				visionCtx context.Context,
				_ []byte,
				_ string,
			) (string, error) {
				call, _ := visionCtx.Value(
					recognitionLayoutV2VisionCallContextKey{},
				).(k12.RecognitionPhysicalCall)
				if call.Unit == k12.RecognitionPhysicalUnitWholePage {
					return manifestPayload, nil
				}
				deadline, ok := visionCtx.Deadline()
				if !ok {
					return "", fmt.Errorf("batch %q Provider context has no deadline", call.Unit)
				}
				deadlineMu.Lock()
				providerDeadlines[call.Unit] = deadline
				providerRemaining[call.Unit] = time.Until(deadline)
				deadlineMu.Unlock()
				return recognitionLayoutV2NonQuestionPayload(call)
			}

			parent, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			defer cancel()
			ctx := k12.WithRecognitionLayoutPlanV2(parent, headerDigest)
			ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, executor)
			done := make(chan recognitionLayoutV2Result, 1)
			go func() {
				_, err := NewRecognizerAdapter(vision).Recognize(ctx, pagePNG)
				done <- recognitionLayoutV2Result{err: err}
			}()

			started := make([]k12.RecognitionPhysicalUnit, 0, 3)
			for index := 0; index < test.effectiveConcurrency; index++ {
				started = append(started, awaitRecognitionLayoutV2Start(
					t,
					ctx,
					executor.started,
					done,
				))
			}
			select {
			case unexpected := <-executor.started:
				t.Fatalf("batch %q exceeded effective concurrency=%d", unexpected, test.effectiveConcurrency)
			default:
			}

			remainingUnits := []k12.RecognitionPhysicalUnit{
				"layout_batch_0001", "layout_batch_0002", "layout_batch_0003",
			}
			for _, unit := range started {
				remainingUnits = deleteRecognitionLayoutV2Unit(remainingUnits, unit)
			}
			for len(remainingUnits) > 0 {
				unit := started[0]
				executor.release(unit)
				if completed := awaitRecognitionLayoutV2Unit(t, ctx, executor.completed); completed != unit {
					t.Fatalf("completed batch=%q want released=%q", completed, unit)
				}
				started = started[1:]
				next := awaitRecognitionLayoutV2Start(
					t,
					ctx,
					executor.started,
					done,
				)
				started = append(started, next)
				remainingUnits = deleteRecognitionLayoutV2Unit(remainingUnits, next)
			}
			for _, unit := range started {
				executor.release(unit)
			}
			for range started {
				awaitRecognitionLayoutV2Unit(t, ctx, executor.completed)
			}
			select {
			case result := <-done:
				if result.err != nil {
					t.Fatal(result.err)
				}
			case <-ctx.Done():
				t.Fatalf("recognition did not converge: %v", ctx.Err())
			}

			executor.mu.Lock()
			maxInFlight := executor.maxInFlight
			executor.mu.Unlock()
			if maxInFlight != test.effectiveConcurrency {
				t.Fatalf("max batch in-flight=%d want=%d", maxInFlight, test.effectiveConcurrency)
			}
			deadlineMu.Lock()
			defer deadlineMu.Unlock()
			if len(providerDeadlines) != 3 {
				t.Fatalf("batch Provider deadlines=%v want all 3 batches", providerDeadlines)
			}
			for unit, deadline := range providerDeadlines {
				if test.wantStageDeadline {
					if deadline != stageDeadline {
						t.Fatalf("batch %q deadline=%v want selected stage=%v", unit, deadline, stageDeadline)
					}
					continue
				}
				remaining := providerRemaining[unit]
				if remaining <= 0 || remaining > 121*time.Second {
					t.Fatalf("batch %q remaining=%v want positive and capped at 120s", unit, remaining)
				}
			}
		})
	}
}

func deleteRecognitionLayoutV2Unit(
	units []k12.RecognitionPhysicalUnit,
	remove k12.RecognitionPhysicalUnit,
) []k12.RecognitionPhysicalUnit {
	for index, unit := range units {
		if unit == remove {
			return append(units[:index:index], units[index+1:]...)
		}
	}
	return units
}
