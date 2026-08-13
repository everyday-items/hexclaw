package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestREGK12RecognitionDurabilityBudget20260808001PhysicalIdentity(t *testing.T) {
	policy := k12.ApprovedRecognizingRequestPolicy()
	parent := k12.ModelInvocation{
		InvocationID:  "parent-physical-identity-v2",
		AgentName:     "mingming",
		JobID:         "job-physical-identity-v2",
		Stage:         k12.GradingStageRecognizing,
		RequestDigest: "sha256:parent-physical-identity-v2",
		RouteSnapshot: k12.GradingModelSnapshot{
			Provider:                 "hexclaw-gpt",
			Model:                    k12.RecognizingPolicyModel,
			Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
			Capability:               "vision",
			RecognizingRequestPolicy: policy,
		},
		RequestPolicySnapshot: policy,
		Status:                k12.ModelInvocationSent,
		Attempt:               1,
	}
	legacyCall := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: []byte("legacy-whole-image"),
	}

	t.Run("legacy zero and explicit v1 preserve exact bytes", func(t *testing.T) {
		legacyDigest, err := recognizingPhysicalInvocationDigest(parent, legacyCall)
		if err != nil {
			t.Fatal(err)
		}
		if want := "sha256:ee65311c5629560f898d502cf24582a7644c4fd256553d5317d6d5e35bb4c6bc"; legacyDigest != want {
			t.Fatalf("legacy digest drifted: got=%s want=%s", legacyDigest, want)
		}
		legacyID, err := stableRecognitionPhysicalInvocationIDForCall(parent.InvocationID, legacyCall)
		if err != nil {
			t.Fatal(err)
		}
		if want := "modelphysical-378a19a9e5495ba9908f44cd34f18658"; legacyID != want {
			t.Fatalf("legacy physical ID drifted: got=%s want=%s", legacyID, want)
		}

		explicitV1 := legacyCall
		explicitV1.PlanVersion = k12.RecognitionPlanVersionV1
		explicitDigest, err := recognizingPhysicalInvocationDigest(parent, explicitV1)
		if err != nil {
			t.Fatal(err)
		}
		explicitID, err := stableRecognitionPhysicalInvocationIDForCall(parent.InvocationID, explicitV1)
		if err != nil {
			t.Fatal(err)
		}
		if explicitDigest != legacyDigest || explicitID != legacyID {
			t.Fatalf(
				"explicit v1 changed legacy bytes: digest=%s/%s id=%s/%s",
				explicitDigest,
				legacyDigest,
				explicitID,
				legacyID,
			)
		}
	})

	planA := recognitionPhysicalIdentityTestDigest("plan-a")
	planB := recognitionPhysicalIdentityTestDigest("plan-b")
	batch := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  planA,
		Unit:        k12.RecognitionPhysicalUnit("layout_batch_0001"),
		TargetIDs:   []string{"target-0001", "target-0002"},
		Image:       []byte("batch-image-a"),
	}
	repair := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  planA,
		Unit:        k12.RecognitionPhysicalUnit("layout_repair_0001"),
		TargetIDs:   []string{"target-0002"},
		Image:       []byte("repair-image-a"),
	}
	manifest := k12.RecognitionPhysicalCall{
		PlanVersion: k12.RecognitionPlanVersionV2,
		PlanDigest:  planA,
		Unit:        k12.RecognitionPhysicalUnitWholePage,
		Image:       legacyCall.Image,
	}

	t.Run("v2 validates only manifest batch and one-target repair shapes", func(t *testing.T) {
		for name, call := range map[string]k12.RecognitionPhysicalCall{
			"manifest": manifest,
			"batch":    batch,
			"repair":   repair,
		} {
			t.Run(name, func(t *testing.T) {
				if err := call.Validate(); err != nil {
					t.Fatalf("valid v2 call rejected: %v", err)
				}
			})
		}

		invalid := map[string]k12.RecognitionPhysicalCall{
			"legacy layout unit": {
				Unit:  batch.Unit,
				Image: batch.Image,
			},
			"explicit v1 layout unit": {
				PlanVersion: k12.RecognitionPlanVersionV1,
				Unit:        batch.Unit,
				Image:       batch.Image,
			},
			"v1 smuggles plan": {
				PlanVersion: k12.RecognitionPlanVersionV1,
				PlanDigest:  planA,
				Unit:        legacyCall.Unit,
				Image:       legacyCall.Image,
			},
			"v2 legacy segment": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				Unit:        k12.RecognitionPhysicalUnitSegment1,
				Image:       batch.Image,
			},
			"v2 legacy inventory": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				Unit:        k12.RecognitionPhysicalUnitPrintedInventory,
				Image:       batch.Image,
			},
			"v2 noncanonical batch": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				PlanDigest:  planA,
				Unit:        k12.RecognitionPhysicalUnit("layout_batch_1"),
				TargetIDs:   []string{"target-0001"},
				Image:       batch.Image,
			},
			"batch has no plan": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				Unit:        batch.Unit,
				TargetIDs:   []string{"target-0001"},
				Image:       batch.Image,
			},
			"batch has noncanonical plan": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				PlanDigest:  "sha256:ABCDEF",
				Unit:        batch.Unit,
				TargetIDs:   []string{"target-0001"},
				Image:       batch.Image,
			},
			"batch target set empty": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				PlanDigest:  planA,
				Unit:        batch.Unit,
				Image:       batch.Image,
			},
			"batch target set too large": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				PlanDigest:  planA,
				Unit:        batch.Unit,
				TargetIDs:   []string{"t1", "t2", "t3", "t4", "t5"},
				Image:       batch.Image,
			},
			"batch duplicate target": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				PlanDigest:  planA,
				Unit:        batch.Unit,
				TargetIDs:   []string{"target-0001", "target-0001"},
				Image:       batch.Image,
			},
			"repair has two targets": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				PlanDigest:  planA,
				Unit:        repair.Unit,
				TargetIDs:   []string{"target-0001", "target-0002"},
				Image:       repair.Image,
			},
			"manifest omits plan header": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				Unit:        manifest.Unit,
				Image:       manifest.Image,
			},
			"manifest claims targets": {
				PlanVersion: k12.RecognitionPlanVersionV2,
				PlanDigest:  planA,
				Unit:        manifest.Unit,
				TargetIDs:   []string{"target-0001"},
				Image:       manifest.Image,
			},
			"unknown plan version": {
				PlanVersion: 99,
				Unit:        legacyCall.Unit,
				Image:       legacyCall.Image,
			},
		}
		for name, call := range invalid {
			t.Run(name, func(t *testing.T) {
				sent := false
				_, err := k12.ExecuteRecognitionPhysicalCall(
					context.Background(),
					call,
					func(context.Context) (string, error) {
						sent = true
						return "should-not-send", nil
					},
				)
				if err == nil {
					t.Fatal("invalid physical identity was accepted")
				}
				if sent {
					t.Fatal("invalid identity crossed the provider send boundary")
				}
			})
		}
	})

	t.Run("v2 digest binds every request fact", func(t *testing.T) {
		baseDigest, err := recognizingPhysicalInvocationDigest(parent, batch)
		if err != nil {
			t.Fatal(err)
		}
		v2WholeDigest, err := recognizingPhysicalInvocationDigest(parent, manifest)
		if err != nil {
			t.Fatal(err)
		}
		v1WholeDigest, err := recognizingPhysicalInvocationDigest(parent, legacyCall)
		if err != nil {
			t.Fatal(err)
		}
		if v2WholeDigest == v1WholeDigest {
			t.Fatal("plan-version mutation did not change whole-page digest")
		}

		mutations := map[string]struct {
			parent k12.ModelInvocation
			call   k12.RecognitionPhysicalCall
		}{
			"plan": {parent: parent, call: func() k12.RecognitionPhysicalCall {
				changed := batch
				changed.PlanDigest = planB
				return changed
			}()},
			"unit": {parent: parent, call: func() k12.RecognitionPhysicalCall {
				changed := batch
				changed.Unit = k12.RecognitionPhysicalUnit("layout_batch_0002")
				return changed
			}()},
			"image": {parent: parent, call: func() k12.RecognitionPhysicalCall {
				changed := batch
				changed.Image = []byte("batch-image-b")
				return changed
			}()},
			"ordered targets": {parent: parent, call: func() k12.RecognitionPhysicalCall {
				changed := batch
				changed.TargetIDs = []string{"target-0002", "target-0001"}
				return changed
			}()},
			"route": {parent: func() k12.ModelInvocation {
				changed := parent
				changed.RouteSnapshot.Model = "gpt-5.6-sol-route-mutation"
				changed.RouteSnapshot.Route = "hexclaw-gpt/gpt-5.6-sol-route-mutation"
				return changed
			}(), call: batch},
			"policy": {parent: func() k12.ModelInvocation {
				changed := parent
				changed.RequestPolicySnapshot.ReasoningEffort = "high"
				return changed
			}(), call: batch},
		}
		for name, mutation := range mutations {
			t.Run(name, func(t *testing.T) {
				got, digestErr := recognizingPhysicalInvocationDigest(mutation.parent, mutation.call)
				if digestErr != nil {
					t.Fatal(digestErr)
				}
				if got == baseDigest {
					t.Fatalf("%s mutation did not change v2 request digest", name)
				}
			})
		}
	})

	t.Run("v2 stable ID binds authorization but excludes transient bytes", func(t *testing.T) {
		baseID, err := stableRecognitionPhysicalInvocationIDForCall(parent.InvocationID, batch)
		if err != nil {
			t.Fatal(err)
		}
		imageMutation := batch
		imageMutation.Image = []byte("same-authorized-call-rebuilt-in-another-wave")
		imageMutationID, err := stableRecognitionPhysicalInvocationIDForCall(parent.InvocationID, imageMutation)
		if err != nil {
			t.Fatal(err)
		}
		if imageMutationID != baseID {
			t.Fatal("transient rebuilt image/scheduling wave changed stable identity")
		}

		mutations := []k12.RecognitionPhysicalCall{
			func() k12.RecognitionPhysicalCall { changed := batch; changed.PlanDigest = planB; return changed }(),
			func() k12.RecognitionPhysicalCall {
				changed := batch
				changed.Unit = k12.RecognitionPhysicalUnit("layout_batch_0002")
				return changed
			}(),
			func() k12.RecognitionPhysicalCall {
				changed := batch
				changed.TargetIDs = []string{"target-0002", "target-0001"}
				return changed
			}(),
		}
		for _, mutation := range mutations {
			got, identityErr := stableRecognitionPhysicalInvocationIDForCall(parent.InvocationID, mutation)
			if identityErr != nil {
				t.Fatal(identityErr)
			}
			if got == baseID {
				t.Fatalf("authorization mutation reused stable ID %s", baseID)
			}
		}
		if got, parentMutationErr := stableRecognitionPhysicalInvocationIDForCall("another-parent", batch); parentMutationErr != nil || got == baseID {
			t.Fatalf("parent mutation must change stable ID: got=%s err=%v", got, parentMutationErr)
		}
		manifestID, err := stableRecognitionPhysicalInvocationIDForCall(parent.InvocationID, manifest)
		if err != nil {
			t.Fatal(err)
		}
		legacyID, err := stableRecognitionPhysicalInvocationIDForCall(parent.InvocationID, legacyCall)
		if err != nil {
			t.Fatal(err)
		}
		if manifestID == legacyID {
			t.Fatal("v1 and v2 whole-page calls shared a stable ID")
		}
	})

	t.Run("v2 opt-in and durable plan authorization are explicit", func(t *testing.T) {
		if k12.RecognitionLayoutPlanV2Enabled(context.Background()) {
			t.Fatal("v2 layout plan was implicitly enabled")
		}
		ctx := k12.WithRecognitionLayoutPlanV2(context.Background(), planA)
		if !k12.RecognitionLayoutPlanV2Enabled(ctx) {
			t.Fatal("v2 layout plan opt-in was not retained")
		}
		if got, ok := k12.RecognitionLayoutPlanV2HeaderDigestFromContext(ctx); !ok || got != planA {
			t.Fatalf("v2 plan header digest=%q ok=%v want=%q,true", got, ok, planA)
		}
		if k12.RecognitionLayoutPlanV2Enabled(
			k12.WithRecognitionLayoutPlanV2(context.Background(), "sha256:not-canonical"),
		) {
			t.Fatal("noncanonical plan header enabled v2")
		}
		manifestResult := k12.RecognitionPhysicalCallResult{
			InvocationID: "modelphysical-manifest",
			ResultDigest: recognitionPhysicalIdentityTestDigest("manifest-result"),
		}
		plan := k12.RecognitionLayoutPlanV2{
			Version:              k12.RecognitionPlanVersionV2,
			ManifestInvocationID: manifestResult.InvocationID,
			ManifestResultDigest: manifestResult.ResultDigest,
			AuthorizedPlanDigest: planA,
		}
		spy := &recognitionLayoutPlanAuthorizerSpy{}
		ctx = k12.WithRecognitionPhysicalCallExecutor(ctx, spy)
		if err := k12.AuthorizeRecognitionLayoutPlanV2(ctx, manifestResult, plan); err != nil {
			t.Fatal(err)
		}
		if spy.authorized != 1 {
			t.Fatalf("durable authorizer calls=%d want=1", spy.authorized)
		}

		approvedWithoutExecutor := k12.WithRecognitionLayoutPlanV2(
			k12.WithGradingModelRequestPolicy(context.Background(), policy),
			planA,
		)
		if err := k12.AuthorizeRecognitionLayoutPlanV2(approvedWithoutExecutor, manifestResult, plan); err == nil {
			t.Fatal("approved request policy accepted a non-durable v2 plan")
		}
	})

	t.Run("lightweight result digest binds exact payload", func(t *testing.T) {
		result, err := k12.ExecuteRecognitionPhysicalCall(
			context.Background(),
			legacyCall,
			func(context.Context) (string, error) {
				return "exact manifest payload", nil
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if want := recognitionPhysicalIdentityTestDigest("exact manifest payload"); result.ResultDigest != want {
			t.Fatalf("result digest=%s want=%s", result.ResultDigest, want)
		}
	})
}

type recognitionLayoutPlanAuthorizerSpy struct {
	authorized int
}

func (s *recognitionLayoutPlanAuthorizerSpy) ExecuteRecognitionPhysicalCall(
	ctx context.Context,
	_ k12.RecognitionPhysicalCall,
	send func(context.Context) (string, error),
) (k12.RecognitionPhysicalCallResult, error) {
	payload, err := send(ctx)
	return k12.RecognitionPhysicalCallResult{Payload: payload}, err
}

func (s *recognitionLayoutPlanAuthorizerSpy) AuthorizeRecognitionLayoutPlanV2(
	_ context.Context,
	_ k12.RecognitionPhysicalCallResult,
	_ k12.RecognitionLayoutPlanV2,
) error {
	s.authorized++
	return nil
}

func recognitionPhysicalIdentityTestDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}
