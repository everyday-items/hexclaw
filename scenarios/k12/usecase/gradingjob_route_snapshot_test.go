package usecase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type routeSnapshotRecognizer struct {
	failures int
	calls    int
	seen     []k12.GradingModelSnapshot
}

func (r *routeSnapshotRecognizer) Recognize(ctx context.Context, _ []byte) ([]RecognizedQuestion, error) {
	r.calls++
	snapshot, ok := k12.GradingModelSnapshotFromContext(ctx)
	if !ok {
		return nil, errors.New("route snapshot missing at model boundary")
	}
	r.seen = append(r.seen, snapshot)
	if r.calls <= r.failures {
		return nil, &gradingProviderResponseError{status: 503}
	}
	return []RecognizedQuestion{{Question: "1+1=", AnswerState: AnswerStateBlank}}, nil
}

// REG-DD-018: changing global defaults after a Job fails cannot change the
// provider/model/route used by the retry. Every attempt receives the immutable
// snapshot from the persisted GradingJob.
func TestGradingRetryKeepsOriginalProviderModelRouteSnapshot(t *testing.T) {
	recognizer := &routeSnapshotRecognizer{failures: 1}
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	d.Recognizer = recognizer
	current := k12.GradingModelSnapshot{Provider: "provider-a", Model: "vision-a", Route: "provider-a/vision-a", Capability: "vision"}
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		return current, nil
	}))
	v, created, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "im", SourceKey: "route-snapshot",
	})
	if err != nil || !created {
		t.Fatalf("start created=%v err=%v", created, err)
	}
	if _, err := o.RunGradingJob(context.Background(), v.Record.RecordID); err == nil {
		t.Fatal("first provider failure expected")
	}

	// Simulate a settings change between attempts. Retry must not consult it.
	current = k12.GradingModelSnapshot{Provider: "provider-b", Model: "vision-b", Route: "provider-b/vision-b", Capability: "vision"}
	if _, err := o.RetryAndRun(context.Background(), v.Record.RecordID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	waitForRouteTestAnchor(t, o, v.Record.RecordID)
	if len(recognizer.seen) != 2 {
		t.Fatalf("captured routes=%v", recognizer.seen)
	}
	for i, snapshot := range recognizer.seen {
		if snapshot.Provider != "provider-a" || snapshot.Model != "vision-a" || snapshot.Route != "provider-a/vision-a" {
			t.Fatalf("attempt %d route drifted to %+v", i+1, snapshot)
		}
	}
}

func TestValidateGradingModelRouteFailsClosedOnCrossRouteUse(t *testing.T) {
	ctx := k12.WithGradingModelSnapshot(context.Background(), k12.GradingModelSnapshot{
		Provider: "provider-a", Model: "model-a", Route: "provider-a/model-a",
	})
	if err := k12.ValidateGradingModelRoute(ctx, "provider-a", "model-a"); err != nil {
		t.Fatalf("frozen route rejected: %v", err)
	}
	if err := k12.ValidateGradingModelRoute(ctx, "provider-b", "model-b"); err == nil {
		t.Fatal("cross-route model call must fail closed")
	}
}

type outcomeUnknownRecognizer struct{ calls int }

func (r *outcomeUnknownRecognizer) Recognize(context.Context, []byte) ([]RecognizedQuestion, error) {
	r.calls++
	return nil, context.DeadlineExceeded
}

type reconcileThenSucceedRecognizer struct {
	calls int
	seen  []k12.GradingModelSnapshot
}

func (r *reconcileThenSucceedRecognizer) Recognize(ctx context.Context, _ []byte) ([]RecognizedQuestion, error) {
	r.calls++
	snapshot, _ := k12.GradingModelSnapshotFromContext(ctx)
	r.seen = append(r.seen, snapshot)
	if r.calls == 1 {
		return nil, context.DeadlineExceeded
	}
	return []RecognizedQuestion{{Question: "2+2=", AnswerState: AnswerStateBlank}}, nil
}

// REG-DD-020: a request that may have reached a provider but times out must
// converge to outcome_unknown. Ordinary retry and crash recovery may not send a
// second request until reconciliation proves the first one did not execute.
func TestGradingModelTimeoutUsesInvocationLedgerAndBlocksBlindRetry(t *testing.T) {
	recognizer := &outcomeUnknownRecognizer{}
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	if _, err := d.Records.DB().ExecContext(context.Background(), migrate.K12ModelInvocationsV15DDL); err != nil {
		t.Fatalf("init invocation ledger: %v", err)
	}
	d.Recognizer = recognizer
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		return k12.GradingModelSnapshot{Provider: "provider-a", Model: "vision-a", Route: "provider-a/vision-a"}, nil
	}))
	v, _, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "im", SourceKey: "unknown-ledger",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.RunGradingJob(context.Background(), v.Record.RecordID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run error=%v", err)
	}
	stored, err := d.GetGradingJob(context.Background(), "mingming", v.Record.RecordID)
	if err != nil || stored.Record.Status != k12.GradingStageOutcomeUnknown || stored.Fields.Retryable {
		t.Fatalf("job=%+v fields=%+v err=%v", stored.Record, stored.Fields, err)
	}
	invocations, err := d.Records.ListModelInvocations(context.Background(), "mingming", v.Record.RecordID)
	if err != nil || len(invocations) != 1 || invocations[0].Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf("invocations=%+v err=%v", invocations, err)
	}
	if _, err := o.RetryAndRun(context.Background(), v.Record.RecordID); err == nil {
		t.Fatal("outcome_unknown must not expose ordinary retry")
	}
	if _, err := o.RunGradingJob(context.Background(), v.Record.RecordID); err != nil {
		t.Fatalf("query/recovery pass should remain parked, got %v", err)
	}
	if recognizer.calls != 1 {
		t.Fatalf("blind replay called provider %d times", recognizer.calls)
	}
}

// REG-DD-020 crash point: provider result was durably recorded, then the
// process died before the stage checkpoint/artifact commit. Recovery must park
// for reconciliation instead of calling the provider again and risking a
// second paid result.
func TestGradingSucceededInvocationBeforeCheckpointIsNotBlindlyReplayed(t *testing.T) {
	recognizer := &routeSnapshotRecognizer{}
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	d.Recognizer = recognizer
	snapshot := k12.GradingModelSnapshot{Provider: "provider-a", Model: "vision-a", Route: "provider-a/vision-a"}
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		return snapshot, nil
	}))
	v, _, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "im", SourceKey: "crash-after-result",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-call stage position without invoking the provider.
	if v, err = d.AdvanceGradingStage(context.Background(), "mingming", v.Record.RecordID, AdvanceGradingInput{Outcome: GradingOutcomeOK}); err != nil {
		t.Fatal(err)
	}
	if v, err = d.AdvanceGradingStage(context.Background(), "mingming", v.Record.RecordID, AdvanceGradingInput{Outcome: GradingOutcomeOK, ArtifactDigest: "image"}); err != nil {
		t.Fatal(err)
	}
	invocation, _, err := d.Records.PrepareModelInvocation(context.Background(), k12.ModelInvocation{
		InvocationID: "inv-crash-gap", AgentName: "mingming", JobID: v.Record.RecordID,
		Stage: k12.GradingStageRecognizing,
		RequestDigest: recognizingInvocationDigest(
			orchestratorPhotoRequest().Image,
			snapshot,
			k12.ModelRequestPolicySnapshot{},
		),
		RouteSnapshot: snapshot, Attempt: 1, CreatedAt: 100, UpdatedAt: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocation, err = d.Records.MarkModelInvocationSent(context.Background(), "mingming", invocation.InvocationID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Records.MarkModelInvocationSucceeded(context.Background(), "mingming", invocation.InvocationID, "sha256:result", "upstream-1"); err != nil {
		t.Fatal(err)
	}

	if _, err := o.RunGradingJob(context.Background(), v.Record.RecordID); !errors.Is(err, ErrModelInvocationRequiresReconciliation) {
		t.Fatalf("recovery err=%v", err)
	}
	parked, err := d.GetGradingJob(context.Background(), "mingming", v.Record.RecordID)
	if err != nil || parked.Record.Status != k12.GradingStageOutcomeUnknown {
		t.Fatalf("parked=%+v err=%v", parked.Record, err)
	}
	if recognizer.calls != 0 {
		t.Fatalf("provider replayed %d times after succeeded-ledger crash gap", recognizer.calls)
	}
	rows, err := d.Records.ListModelInvocations(context.Background(), "mingming", v.Record.RecordID)
	if err != nil || len(rows) != 1 || rows[0].Status != k12.ModelInvocationSucceeded {
		t.Fatalf("ledger=%+v err=%v", rows, err)
	}
}

func TestGradingInvocationIdentityConflictBeforeSendFailsTerminalInsteadOfOutcomeUnknown(t *testing.T) {
	recognizer := &routeSnapshotRecognizer{}
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	d.Recognizer = recognizer
	policy := k12.ApprovedRecognizingRequestPolicy()
	currentRoute := k12.GradingModelSnapshot{
		Provider:                 "hexclaw-gpt",
		Model:                    k12.RecognizingPolicyModel,
		Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
		RecognizingRequestPolicy: policy,
	}
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		return currentRoute, nil
	}))
	v, _, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "desktop", SourceKey: "route-conflict-before-send",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, err = d.AdvanceGradingStage(context.Background(), "mingming", v.Record.RecordID, AdvanceGradingInput{Outcome: GradingOutcomeOK}); err != nil {
		t.Fatal(err)
	}
	if v, err = d.AdvanceGradingStage(context.Background(), "mingming", v.Record.RecordID, AdvanceGradingInput{
		Outcome: GradingOutcomeOK, ArtifactDigest: "image",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Records.PrepareModelInvocation(context.Background(), k12.ModelInvocation{
		InvocationID: "inv-bound-to-old-spark", AgentName: "mingming", JobID: v.Record.RecordID,
		Stage: k12.GradingStageRecognizing,
		RequestDigest: modelInvocationDigest(
			[]byte(k12.GradingStageRecognizing), orchestratorPhotoRequest().Image,
		),
		RouteSnapshot: k12.GradingModelSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.3-codex-spark",
			Route: "hexclaw-gpt/gpt-5.3-codex-spark",
		},
		Attempt: 1, CreatedAt: 100, UpdatedAt: 100,
	}); err != nil {
		t.Fatal(err)
	}

	failed, err := o.RunGradingJob(context.Background(), v.Record.RecordID)
	if !errors.Is(err, k12storage.ErrModelInvocationConflict) {
		t.Fatalf("run error=%v, want immutable invocation conflict", err)
	}
	if failed.Record.Status != k12.GradingStageFailedTerminal ||
		failed.Fields.FailureKind != "invocation_identity_conflict" {
		t.Fatalf("pre-send identity conflict must fail terminal, got record=%+v fields=%+v",
			failed.Record, failed.Fields)
	}
	if recognizer.calls != 0 {
		t.Fatalf("provider must not run after pre-send identity conflict: calls=%d", recognizer.calls)
	}
}

func TestGradingSucceededInvocationWithDurableArtifactRecoversCheckpointWithoutProviderReplay(t *testing.T) {
	recognizer := &routeSnapshotRecognizer{}
	d, _ := newPipeline(t,
		fakeSolver{solution: "2", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	d.Recognizer = recognizer
	snapshot := k12.GradingModelSnapshot{Provider: "provider-a", Model: "vision-a", Route: "provider-a/vision-a"}
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		return snapshot, nil
	}, WithGradingRunDir(t.TempDir())))
	v, _, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "im", SourceKey: "crash-with-artifact",
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, err = d.AdvanceGradingStage(context.Background(), "mingming", v.Record.RecordID, AdvanceGradingInput{Outcome: GradingOutcomeOK}); err != nil {
		t.Fatal(err)
	}
	if v, err = d.AdvanceGradingStage(context.Background(), "mingming", v.Record.RecordID, AdvanceGradingInput{Outcome: GradingOutcomeOK, ArtifactDigest: "image"}); err != nil {
		t.Fatal(err)
	}
	run := o.lookup(v.Record.RecordID)
	run.questions = []RecognizedQuestion{{Question: "1+1=", ProblemID: "p1", CanonicalMarkdown: "1+1=", AnswerState: AnswerStateBlank}}
	if err := o.persistRun(v.Record.RecordID, run); err != nil {
		t.Fatal(err)
	}
	invocation, _, err := d.Records.PrepareModelInvocation(context.Background(), k12.ModelInvocation{
		InvocationID: "inv-crash-with-artifact", AgentName: "mingming", JobID: v.Record.RecordID,
		Stage: k12.GradingStageRecognizing,
		RequestDigest: recognizingInvocationDigest(
			orchestratorPhotoRequest().Image,
			snapshot,
			k12.ModelRequestPolicySnapshot{},
		),
		RouteSnapshot: snapshot, Attempt: 1, CreatedAt: 100, UpdatedAt: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err = d.Records.MarkModelInvocationSent(context.Background(), "mingming", invocation.InvocationID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Records.MarkModelInvocationSucceeded(context.Background(), "mingming", invocation.InvocationID,
		modelInvocationResultDigest(run.questions), "upstream-1"); err != nil {
		t.Fatal(err)
	}

	recovered, err := o.RunGradingJob(context.Background(), v.Record.RecordID)
	if err != nil || recovered.Record.Status != k12.GradingStageAwaitingConfirmation {
		t.Fatalf("recovered=%+v err=%v", recovered.Record, err)
	}
	if recognizer.calls != 0 {
		t.Fatalf("provider replayed %d times despite durable artifact", recognizer.calls)
	}
	waitForRouteTestAnchor(t, o, v.Record.RecordID)
}

func TestGradingReconciliationProvesNotExecutedBeforeSafeSameRouteRetry(t *testing.T) {
	recognizer := &reconcileThenSucceedRecognizer{}
	d, _ := newPipeline(t,
		fakeSolver{solution: "4", ev: SolveEvidence{Verdict: VerdictAgree, EvidenceType: EvidenceNumericExec}},
		fakeGrader{outcome: GradeOutcome{Verdict: VerdictAgree}}, nil)
	d.Recognizer = recognizer
	snapshot := k12.GradingModelSnapshot{Provider: "provider-a", Model: "vision-a", Route: "provider-a/vision-a"}
	o := trackGradingOrchestrator(t, NewGradingOrchestrator(d, func(k12.GradingModelSnapshot) (k12.GradingModelSnapshot, error) {
		return snapshot, nil
	}))
	v, _, err := o.StartPhotoGradingJob(context.Background(), StartPhotoGradingInput{
		Photo: orchestratorPhotoRequest(), SourceKind: "im", SourceKey: "reconcile-then-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.RunGradingJob(context.Background(), v.Record.RecordID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first run err=%v", err)
	}
	rows, err := d.Records.ListModelInvocations(context.Background(), "mingming", v.Record.RecordID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("unknown ledger=%+v err=%v", rows, err)
	}
	reconciled, err := d.ReconcileGradingInvocationNotExecuted(context.Background(), "mingming", v.Record.RecordID, rows[0].InvocationID)
	if err != nil || reconciled.Record.Status != k12.GradingStageFailedRetryable || !reconciled.Fields.Retryable {
		t.Fatalf("reconciled job=%+v fields=%+v err=%v", reconciled.Record, reconciled.Fields, err)
	}
	if _, err := o.RetryAndRun(context.Background(), v.Record.RecordID); err != nil {
		t.Fatalf("safe retry after reconciliation: %v", err)
	}
	waitForRouteTestAnchor(t, o, v.Record.RecordID)
	rows, err = d.Records.ListModelInvocations(context.Background(), "mingming", v.Record.RecordID)
	if err != nil || len(rows) != 2 || rows[0].Attempt != 1 || rows[1].Attempt != 2 ||
		rows[0].Status != k12.ModelInvocationReconciled || rows[1].Status != k12.ModelInvocationSucceeded {
		t.Fatalf("reconciled ledger=%+v err=%v", rows, err)
	}
	if recognizer.calls != 2 {
		t.Fatalf("provider calls=%d", recognizer.calls)
	}
	for _, seen := range recognizer.seen {
		if seen.Route != snapshot.Route {
			t.Fatalf("retry route drifted: %+v", seen)
		}
	}
}

func waitForRouteTestAnchor(t *testing.T, o *GradingOrchestrator, jobID string) {
	t.Helper()
	if done := o.anchorDoneChannel(jobID); done != nil {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("anchor branch did not settle for job %s", jobID)
		}
	}
}
