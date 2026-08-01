package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type dd036FallbackAuthorizationProviderSpy struct {
	posts int
}

func (s *dd036FallbackAuthorizationProviderSpy) send(
	context.Context,
) (string, error) {
	s.posts++
	return `[]`, nil
}

func dd036FallbackAuthorizationInputs(
	t *testing.T,
) (map[k12.RecognitionPhysicalUnit][]byte, []byte) {
	t.Helper()

	whole := dd036DenseReconcileImage(t)
	inputs, ok := k12.DenseWorksheetFallbackPhysicalInputs(whole)
	if !ok {
		t.Fatal("DD036 dense fixture did not produce deterministic fallback inputs")
	}
	images := map[k12.RecognitionPhysicalUnit][]byte{
		k12.RecognitionPhysicalUnitWholePage: whole,
	}
	for _, input := range inputs {
		images[input.Unit] = input.Image
	}
	return images, whole
}

func assertDD036PhysicalCallRejectedBeforeProvider(
	t *testing.T,
	executor *durableRecognitionPhysicalCallExecutor,
	deps Deps,
	job GradingJobView,
	spy *dd036FallbackAuthorizationProviderSpy,
	call k12.RecognitionPhysicalCall,
	wantExistingChildren int,
) {
	t.Helper()

	postsBefore := spy.posts
	_, callErr := executor.ExecuteRecognitionPhysicalCall(
		context.Background(),
		call,
		spy.send,
	)
	if callErr == nil {
		t.Errorf(
			"DD036 illegal fallback unit %s returned success; want pre-send rejection",
			call.Unit,
		)
	}
	if got := spy.posts - postsBefore; got != 0 {
		t.Errorf(
			"DD036 illegal fallback unit %s reached Provider POST %d times; want 0",
			call.Unit,
			got,
		)
	}
	children, err := deps.Records.ListModelPhysicalInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list DD036 physical children: %v", err)
	}
	if len(children) != wantExistingChildren {
		t.Errorf(
			"DD036 illegal fallback unit %s created a physical child: count=%d want=%d children=%+v",
			call.Unit,
			len(children),
			wantExistingChildren,
			children,
		)
	}
}

// REG-K12-RECOGNIZING-POLICY-004 RED: enumerating a fallback unit is not
// authorization to spend a Provider request. A fresh durable executor may
// begin only with whole_page.
func TestDD036DurableExecutorRejectsFallbackUnitAsFirstProviderCall(
	t *testing.T,
) {
	tests := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitSegment1,
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitPrintedInventory,
	}
	for _, unit := range tests {
		t.Run(string(unit), func(t *testing.T) {
			executor, deps, job := newDD036PhysicalExecutorHarness(
				t,
				"dd036-fallback-first-"+string(unit),
			)
			images, _ := dd036FallbackAuthorizationInputs(t)
			spy := &dd036FallbackAuthorizationProviderSpy{}

			assertDD036PhysicalCallRejectedBeforeProvider(
				t,
				executor,
				deps,
				job,
				spy,
				k12.RecognitionPhysicalCall{
					Unit:  unit,
					Image: images[unit],
				},
				0,
			)
		})
	}
}

// REG-K12-RECOGNIZING-POLICY-004 RED: a successful whole-page HTTP response,
// even when its content is known locally to violate the structured protocol,
// does not authorize segment_1 until that exact whole result digest and the
// protocol-failure decision have been persisted durably.
func TestDD036DurableExecutorRequiresPersistedWholeProtocolFailureAuthorization(
	t *testing.T,
) {
	executor, deps, job := newDD036PhysicalExecutorHarness(
		t,
		"dd036-fallback-missing-protocol-authorization",
	)
	images, whole := dd036FallbackAuthorizationInputs(t)
	spy := &dd036FallbackAuthorizationProviderSpy{}
	wholeResult, err := executor.ExecuteRecognitionPhysicalCall(
		context.Background(),
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: whole,
		},
		func(context.Context) (string, error) {
			spy.posts++
			return `not-json`, nil
		},
	)
	if err != nil || wholeResult.InvocationID == "" || spy.posts != 1 {
		t.Fatalf(
			"seed whole-page succeeded response: result=%+v posts=%d err=%v",
			wholeResult,
			spy.posts,
			err,
		)
	}

	assertDD036PhysicalCallRejectedBeforeProvider(
		t,
		executor,
		deps,
		job,
		spy,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitSegment1,
			Image: images[k12.RecognitionPhysicalUnitSegment1],
		},
		1,
	)
}

// REG-K12-RECOGNIZING-POLICY-004 RED: a whole-page success alone cannot be
// used to skip the direct fallback predecessor or to send printed inventory
// before segment_5 has succeeded.
func TestDD036DurableExecutorRejectsFallbackPredecessorSkipping(
	t *testing.T,
) {
	tests := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitPrintedInventory,
	}
	for _, unit := range tests {
		t.Run(string(unit), func(t *testing.T) {
			executor, deps, job := newDD036PhysicalExecutorHarness(
				t,
				"dd036-fallback-skip-"+string(unit),
			)
			images, whole := dd036FallbackAuthorizationInputs(t)
			spy := &dd036FallbackAuthorizationProviderSpy{}
			wholeResult, err := executor.ExecuteRecognitionPhysicalCall(
				context.Background(),
				k12.RecognitionPhysicalCall{
					Unit:  k12.RecognitionPhysicalUnitWholePage,
					Image: whole,
				},
				func(context.Context) (string, error) {
					spy.posts++
					return `not-json`, nil
				},
			)
			if err != nil || wholeResult.InvocationID == "" ||
				spy.posts != 1 {
				t.Fatalf(
					"seed whole-page succeeded response: result=%+v posts=%d err=%v",
					wholeResult,
					spy.posts,
					err,
				)
			}

			assertDD036PhysicalCallRejectedBeforeProvider(
				t,
				executor,
				deps,
				job,
				spy,
				k12.RecognitionPhysicalCall{
					Unit:  unit,
					Image: images[unit],
				},
				1,
			)
		})
	}
}

// REG-K12-RECOGNIZING-POLICY-004: the sole legal dense fallback path binds an
// immutable authorization to the exact succeeded whole-page content, then
// admits each remaining unit once and only after its direct predecessor.
func TestDD036DurableExecutorAuthorizedFallbackCompletesExactSevenCalls(
	t *testing.T,
) {
	executor, deps, job := newDD036PhysicalExecutorHarness(
		t,
		"dd036-fallback-authorized-exact-seven",
	)
	images, whole := dd036FallbackAuthorizationInputs(t)
	spy := &dd036FallbackAuthorizationProviderSpy{}
	ctx := context.Background()

	wholeResult, err := executor.ExecuteRecognitionPhysicalCall(
		ctx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: whole,
		},
		func(context.Context) (string, error) {
			spy.posts++
			return `not-json`, nil
		},
	)
	if err != nil || wholeResult.InvocationID == "" || spy.posts != 1 {
		t.Fatalf(
			"whole-page physical success: result=%+v posts=%d err=%v",
			wholeResult,
			spy.posts,
			err,
		)
	}
	if err := executor.AuthorizeRecognitionPhysicalFallback(
		ctx,
		wholeResult,
	); err != nil {
		t.Fatalf("authorize exact whole protocol failure: %v", err)
	}
	if err := executor.AuthorizeRecognitionPhysicalFallback(
		ctx,
		wholeResult,
	); err != nil {
		t.Fatalf("exact fallback authorization replay is not idempotent: %v", err)
	}
	changedWhole := wholeResult
	changedWhole.Payload = wholeResult.Payload + "-changed"
	if err := executor.AuthorizeRecognitionPhysicalFallback(
		ctx,
		changedWhole,
	); err == nil {
		t.Fatal("fallback authorization accepted changed whole content")
	}
	if spy.posts != 1 {
		t.Fatalf(
			"authorization replay/content conflict changed Provider POSTs=%d, want 1",
			spy.posts,
		)
	}

	fallbackUnits := []k12.RecognitionPhysicalUnit{
		k12.RecognitionPhysicalUnitSegment1,
		k12.RecognitionPhysicalUnitSegment2,
		k12.RecognitionPhysicalUnitSegment3,
		k12.RecognitionPhysicalUnitSegment4,
		k12.RecognitionPhysicalUnitSegment5,
		k12.RecognitionPhysicalUnitPrintedInventory,
	}
	for _, unit := range fallbackUnits {
		result, callErr := executor.ExecuteRecognitionPhysicalCall(
			ctx,
			k12.RecognitionPhysicalCall{
				Unit:  unit,
				Image: images[unit],
			},
			spy.send,
		)
		if callErr != nil || result.InvocationID == "" {
			t.Fatalf(
				"authorized fallback unit %s: result=%+v posts=%d err=%v",
				unit,
				result,
				spy.posts,
				callErr,
			)
		}
	}
	if spy.posts != 7 {
		t.Fatalf("authorized physical Provider POSTs=%d, want exact 7", spy.posts)
	}

	children, err := deps.Records.ListModelPhysicalInvocations(
		ctx,
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list authorized physical exact-set: %v", err)
	}
	wantUnits := append(
		[]k12.RecognitionPhysicalUnit{
			k12.RecognitionPhysicalUnitWholePage,
		},
		fallbackUnits...,
	)
	if len(children) != len(wantUnits) {
		t.Fatalf(
			"authorized physical child count=%d want=%d children=%+v",
			len(children),
			len(wantUnits),
			children,
		)
	}
	for index, child := range children {
		if child.ParentInvocationID != executor.parent.InvocationID ||
			child.PhysicalUnit != wantUnits[index] ||
			child.Attempt != 1 ||
			child.Status != k12.ModelInvocationSucceeded {
			t.Errorf(
				"authorized physical child[%d]=%+v, want parent=%s unit=%s attempt=1 succeeded",
				index,
				child,
				executor.parent.InvocationID,
				wantUnits[index],
			)
		}
	}
}
