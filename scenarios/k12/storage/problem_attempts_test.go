package k12storage_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

func confidence(value float64) *float64 { return &value }

func problemAttemptFixture(agent, submission string) k12.ProblemAttemptSnapshot {
	return k12.ProblemAttemptSnapshot{
		Problems: []k12.Problem{
			{
				ProblemID: "parent-1", AgentName: agent, SubmissionID: submission,
				PageAssetID: "page-1", Ordinal: 0, ProblemKind: k12.ProblemKindCompoundParent,
				Subject: "数学", StemRaw: "根据统计图回答下列问题", StemMarkdown: "根据统计图回答下列问题",
				ConceptIDs: []string{"统计图"}, TranscriptionConfidence: confidence(0.98),
				CanonicalVersion: 1, CreatedAt: 100, UpdatedAt: 100,
			},
			{
				ProblemID: "child-1", AgentName: agent, SubmissionID: submission,
				PageAssetID: "page-1", Ordinal: 1, ProblemKind: k12.ProblemKindSubproblem,
				ParentProblemID: "parent-1", SubproblemNo: "1", Subject: "数学",
				SourceSectionPath: []string{"一"}, SourceSectionLabel: "一、读图回答",
				SystemSectionOrdinal: 1, SystemDisplayLabel: "第 1 题（系统序号）",
				StemRaw: "第一天有多少人？", StemMarkdown: "第一天有多少人？",
				ConceptIDs: []string{"读图"}, TranscriptionConfidence: confidence(0.86),
				ConfirmationRequired: true, ConfirmationReasons: []string{"low_confidence"},
				CanonicalVersion: 1, CreatedAt: 100, UpdatedAt: 100,
			},
			{
				ProblemID: "child-2", AgentName: agent, SubmissionID: submission,
				PageAssetID: "page-1", Ordinal: 2, ProblemKind: k12.ProblemKindSubproblem,
				ParentProblemID: "parent-1", SubproblemNo: "2", Subject: "数学",
				SourceNumberPath: []string{"一", "2"}, DisplayLabel: "一、2",
				SourceSectionPath: []string{"一"}, SourceSectionLabel: "一、读图回答",
				StemRaw: "第二天有多少人？", StemMarkdown: "第二天有多少人？",
				ConceptIDs: []string{"读图"}, TranscriptionConfidence: confidence(0.99),
				CanonicalVersion: 1, CreatedAt: 100, UpdatedAt: 100,
			},
		},
		Attempts: []k12.Attempt{
			{
				AttemptID: "attempt-1", AgentName: agent, SubmissionID: submission,
				ProblemID: "child-1", AnswerState: "present", AnswerRaw: "31",
				AnswerMarkdown: "31", CreatedAt: 100, UpdatedAt: 100,
			},
			{
				AttemptID: "attempt-2", AgentName: agent, SubmissionID: submission,
				ProblemID: "child-2", AnswerState: "present", AnswerRaw: "42",
				AnswerMarkdown: "42", BBox: &k12.AttemptBBox{X: .1, Y: .5, W: .2, H: .1},
				CreatedAt: 100, UpdatedAt: 100,
			},
		},
	}
}

func TestProblemAttemptSnapshot_RoundTripVersionsAndSiblingIsolation(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ProblemAttemptsDDL); err != nil {
		t.Fatalf("problem/attempt ddl: %v", err)
	}
	ctx := context.Background()
	snapshot := problemAttemptFixture("mingming", "submission-1")
	if err := store.PutProblemAttemptSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("initial put: %v", err)
	}

	updated := problemAttemptFixture("mingming", "submission-1")
	updated.Problems[1].StemMarkdown = "第一天共有多少人？"
	updated.Problems[1].CanonicalVersion = 2
	updated.Problems[1].UpdatedAt = 200
	updated.Attempts[0].AnswerMarkdown = "30"
	updated.Attempts[0].ConfirmedVersion = 1
	updated.Attempts[0].InputDigest = "digest-child-1-v1"
	updated.Attempts[0].BBox = &k12.AttemptBBox{X: .1, Y: .2, W: .2, H: .1}
	updated.Attempts[0].UpdatedAt = 200
	if err := store.PutProblemAttemptSnapshot(ctx, updated); err != nil {
		t.Fatalf("versioned put: %v", err)
	}

	got, err := store.GetProblemAttemptSnapshot(ctx, "mingming", "submission-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Problems) != 3 || got.Problems[0].ProblemID != "parent-1" || got.Problems[1].ProblemID != "child-1" {
		t.Fatalf("problem ordering/shape lost: %+v", got.Problems)
	}
	if first := got.Problems[1]; first.SourceSectionLabel != "一、读图回答" ||
		len(first.SourceSectionPath) != 1 || first.SourceSectionPath[0] != "一" ||
		first.SystemSectionOrdinal != 1 || first.SystemDisplayLabel != "第 1 题（系统序号）" {
		t.Fatalf("DD-041 system/source section facts lost: %+v", first)
	}
	if second := got.Problems[2]; second.DisplayLabel != "一、2" ||
		second.SystemSectionOrdinal != 0 || second.SystemDisplayLabel != "" {
		t.Fatalf("DD-041 printed source fact drift: %+v", second)
	}
	byAttempt := map[string]k12.Attempt{}
	for _, attempt := range got.Attempts {
		byAttempt[attempt.AttemptID] = attempt
	}
	if first := byAttempt["attempt-1"]; first.AnswerRaw != "31" || first.AnswerMarkdown != "30" ||
		first.ConfirmedVersion != 1 || first.InputDigest != "digest-child-1-v1" || first.BBox == nil {
		t.Fatalf("child-1 raw/canonical/version/bbox round-trip failed: %+v", first)
	}
	if sibling := byAttempt["attempt-2"]; sibling.AnswerRaw != "42" || sibling.AnswerMarkdown != "42" ||
		sibling.ConfirmedVersion != 0 || sibling.BBox == nil {
		t.Fatalf("updating child-1 must not mutate sibling: %+v", sibling)
	}

	invalid := updated
	invalid.Problems = append([]k12.Problem(nil), updated.Problems...)
	invalid.Problems[1].StemRaw = "rewritten raw"
	invalid.Problems[1].CanonicalVersion = 3
	if err := store.PutProblemAttemptSnapshot(ctx, invalid); !errors.Is(err, k12storage.ErrProblemAttemptConflict) {
		t.Fatalf("raw rewrite must fail closed, got %v", err)
	}
}

func TestProblemAttemptSnapshot_RejectsForgedSystemOrder(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ProblemAttemptsDDL); err != nil {
		t.Fatalf("problem/attempt ddl: %v", err)
	}
	for _, mutate := range []struct {
		name  string
		apply func(*k12.ProblemAttemptSnapshot)
	}{
		{
			name: "section ordinal skips the server-derived first item",
			apply: func(snapshot *k12.ProblemAttemptSnapshot) {
				snapshot.Problems[1].SystemSectionOrdinal = 2
				snapshot.Problems[1].SystemDisplayLabel = "第 2 题（系统序号）"
			},
		},
		{
			name: "compound parent cannot receive a system item ordinal",
			apply: func(snapshot *k12.ProblemAttemptSnapshot) {
				snapshot.Problems[0].SourceSectionPath = []string{"一"}
				snapshot.Problems[0].SourceSectionLabel = "一、读图回答"
				snapshot.Problems[0].SystemSectionOrdinal = 1
				snapshot.Problems[0].SystemDisplayLabel = "第 1 题（系统序号）"
			},
		},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			snapshot := problemAttemptFixture("mingming", "submission-"+t.Name())
			mutate.apply(&snapshot)
			if err := store.PutProblemAttemptSnapshot(context.Background(), snapshot); err == nil {
				t.Fatal("forged system order must fail closed")
			}
		})
	}
}

func TestProblemAttemptSnapshot_OwnerIsolationAndParentHasNoAttempt(t *testing.T) {
	store, db := setup(t)
	if _, err := db.ExecContext(context.Background(), migrate.K12ProblemAttemptsDDL); err != nil {
		t.Fatalf("problem/attempt ddl: %v", err)
	}
	ctx := context.Background()
	// Composite owner keys allow two tutors to ingest the same stable submission IDs.
	for _, owner := range []string{"mingming", "lele"} {
		if err := store.PutProblemAttemptSnapshot(ctx, problemAttemptFixture(owner, "same-photo")); err != nil {
			t.Fatalf("put %s: %v", owner, err)
		}
	}
	for _, owner := range []string{"mingming", "lele"} {
		got, err := store.GetProblemAttemptSnapshot(ctx, owner, "same-photo")
		if err != nil || len(got.Problems) != 3 || len(got.Attempts) != 2 {
			t.Fatalf("owner %s isolation failed: problems=%d attempts=%d err=%v", owner, len(got.Problems), len(got.Attempts), err)
		}
		for _, attempt := range got.Attempts {
			if attempt.ProblemID == "parent-1" {
				t.Fatal("compound parent must never own an Attempt")
			}
		}
	}
	if _, err := store.GetProblemAttemptSnapshot(ctx, "missing", "same-photo"); !errors.Is(err, records.ErrNotFound) {
		t.Fatalf("cross-owner lookup must be not found, got %v", err)
	}
}
