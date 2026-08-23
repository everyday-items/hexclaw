package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func imageTaskCapabilityReceiptRouteFixture() k12.ImageTaskRouteSnapshot {
	return k12.ImageTaskRouteSnapshot{
		Provider:                "hexclaw-gpt",
		Model:                   "gpt-5.6-sol",
		Route:                   "hexclaw-gpt/gpt-5.6-sol",
		ProviderInstanceID:      "pvd_v1_00112233445566778899aabbccddeeff",
		ConfigFingerprint:       "fingerprint",
		CapabilityReceiptDigest: "receipt-digest",
		ProbePolicyVersion:      "v1",
		Capability:              "text+vision",
		SelectionSource:         "auto",
		PolicyVersion:           "image-task-routing-v1",
		PromptVersion:           "image-task-classifier-v1",
	}
}

// K12-FROZEN-MODEL-ROUTE-001：ImageTask 进入真实 Provider 前转换为 Grading 快照时，
// 不得丢掉创建阶段冻结的能力回执证据。
func TestImageTaskProviderContextCarriesFrozenCapabilityReceiptEvidence(t *testing.T) {
	route := imageTaskCapabilityReceiptRouteFixture()
	ctx, cancel := imageTaskProviderContext(context.Background(), route)
	defer cancel()
	snapshot, ok := k12.GradingModelSnapshotFromContext(ctx)
	if !ok || !snapshot.HasFrozenCapabilityProbeEvidence() {
		t.Fatalf("provider context lost frozen capability evidence: %+v", snapshot)
	}
	if snapshot.ConfigFingerprint != route.ConfigFingerprint ||
		snapshot.CapabilityReceiptDigest != route.CapabilityReceiptDigest ||
		snapshot.ProbePolicyVersion != route.ProbePolicyVersion {
		t.Fatalf("provider context evidence drifted: %+v", snapshot)
	}
}

func TestImageTaskWorkFeedbackContextCarriesFrozenCapabilityReceiptEvidence(t *testing.T) {
	route := imageTaskCapabilityReceiptRouteFixture()
	ctx := imageTaskWorkFeedbackContext(context.Background(), route)
	snapshot, ok := k12.GradingModelSnapshotFromContext(ctx)
	if !ok || !snapshot.HasFrozenCapabilityProbeEvidence() {
		t.Fatalf("work feedback context lost frozen capability evidence: %+v", snapshot)
	}
}
