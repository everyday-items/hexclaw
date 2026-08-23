package k12

import (
	"context"
	"encoding/json"
	"testing"
)

// K12-FROZEN-MODEL-ROUTE-001：批改任务一经创建，模型能力证据随路由快照固化，
// 重试/恢复只能读取同一份快照，不能重新解释当前配置。
func TestGradingModelSnapshotFreezesCapabilityEvidenceAcrossJSONAndContext(t *testing.T) {
	want := GradingModelSnapshot{
		Provider:                "hexclaw-gpt",
		Model:                   "gpt-5.6-sol",
		Route:                   "hexclaw-gpt/gpt-5.6-sol",
		Capability:              "vision",
		ProviderInstanceID:      " pvd_v1_00112233445566778899aabbccddeeff ",
		ConfigFingerprint:       " sha256:config-fingerprint ",
		CapabilityReceiptDigest: " sha256:vision-receipt ",
		ProbePolicyVersion:      " model-capability-probe-v1 ",
	}

	want = NormalizeGradingModelSnapshot(want)
	if want.ProviderInstanceID != "pvd_v1_00112233445566778899aabbccddeeff" ||
		want.ConfigFingerprint != "sha256:config-fingerprint" ||
		want.CapabilityReceiptDigest != "sha256:vision-receipt" ||
		want.ProbePolicyVersion != "model-capability-probe-v1" {
		t.Fatalf("capability evidence was not normalized: %+v", want)
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var restored GradingModelSnapshot
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored != want {
		t.Fatalf("snapshot drifted after persistence: got=%+v want=%+v", restored, want)
	}

	fromContext, ok := GradingModelSnapshotFromContext(
		WithGradingModelSnapshot(context.Background(), want),
	)
	if !ok || fromContext != want {
		t.Fatalf("snapshot drifted in invocation context: ok=%v got=%+v want=%+v", ok, fromContext, want)
	}
	if !want.HasFrozenCapabilityProbeEvidence() {
		t.Fatal("complete frozen capability evidence was not recognized")
	}
}

// 旧任务不具有历史模型探测证据，升级后必须仍可读取；不得虚构为当前探测结果。
func TestGradingModelSnapshotLegacyWithoutCapabilityEvidenceRemainsCompatible(t *testing.T) {
	legacy := NormalizeGradingModelSnapshot(GradingModelSnapshot{
		Provider: "provider-a",
		Model:    "vision-a",
		Route:    "provider-a/vision-a",
	})
	if legacy.ProviderInstanceID != "" || legacy.ConfigFingerprint != "" ||
		legacy.CapabilityReceiptDigest != "" || legacy.ProbePolicyVersion != "" {
		t.Fatalf("legacy snapshot invented capability evidence: %+v", legacy)
	}
	if legacy.HasFrozenCapabilityProbeEvidence() {
		t.Fatal("legacy route must not be misrepresented as capability-verified")
	}

	partial := legacy
	partial.ProviderInstanceID = "pvd_v1_00112233445566778899aabbccddeeff"
	if partial.HasFrozenCapabilityProbeEvidence() {
		t.Fatal("partial rollout fields must not be treated as verified evidence")
	}
}

// 图片任务自己的路由回执必须和后续 GradingJob 使用相同的冻结证据字段；它不能在
// ImageTask → GradingJob 边界重新按当前默认 Provider 解释。
func TestImageTaskRouteSnapshotFreezesCapabilityEvidence(t *testing.T) {
	snapshot := NormalizeImageTaskRouteSnapshot(ImageTaskRouteSnapshot{
		Provider:                "hexclaw-gpt",
		Model:                   "gpt-5.6-sol",
		Route:                   "hexclaw-gpt/gpt-5.6-sol",
		Capability:              "vision",
		SelectionSource:         "explicit",
		PolicyVersion:           "image-task-routing-v1",
		PromptVersion:           "image-task-classifier-v1",
		ProviderInstanceID:      " pvd_v1_00112233445566778899aabbccddeeff ",
		ConfigFingerprint:       " sha256:config-fingerprint ",
		CapabilityReceiptDigest: " sha256:vision-receipt ",
		ProbePolicyVersion:      " model-capability-probe-v1 ",
	})
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("valid image task snapshot rejected: %v", err)
	}
	if !snapshot.HasFrozenCapabilityProbeEvidence() {
		t.Fatal("complete image task capability evidence was not recognized")
	}
	if snapshot.ProviderInstanceID != "pvd_v1_00112233445566778899aabbccddeeff" ||
		snapshot.ConfigFingerprint != "sha256:config-fingerprint" ||
		snapshot.CapabilityReceiptDigest != "sha256:vision-receipt" ||
		snapshot.ProbePolicyVersion != "model-capability-probe-v1" {
		t.Fatalf("image task evidence was not normalized: %+v", snapshot)
	}
}
