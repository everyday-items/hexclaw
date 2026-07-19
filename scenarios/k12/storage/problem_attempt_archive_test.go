package k12storage_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestProblemAttemptArchive_DeterministicExportIdempotentImportAndExactReplace(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()

	late := scopeProblemAttemptIDs(problemAttemptFixture("mingming", "submission-b"), "b-")
	early := scopeProblemAttemptIDs(problemAttemptFixture("mingming", "submission-a"), "a-")
	for _, snapshot := range []k12.ProblemAttemptSnapshot{late, early} {
		if err := store.PutProblemAttemptSnapshot(ctx, snapshot); err != nil {
			t.Fatalf("seed %s: %v", snapshot.Problems[0].SubmissionID, err)
		}
	}

	exported, err := store.ExportProblemAttemptSnapshots(ctx, "mingming")
	if err != nil {
		t.Fatal(err)
	}
	if len(exported) != 2 || exported[0].Problems[0].SubmissionID != "submission-a" ||
		exported[1].Problems[0].SubmissionID != "submission-b" {
		t.Fatalf("export must be deterministic by submission: %+v", exported)
	}
	if err := k12storage.ValidateProblemAttemptArchive("mingming", exported); err != nil {
		t.Fatalf("exported archive must validate: %v", err)
	}

	target := rewriteProblemAttemptOwner(exported, "lele")
	if err := store.ImportProblemAttemptSnapshots(ctx, "lele", target); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if err := store.ImportProblemAttemptSnapshots(ctx, "lele", target); err != nil {
		t.Fatalf("same-fact retry must be idempotent: %v", err)
	}
	got, err := store.ExportProblemAttemptSnapshots(ctx, "lele")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, target) {
		t.Fatalf("round trip changed facts:\n got=%+v\nwant=%+v", got, target)
	}

	conflict := rewriteProblemAttemptOwner(exported, "lele")
	conflict[0].Problems[1].StemRaw = "不可覆盖的另一份 OCR 原文"
	if err := store.ImportProblemAttemptSnapshots(ctx, "lele", conflict); !errors.Is(err, k12storage.ErrProblemAttemptConflict) {
		t.Fatalf("same ID with different immutable fact must fail closed, got %v", err)
	}
	afterConflict, err := store.ExportProblemAttemptSnapshots(ctx, "lele")
	if err != nil || !reflect.DeepEqual(afterConflict, target) {
		t.Fatalf("conflict leaked partial writes: err=%v got=%+v", err, afterConflict)
	}

	keepOnlyB := []k12.ProblemAttemptSnapshot{target[1]}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceProblemAttemptSnapshotsTx(ctx, tx, "lele", keepOnlyB); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	afterReplace, err := store.ExportProblemAttemptSnapshots(ctx, "lele")
	if err != nil || !reflect.DeepEqual(afterReplace, keepOnlyB) {
		t.Fatalf("exact replace failed: err=%v got=%+v", err, afterReplace)
	}
}

func TestProblemAttemptArchive_RejectsCrossSnapshotDuplicateAndMissingDurableTime(t *testing.T) {
	snapshot := scopeProblemAttemptIDs(problemAttemptFixture("mingming", "submission-a"), "a-")
	duplicate := scopeProblemAttemptIDs(problemAttemptFixture("mingming", "submission-b"), "b-")
	duplicate.Problems[1].ProblemID = snapshot.Problems[1].ProblemID
	duplicate.Attempts[0].ProblemID = snapshot.Problems[1].ProblemID
	if err := k12storage.ValidateProblemAttemptArchive(
		"mingming", []k12.ProblemAttemptSnapshot{snapshot, duplicate},
	); err == nil {
		t.Fatal("cross-submission duplicate problem_id must be rejected")
	}

	missingTime := problemAttemptFixture("mingming", "submission-a")
	missingTime.Attempts[0].CreatedAt = 0
	if err := k12storage.ValidateProblemAttemptArchive(
		"mingming", []k12.ProblemAttemptSnapshot{missingTime},
	); err == nil {
		t.Fatal("archive facts without durable timestamps must be rejected")
	}
}

func rewriteProblemAttemptOwner(
	snapshots []k12.ProblemAttemptSnapshot,
	target string,
) []k12.ProblemAttemptSnapshot {
	out := make([]k12.ProblemAttemptSnapshot, len(snapshots))
	for i, snapshot := range snapshots {
		out[i].Problems = append([]k12.Problem(nil), snapshot.Problems...)
		out[i].Attempts = append([]k12.Attempt(nil), snapshot.Attempts...)
		for j := range out[i].Problems {
			out[i].Problems[j].AgentName = target
		}
		for j := range out[i].Attempts {
			out[i].Attempts[j].AgentName = target
		}
	}
	return out
}

func scopeProblemAttemptIDs(snapshot k12.ProblemAttemptSnapshot, prefix string) k12.ProblemAttemptSnapshot {
	problemIDs := make(map[string]string, len(snapshot.Problems))
	for i := range snapshot.Problems {
		old := snapshot.Problems[i].ProblemID
		problemIDs[old] = prefix + old
		snapshot.Problems[i].ProblemID = prefix + old
	}
	for i := range snapshot.Problems {
		if snapshot.Problems[i].ParentProblemID != "" {
			snapshot.Problems[i].ParentProblemID = problemIDs[snapshot.Problems[i].ParentProblemID]
		}
	}
	for i := range snapshot.Attempts {
		snapshot.Attempts[i].AttemptID = prefix + snapshot.Attempts[i].AttemptID
		snapshot.Attempts[i].ProblemID = problemIDs[snapshot.Attempts[i].ProblemID]
	}
	return snapshot
}
