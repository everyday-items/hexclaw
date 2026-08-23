package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// K12-FROZEN-MODEL-ROUTE-001：能力回执和配置指纹是与 provider/model 同级的
// 路由事实；失败重试不能用当前控制面再次解析或替换它们。
func TestGradingRetryKeepsFrozenCapabilityEvidence(t *testing.T) {
	recognizer := &routeSnapshotRecognizer{failures: 1}
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	d.Recognizer = recognizer
	frozen := k12.GradingModelSnapshot{
		Provider:                "provider-a",
		Model:                   "vision-a",
		Route:                   "provider-a/vision-a",
		Capability:              "vision",
		ProviderInstanceID:      "pvd_v1_00112233445566778899aabbccddeeff",
		ConfigFingerprint:       "sha256:config-a",
		CapabilityReceiptDigest: "sha256:receipt-a",
		ProbePolicyVersion:      "model-capability-probe-v1",
	}
	current := frozen
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		return current, nil
	}))
	v, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "im", SourceKey: "capability-snapshot",
	})
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}
	if _, err := o.RunGradingJob(context.Background(), v.Record.RecordID); err == nil {
		t.Fatal("first provider failure expected")
	}

	current = k12.GradingModelSnapshot{
		Provider:                "provider-b",
		Model:                   "vision-b",
		Route:                   "provider-b/vision-b",
		Capability:              "vision",
		ProviderInstanceID:      "pvd_v1_ffeeddccbbaa99887766554433221100",
		ConfigFingerprint:       "sha256:config-b",
		CapabilityReceiptDigest: "sha256:receipt-b",
		ProbePolicyVersion:      "model-capability-probe-v2",
	}
	if _, err := o.RetryAndRun(context.Background(), v.Record.RecordID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitForRouteTestAnchor(t, o, v.Record.RecordID)
	if len(recognizer.seen) != 2 {
		t.Fatalf("captured routes=%v", recognizer.seen)
	}
	for attempt, got := range recognizer.seen {
		if got != frozen {
			t.Fatalf("attempt %d capability evidence drifted: got=%+v want=%+v", attempt+1, got, frozen)
		}
	}
}
