package k12storage_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func expectedAssessmentEventID(item k12.GradingAssessmentItem, effectKind string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%s",
		item.JobID, item.ProblemID, item.ConfirmedVersion, effectKind)))
	return "k12-grading-" + hex.EncodeToString(sum[:])
}

func successfulAssessmentInvocations(t *testing.T, store *k12storage.Store, jobID string, attempt k12.Attempt) (string, string) {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, 2)
	for _, operation := range []k12.GradingItemOperation{k12.GradingItemOperationSolve, k12.GradingItemOperationGrade} {
		input := itemInvocation(jobID, attempt, operation, 1)
		input.InvocationID = fmt.Sprintf("item-inv-%s-%s", jobID, operation)
		invocation, _, err := store.PrepareGradingItemInvocation(ctx, input)
		if err != nil {
			t.Fatalf("prepare %s: %v", operation, err)
		}
		if _, err := store.MarkGradingItemInvocationSent(ctx, "mingming", invocation.InvocationID); err != nil {
			t.Fatalf("sent %s: %v", operation, err)
		}
		if _, err := store.MarkGradingItemInvocationSucceeded(ctx, "mingming", invocation.InvocationID,
			"sha256:"+string(operation)+"-result", `{"ok":true}`); err != nil {
			t.Fatalf("succeeded %s: %v", operation, err)
		}
		ids = append(ids, invocation.InvocationID)
	}
	return ids[0], ids[1]
}

func assessmentReceipt(jobID string, attempt k12.Attempt, solveID, gradeID string) k12.GradingAssessmentItem {
	return k12.GradingAssessmentItem{
		AgentName: "mingming", JobID: jobID, ProblemID: attempt.ProblemID, AttemptID: attempt.AttemptID,
		ConfirmedVersion: attempt.ConfirmedVersion, InputDigest: attempt.InputDigest,
		Status: k12.GradingAssessmentWrong, ResultJSON: `{"status":"wrong"}`,
		ResultDigest: "sha256:assessment-result", SolveInvocationID: solveID, GradeInvocationID: gradeID,
		ProjectionStatus: k12.GradingProjectionCommitted, CreatedAt: 200,
	}
}

func TestCommitGradingAssessmentItemAtomicallyWritesReceiptMistakeAndOutboxOnce(t *testing.T) {
	store, db := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "assessment-atomic")
	solveID, gradeID := successfulAssessmentInvocations(t, store, job.RecordID, attempt)
	receipt := assessmentReceipt(job.RecordID, attempt, solveID, gradeID)
	due := int64(86400)
	effects := k12storage.GradingAssessmentEffects{Mistake: &k12storage.GradingMistakeEffect{
		SourceSession: "session-1", DueAt: &due,
		Fields: k12.MistakeFields{Subject: "数学", Question: "1+1=?", KnowledgePoint: "加法",
			ErrorCause: "计算失误", CanonicalAnswer: "2", EntrySource: k12.MistakeEntryPhoto},
	}}
	stored, created, err := store.CommitGradingAssessmentItem(context.Background(), receipt, effects)
	if err != nil || !created || stored.ResultDigest != receipt.ResultDigest ||
		!stored.ProjectionCreated || stored.ProjectionRecordID == "" {
		t.Fatalf("commit stored=%+v created=%v err=%v", stored, created, err)
	}
	var mistakes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_mistakes WHERE agent_name='mingming'`).Scan(&mistakes); err != nil {
		t.Fatal(err)
	}
	events, err := k12storage.PendingEvents(context.Background(), db, 10)
	if err != nil || mistakes != 1 || len(events) != 2 {
		t.Fatalf("receipt effects not atomic: mistakes=%d events=%d err=%v", mistakes, len(events), err)
	}
	var mistakeID string
	if err := db.QueryRow(`SELECT record_id FROM k12_mistakes WHERE agent_name='mingming'`).Scan(&mistakeID); err != nil {
		t.Fatal(err)
	}
	if stored.ProjectionRecordID != mistakeID {
		t.Fatalf("receipt projection id=%q, want mistake %q", stored.ProjectionRecordID, mistakeID)
	}
	wantEventIDs := map[string]string{
		k12storage.EventGradingAssessmentCommitted: expectedAssessmentEventID(receipt, "assessment_committed"),
		k12storage.EventMistakeRecorded:            expectedAssessmentEventID(receipt, "mistake_recorded"),
	}
	for _, event := range events {
		if want := wantEventIDs[event.EventType]; event.EventID != want {
			t.Fatalf("event %s id=%q, want deterministic %q", event.EventType, event.EventID, want)
		}
		if event.EventType == k12storage.EventGradingAssessmentCommitted {
			var payload map[string]any
			if err := json.Unmarshal([]byte(event.Payload), &payload); err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"result_json", "solution", "model_output"} {
				if _, ok := payload[forbidden]; ok {
					t.Fatalf("assessment outbox leaked %s: %s", forbidden, event.Payload)
				}
			}
			if len(payload) != 6 {
				t.Fatalf("assessment outbox must remain metadata-only: %s", event.Payload)
			}
		}
		delete(wantEventIDs, event.EventType)
	}
	if len(wantEventIDs) != 0 {
		t.Fatalf("missing deterministic events: %v", wantEventIDs)
	}

	// Exact receipt replay must return before applying even a different proposed
	// effect, otherwise restart would duplicate a Mistake/Outbox side effect.
	restarted := k12storage.NewStore(db, nil)
	replayEffects := effects
	replayEffects.Mistake = &k12storage.GradingMistakeEffect{
		SourceSession: "session-1", Fields: k12.MistakeFields{Question: "must-not-be-written"},
	}
	replayed, created, err := restarted.CommitGradingAssessmentItem(context.Background(), receipt, replayEffects)
	if err != nil || created || replayed.ResultDigest != receipt.ResultDigest ||
		replayed.ProjectionRecordID != stored.ProjectionRecordID || !replayed.ProjectionCreated {
		t.Fatalf("replay stored=%+v created=%v err=%v", replayed, created, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_mistakes WHERE agent_name='mingming'`).Scan(&mistakes); err != nil {
		t.Fatal(err)
	}
	events, _ = k12storage.PendingEvents(context.Background(), db, 10)
	if mistakes != 1 || len(events) != 2 {
		t.Fatalf("replay duplicated effects: mistakes=%d events=%d", mistakes, len(events))
	}

	// A distinct Job may legitimately assess the same Attempt again. Mistake
	// dedupe must be reflected in that Job's own durable receipt, not recomputed
	// after restart.
	job2 := newGradingJobRecord(t, "mingming", "assessment-atomic-second-job")
	if _, err := store.Put(context.Background(), job2); err != nil {
		t.Fatal(err)
	}
	solve2, grade2 := successfulAssessmentInvocations(t, store, job2.RecordID, attempt)
	receipt2 := assessmentReceipt(job2.RecordID, attempt, solve2, grade2)
	stored2, created, err := store.CommitGradingAssessmentItem(context.Background(), receipt2, effects)
	if err != nil || !created || stored2.ProjectionCreated || stored2.ProjectionRecordID != mistakeID {
		t.Fatalf("deduped projection stored=%+v receipt_created=%v err=%v", stored2, created, err)
	}
	reloaded2, err := store.GetGradingAssessmentItem(context.Background(), "mingming", job2.RecordID, attempt.ProblemID)
	if err != nil || reloaded2.ProjectionCreated || reloaded2.ProjectionRecordID != mistakeID {
		t.Fatalf("reloaded deduped projection=%+v err=%v", reloaded2, err)
	}
}

func TestCommitGradingAssessmentReviewWritesDeterministicOutboxOnce(t *testing.T) {
	store, db := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "assessment-review")
	solveID, gradeID := successfulAssessmentInvocations(t, store, job.RecordID, attempt)
	receipt := assessmentReceipt(job.RecordID, attempt, solveID, gradeID)
	receipt.Status = k12.GradingAssessmentCorrect
	receipt.ResultJSON = `{"status":"correct"}`
	receipt.ResultDigest = "sha256:correct"

	existing := newMistake(t, "mingming", "session-1", "2+2=?")
	if _, err := store.Put(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM outbox_events`); err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParseMistakeFields(existing.Fields)
	if err != nil {
		t.Fatal(err)
	}
	effects := k12storage.GradingAssessmentEffects{Review: &k12storage.GradingReviewEffect{
		RecordID: existing.RecordID, ExpectedVersion: 0, NewStatus: k12.StatusRetried, Fields: fields,
	}}
	if _, created, err := store.CommitGradingAssessmentItem(context.Background(), receipt, effects); err != nil || !created {
		t.Fatalf("commit review receipt created=%v err=%v", created, err)
	}
	events, err := k12storage.PendingEvents(context.Background(), db, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("review assessment events=%d err=%v", len(events), err)
	}
	if events[0].EventType != k12storage.EventGradingAssessmentCommitted ||
		events[0].EventID != expectedAssessmentEventID(receipt, "assessment_committed") {
		t.Fatalf("review event=%+v", events[0])
	}

	restarted := k12storage.NewStore(db, nil)
	if _, created, err := restarted.CommitGradingAssessmentItem(context.Background(), receipt, effects); err != nil || created {
		t.Fatalf("restart replay created=%v err=%v", created, err)
	}
	events, _ = k12storage.PendingEvents(context.Background(), db, 10)
	if len(events) != 1 {
		t.Fatalf("restart replay duplicated review event: %d", len(events))
	}
	var status string
	var version int
	if err := db.QueryRow(`SELECT status,version FROM k12_mistakes WHERE record_id=?`, existing.RecordID).
		Scan(&status, &version); err != nil {
		t.Fatal(err)
	}
	if status != k12.StatusRetried || version != 1 {
		t.Fatalf("review side effect replayed or missing: status=%s version=%d", status, version)
	}
}

func TestCommitGradingAssessmentItemRejectsDigestStatusOwnerAndMutuallyExclusiveEffects(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "assessment-conflict")
	solveID, gradeID := successfulAssessmentInvocations(t, store, job.RecordID, attempt)
	receipt := assessmentReceipt(job.RecordID, attempt, solveID, gradeID)
	if _, _, err := store.CommitGradingAssessmentItem(context.Background(), receipt, k12storage.GradingAssessmentEffects{}); err != nil {
		t.Fatal(err)
	}

	changed := receipt
	changed.ResultDigest = "sha256:different"
	if _, _, err := store.CommitGradingAssessmentItem(context.Background(), changed, k12storage.GradingAssessmentEffects{}); !errors.Is(err, k12storage.ErrGradingAssessmentItemConflict) {
		t.Fatalf("same job/problem with different result digest must conflict, got %v", err)
	}
	if _, err := store.GetGradingAssessmentItem(context.Background(), "lele", job.RecordID, attempt.ProblemID); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("cross-owner receipt lookup must be not found, got %v", err)
	}

	invalidStatus := receipt
	invalidStatus.JobID = "other-job"
	invalidStatus.Status = k12.GradingAssessmentStatus("maybe")
	if _, _, err := store.CommitGradingAssessmentItem(context.Background(), invalidStatus, k12storage.GradingAssessmentEffects{}); err == nil {
		t.Fatal("unknown assessment status must be rejected")
	}
	both := k12storage.GradingAssessmentEffects{
		Mistake: &k12storage.GradingMistakeEffect{Fields: k12.MistakeFields{Question: "q"}},
		Review:  &k12storage.GradingReviewEffect{RecordID: "r"},
	}
	other := receipt
	other.JobID = "other-job"
	if _, _, err := store.CommitGradingAssessmentItem(context.Background(), other, both); err == nil {
		t.Fatal("Mistake and Review effects must be mutually exclusive")
	}
}

func TestCommitGradingAssessmentItemEffectFailureRollsBackReceiptAndCASMutation(t *testing.T) {
	store, db := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "assessment-rollback")
	solveID, gradeID := successfulAssessmentInvocations(t, store, job.RecordID, attempt)
	receipt := assessmentReceipt(job.RecordID, attempt, solveID, gradeID)

	existing := newMistake(t, "mingming", "session-1", "2+2=?")
	if _, err := store.Put(context.Background(), existing); err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParseMistakeFields(existing.Fields)
	if err != nil {
		t.Fatal(err)
	}
	effects := k12storage.GradingAssessmentEffects{Review: &k12storage.GradingReviewEffect{
		RecordID: existing.RecordID, ExpectedVersion: 99, NewStatus: k12.StatusRetried,
		Fields: fields,
	}}
	if _, _, err := store.CommitGradingAssessmentItem(context.Background(), receipt, effects); !errors.Is(err, records.ErrVersionConflict) {
		t.Fatalf("stale review CAS must fail, got %v", err)
	}
	if _, err := store.GetGradingAssessmentItem(context.Background(), "mingming", job.RecordID, attempt.ProblemID); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("failed effect must roll back receipt, got %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM k12_mistakes WHERE record_id=?`, existing.RecordID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 0 {
		t.Fatalf("failed effect mutated review version=%d", version)
	}
	var assessmentEvents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM outbox_events WHERE event_type=?`,
		k12storage.EventGradingAssessmentCommitted).Scan(&assessmentEvents); err != nil {
		t.Fatal(err)
	}
	if assessmentEvents != 0 {
		t.Fatalf("failed effect must roll back deterministic outbox, events=%d", assessmentEvents)
	}
}

func TestCommitGradingAssessmentItemTerminalStatusControlsInvocationReferencesWithoutFakeCalls(t *testing.T) {
	store, _ := setup(t)

	// Grade-mode blank/unclear facts are terminal item receipts but intentionally
	// make zero model calls. They still count toward the Job exact-set.
	job, attempt := seedItemLedgerFacts(t, store, "assessment-unanswered")
	unanswered := assessmentReceipt(job.RecordID, attempt, "", "")
	unanswered.Status = k12.GradingAssessmentUnanswered
	unanswered.ResultJSON = `{"status":"unanswered"}`
	unanswered.ResultDigest = "sha256:unanswered"
	if _, created, err := store.CommitGradingAssessmentItem(context.Background(), unanswered, k12storage.GradingAssessmentEffects{}); err != nil || !created {
		t.Fatalf("zero-call unanswered receipt created=%v err=%v", created, err)
	}

	// Solve-mode blank_solved has exactly one solve operation and no grade call.
	job2, attempt2 := seedItemLedgerFacts(t, store, "assessment-blank-solved")
	solve, _, err := store.PrepareGradingItemInvocation(context.Background(),
		itemInvocation(job2.RecordID, attempt2, k12.GradingItemOperationSolve, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkGradingItemInvocationSent(context.Background(), "mingming", solve.InvocationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkGradingItemInvocationSucceeded(context.Background(), "mingming", solve.InvocationID,
		"sha256:solved", `{"solution":"2"}`); err != nil {
		t.Fatal(err)
	}
	solved := assessmentReceipt(job2.RecordID, attempt2, solve.InvocationID, "")
	solved.Status = k12.GradingAssessmentBlankSolved
	solved.ResultJSON = `{"status":"blank_solved"}`
	solved.ResultDigest = "sha256:blank-solved"
	if _, created, err := store.CommitGradingAssessmentItem(context.Background(), solved, k12storage.GradingAssessmentEffects{}); err != nil || !created {
		t.Fatalf("solve-only receipt created=%v err=%v", created, err)
	}

	invalid := solved
	invalid.JobID = "another-job"
	invalid.Status = k12.GradingAssessmentCorrect
	invalid.GradeInvocationID = ""
	if _, _, err := store.CommitGradingAssessmentItem(context.Background(), invalid, k12storage.GradingAssessmentEffects{}); err == nil {
		t.Fatal("correct receipt without solve+grade refs must be rejected")
	}
}

func TestCommitGradingAssessmentItemRejectsAttemptVersionAndInputDigestDrift(t *testing.T) {
	store, _ := setup(t)
	job, attempt := seedItemLedgerFacts(t, store, "assessment-input-binding")
	solveID, gradeID := successfulAssessmentInvocations(t, store, job.RecordID, attempt)

	for _, mutate := range []func(*k12.GradingAssessmentItem){
		func(item *k12.GradingAssessmentItem) { item.ConfirmedVersion++ },
		func(item *k12.GradingAssessmentItem) { item.InputDigest = "sha256:stale-or-other-input" },
	} {
		receipt := assessmentReceipt(job.RecordID, attempt, solveID, gradeID)
		mutate(&receipt)
		if _, _, err := store.CommitGradingAssessmentItem(context.Background(), receipt,
			k12storage.GradingAssessmentEffects{}); !errors.Is(err, k12storage.ErrGradingAssessmentItemConflict) {
			t.Fatalf("receipt drift must fail closed, got %v", err)
		}
	}
}
