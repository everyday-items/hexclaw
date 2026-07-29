package main

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// REG-K12-RECOGNIZING-POLICY-001: the control plane must freeze the approved
// policy before a GradingJob exists. JSON inspection keeps this regression RED
// on unchanged production without inventing a test-only production hook.
func TestBug20260730ResolvedGradingSnapshotFreezesRecognizingPolicy(t *testing.T) {
	router := llmrouter.NewWithProviders(config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				Model:          "gpt-5.6-sol",
				Models:         []string{"gpt-5.6-sol"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{{
					ID: "gpt-5.6-sol",
					Capabilities: []string{
						config.LLMModelCapabilityText,
						config.LLMModelCapabilityVision,
					},
				}},
			},
		},
	}, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	snapshot, err := resolveK12GradingModelSnapshot(router, k12.GradingModelSnapshot{
		Provider: "hexclaw-gpt",
		Model:    "gpt-5.6-sol",
	})
	if err != nil {
		t.Fatalf("resolve grading snapshot: %v", err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal grading snapshot: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode grading snapshot: %v", err)
	}
	policy, ok := fields["recognizing_request_policy"].(map[string]any)
	if !ok {
		t.Fatalf("recognizing_request_policy missing from frozen snapshot: %s", raw)
	}
	if policy["thinking"] != "off" || policy["reasoning_effort"] != "none" {
		t.Fatalf("recognizing policy=%v, want thinking=off reasoning_effort=none", policy)
	}
	if snapshot.TimeoutMS != 120_000 {
		t.Fatalf("recognizing timeout=%d, want unchanged 120000ms", snapshot.TimeoutMS)
	}
}

func TestBug20260730PracticeSnapshotDoesNotInheritRecognizingPolicy(t *testing.T) {
	router := llmrouter.NewWithProviders(config.LLMConfig{
		Default: "hexclaw-gpt",
		Providers: map[string]config.LLMProviderConfig{
			"hexclaw-gpt": {
				Model:          "gpt-5.6-sol",
				Models:         []string{"gpt-5.6-sol"},
				ModelSpecsMode: config.LLMModelSpecsModeExplicit,
				ModelSpecs: []config.LLMProviderModelSpec{{
					ID:           "gpt-5.6-sol",
					Capabilities: []string{config.LLMModelCapabilityText},
				}},
			},
		},
	}, map[string]hexagon.Provider{
		"hexclaw-gpt": mockllm.NewLLMProvider("hexclaw-gpt"),
	})

	snapshot, err := resolveK12PracticeModelSnapshot(router, k12.GradingModelSnapshot{})
	if err != nil {
		t.Fatalf("resolve practice snapshot: %v", err)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if policy, exists := fields["recognizing_request_policy"]; exists {
		t.Fatalf("practice snapshot inherited DD-036 policy: %v", policy)
	}
}

func TestBug20260730VisionMetadataRequiresTypedRecognizingMarker(t *testing.T) {
	snapshot := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    "gpt-5.6-sol",
		Route:                    "hexclaw-gpt/gpt-5.6-sol",
		RecognizingRequestPolicy: k12.ApprovedRecognizingRequestPolicy(),
	}
	routeOnly := k12.WithGradingModelSnapshot(context.Background(), snapshot)
	if metadata, err := k12VisionRequestMetadata(routeOnly); err != nil || metadata != nil {
		t.Fatalf("snapshot-only locating/classification metadata=%v err=%v, want nil", metadata, err)
	}

	recognizing := k12.WithGradingModelRequestPolicy(
		routeOnly,
		k12.ApprovedRecognizingRequestPolicy(),
	)
	metadata, err := k12VisionRequestMetadata(recognizing)
	if err != nil {
		t.Fatalf("recognizing metadata: %v", err)
	}
	if !reflect.DeepEqual(metadata, map[string]any{"thinking": "off"}) {
		t.Fatalf("semantic metadata=%v, want exact thinking-only policy", metadata)
	}
	if _, leaked := metadata["reasoning_effort"]; leaked {
		t.Fatalf("HexClaw must not construct provider wire policy: %v", metadata)
	}

	mutated := k12.ApprovedRecognizingRequestPolicy()
	mutated.Thinking = "on"
	if metadata, err := k12VisionRequestMetadata(
		k12.WithGradingModelRequestPolicy(routeOnly, mutated),
	); err == nil || metadata != nil {
		t.Fatalf("mutated policy metadata=%v err=%v, want fail closed", metadata, err)
	}
}
