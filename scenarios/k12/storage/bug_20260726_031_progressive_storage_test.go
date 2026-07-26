package k12storage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func bug031SuccessfulInvocationsForRevision(
	t *testing.T,
	store *k12storage.Store,
	jobID string,
	attempt k12.Attempt,
	revision int,
) (string, string) {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, 2)
	for _, operation := range []k12.GradingItemOperation{
		k12.GradingItemOperationSolve,
		k12.GradingItemOperationGrade,
	} {
		input := itemInvocation(jobID, attempt, operation, revision)
		input.InvocationID = fmt.Sprintf(
			"bug-20260726-031-%s-r%d",
			operation,
			revision,
		)
		input.RequestDigest = fmt.Sprintf(
			"sha256:bug-20260726-031-%s-r%d",
			operation,
			revision,
		)
		invocation, _, err := store.PrepareGradingItemInvocation(ctx, input)
		if err != nil {
			t.Fatalf("prepare %s revision %d: %v", operation, revision, err)
		}
		if _, err := store.MarkGradingItemInvocationSent(
			ctx,
			"mingming",
			invocation.InvocationID,
		); err != nil {
			t.Fatalf("mark %s sent: %v", operation, err)
		}
		resultDigest := fmt.Sprintf(
			"sha256:bug-20260726-031-result-%s-r%d",
			operation,
			revision,
		)
		if _, err := store.MarkGradingItemInvocationSucceeded(
			ctx,
			"mingming",
			invocation.InvocationID,
			resultDigest,
			fmt.Sprintf(`{"operation":%q,"revision":%d}`, operation, revision),
		); err != nil {
			t.Fatalf("mark %s succeeded: %v", operation, err)
		}
		ids = append(ids, invocation.InvocationID)
	}
	return ids[0], ids[1]
}

func TestBUG_20260726_031_AssessmentCurrentDispositionHasOneCASWinner(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "bug-20260726-031-cas")
	solveID, gradeID := successfulAssessmentInvocations(t, store, job.RecordID, attempt)
	receipt := assessmentReceipt(job.RecordID, attempt, solveID, gradeID)

	const workers = 100
	var created atomic.Int32
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, wasCreated, err := store.CommitGradingAssessmentItem(
				context.Background(),
				receipt,
				k12storage.GradingAssessmentEffects{},
			)
			if err != nil {
				errs <- err
				return
			}
			if wasCreated {
				created.Add(1)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("BUG_20260726_031 concurrent commit: %v", err)
	}
	if got := created.Load(); got != 1 {
		t.Fatalf("BUG_20260726_031 CAS winners=%d, want exactly 1", got)
	}

	current, err := store.GetGradingAssessmentItem(
		context.Background(),
		"mingming",
		job.RecordID,
		attempt.ProblemID,
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	var projected map[string]any
	if err := json.Unmarshal(raw, &projected); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"input_revision",
		"published_revision",
		"current_disposition",
		"structure_version",
	} {
		if _, ok := projected[key]; !ok {
			t.Errorf(
				"BUG_20260726_031 committed receipt lacks %s: %s",
				key,
				raw,
			)
		}
	}
}

func TestBUG_20260726_031_NewerInputRevisionSupersedesOldCurrentReceipt(t *testing.T) {
	store, _ := setup(t)
	job, attemptV1 := seedItemLedgerFacts(t, store, "bug-20260726-031-revision")
	solveV1, gradeV1 := successfulAssessmentInvocations(t, store, job.RecordID, attemptV1)
	receiptV1 := assessmentReceipt(job.RecordID, attemptV1, solveV1, gradeV1)
	if _, created, err := store.CommitGradingAssessmentItem(
		context.Background(),
		receiptV1,
		k12storage.GradingAssessmentEffects{},
	); err != nil || !created {
		t.Fatalf("commit revision 1 created=%v err=%v", created, err)
	}

	snapshotV2 := problemAttemptFixture("mingming", "submission-1")
	for i := range snapshotV2.Attempts {
		snapshotV2.Attempts[i].ConfirmedVersion = 2
		snapshotV2.Attempts[i].InputDigest =
			"sha256:bug-20260726-031-input-" +
				snapshotV2.Attempts[i].ProblemID +
				"-v2"
	}
	if err := store.PutProblemAttemptSnapshot(context.Background(), snapshotV2); err != nil {
		t.Fatalf("persist revision 2 input: %v", err)
	}
	attemptV2 := snapshotV2.Attempts[0]
	solveV2, gradeV2 := bug031SuccessfulInvocationsForRevision(
		t,
		store,
		job.RecordID,
		attemptV2,
		2,
	)
	receiptV2 := assessmentReceipt(job.RecordID, attemptV2, solveV2, gradeV2)
	receiptV2.ResultDigest = "sha256:bug-20260726-031-assessment-v2"

	currentV2, created, err := store.CommitGradingAssessmentItem(
		context.Background(),
		receiptV2,
		k12storage.GradingAssessmentEffects{},
	)
	if err != nil {
		t.Fatalf(
			"BUG_20260726_031 newer input revision must atomically supersede old current: %v",
			err,
		)
	}
	if !created || currentV2.ConfirmedVersion != 2 {
		t.Fatalf(
			"BUG_20260726_031 revision 2 stored=%+v created=%v",
			currentV2,
			created,
		)
	}

	reloaded, err := store.GetGradingAssessmentItem(
		context.Background(),
		"mingming",
		job.RecordID,
		attemptV2.ProblemID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ConfirmedVersion != 2 ||
		reloaded.ResultDigest != receiptV2.ResultDigest {
		t.Fatalf(
			"BUG_20260726_031 current receipt=%+v, want revision 2",
			reloaded,
		)
	}
}

func TestBUG_20260726_031_ProblemSkipReceiptIsDurableAndRevisionScoped(t *testing.T) {
	_, db := setup(t)
	var tableCount int
	if err := db.QueryRow(`
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type='table' AND name='k12_problem_skip_receipts'
	`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 1 {
		t.Fatalf(
			"BUG_20260726_031 missing durable k12_problem_skip_receipts table",
		)
	}

	rows, err := db.Query(`PRAGMA table_info(k12_problem_skip_receipts)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(
			&cid,
			&name,
			&dataType,
			&notNull,
			&defaultValue,
			&primaryKey,
		); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	for _, required := range []string{
		"agent_name",
		"job_id",
		"problem_id",
		"structure_version",
		"input_revision",
		"skip_receipt_id",
		"result_digest",
		"current_disposition",
		"published_revision",
		"superseded_at",
	} {
		if !columns[required] {
			t.Errorf(
				"BUG_20260726_031 skip receipt column %q missing; got=%v",
				required,
				columns,
			)
		}
	}
}
