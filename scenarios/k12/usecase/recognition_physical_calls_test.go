package usecase

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestRecognizingPhysicalInvocationDigestBindsUnitImageRouteAndPolicy(t *testing.T) {
	policy := k12.ApprovedRecognizingRequestPolicy()
	parent := k12.ModelInvocation{
		InvocationID:  "parent-recognizing-digest",
		AgentName:     "mingming",
		JobID:         "job-recognizing-digest",
		Stage:         k12.GradingStageRecognizing,
		RequestDigest: "sha256:parent",
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
	baseCall := k12.RecognitionPhysicalCall{
		Unit:  k12.RecognitionPhysicalUnitWholePage,
		Image: []byte("image-a"),
	}
	baseDigest, err := recognizingPhysicalInvocationDigest(parent, baseCall)
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]struct {
		parent k12.ModelInvocation
		call   k12.RecognitionPhysicalCall
	}{
		"physical unit": {
			parent: parent,
			call: k12.RecognitionPhysicalCall{
				Unit:  k12.RecognitionPhysicalUnitSegment1,
				Image: baseCall.Image,
			},
		},
		"actual image bytes": {
			parent: parent,
			call: k12.RecognitionPhysicalCall{
				Unit:  baseCall.Unit,
				Image: []byte("image-b"),
			},
		},
		"route": {
			parent: func() k12.ModelInvocation {
				changed := parent
				changed.RouteSnapshot.Model = "gpt-5.6-sol-mutated"
				changed.RouteSnapshot.Route = "hexclaw-gpt/gpt-5.6-sol-mutated"
				return changed
			}(),
			call: baseCall,
		},
		"request policy": {
			parent: func() k12.ModelInvocation {
				changed := parent
				changed.RequestPolicySnapshot.ReasoningEffort = "high"
				return changed
			}(),
			call: baseCall,
		},
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			got, digestErr := recognizingPhysicalInvocationDigest(
				mutation.parent,
				mutation.call,
			)
			if digestErr != nil {
				t.Fatal(digestErr)
			}
			if got == baseDigest {
				t.Fatalf("%s mutation did not change child digest %s", name, got)
			}
		})
	}

	if stableRecognitionPhysicalInvocationID(
		parent.InvocationID,
		baseCall.Unit,
	) == stableRecognitionPhysicalInvocationID(
		parent.InvocationID,
		k12.RecognitionPhysicalUnitSegment1,
	) {
		t.Fatal("different physical units must not share a child identity")
	}
}
