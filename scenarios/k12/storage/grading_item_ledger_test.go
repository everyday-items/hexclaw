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
)

func seedItemLedgerFacts(t *testing.T, store *k12storage.Store, sourceKey string) (*records.AgentRecord, k12.Attempt) {
	t.Helper()
	ctx := context.Background()
	snapshot := problemAttemptFixture("mingming", "submission-1")
	for i := range snapshot.Attempts {
		snapshot.Attempts[i].ConfirmedVersion = 1
		snapshot.Attempts[i].InputDigest = "sha256:input-" + snapshot.Attempts[i].ProblemID + "-v1"
	}
	if err := store.PutProblemAttemptSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("seed problem/attempt: %v", err)
	}
	job := newGradingJobRecord(t, "mingming", sourceKey)
	if _, err := store.Put(ctx, job); err != nil {
		t.Fatalf("seed grading job: %v", err)
	}
	return job, snapshot.Attempts[0]
}

func itemInvocation(jobID string, attempt k12.Attempt, operation k12.GradingItemOperation, n int) k12.GradingItemInvocation {
	return k12.GradingItemInvocation{
		InvocationID: fmt.Sprintf("item-inv-%s-%d", operation, n),
		AgentName:    "mingming", JobID: jobID, ProblemID: attempt.ProblemID, AttemptID: attempt.AttemptID,
		Operation: operation, OperationAttempt: n, RequestDigest: "sha256:req-" + string(operation),
		InputRevision: attempt.ConfirmedVersion, InputDigest: attempt.InputDigest,
		RouteSnapshot: k12.GradingModelSnapshot{Provider: "proxy", Model: "gpt", Route: "proxy/gpt"},
		CreatedAt:     100,
	}
}

func TestGradingItemInvocationPrepareRejectsDigestRouteOperationAndOwnerDrift(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "item-identity")
	want := itemInvocation(job.RecordID, attempt, k12.GradingItemOperationSolve, 1)
	first, created, err := store.PrepareGradingItemInvocation(context.Background(), want)
	if err != nil || !created {
		t.Fatalf("prepare created=%v err=%v", created, err)
	}
	replay, created, err := store.PrepareGradingItemInvocation(context.Background(), want)
	if err != nil || created || replay.InvocationID != first.InvocationID {
		t.Fatalf("exact replay invocation=%+v created=%v err=%v", replay, created, err)
	}
	var storedInputRevision int
	var storedInputDigest string
	if err := store.DB().QueryRow(`SELECT input_revision,input_digest
		FROM k12_grading_item_invocations WHERE item_invocation_id=?`, first.InvocationID).
		Scan(&storedInputRevision, &storedInputDigest); err != nil {
		t.Fatalf("grading item invocation must persist input revision/digest: %v", err)
	}
	wantInputDigest := "sha256:input-" + attempt.ProblemID + "-v1"
	if storedInputRevision != 1 || storedInputDigest != wantInputDigest {
		t.Fatalf("persisted input binding=(%d,%q), want (1,%q)",
			storedInputRevision, storedInputDigest, wantInputDigest)
	}

	changedDigest := want
	changedDigest.InvocationID = "item-inv-digest-drift"
	changedDigest.RequestDigest = "sha256:different"
	if _, _, err := store.PrepareGradingItemInvocation(context.Background(), changedDigest); !errors.Is(err, k12storage.ErrGradingItemInvocationConflict) {
		t.Fatalf("same stable key with changed digest must fail closed, got %v", err)
	}
	changedRoute := want
	changedRoute.InvocationID = "item-inv-route-drift"
	changedRoute.RouteSnapshot = k12.GradingModelSnapshot{Provider: "other", Model: "gpt", Route: "other/gpt"}
	if _, _, err := store.PrepareGradingItemInvocation(context.Background(), changedRoute); !errors.Is(err, k12storage.ErrGradingItemInvocationConflict) {
		t.Fatalf("same stable key with changed route must fail closed, got %v", err)
	}
	changedRoutePolicy := want
	changedRoutePolicy.InvocationID = "item-inv-route-policy-drift"
	changedRoutePolicy.RouteSnapshot.TimeoutMS = 90000
	if _, _, err := store.PrepareGradingItemInvocation(context.Background(), changedRoutePolicy); !errors.Is(err, k12storage.ErrGradingItemInvocationConflict) {
		t.Fatalf("same stable key with changed route policy must fail closed, got %v", err)
	}
	invalidOperation := want
	invalidOperation.Operation = k12.GradingItemOperation("render")
	if _, _, err := store.PrepareGradingItemInvocation(context.Background(), invalidOperation); err == nil {
		t.Fatal("unknown item operation must be rejected")
	}
	if _, err := store.GetGradingItemInvocation(context.Background(), "lele", first.InvocationID); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("cross-owner lookup must be not found, got %v", err)
	}
}

func TestGradingItemInvocationRejectsProblemAttemptFromAnotherSubmission(t *testing.T) {
	store, _ := setup(t)
	_, attempt := seedItemLedgerFacts(t, store, "item-cross-submission-source")
	foreignJob, err := k12.NewGradingJobRecord("mingming", "session-2", k12.GradingJobFields{
		SubmissionID: "submission-2", SourceKind: "test",
		IdempotencyKey:    k12.BuildGradingIdempotencyKey("test", "item-cross-submission-job", 0),
		ConfirmationState: k12.GradingConfirmationPending, AnchorState: k12.GradingAnchorPending,
		ModelSnapshot: k12.GradingModelSnapshot{Provider: "provider-a", Model: "model-a", Route: "provider-a/model-a"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), foreignJob); err != nil {
		t.Fatal(err)
	}

	_, _, err = store.PrepareGradingItemInvocation(context.Background(),
		itemInvocation(foreignJob.RecordID, attempt, k12.GradingItemOperationSolve, 1))
	if !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("cross-submission problem/attempt must not bind to job: %v", err)
	}
}

func TestGradingItemInvocationConcurrentPrepareCreatesOneStableOperationAttempt(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "item-concurrent")
	const workers = 64
	var created atomic.Int32
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := itemInvocation(job.RecordID, attempt, k12.GradingItemOperationSolve, 1)
			in.InvocationID = fmt.Sprintf("item-inv-concurrent-%d", i)
			stored, wasCreated, err := store.PrepareGradingItemInvocation(context.Background(), in)
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

func TestGradingItemInvocationTransitionsAreDurableAndResultImmutable(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "item-transition")
	prepared, _, err := store.PrepareGradingItemInvocation(context.Background(),
		itemInvocation(job.RecordID, attempt, k12.GradingItemOperationSolve, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkGradingItemInvocationSent(context.Background(), "mingming", prepared.InvocationID); err != nil {
		t.Fatal(err)
	}
	succeeded, err := store.MarkGradingItemInvocationSucceeded(context.Background(), "mingming",
		prepared.InvocationID, "sha256:result", `{"solution":"2"}`)
	if err != nil || succeeded.Status != k12.ModelInvocationSucceeded || succeeded.ResultJSON == "" {
		t.Fatalf("succeeded=%+v err=%v", succeeded, err)
	}
	if _, err := store.MarkGradingItemInvocationSucceeded(context.Background(), "mingming",
		prepared.InvocationID, "sha256:different", `{"solution":"3"}`); !errors.Is(err, k12storage.ErrGradingItemInvocationConflict) {
		t.Fatalf("successful result rewrite must fail closed, got %v", err)
	}
	if _, err := store.MarkGradingItemInvocationOutcomeUnknown(context.Background(), "mingming",
		prepared.InvocationID, "provider", "timeout"); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("succeeded -> unknown must be illegal, got %v", err)
	}

	unknownIn := itemInvocation(job.RecordID, attempt, k12.GradingItemOperationGrade, 1)
	unknown, _, err := store.PrepareGradingItemInvocation(context.Background(), unknownIn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkGradingItemInvocationSent(context.Background(), "mingming", unknown.InvocationID); err != nil {
		t.Fatal(err)
	}
	unknown, err = store.MarkGradingItemInvocationOutcomeUnknown(context.Background(), "mingming",
		unknown.InvocationID, "provider", "timeout")
	if err != nil || unknown.Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	listed, err := store.ListGradingItemInvocations(context.Background(), "mingming", job.RecordID)
	if err != nil || len(listed) != 2 {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
}
