package k12

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestApprovedRecognizingRequestPolicyIsStableAndScoped(t *testing.T) {
	policy := ApprovedRecognizingRequestPolicy()
	if !policy.IsApprovedRecognizing() || policy.Digest() == "" {
		t.Fatalf("approved policy invalid: %+v", policy)
	}
	mutated := policy
	mutated.Thinking = "on"
	if mutated.Digest() == policy.Digest() {
		t.Fatal("policy mutation must change its digest")
	}

	route := GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + RecognizingPolicyModel,
		RecognizingRequestPolicy: policy,
	}
	if err := ValidateModelInvocationRequestPolicy(
		GradingStageRecognizing,
		route,
		policy,
	); err != nil {
		t.Fatalf("approved recognizing policy rejected: %v", err)
	}
	if err := ValidateModelInvocationRequestPolicy(
		GradingStageLocating,
		route,
		policy,
	); err == nil {
		t.Fatal("locating must not inherit recognizing request policy")
	}
	if err := ValidateModelInvocationRequestPolicy(
		GradingStageLocating,
		route,
		ModelRequestPolicySnapshot{},
	); err != nil {
		t.Fatalf("policy-free locating rejected: %v", err)
	}
}

func TestZeroRecognizingPolicyIsOmittedFromSnapshotJSON(t *testing.T) {
	raw, err := json.Marshal(GradingModelSnapshot{
		Provider: "provider",
		Model:    "vision-model",
		Route:    "provider/vision-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "recognizing_request_policy") {
		t.Fatalf("zero recognizing policy leaked into JSON: %s", raw)
	}
}
