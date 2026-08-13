package k12

import "testing"

func TestREGK12RecognitionPlanVersion20260808001(t *testing.T) {
	t.Run("wire_policy_does_not_impersonate_plan_version", func(t *testing.T) {
		policy := ApprovedRecognizingRequestPolicy()
		if policy.PolicyVersion != "dd036-recognizing-v1" {
			t.Fatalf("request policy version=%q, want unchanged DD-036 wire policy", policy.PolicyVersion)
		}
	})

	t.Run("v2_physical_units_are_canonical", func(t *testing.T) {
		for _, unit := range []RecognitionPhysicalUnit{
			"layout_batch_0001",
			"layout_batch_9999",
			"layout_repair_0001",
			"layout_repair_9999",
		} {
			if !unit.Valid() {
				t.Errorf("v2 physical unit %q must be valid", unit)
			}
		}
		for _, unit := range []RecognitionPhysicalUnit{
			"layout_batch_1",
			"layout_batch_0000",
			"layout_batch_10000",
			"layout_repair_0000",
			"layout_repair_abcd",
		} {
			if unit.Valid() {
				t.Errorf("non-canonical v2 physical unit %q must be rejected", unit)
			}
		}
	})

	t.Run("persisted_v1_policy_remains_valid", func(t *testing.T) {
		legacy := ModelRequestPolicySnapshot{
			PolicyVersion:   "dd036-recognizing-v1",
			Stage:           GradingStageRecognizing,
			Thinking:        "off",
			ReasoningEffort: "none",
		}
		route := GradingModelSnapshot{
			Provider:                 "hexclaw-gpt",
			Model:                    RecognizingPolicyModel,
			Route:                    "hexclaw-gpt/" + RecognizingPolicyModel,
			RecognizingRequestPolicy: legacy,
		}
		if err := ValidateGradingRecognizingRequestPolicy(route); err != nil {
			t.Fatalf("persisted v1 recognizing policy must remain recoverable: %v", err)
		}
	})
}
