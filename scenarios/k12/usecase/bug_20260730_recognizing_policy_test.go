package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
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
	image []byte,
) ([]RecognizedQuestion, error) {
	r.calls++
	snapshot, ok := k12.GradingModelSnapshotFromContext(ctx)
	if !ok {
		return nil, ErrInvalidInput
	}
	requestPolicy, ok := k12.GradingModelRequestPolicyFromContext(ctx)
	if !ok || requestPolicy != k12.ApprovedRecognizingRequestPolicy() {
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
	physical, err := k12.ExecuteRecognitionPhysicalCall(
		ctx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: image,
		},
		func(context.Context) (string, error) {
			return `[{"question":"1+1=","answer_state":"blank"}]`, nil
		},
	)
	if err != nil {
		return nil, err
	}
	if physical.InvocationID == "" || physical.Payload == "" {
		return nil, ErrModelInvocationRequiresReconciliation
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
	deps.Now = func() int64 { return time.Now().Unix() }
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

type dd036PhysicalLedgerRecognizer struct {
	records *k12storage.Store
	jobID   string
	calls   int
	sawSent bool
}

func (r *dd036PhysicalLedgerRecognizer) Recognize(
	ctx context.Context,
	image []byte,
) ([]RecognizedQuestion, error) {
	result, err := k12.ExecuteRecognitionPhysicalCall(
		ctx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: image,
		},
		func(context.Context) (string, error) {
			r.calls++
			rows, listErr := r.records.ListModelPhysicalInvocations(
				context.Background(),
				"mingming",
				r.jobID,
			)
			if listErr != nil {
				return "", listErr
			}
			r.sawSent = len(rows) == 1 &&
				rows[0].PhysicalUnit == k12.RecognitionPhysicalUnitWholePage &&
				rows[0].Status == k12.ModelInvocationSent
			return `[{"question":"1+1=","answer_state":"blank"}]`, nil
		},
	)
	if err != nil {
		return nil, err
	}
	if result.InvocationID == "" || result.Payload == "" {
		return nil, ErrModelInvocationRequiresReconciliation
	}
	return []RecognizedQuestion{{
		Question:    "1+1=",
		AnswerState: AnswerStateBlank,
	}}, nil
}

// REG-K12-RECOGNIZING-POLICY-002: the stage invocation is the parent receipt;
// the actual provider boundary must have its own immutable child already sent
// before the callback can escape.
func TestBug20260730RecognizingPhysicalCallHasDurableParentChildReceipt(t *testing.T) {
	var snapshot k12.GradingModelSnapshot
	if err := json.Unmarshal([]byte(dd036PolicySnapshotJSON), &snapshot); err != nil {
		t.Fatalf("decode approved snapshot: %v", err)
	}
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
	deps.Now = func() int64 { return time.Now().Unix() }
	recognizer := &dd036PhysicalLedgerRecognizer{records: deps.Records}
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
			SourceKey:  "dd036-physical-child",
		},
	)
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}
	recognizer.jobID = job.Record.RecordID
	if _, err := orchestrator.RunGradingJob(
		context.Background(),
		job.Record.RecordID,
	); err != nil {
		t.Fatalf("run with physical child ledger: %v", err)
	}

	parents, err := deps.Records.ListModelInvocations(
		context.Background(),
		"mingming",
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list parent invocations: %v", err)
	}
	var parent k12.ModelInvocation
	for _, invocation := range parents {
		if invocation.Stage == k12.GradingStageRecognizing {
			parent = invocation
			break
		}
	}
	children, err := deps.Records.ListModelPhysicalInvocations(
		context.Background(),
		"mingming",
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list physical child invocations: %v", err)
	}
	if parent.InvocationID == "" || len(children) != 1 {
		t.Fatalf("parent=%+v children=%+v", parent, children)
	}
	child := children[0]
	if !recognizer.sawSent || recognizer.calls != 1 ||
		child.ParentInvocationID != parent.InvocationID ||
		child.PhysicalUnit != k12.RecognitionPhysicalUnitWholePage ||
		child.Attempt != 1 ||
		child.Status != k12.ModelInvocationSucceeded ||
		child.RequestDigest == "" ||
		child.RequestDigest == parent.RequestDigest ||
		child.RouteSnapshot != parent.RouteSnapshot ||
		child.RequestPolicySnapshot != parent.RequestPolicySnapshot ||
		child.ResultDigest == "" {
		t.Fatalf(
			"physical receipt drift: sawSent=%v calls=%d parent=%+v child=%+v",
			recognizer.sawSent,
			recognizer.calls,
			parent,
			child,
		)
	}
}

func TestBug20260730LatePhysicalHTTP200CannotBecomeSuccess(t *testing.T) {
	var snapshot k12.GradingModelSnapshot
	if err := json.Unmarshal([]byte(dd036PolicySnapshotJSON), &snapshot); err != nil {
		t.Fatalf("decode approved snapshot: %v", err)
	}
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
	deps.Now = func() int64 { return time.Now().Unix() }
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
			SourceKey:  "dd036-late-http-200",
		},
	)
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}
	parent, _, err := deps.Records.PrepareModelInvocation(
		context.Background(),
		k12.ModelInvocation{
			InvocationID:  "parent-dd036-late-http-200",
			AgentName:     job.Record.AgentName,
			JobID:         job.Record.RecordID,
			Stage:         k12.GradingStageRecognizing,
			RequestDigest: "sha256:parent-dd036-late-http-200",
			RouteSnapshot: snapshot,
			RequestPolicySnapshot: k12.NormalizeModelRequestPolicySnapshot(
				snapshot.RecognizingRequestPolicy,
			),
			Attempt:   1,
			CreatedAt: deps.now(),
			UpdatedAt: deps.now(),
		},
	)
	if err != nil {
		t.Fatalf("prepare parent invocation: %v", err)
	}
	parent, err = deps.Records.MarkModelInvocationSent(
		context.Background(),
		job.Record.AgentName,
		parent.InvocationID,
		"",
	)
	if err != nil {
		t.Fatalf("mark parent sent: %v", err)
	}

	callCtx, cancel := context.WithTimeout(
		context.Background(),
		40*time.Millisecond,
	)
	defer cancel()
	sawSent := false
	executor := newDurableRecognitionPhysicalCallExecutor(
		orchestrator,
		parent,
	)
	if _, err := executor.ExecuteRecognitionPhysicalCall(
		callCtx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: []byte("late-provider-image"),
		},
		func(providerCtx context.Context) (string, error) {
			rows, listErr := deps.Records.ListModelPhysicalInvocations(
				context.Background(),
				job.Record.AgentName,
				job.Record.RecordID,
			)
			if listErr != nil {
				return "", listErr
			}
			sawSent = len(rows) == 1 &&
				rows[0].Status == k12.ModelInvocationSent
			<-providerCtx.Done()
			return `[{"question":"late 200"}]`, nil
		},
	); err == nil {
		t.Fatal("late provider success unexpectedly completed the stage")
	}

	children, err := deps.Records.ListModelPhysicalInvocations(
		context.Background(),
		"mingming",
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatalf("list physical child invocations: %v", err)
	}
	if !sawSent || len(children) != 1 ||
		children[0].Status != k12.ModelInvocationOutcomeUnknown ||
		children[0].ResultDigest != "" ||
		children[0].FailureKind != "provider_outcome_unknown" {
		t.Fatalf(
			"late response was not fenced: sawSent=%v children=%+v",
			sawSent,
			children,
		)
	}
}

type dd036DefinitivePhysicalFailureRecognizer struct{}

func (dd036DefinitivePhysicalFailureRecognizer) Recognize(
	ctx context.Context,
	image []byte,
) ([]RecognizedQuestion, error) {
	_, err := k12.ExecuteRecognitionPhysicalCall(
		ctx,
		k12.RecognitionPhysicalCall{
			Unit:  k12.RecognitionPhysicalUnitWholePage,
			Image: image,
		},
		func(context.Context) (string, error) {
			return "", &gradingProviderResponseError{status: 400}
		},
	)
	return nil, err
}

func TestBug20260730PhysicalTerminalWriteFailureMakesParentUnknown(t *testing.T) {
	var snapshot k12.GradingModelSnapshot
	if err := json.Unmarshal([]byte(dd036PolicySnapshotJSON), &snapshot); err != nil {
		t.Fatalf("decode approved snapshot: %v", err)
	}
	deps, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	deps.Now = func() int64 { return time.Now().Unix() }
	deps.Recognizer = dd036DefinitivePhysicalFailureRecognizer{}
	if _, err := deps.Records.DB().Exec(`
		CREATE TRIGGER reject_physical_failed_terminal
		BEFORE UPDATE OF status ON k12_model_physical_invocations
		WHEN OLD.status = 'sent' AND NEW.status = 'failed'
		BEGIN
			SELECT RAISE(ABORT, 'injected physical terminal write failure');
		END
	`); err != nil {
		t.Fatalf("create terminal failure trigger: %v", err)
	}
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
			SourceKey:  "dd036-physical-terminal-write-failure",
		},
	)
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}
	result, runErr := orchestrator.RunGradingJob(
		context.Background(),
		job.Record.RecordID,
	)
	if !errors.Is(runErr, ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("terminal ledger failure err=%v, want reconciliation", runErr)
	}
	if result.Record.Status != k12.GradingStageOutcomeUnknown || result.Fields.Retryable {
		t.Fatalf(
			"terminal ledger failure must fail closed: stage=%s retryable=%v err=%v",
			result.Record.Status,
			result.Fields.Retryable,
			runErr,
		)
	}
	parents, err := deps.Records.ListModelInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	children, err := deps.Records.ListModelPhysicalInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(parents) != 1 ||
		parents[0].Status != k12.ModelInvocationOutcomeUnknown ||
		len(children) != 1 ||
		children[0].Status != k12.ModelInvocationSent {
		t.Fatalf(
			"parent/child ambiguity was misclassified: parents=%+v children=%+v",
			parents,
			children,
		)
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

func TestBug20260730ApprovedRecognitionWithoutPhysicalChildFailsClosed(t *testing.T) {
	var snapshot k12.GradingModelSnapshot
	if err := json.Unmarshal([]byte(dd036PolicySnapshotJSON), &snapshot); err != nil {
		t.Fatalf("decode approved snapshot: %v", err)
	}
	recognizer := &dd036UnexpectedCallRecognizer{}
	deps, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, nil)
	deps.Now = func() int64 { return time.Now().Unix() }
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
			SourceKey:  "dd036-missing-physical-child",
		},
	)
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}
	result, runErr := orchestrator.RunGradingJob(
		context.Background(),
		job.Record.RecordID,
	)
	if !errors.Is(runErr, ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("missing physical child err=%v, want reconciliation", runErr)
	}
	if result.Record.Status != k12.GradingStageOutcomeUnknown || result.Fields.Retryable {
		t.Fatalf(
			"missing physical child must fail closed: stage=%s retryable=%v err=%v",
			result.Record.Status,
			result.Fields.Retryable,
			runErr,
		)
	}
	parents, err := deps.Records.ListModelInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	children, err := deps.Records.ListModelPhysicalInvocations(
		context.Background(),
		job.Record.AgentName,
		job.Record.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if recognizer.calls != 1 ||
		len(parents) != 1 ||
		parents[0].Status != k12.ModelInvocationOutcomeUnknown ||
		len(children) != 1 ||
		children[0].ParentInvocationID != parents[0].InvocationID ||
		children[0].PhysicalUnit != k12.RecognitionPhysicalUnitWholePage ||
		children[0].Status != k12.ModelInvocationPrepared {
		t.Fatalf(
			"untracked recognizing success escaped: calls=%d parents=%+v children=%+v",
			recognizer.calls,
			parents,
			children,
		)
	}
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

func TestBug20260730RecognizingPolicyReceiptIsAllowlistedAndRedacted(t *testing.T) {
	policy := k12.ApprovedRecognizingRequestPolicy()
	receipt := modelInvocationReceipt(k12.ModelInvocation{
		InvocationID:  "inv-dd036-receipt",
		Stage:         k12.GradingStageRecognizing,
		RequestDigest: "sha256:sensitive-request-digest",
		RouteSnapshot: k12.GradingModelSnapshot{
			Provider:                 "hexclaw-gpt",
			Model:                    k12.RecognizingPolicyModel,
			Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
			Capability:               "vision",
			RecognizingRequestPolicy: policy,
		},
		RequestPolicySnapshot:  policy,
		ProviderIdempotencyKey: "sensitive-provider-idempotency-key",
		Status:                 k12.ModelInvocationSucceeded,
		Attempt:                1,
		ResultDigest:           "sha256:result",
		ExternalRequestID:      "sensitive-external-request-id",
	})
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"invocation_id",
		"operation",
		"provider",
		"model",
		"status",
		"attempt",
		"result_digest",
		"request_policy_digest",
		"request_policy",
	} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("allowlisted receipt field %q missing: %s", key, raw)
		}
	}
	if len(fields) != 9 {
		t.Fatalf("receipt exposed fields outside the allowlist: %s", raw)
	}
	if fields["request_policy_digest"] != policy.Digest() {
		t.Fatalf("policy digest drifted: %s", raw)
	}
	projectedPolicy, ok := fields["request_policy"].(map[string]any)
	if !ok ||
		projectedPolicy["policy_version"] != k12.RecognizingRequestPolicyVersion ||
		projectedPolicy["stage"] != k12.GradingStageRecognizing ||
		projectedPolicy["thinking"] != "off" ||
		projectedPolicy["reasoning_effort"] != "none" {
		t.Fatalf("receipt policy projection drifted: %s", raw)
	}
	for _, secret := range []string{
		"sensitive-request-digest",
		"sensitive-provider-idempotency-key",
		"sensitive-external-request-id",
		"route_snapshot",
		"raw_payload",
		"prompt",
		"image",
		"api_key",
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("receipt leaked %q: %s", secret, raw)
		}
	}
}

func TestBug20260730RecognizingPhysicalReceiptIsAllowlistedAndRedacted(t *testing.T) {
	policy := k12.ApprovedRecognizingRequestPolicy()
	receipt := modelPhysicalInvocationReceipt(k12.ModelPhysicalInvocation{
		PhysicalInvocationID: "physical-dd036-receipt",
		ParentInvocationID:   "parent-dd036-receipt",
		Stage:                k12.GradingStageRecognizing,
		PhysicalUnit:         k12.RecognitionPhysicalUnitWholePage,
		RequestDigest:        "sha256:sensitive-physical-request-digest",
		RouteSnapshot: k12.GradingModelSnapshot{
			Provider:                 "hexclaw-gpt",
			Model:                    k12.RecognizingPolicyModel,
			Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
			Capability:               "vision",
			RecognizingRequestPolicy: policy,
		},
		RequestPolicySnapshot: policy,
		Status:                k12.ModelInvocationSucceeded,
		Attempt:               1,
		ResultDigest:          "sha256:physical-result",
		ExternalRequestID:     "sensitive-physical-external-request-id",
	})
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"invocation_id",
		"parent_invocation_id",
		"physical_unit",
		"operation",
		"provider",
		"model",
		"status",
		"attempt",
		"result_digest",
		"request_policy_digest",
		"request_policy",
	} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("allowlisted physical receipt field %q missing: %s", key, raw)
		}
	}
	if len(fields) != 11 {
		t.Fatalf("physical receipt exposed fields outside the allowlist: %s", raw)
	}
	if fields["parent_invocation_id"] != "parent-dd036-receipt" ||
		fields["physical_unit"] != string(k12.RecognitionPhysicalUnitWholePage) ||
		fields["request_policy_digest"] != policy.Digest() {
		t.Fatalf("physical receipt identity/policy drifted: %s", raw)
	}
	for _, secret := range []string{
		"sensitive-physical-request-digest",
		"sensitive-physical-external-request-id",
		"route_snapshot",
		"raw_payload",
		"prompt",
		"image",
		"api_key",
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("physical receipt leaked %q: %s", secret, raw)
		}
	}
}
