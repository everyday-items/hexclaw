package k12storage_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestGrading20260726_ConcurrentRecoveryClaimsPreparedCallOnce(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "physical-concurrent-claim")
	prepared, _, err := store.PrepareGradingItemInvocation(
		context.Background(),
		itemInvocation(job.RecordID, attempt, k12.GradingItemOperationSolveGenerate, 1001),
	)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 32
	var claimed atomic.Int32
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, won, claimErr := store.ClaimGradingItemInvocationSent(
				context.Background(), "mingming", prepared.InvocationID,
			)
			if claimErr != nil {
				errs <- claimErr
				return
			}
			if won {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent claim: %v", err)
	}
	if got := claimed.Load(); got != 1 {
		t.Fatalf("provider send owners=%d, want exactly one", got)
	}
}

func TestGrading20260726_CommitFailureParksSentCallAndCannotReclaim(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "physical-commit-failure")
	prepared, _, err := store.PrepareGradingItemInvocation(
		context.Background(),
		itemInvocation(job.RecordID, attempt, k12.GradingItemOperationGrade, 1001),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed, err := store.ClaimGradingItemInvocationSent(
		context.Background(), "mingming", prepared.InvocationID,
	); err != nil || !claimed {
		t.Fatalf("initial claim=%v err=%v", claimed, err)
	}
	if _, err := store.DB().Exec(`
CREATE TRIGGER grading_20260726_fail_success
BEFORE UPDATE OF status ON k12_grading_item_invocations
WHEN NEW.status='succeeded'
BEGIN
    SELECT RAISE(ABORT, 'forced success commit failure');
END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkGradingItemInvocationSucceeded(
		context.Background(), "mingming", prepared.InvocationID,
		"sha256:result", `{"verdict":"agree"}`,
	); err == nil {
		t.Fatal("forced success commit failure was not observed")
	}
	unknown, err := store.MarkGradingItemInvocationOutcomeUnknown(
		context.Background(), "mingming", prepared.InvocationID,
		"local", "result_not_durable",
	)
	if err != nil || unknown.Status != k12.ModelInvocationOutcomeUnknown {
		t.Fatalf("commit failure ledger=%+v err=%v", unknown, err)
	}
	if _, claimed, err := store.ClaimGradingItemInvocationSent(
		context.Background(), "mingming", prepared.InvocationID,
	); err != nil || claimed {
		t.Fatalf("unknown call reclaimed=%v err=%v", claimed, err)
	}
}

func TestGrading20260726_CostReceiptIsUniqueAndIdempotentPerInvocation(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "physical-cost-dedupe")
	var receipts []string
	for i, operation := range []k12.GradingItemOperation{
		k12.GradingItemOperationSolveGenerate,
		k12.GradingItemOperationSolveVerify,
	} {
		in := itemInvocation(job.RecordID, attempt, operation, 1001+i)
		prepared, _, err := store.PrepareGradingItemInvocation(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if _, claimed, err := store.ClaimGradingItemInvocationSent(
			context.Background(), "mingming", prepared.InvocationID,
		); err != nil || !claimed {
			t.Fatalf("claim %s=%v err=%v", operation, claimed, err)
		}
		succeeded, err := store.MarkGradingItemInvocationSucceeded(
			context.Background(), "mingming", prepared.InvocationID,
			"sha256:result", `{"result":"ok"}`,
		)
		if err != nil || succeeded.CostReceiptID == "" {
			t.Fatalf("succeeded=%+v err=%v", succeeded, err)
		}
		replayed, err := store.MarkGradingItemInvocationSucceeded(
			context.Background(), "mingming", prepared.InvocationID,
			"sha256:result", `{"result":"ok"}`,
		)
		if err != nil || replayed.CostReceiptID != succeeded.CostReceiptID {
			t.Fatalf("receipt replay=%+v err=%v", replayed, err)
		}
		receipts = append(receipts, succeeded.CostReceiptID)
	}
	if receipts[0] == receipts[1] {
		t.Fatalf("two invocations share cost receipt %q", receipts[0])
	}
	if _, err := store.DB().Exec(
		`UPDATE k12_grading_item_invocations SET cost_receipt_id=? WHERE item_invocation_id=?`,
		receipts[0],
		"item-inv-"+string(k12.GradingItemOperationSolveVerify)+"-1002",
	); err == nil {
		t.Fatal("database accepted duplicate cost receipt")
	}
	var count int
	if err := store.DB().QueryRow(
		`SELECT COUNT(*) FROM k12_grading_item_invocations WHERE cost_receipt_id != ''`,
	).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("cost receipt rows=%d, want one per invocation", count)
	}
}

var _ = errors.Is
