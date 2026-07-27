package k12storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func seedBUG20260725Mistake(t *testing.T, store *k12storage.Store, recordID string) {
	t.Helper()
	_, err := store.DB().Exec(`INSERT INTO k12_mistakes
		(record_id,agent_name,status,subject,question,knowledge_point,canonical_answer,
		 dedupe_key,tags_json,due_at,version,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		recordID, "mingming", k12.StatusNew, "数学", "2 + 3 = ?", "整数加法", "5",
		"bug-20260725:"+recordID, "[]", int64(1_700_000_000), 0, int64(1_700_000_000), int64(1_700_000_000))
	if err != nil {
		t.Fatal(err)
	}
}

func TestBUG20260725011CandidateCommitIsAtomicAndHashIdempotent(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	seedBUG20260725Mistake(t, store, "mistake-candidate")

	selection, _, err := store.OpenPracticeCandidateSelection(ctx, k12storage.PracticeCandidateOpenInput{
		AgentName: "mingming", SourceMistakeID: "mistake-candidate",
		IdempotencyKey: "open-candidate", SourceSession: "session-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Candidates) != 1 ||
		selection.Candidates[0].CandidateKind != k12.PracticeCandidateOriginal {
		t.Fatalf("selection=%+v, want one original candidate", selection)
	}

	reserved, replayed, err := store.ReservePracticeCandidateBatch(ctx,
		"mingming", selection.SelectionID, selection.Revision, "batch-1", 2)
	if err != nil || replayed || len(reserved) != 2 {
		t.Fatalf("reserve=%+v replayed=%v err=%v", reserved, replayed, err)
	}
	for i, candidate := range reserved {
		problem := k12.PracticeCandidateProblem{
			Subject: "数学", QuestionMarkdown: []string{"4 + 5 = ?", "7 + 8 = ?"}[i],
			ExpectedAnswerMarkdown: []string{"9", "15"}[i],
		}
		if _, err := store.CompletePracticeCandidate(ctx, "mingming",
			candidate.CandidateID, problem, ""); err != nil {
			t.Fatal(err)
		}
	}
	current, err := store.GetPracticeCandidateSelection(ctx, "mingming", selection.SelectionID)
	if err != nil {
		t.Fatal(err)
	}
	selected := []string{
		current.Candidates[0].CandidateID,
		current.Candidates[1].CandidateID,
		current.Candidates[2].CandidateID,
	}
	receipt, err := store.CommitPracticeCandidateSelection(ctx, k12storage.PracticeCandidateCommitInput{
		AgentName: "mingming", SelectionID: current.SelectionID, Revision: current.Revision,
		CandidateIDs: selected, IdempotencyKey: "commit-candidate",
	})
	if err != nil || receipt.AddedCount != 3 || receipt.Replayed {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	replay, err := store.CommitPracticeCandidateSelection(ctx, k12storage.PracticeCandidateCommitInput{
		AgentName: "mingming", SelectionID: current.SelectionID, Revision: current.Revision,
		CandidateIDs: selected, IdempotencyKey: "commit-candidate",
	})
	if err != nil || !replay.Replayed || replay.AddedCount != receipt.AddedCount {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	var total, distinct int
	if err := db.QueryRow(`SELECT COUNT(*),COUNT(DISTINCT normalized_content_hash)
		FROM k12_practice_set_items WHERE set_record_id=?`,
		selection.TargetSetRecordID).Scan(&total, &distinct); err != nil {
		t.Fatal(err)
	}
	if total != 3 || distinct != 3 {
		t.Fatalf("items total=%d distinct=%d, want 3/3", total, distinct)
	}
}

func TestBUG20260725013DeferDoesNotChangeMistakeMastery(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	seedBUG20260725Mistake(t, store, "mistake-defer")

	result, err := store.ApplyMistakeReviewCommand(ctx, k12storage.MistakeReviewCommandInput{
		AgentName: "mingming", MistakeRecordID: "mistake-defer",
		ExpectedVersion: 0, IdempotencyKey: "defer-1",
		CommandType: k12.MistakeReviewCommandDeferThisWeek,
		ISOYear: 2026, ISOWeek: 31,
	})
	if err != nil || result.State != k12.MistakeReviewDeferredThisWeek {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var status string
	var parentConfirmed, lastRetried int64
	if err := db.QueryRow(`SELECT status,parent_confirmed_at,last_retried_at
		FROM k12_mistakes WHERE record_id='mistake-defer'`).
		Scan(&status, &parentConfirmed, &lastRetried); err != nil {
		t.Fatal(err)
	}
	if status == k12.StatusMastered || parentConfirmed != 0 || lastRetried != 0 {
		t.Fatalf("defer forged mastery: status=%s parent=%d retried=%d",
			status, parentConfirmed, lastRetried)
	}
}

func TestBUG20260725017SuppressRestoreReplaysOnePriorSchedule(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	seedBUG20260725Mistake(t, store, "mistake-suppress")

	suppressed, err := store.ApplyMistakeReviewCommand(ctx, k12storage.MistakeReviewCommandInput{
		AgentName: "mingming", MistakeRecordID: "mistake-suppress",
		ExpectedVersion: 0, IdempotencyKey: "suppress-1",
		CommandType: k12.MistakeReviewCommandSuppress,
	})
	if err != nil || suppressed.State != k12.MistakeReviewSuppressed {
		t.Fatalf("suppressed=%+v err=%v", suppressed, err)
	}
	replay, err := store.ApplyMistakeReviewCommand(ctx, k12storage.MistakeReviewCommandInput{
		AgentName: "mingming", MistakeRecordID: "mistake-suppress",
		ExpectedVersion: 0, IdempotencyKey: "suppress-1",
		CommandType: k12.MistakeReviewCommandSuppress,
	})
	if err != nil || !replay.Replayed {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	restored, err := store.ApplyMistakeReviewCommand(ctx, k12storage.MistakeReviewCommandInput{
		AgentName: "mingming", MistakeRecordID: "mistake-suppress",
		ExpectedVersion: suppressed.MistakeVersion, IdempotencyKey: "restore-1",
		CommandType: k12.MistakeReviewCommandRestore,
	})
	if err != nil || restored.State != k12.MistakeReviewScheduled {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	_, err = store.ApplyMistakeReviewCommand(ctx, k12storage.MistakeReviewCommandInput{
		AgentName: "mingming", MistakeRecordID: "mistake-suppress",
		ExpectedVersion: restored.MistakeVersion, IdempotencyKey: "restore-2",
		CommandType: k12.MistakeReviewCommandRestore,
	})
	if !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("second restore err=%v, want illegal transition", err)
	}
}
