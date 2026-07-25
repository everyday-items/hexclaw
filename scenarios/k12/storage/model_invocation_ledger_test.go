package k12storage_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

// REG-DD-020: every external model attempt has a durable, immutable route and
// request ledger before the request can be sent. A replay returns the same row;
// it may never rewrite the route snapshot under the same job/stage/attempt.
func TestModelInvocationLedgerPrepareIsIdempotentAndRouteImmutable(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ModelInvocationsV15DDL); err != nil {
		t.Fatalf("create invocation ledger: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", "route-immutable")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}

	want := k12.ModelInvocation{
		InvocationID: "inv-route-1", AgentName: "mingming", JobID: job.RecordID,
		Stage: k12.GradingStageRecognizing, RequestDigest: "sha256:req-a",
		RouteSnapshot: k12.GradingModelSnapshot{Provider: "openrouter", Model: "vision-v1", Route: "openrouter/vision-v1"},
		Attempt:       1, CreatedAt: 100, UpdatedAt: 100,
	}
	first, created, err := store.PrepareModelInvocation(context.Background(), want)
	if err != nil || !created {
		t.Fatalf("prepare created=%v err=%v", created, err)
	}
	replay, created, err := store.PrepareModelInvocation(context.Background(), want)
	if err != nil || created || replay.InvocationID != first.InvocationID {
		t.Fatalf("replay invocation=%+v created=%v err=%v", replay, created, err)
	}

	changed := want
	changed.InvocationID = "inv-route-attacker"
	changed.RouteSnapshot = k12.GradingModelSnapshot{Provider: "cloud-b", Model: "vision-v2", Route: "cloud-b/vision-v2"}
	if _, _, err := store.PrepareModelInvocation(context.Background(), changed); !errors.Is(err, k12storage.ErrModelInvocationConflict) {
		t.Fatalf("cross-route replay must fail closed, got %v", err)
	}
}

// REG-DD-020: once a request may have reached a provider, timeout/connection
// ambiguity is durable outcome_unknown. Restart/replay must return that row and
// must not create a fresh attempt that could duplicate provider side effects.
func TestModelInvocationLedgerOutcomeUnknownSurvivesStoreRestart(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ModelInvocationsV15DDL); err != nil {
		t.Fatalf("create invocation ledger: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", "unknown-restart")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}
	prepared, _, err := store.PrepareModelInvocation(context.Background(), k12.ModelInvocation{
		InvocationID: "inv-unknown-1", AgentName: "mingming", JobID: job.RecordID,
		Stage: k12.GradingStageAssessing, RequestDigest: "sha256:req-b",
		RouteSnapshot: k12.GradingModelSnapshot{Provider: "provider-a", Model: "model-a", Route: "provider-a/model-a"},
		Attempt:       1, CreatedAt: 200, UpdatedAt: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	sent, err := store.MarkModelInvocationSent(context.Background(), "mingming", prepared.InvocationID, "")
	if err != nil || sent.Status != k12.ModelInvocationSent {
		t.Fatalf("sent=%+v err=%v", sent, err)
	}
	unknown, err := store.MarkModelInvocationOutcomeUnknown(context.Background(), "mingming", prepared.InvocationID, "provider_timeout")
	if err != nil || unknown.Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}

	restarted := k12storage.NewStore(db, nil)
	got, err := restarted.GetModelInvocation(context.Background(), "mingming", prepared.InvocationID)
	if err != nil || got.Status != k12.ModelInvocationOutcomeUnknown || got.FailureKind != "provider_timeout" {
		t.Fatalf("after restart invocation=%+v err=%v", got, err)
	}
	if _, created, err := restarted.PrepareModelInvocation(context.Background(), prepared); err != nil || created {
		t.Fatalf("unknown replay created=%v err=%v", created, err)
	}
}

func TestModelInvocationLedgerReconcilesDurableSuccessWithoutResend(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ModelInvocationsV15DDL); err != nil {
		t.Fatalf("create invocation ledger: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", "reconcile-success")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatalf("create grading job: %v", err)
	}
	ctx := context.Background()
	prepared, _, err := store.PrepareModelInvocation(ctx, k12.ModelInvocation{
		InvocationID: "inv-reconcile-success", AgentName: "mingming", JobID: job.RecordID,
		Stage: k12.GradingStageRecognizing, RequestDigest: "sha256:request",
		RouteSnapshot: k12.GradingModelSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", Route: "hexclaw-gpt/gpt-5.6-sol",
		},
		Attempt: 1, CreatedAt: 300, UpdatedAt: 300,
	})
	if err != nil {
		t.Fatalf("prepare invocation: %v", err)
	}
	if _, err = store.MarkModelInvocationSent(ctx, "mingming", prepared.InvocationID, "provider-key"); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if _, err = store.MarkModelInvocationOutcomeUnknown(ctx, "mingming", prepared.InvocationID, "response_lost"); err != nil {
		t.Fatalf("mark outcome unknown: %v", err)
	}

	reconciled, err := store.ReconcileModelInvocationSucceeded(
		ctx, "mingming", prepared.InvocationID, "sha256:durable-result", "provider-request-1",
	)
	if err != nil {
		t.Fatalf("reconcile durable success: %v", err)
	}
	if reconciled.Status != k12.ModelInvocationReconciled ||
		reconciled.ResultDigest != "sha256:durable-result" ||
		reconciled.ExternalRequestID != "provider-request-1" ||
		reconciled.FailureKind != "reconciled_succeeded" {
		t.Fatalf("unexpected reconciled invocation: %+v", reconciled)
	}

	replay, err := store.ReconcileModelInvocationSucceeded(
		ctx, "mingming", prepared.InvocationID, "sha256:durable-result", "provider-request-1",
	)
	if err != nil || replay != reconciled {
		t.Fatalf("exact reconciliation replay must be idempotent: replay=%+v err=%v", replay, err)
	}
	if _, err = store.ReconcileModelInvocationSucceeded(
		ctx, "mingming", prepared.InvocationID, "sha256:different", "provider-request-1",
	); !errors.Is(err, k12storage.ErrModelInvocationConflict) {
		t.Fatalf("conflicting durable result must fail closed, got %v", err)
	}
}

func newGradingJobRecord(t *testing.T, agent, sourceKey string) *records.AgentRecord {
	t.Helper()
	rec, err := k12.NewGradingJobRecord(agent, "session-1", k12.GradingJobFields{
		SubmissionID: "submission-1", SourceKind: "test",
		IdempotencyKey:    k12.BuildGradingIdempotencyKey("test", sourceKey, 0),
		ConfirmationState: k12.GradingConfirmationPending, AnchorState: k12.GradingAnchorPending,
		ModelSnapshot: k12.GradingModelSnapshot{Provider: "provider-a", Model: "model-a", Route: "provider-a/model-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestModelInvocationLedgerConcurrentPrepareCreatesOneAttempt(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ModelInvocationsV15DDL); err != nil {
		t.Fatal(err)
	}
	job := newGradingJobRecord(t, "mingming", "concurrent-invocation")
	if _, err := store.Put(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	const workers = 100
	var created atomic.Int32
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stored, wasCreated, err := store.PrepareModelInvocation(context.Background(), k12.ModelInvocation{
				InvocationID: fmt.Sprintf("inv-concurrent-%03d", i), AgentName: "mingming", JobID: job.RecordID,
				Stage: k12.GradingStageRecognizing, RequestDigest: "sha256:stable",
				RouteSnapshot: k12.GradingModelSnapshot{Provider: "provider-a", Model: "model-a", Route: "provider-a/model-a"},
				Attempt:       1, CreatedAt: 300, UpdatedAt: 300,
			})
			if err != nil {
				errs <- err
				return
			}
			if wasCreated {
				created.Add(1)
			}
			ids <- stored.InvocationID
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatalf("concurrent prepare: %v", err)
	}
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	if created.Load() != 1 || len(unique) != 1 {
		t.Fatalf("created=%d invocation_ids=%v", created.Load(), unique)
	}
}
