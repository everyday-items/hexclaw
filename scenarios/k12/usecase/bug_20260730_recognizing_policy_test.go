package usecase

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const dd036PolicySnapshotJSON = `{
	"provider":"hexclaw-gpt",
	"model":"gpt-5.6-sol",
	"route":"hexclaw-gpt/gpt-5.6-sol",
	"capability":"vision",
	"timeout_ms":120000,
	"recognizing_request_policy":{
		"policy_version":"dd036-recognizing-v1",
		"stage":"recognizing",
		"thinking":"off",
		"reasoning_effort":"none"
	}
}`

type dd036PolicyRecognizer struct {
	calls int
}

func (r *dd036PolicyRecognizer) Recognize(
	ctx context.Context,
	_ []byte,
) ([]RecognizedQuestion, error) {
	r.calls++
	snapshot, ok := k12.GradingModelSnapshotFromContext(ctx)
	if !ok {
		return nil, ErrInvalidInput
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	policy, ok := fields["recognizing_request_policy"].(map[string]any)
	if !ok ||
		policy["policy_version"] != "dd036-recognizing-v1" ||
		policy["stage"] != k12.GradingStageRecognizing ||
		policy["thinking"] != "off" ||
		policy["reasoning_effort"] != "none" {
		return nil, ErrInvalidInput
	}
	return []RecognizedQuestion{{
		Question:    "1+1=",
		AnswerState: AnswerStateBlank,
	}}, nil
}

// REG-K12-RECOGNIZING-POLICY-002: the policy must already be frozen in the
// Job snapshot, copied into the durable invocation receipt, and bound into the
// recognizing request digest before the recognizer boundary is reached.
func TestBug20260730RecognizingPolicyIsFrozenInContextDigestAndReceipt(t *testing.T) {
	var snapshot k12.GradingModelSnapshot
	if err := json.Unmarshal([]byte(dd036PolicySnapshotJSON), &snapshot); err != nil {
		t.Fatalf("decode approved snapshot: %v", err)
	}
	recognizer := &dd036PolicyRecognizer{}
	deps, _ := newPipeline(t,
		fakeSolver{
			solution: "2",
			ev: SolveEvidence{
				Verdict:      VerdictAgree,
				EvidenceType: EvidenceNumericExec,
			},
		},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		nil,
	)
	deps.Recognizer = recognizer
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return snapshot, nil
		},
	))
	job, created, err := orchestrator.StartPhotoGradingJob(
		context.Background(),
		StartPhotoGradingInput{
			Photo:      orchestratorPhotoRequest(),
			SourceKind: "desktop",
			SourceKey:  "dd036-policy-receipt",
		},
	)
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}
	if _, err := orchestrator.RunGradingJob(context.Background(), job.Record.RecordID); err != nil {
		t.Fatalf("run with approved frozen policy: %v", err)
	}

	invocations, err := deps.Records.ListModelInvocations(
		context.Background(),
		"mingming",
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list invocations: %v", err)
	}
	var recognizing k12.ModelInvocation
	for _, invocation := range invocations {
		if invocation.Stage == k12.GradingStageRecognizing {
			recognizing = invocation
			break
		}
	}
	if recognizing.InvocationID == "" {
		t.Fatalf("recognizing invocation missing: %+v", invocations)
	}
	rawReceipt, err := json.Marshal(recognizing)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(rawReceipt, &receipt); err != nil {
		t.Fatal(err)
	}
	policy, ok := receipt["request_policy_snapshot"].(map[string]any)
	if !ok || policy["thinking"] != "off" || policy["reasoning_effort"] != "none" {
		t.Fatalf("redacted invocation policy missing/drifted: %s", rawReceipt)
	}
	legacyDigest := modelInvocationDigest(
		[]byte(k12.GradingStageRecognizing),
		orchestratorPhotoRequest().Image,
	)
	if recognizing.RequestDigest == legacyDigest {
		t.Fatalf("recognizing request digest still ignores route/policy: %s", recognizing.RequestDigest)
	}
	if recognizer.calls != 1 {
		t.Fatalf("recognizer calls=%d, want one physical attempt", recognizer.calls)
	}
}

type dd036UnexpectedCallRecognizer struct {
	calls int
}

func (r *dd036UnexpectedCallRecognizer) Recognize(
	context.Context,
	[]byte,
) ([]RecognizedQuestion, error) {
	r.calls++
	return []RecognizedQuestion{{
		Question:    "1+1=",
		AnswerState: AnswerStateBlank,
	}}, nil
}

func TestBug20260730MissingRecognizingPolicyFailsBeforeProvider(t *testing.T) {
	recognizer := &dd036UnexpectedCallRecognizer{}
	deps, _ := newPipeline(t,
		fakeSolver{
			solution: "2",
			ev: SolveEvidence{
				Verdict:      VerdictAgree,
				EvidenceType: EvidenceNumericExec,
			},
		},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}},
		nil,
	)
	deps.Recognizer = recognizer
	snapshot := k12.GradingModelSnapshot{
		Provider: "hexclaw-gpt",
		Model:    "gpt-5.6-sol",
		Route:    "hexclaw-gpt/gpt-5.6-sol",
	}
	orchestrator := trackGradingOrchestrator(t, NewGradingOrchestrator(
		deps,
		func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
			return snapshot, nil
		},
	))
	_, created, err := orchestrator.StartPhotoGradingJob(
		context.Background(),
		StartPhotoGradingInput{
			Photo:      orchestratorPhotoRequest(),
			SourceKind: "desktop",
			SourceKey:  "dd036-missing-policy",
		},
	)
	if err == nil || created {
		t.Fatalf("missing frozen policy start created=%v err=%v, want pre-persistence rejection", created, err)
	}
	if recognizer.calls != 0 {
		t.Fatalf("missing policy reached provider boundary %d times", recognizer.calls)
	}
}
