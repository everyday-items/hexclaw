package k12storage_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

const (
	recognitionOwner      = "owner-recognition"
	recognitionSubmission = "recognition-submission"
	recognitionDispatch   = "recognition-dispatch"
	recognitionJob        = "recognition-job"
	recognitionParent     = "recognition-parent"
	recognitionChildOne   = "recognition-child-1"
	recognitionChildTwo   = "recognition-child-2"
	recognitionWork       = "recognition-work"
	recognitionReceipt    = "recognition-receipt"
)

func TestCommitProblemSourceRecognitionResultAppendsTypedRevisionWithoutOverwritingV19(t *testing.T) {
	store, db := setup(t)
	lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
	ctx := context.Background()
	result := validProblemSourceRecognitionResult()

	before := readRecognitionV19Facts(t, db)
	committed, created, err := store.CommitProblemSourceRecognitionResult(ctx, lease, result)
	if err != nil || !created {
		t.Fatalf("commit source recognition result: created=%v err=%v", created, err)
	}
	if committed.WorkID != recognitionWork || committed.OwnerScope != recognitionOwner ||
		committed.Action != "select_region" || committed.SourceInputRevision != 2 ||
		committed.ResultInputRevision != 3 || len(committed.ResultDigest) != 64 ||
		committed.ParentInvocationID != "recognition-parent-invocation" ||
		!strings.HasPrefix(committed.ParentRequestDigest, "sha256:") ||
		committed.ParentInvocationAttempt != 2 || len(committed.PhysicalResults) != 2 ||
		committed.MappingState != k12storage.ProblemSourceRecognitionMappingStableExactSet ||
		len(committed.Items) != 2 {
		t.Fatalf("committed recognition projection drift: %+v", committed)
	}
	if got := committed.AffectedProblemIDs; len(got) != 2 ||
		got[0] != recognitionChildOne || got[1] != recognitionChildTwo {
		t.Fatalf("committed affected exact-set=%v", got)
	}
	if item := committed.Items[0]; item.ProblemID != recognitionChildOne ||
		item.InputRevision != 3 || item.Source.PageAssetID == "" ||
		item.Source.ContentDigest != strings.Repeat("b", 64) ||
		item.Source.MediaType != "image/png" || item.Source.PixelWidth != 200 ||
		item.Source.PixelHeight != 120 || item.Source.Region == nil ||
		item.Source.Region.X != 10 || item.Source.Region.Width != 100 ||
		item.AnswerState != "present" || item.AnswerBBox == nil ||
		item.Subject != "数学" || len(item.KnowledgePoints) != 2 ||
		item.RecognitionConfidence == nil || *item.RecognitionConfidence != 0.97 ||
		len(item.OCRSignals) != 2 || len(item.EvidenceTranscriptions) != 1 ||
		len(item.AnswerEvidenceTranscriptions) != 1 ||
		!item.ConfirmationRequired || len(item.ConfirmationReasons) != 1 {
		t.Fatalf("typed recognition item drift: %+v", item)
	}

	after := readRecognitionV19Facts(t, db)
	if before != after {
		t.Fatalf("V19 raw/canonical/answer/bbox facts were overwritten\nbefore=%+v\nafter=%+v", before, after)
	}
	assertRecognitionHeadsAtRevision(t, db, committed)

	rebuilt, err := store.GetProblemSourceRecognitionResultByWork(
		ctx, recognitionOwner, recognitionWork,
	)
	if err != nil {
		t.Fatalf("rebuild recognition result: %v", err)
	}
	if rebuilt.ResultDigest != committed.ResultDigest || len(rebuilt.Items) != 2 ||
		len(rebuilt.PhysicalResults) != 2 ||
		rebuilt.PhysicalResults[0].ResultDigest != strings.Repeat("c", 64) ||
		rebuilt.Items[0].StemRaw != result.Items[0].StemRaw ||
		rebuilt.Items[1].AnswerState != "blank" {
		t.Fatalf("restart-safe recognition rebuild drift: %+v", rebuilt)
	}
	currentFacts, err := store.ListCurrentProblemSourceRecognitionFacts(
		ctx, "mingming", recognitionSubmission,
	)
	if err != nil || len(currentFacts) != 2 {
		t.Fatalf("list current typed recognition facts=%+v err=%v", currentFacts, err)
	}
	currentOne := currentFacts[recognitionChildOne]
	if currentOne.InputRevision != 3 || currentOne.AnswerState != "present" ||
		currentOne.AnswerBBox == nil || currentOne.Subject != "数学" ||
		len(currentOne.KnowledgePoints) != 2 ||
		currentOne.RecognitionConfidence == nil ||
		len(currentOne.OCRSignals) != 2 ||
		len(currentOne.EvidenceTranscriptions) != 1 ||
		len(currentOne.AnswerEvidenceTranscriptions) != 1 ||
		!currentOne.ConfirmationRequired || len(currentOne.ConfirmationReasons) != 1 {
		t.Fatalf("current V73 typed projection fell back to stale V19 facts: %+v", currentOne)
	}
	if _, err := store.GetProblemSourceRecognitionResultByWork(
		ctx, "other-owner", recognitionWork,
	); !errors.Is(err, k12storage.ErrProblemSourceRecognitionNotFound) {
		t.Fatalf("cross-owner get error=%v, want not found", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE k12_problem_source_recognition_items
		SET subject='mutated'
		WHERE work_id=? AND problem_id=?`, recognitionWork, recognitionChildOne); err == nil {
		t.Fatal("immutable recognition fact accepted an UPDATE")
	}
}

func TestListCurrentProblemSourceRecognitionFactsExcludesInputDigestDrift(t *testing.T) {
	store, db := setup(t)
	lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
	commit, created, err := store.CommitProblemSourceRecognitionResult(
		context.Background(), lease, validProblemSourceRecognitionResult(),
	)
	if err != nil || !created {
		t.Fatalf("commit source recognition fixture: created=%v err=%v", created, err)
	}
	if _, err := db.ExecContext(context.Background(), `
		DROP TRIGGER k12_problem_input_revision_evidence_immutable`); err != nil {
		t.Fatalf("allow controlled corrupt-head fixture: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
		UPDATE k12_problem_input_revisions
		SET input_digest=?
		WHERE agent_name='mingming' AND submission_id=? AND structure_version=?
		  AND problem_id=? AND input_revision=? AND current_disposition='current'`,
		"sha256:other-current-input",
		recognitionSubmission,
		commit.StructureVersion,
		recognitionChildOne,
		commit.ResultInputRevision,
	); err != nil {
		t.Fatalf("drift current immutable input digest: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `
		UPDATE k12_attempts
		SET input_digest=?
		WHERE agent_name='mingming' AND submission_id=? AND problem_id=?`,
		"sha256:other-current-input",
		recognitionSubmission,
		recognitionChildOne,
	); err != nil {
		t.Fatalf("keep Attempt binding aligned with the current input: %v", err)
	}

	current, err := store.ListCurrentProblemSourceRecognitionFacts(
		context.Background(), "mingming", recognitionSubmission,
	)
	if err != nil {
		t.Fatalf("list current typed recognition facts: %v", err)
	}
	if _, found := current[recognitionChildOne]; found {
		t.Fatalf("digest-drifted V73 fact remained current: %+v", current[recognitionChildOne])
	}
	if currentTwo, found := current[recognitionChildTwo]; !found ||
		currentTwo.InputDigest != commit.Items[1].InputDigest {
		t.Fatalf("unaffected current V73 fact was lost: %+v", currentTwo)
	}
}

func TestCommitProblemSourceRecognitionResultReplayAndConflicts(t *testing.T) {
	t.Run("same digest replays without another revision", func(t *testing.T) {
		store, db := setup(t)
		lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		ctx := context.Background()
		result := validProblemSourceRecognitionResult()
		first, created, err := store.CommitProblemSourceRecognitionResult(ctx, lease, result)
		if err != nil || !created {
			t.Fatalf("first commit: created=%v err=%v", created, err)
		}
		replay, created, err := store.CommitProblemSourceRecognitionResult(ctx, lease, result)
		if err != nil || created {
			t.Fatalf("exact replay: created=%v err=%v", created, err)
		}
		if replay.ResultDigest != first.ResultDigest || replay.ResultInputRevision != 3 {
			t.Fatalf("replay identity drift: first=%+v replay=%+v", first, replay)
		}
		assertRecognitionRowCounts(t, db, 1, 2, 2, 2)

		changed := validProblemSourceRecognitionResult()
		changed.Items[0].QuestionCanonicalMarkdown = "变化后的规范题干"
		if _, _, err := store.CommitProblemSourceRecognitionResult(
			ctx, lease, changed,
		); !errors.Is(err, k12storage.ErrProblemSourceRecognitionConflict) {
			t.Fatalf("different result for same work error=%v, want conflict", err)
		}
		assertRecognitionRowCounts(t, db, 1, 2, 2, 2)
	})

	t.Run("lease owner epoch and expiry fence all writes", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			mutate func(*testing.T, *sql.DB, *k12storage.ProblemSourceReprocessLease)
		}{
			{name: "wrong owner", mutate: func(_ *testing.T, _ *sql.DB, lease *k12storage.ProblemSourceReprocessLease) {
				lease.LeaseOwner = "stale-worker"
			}},
			{name: "wrong epoch", mutate: func(_ *testing.T, _ *sql.DB, lease *k12storage.ProblemSourceReprocessLease) {
				lease.LeaseEpoch++
			}},
			{name: "expired", mutate: func(t *testing.T, db *sql.DB, _ *k12storage.ProblemSourceReprocessLease) {
				t.Helper()
				if _, err := db.Exec(`
					UPDATE k12_problem_source_reprocess_jobs
					SET lease_expires_at=? WHERE work_id=?`,
					time.Now().Add(-time.Minute).UnixMilli(), recognitionWork); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store, db := setup(t)
				lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
				tc.mutate(t, db, &lease)
				if _, _, err := store.CommitProblemSourceRecognitionResult(
					context.Background(), lease, validProblemSourceRecognitionResult(),
				); !errors.Is(err, k12storage.ErrProblemSourceReprocessFenced) {
					t.Fatalf("fenced commit error=%v", err)
				}
				assertRecognitionRowCounts(t, db, 0, 0, 0, 0)
			})
		}
	})
}

func TestCommitProblemSourceRecognitionResultRejectsUnstableOrDriftedMapping(t *testing.T) {
	t.Run("caller mapping must be stable exact set", func(t *testing.T) {
		store, db := setup(t)
		lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		result := validProblemSourceRecognitionResult()
		result.MappingState = "ambiguous"
		if _, _, err := store.CommitProblemSourceRecognitionResult(
			context.Background(), lease, result,
		); !errors.Is(err, k12storage.ErrProblemSourceRecognitionUnstableMapping) {
			t.Fatalf("unstable caller mapping error=%v", err)
		}
		assertRecognitionRowCounts(t, db, 0, 0, 0, 0)
	})

	t.Run("current structure must remain resolved", func(t *testing.T) {
		store, db := setup(t)
		lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		if _, err := db.Exec(`
			UPDATE k12_problem_structure_snapshots
			SET mapping_state='fail_closed'
			WHERE agent_name='mingming' AND submission_id=? AND structure_version=1`,
			recognitionSubmission); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.CommitProblemSourceRecognitionResult(
			context.Background(), lease, validProblemSourceRecognitionResult(),
		); !errors.Is(err, k12storage.ErrProblemSourceRecognitionUnstableMapping) {
			t.Fatalf("fail-closed structure error=%v", err)
		}
		assertRecognitionRowCounts(t, db, 0, 0, 0, 0)
	})

	t.Run("result items must equal the durable affected order", func(t *testing.T) {
		store, db := setup(t)
		lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		result := validProblemSourceRecognitionResult()
		result.Items[0], result.Items[1] = result.Items[1], result.Items[0]
		if _, _, err := store.CommitProblemSourceRecognitionResult(
			context.Background(), lease, result,
		); !errors.Is(err, k12storage.ErrProblemSourceRecognitionConflict) {
			t.Fatalf("reordered exact-set error=%v", err)
		}
		assertRecognitionRowCounts(t, db, 0, 0, 0, 0)
	})

	t.Run("source revision must still be every current head", func(t *testing.T) {
		store, db := setup(t)
		lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		if _, err := db.Exec(`
			UPDATE k12_problem_input_revisions
			SET current_disposition='superseded',updated_at=updated_at+1
			WHERE agent_name='mingming' AND submission_id=?
			  AND structure_version=1 AND problem_id=? AND input_revision=2`,
			recognitionSubmission, recognitionChildOne); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.CommitProblemSourceRecognitionResult(
			context.Background(), lease, validProblemSourceRecognitionResult(),
		); !errors.Is(err, k12storage.ErrProblemSourceRecognitionConflict) {
			t.Fatalf("drifted current head error=%v", err)
		}
		assertRecognitionRowCounts(t, db, 0, 0, 0, 0)
	})

	t.Run("physical child must be terminal succeeded with the exact digest", func(t *testing.T) {
		store, db := setup(t)
		lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		result := validProblemSourceRecognitionResult()
		result.PhysicalResults[0].ResultDigest = strings.Repeat("e", 64)
		if _, _, err := store.CommitProblemSourceRecognitionResult(
			context.Background(), lease, result,
		); !errors.Is(err, k12storage.ErrProblemSourceRecognitionConflict) {
			t.Fatalf("physical result digest drift error=%v", err)
		}
		assertRecognitionRowCounts(t, db, 0, 0, 0, 0)
	})

	t.Run("physical unit is part of the exact lineage", func(t *testing.T) {
		store, db := setup(t)
		lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		result := validProblemSourceRecognitionResult()
		result.PhysicalResults[0].PhysicalUnit = "segment_3"
		if _, _, err := store.CommitProblemSourceRecognitionResult(
			context.Background(), lease, result,
		); !errors.Is(err, k12storage.ErrProblemSourceRecognitionConflict) {
			t.Fatalf("physical unit drift error=%v", err)
		}
		assertRecognitionRowCounts(t, db, 0, 0, 0, 0)
	})

	t.Run("typed items are bound to the terminal parent result digest", func(t *testing.T) {
		store, db := setup(t)
		lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		result := validProblemSourceRecognitionResult()
		result.Items[0].QuestionCanonicalMarkdown = "与父调用结果不一致的伪造题干"
		if _, _, err := store.CommitProblemSourceRecognitionResult(
			context.Background(), lease, result,
		); !errors.Is(err, k12storage.ErrProblemSourceRecognitionConflict) {
			t.Fatalf("unbound typed item error=%v, want conflict", err)
		}
		assertRecognitionRowCounts(t, db, 0, 0, 0, 0)
	})

	t.Run("latest parent from another work cannot be rebound", func(t *testing.T) {
		store, db := setup(t)
		lease := seedProblemSourceRecognitionFixture(t, store, db, recognitionWork)
		if _, err := db.Exec(`
			UPDATE k12_model_invocations
			SET request_digest=?
			WHERE invocation_id='recognition-parent-invocation'`,
			"sha256:"+strings.Repeat("e", 64)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.CommitProblemSourceRecognitionResult(
			context.Background(), lease, validProblemSourceRecognitionResult(),
		); !errors.Is(err, k12storage.ErrProblemSourceRecognitionConflict) {
			t.Fatalf("wrong-work parent binding error=%v", err)
		}
		assertRecognitionRowCounts(t, db, 0, 0, 0, 0)
	})
}

func TestCommitProblemSourceRecognitionResultConcurrentReplayAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "recognition-result.db")
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	seedDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, seedDB, migrate.All); err != nil {
		t.Fatalf("migrate recognition database: %v", err)
	}
	if _, err := seedDB.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if _, err := seedDB.ExecContext(ctx, `INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	seedStore := k12storage.NewStore(seedDB, nil)
	lease := seedProblemSourceRecognitionFixture(t, seedStore, seedDB, recognitionWork)

	stores := make([]*k12storage.Store, 2)
	dbs := make([]*sql.DB, 2)
	for index := range stores {
		db, openErr := sql.Open("sqlite", dsn)
		if openErr != nil {
			t.Fatal(openErr)
		}
		db.SetMaxOpenConns(1)
		dbs[index] = db
		stores[index] = k12storage.NewStore(db, nil)
	}
	start := make(chan struct{})
	results := make(chan struct {
		commit  k12storage.ProblemSourceRecognitionCommit
		created bool
		err     error
	}, 2)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(2)
	done.Add(2)
	for _, store := range stores {
		go func(store *k12storage.Store) {
			defer done.Done()
			ready.Done()
			<-start
			commit, created, commitErr := store.CommitProblemSourceRecognitionResult(
				ctx, lease, validProblemSourceRecognitionResult(),
			)
			results <- struct {
				commit  k12storage.ProblemSourceRecognitionCommit
				created bool
				err     error
			}{commit: commit, created: created, err: commitErr}
		}(store)
	}
	ready.Wait()
	close(start)
	done.Wait()
	close(results)

	createdCount := 0
	var digest string
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent commit: %v", result.err)
			continue
		}
		if result.created {
			createdCount++
		}
		if digest == "" {
			digest = result.commit.ResultDigest
		} else if result.commit.ResultDigest != digest {
			t.Errorf("concurrent replay digest drift: %q vs %q", result.commit.ResultDigest, digest)
		}
	}
	if createdCount != 1 {
		t.Fatalf("concurrent created count=%d, want 1", createdCount)
	}
	assertRecognitionRowCounts(t, seedDB, 1, 2, 2, 2)

	for _, db := range dbs {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := seedDB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	rebuilt, err := k12storage.NewStore(reopened, nil).
		GetProblemSourceRecognitionResultByWork(ctx, recognitionOwner, recognitionWork)
	if err != nil || rebuilt.ResultDigest != digest || len(rebuilt.Items) != 2 {
		t.Fatalf("restart rebuild=%+v err=%v", rebuilt, err)
	}
	current, err := k12storage.NewStore(reopened, nil).
		ListCurrentProblemSourceRecognitionFacts(ctx, "mingming", recognitionSubmission)
	if err != nil || len(current) != 2 ||
		current[recognitionChildTwo].AnswerState != "blank" {
		t.Fatalf("restart current typed projection=%+v err=%v", current, err)
	}
}

func validProblemSourceRecognitionResult() k12storage.ProblemSourceRecognitionResult {
	confidenceOne := 0.97
	confidenceTwo := 0.91
	return k12storage.ProblemSourceRecognitionResult{
		MappingState:       k12storage.ProblemSourceRecognitionMappingStableExactSet,
		ParentInvocationID: "recognition-parent-invocation",
		PhysicalResults: []k12storage.ProblemSourceRecognitionPhysicalResultRef{
			{
				PhysicalInvocationID: "recognition-physical-1",
				PhysicalUnit:         "segment_1",
				ResultDigest:         strings.Repeat("c", 64),
			},
			{
				PhysicalInvocationID: "recognition-physical-2",
				PhysicalUnit:         "segment_2",
				ResultDigest:         strings.Repeat("d", 64),
			},
		},
		Items: []k12storage.ProblemSourceRecognitionItem{
			{
				ProblemID:                    recognitionChildOne,
				StemRaw:                      "1. 计算 17 + 25",
				QuestionCanonicalMarkdown:    "计算 $17+25$。",
				AnswerState:                  "present",
				AnswerRaw:                    "42",
				AnswerCanonicalMarkdown:      "$42$",
				AnswerBBox:                   &k12.AttemptBBox{X: 0.62, Y: 0.31, W: 0.2, H: 0.12},
				Subject:                      "数学",
				KnowledgePoints:              []string{"两位数加法", "进位加法"},
				RecognitionConfidence:        &confidenceOne,
				OCRSignals:                   []string{"clear_stem", "handwriting_present"},
				EvidenceTranscriptions:       []string{"17 + 25"},
				AnswerEvidenceTranscriptions: []string{"42"},
				ConfirmationRequired:         true,
				ConfirmationReasons:          []string{"handwriting_overlap"},
			},
			{
				ProblemID:                 recognitionChildTwo,
				StemRaw:                   "2. 计算 36 - 8",
				QuestionCanonicalMarkdown: "计算 $36-8$。",
				AnswerState:               "blank",
				Subject:                   "数学",
				KnowledgePoints:           []string{"两位数减法"},
				RecognitionConfidence:     &confidenceTwo,
				OCRSignals:                []string{"clear_stem", "blank_answer_area"},
				EvidenceTranscriptions:    []string{"36 - 8"},
				ConfirmationRequired:      false,
				ConfirmationReasons:       nil,
			},
		},
	}
}

func seedProblemSourceRecognitionFixture(
	t *testing.T,
	store *k12storage.Store,
	db *sql.DB,
	workID string,
) k12storage.ProblemSourceReprocessLease {
	t.Helper()
	return seedProblemSourceRecognitionFixtureForAction(
		t,
		store,
		db,
		workID,
		"select_region",
	)
}

func seedProblemSourceRecognitionFixtureForAction(
	t *testing.T,
	store *k12storage.Store,
	db *sql.DB,
	workID string,
	action string,
) k12storage.ProblemSourceReprocessLease {
	t.Helper()
	parentResultDigest, err := k12storage.ProblemSourceRecognitionTypedResultDigest(
		validProblemSourceRecognitionResult(),
	)
	if err != nil {
		t.Fatalf("digest typed source-recognition fixture: %v", err)
	}
	contentDigest := strings.Repeat("b", 64)
	pageAssetID := "asset://mingming/" + contentDigest + ".png"
	payload := json.RawMessage(`{"page_asset_id":"` + pageAssetID + `","region":{"x":10,"y":20,"width":100,"height":80}}`)
	switch action {
	case "retake":
		payload = json.RawMessage(fmt.Sprintf(
			`{"page_asset_id":%q}`,
			pageAssetID,
		))
	case "correct_text":
		payload = json.RawMessage(`{"question_canonical_markdown":"fixed","answer_canonical_markdown":""}`)
	case "resume":
		payload = json.RawMessage(`{}`)
	}
	requestJSON, err := json.Marshal(struct {
		Action                string          `json:"action"`
		StructureVersion      int             `json:"structure_version"`
		ExpectedInputRevision int             `json:"expected_input_revision"`
		Payload               json.RawMessage `json:"payload"`
	}{
		Action: action, StructureVersion: 1, ExpectedInputRevision: 1,
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("encode recognition action request: %v", err)
	}
	requestDigest := recognitionActionDigest(
		recognitionOwner,
		"mingming",
		recognitionDispatch,
		recognitionParent,
		action,
		1,
		1,
		payload,
	)
	structureDigest := recognitionStructureDigest(t)
	childOneV2Digest := recognitionInputDigest(requestDigest, recognitionChildOne, 2)
	childTwoV2Digest := recognitionInputDigest(requestDigest, recognitionChildTwo, 2)
	workDigest := recognitionInputDigest(
		requestDigest,
		recognitionChildOne+"\x00"+recognitionChildTwo,
		2,
	)
	parentRoute := k12.GradingModelSnapshot{
		Provider: "openai",
		Model:    "gpt-5.6-sol",
		Route:    "cloud",
	}
	parentRequestDigest := "sha256:" + strings.Repeat("e", 64)
	if action == "select_region" || action == "retake" {
		parentRequestDigest, err = k12storage.ProblemSourceRecognitionParentRequestDigest(
			k12storage.ProblemSourceReprocessJob{
				WorkID:             workID,
				CommandReceiptID:   recognitionReceipt,
				OwnerScope:         recognitionOwner,
				AgentName:          "mingming",
				DispatchID:         recognitionDispatch,
				JobID:              recognitionJob,
				ProblemID:          recognitionParent,
				Action:             action,
				StructureVersion:   1,
				InputRevision:      2,
				InputDigest:        workDigest,
				AffectedProblemIDs: []string{recognitionChildOne, recognitionChildTwo},
				RequestJSON:        requestJSON,
			},
			parentRoute,
			k12.ModelRequestPolicySnapshot{},
		)
		if err != nil {
			t.Fatalf("derive recognition parent request digest: %v", err)
		}
	}
	execRecognitionFixtureScript(t, db, `
INSERT OR IGNORE INTO agents(name) VALUES('mingming');
INSERT INTO k12_grading_jobs (
    record_id,agent_name,status,submission_id,source_kind,idempotency_key,
    dedupe_key,created_at,updated_at
) VALUES (
	:job, 'mingming','active',:submission,'desktop','recognition-job-key',
    'recognition-job-dedupe',100,100
);
INSERT INTO k12_image_task_dispatches (
    dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,
    source_asset_refs_json,source_digest,message_intent,task_intent,
    intent_evidence_json,intent_confidence,confirmation_candidates_json,status,
    target_object_type,target_object_id,classification_route_snapshot_json,
    classification_invocation_id,route_policy_snapshot_json,idempotency_key,
    request_digest,attempt_generation,retry_safe,failure_kind,version,created_at,updated_at
) VALUES (
	:dispatch,'mingming','recognition-learner','desktop','recognition-message','recognition-session',
	:asset_refs,:source_digest,'grade','completed_homework','[]',1,'[]','routed',
	'homework_submission',:submission,'{}','recognition-classification','{}',
    'recognition-dispatch-key','sha256:recognition-request',1,0,'',1,100,100
);
INSERT INTO k12_page_assets (
    owner_scope,page_asset_id,agent_name,content_digest,media_type,size_bytes,
    pixel_width,pixel_height,orientation_policy,orientation_policy_version,
    transform_chain_json,storage_state,ready_at,last_error,created_at,updated_at
) VALUES (
	:owner,:asset,'mingming',:content_digest,'image/png',4096,200,120,'verified','exif-v1',
    '[{"operation":"exif_normalize","orientation":1}]','ready',100,'',100,100
);
INSERT INTO k12_problems (
    problem_id,agent_name,submission_id,page_asset_id,ordinal,problem_kind,
    parent_problem_id,subproblem_no,subject,stem_raw,stem_markdown,concept_ids_json,
    transcription_confidence,confirmation_required,confirmation_reasons_json,
    canonical_version,created_at,updated_at
) VALUES
	(:parent,'mingming',:submission,:asset,0,'compound_parent',NULL,'','数学','旧公共题干','旧公共规范题干','[]',0.7,1,'["old"]',2,100,100),
	(:child1,'mingming',:submission,:asset,1,'subproblem',:parent,'1','数学','旧题干一','旧规范题干一','["old-kp-1"]',0.7,1,'["old"]',2,100,100),
	(:child2,'mingming',:submission,:asset,2,'subproblem',:parent,'2','数学','旧题干二','旧规范题干二','["old-kp-2"]',0.7,1,'["old"]',2,100,100);
INSERT INTO k12_attempts (
    attempt_id,agent_name,submission_id,problem_id,answer_state,answer_raw,
    answer_markdown,confirmed_version,input_digest,bbox_json,created_at,updated_at
) VALUES
	('recognition-attempt-1','mingming',:submission,:child1,'present','旧答案一','旧规范答案一',2,:child1_v2,
     '{"x":0.1,"y":0.2,"w":0.3,"h":0.1}',100,100),
	('recognition-attempt-2','mingming',:submission,:child2,'unclear','旧答案二','',2,:child2_v2,
     '{"x":0.2,"y":0.3,"w":0.2,"h":0.1}',100,100);
INSERT INTO k12_problem_structure_snapshots (
    agent_name,submission_id,structure_version,structure_digest,mapping_state,
    current_disposition,created_at,updated_at
) VALUES ('mingming',:submission,1,:structure_digest,'resolved','current',100,100);
INSERT INTO k12_problem_structure_members (
    agent_name,submission_id,structure_version,problem_id,ordinal,problem_kind,
    parent_problem_id,subproblem_no,source_number_path_json,display_label,
    dependency_group_id,input_revision
) VALUES
	('mingming',:submission,1,:parent,0,'compound_parent','', '', '["1"]','1','recognition-group',1),
	('mingming',:submission,1,:child1,1,'subproblem',:parent,'1','["1","1"]','1(1)','recognition-group',2),
	('mingming',:submission,1,:child2,2,'subproblem',:parent,'2','["1","2"]','1(2)','recognition-group',2);
INSERT INTO k12_problem_dependency_groups (
    agent_name,submission_id,structure_version,dependency_group_id,state,
    state_revision,created_at,updated_at
) VALUES ('mingming',:submission,1,'recognition-group','processing',1,100,100);
INSERT INTO k12_problem_source_action_receipts (
    command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,
    idempotency_key,request_digest,action,structure_version,
    expected_input_revision,result_input_revision,response_json,created_at,updated_at,
    request_json,affected_problem_ids_json
) VALUES (
	:receipt,:owner,'mingming',:dispatch,:job,:parent,'recognition-receipt-key',:request_digest,:action,1,
    1,2,'{}',100,100,
	:request_json,
	:affected
);
INSERT INTO k12_problem_input_revisions (
    agent_name,submission_id,structure_version,problem_id,input_revision,
    page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
    question_canonical_markdown,answer_canonical_markdown,input_digest,
    current_disposition,origin_command_receipt_id,origin_kind,created_at,updated_at
) VALUES
	('mingming',:submission,1,:child1,1,:asset,NULL,'旧题干一','旧答案一','{"x":0.1,"y":0.2,"w":0.3,"h":0.1}',
     '旧规范题干一','旧规范答案一','sha256:legacy-one','superseded',NULL,'legacy_unverified',90,100),
	('mingming',:submission,1,:child1,2,:asset,'{"x":10,"y":20,"width":100,"height":80}',
     '旧题干一','旧答案一','{"x":0.1,"y":0.2,"w":0.3,"h":0.1}',
	 '旧规范题干一','旧规范答案一',:child1_v2,'current',:receipt,'command',100,100),
	('mingming',:submission,1,:child2,1,:asset,NULL,'旧题干二','旧答案二','{"x":0.2,"y":0.3,"w":0.2,"h":0.1}',
     '旧规范题干二','','sha256:legacy-two','superseded',NULL,'legacy_unverified',90,100),
	('mingming',:submission,1,:child2,2,:asset,'{"x":10,"y":20,"width":100,"height":80}',
     '旧题干二','旧答案二','{"x":0.2,"y":0.3,"w":0.2,"h":0.1}',
	 '旧规范题干二','',:child2_v2,'current',:receipt,'command',100,100);
INSERT INTO k12_problem_source_reprocess_jobs (
    work_id,command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,
    problem_id,action,structure_version,input_revision,input_digest,
    affected_problem_ids_json,request_json,status,created_at,updated_at
) VALUES (
	:work,:receipt,:owner, 'mingming',:dispatch,:job,:parent,:action,1,2,:work_digest,:affected,
    :request_json,
    'queued',100,100
);
INSERT INTO k12_model_invocations (
    invocation_id,agent_name,job_id,stage,request_digest,provider,model,
    route_snapshot_json,request_policy_snapshot_json,provider_idempotency_key,
    status,attempt,result_digest,external_request_id,failure_kind,created_at,updated_at
) VALUES (
	'recognition-parent-invocation','mingming',:job,'recognizing',
	:parent_request_digest,'openai','gpt-5.6-sol',
    '{"provider":"openai","model":"gpt-5.6-sol","route":"cloud"}','{}',
	'recognition-parent-provider-key','succeeded',2,:parent_result_digest,
    'recognition-parent-external','','100','100'
);
INSERT INTO k12_model_physical_invocations (
    physical_invocation_id,parent_invocation_id,agent_name,job_id,stage,physical_unit,
    request_digest,route_snapshot_json,request_policy_snapshot_json,status,attempt,
    result_digest,result_content,external_request_id,failure_kind,created_at,updated_at
) VALUES
	('recognition-physical-1','recognition-parent-invocation','mingming',:job,'recognizing','segment_1',
	 'sha256:recognition-child-request-1','{}','{}','succeeded',1,:physical_digest1,
     '{"problems":[{"problem_id":"recognition-child-1"}]}','recognition-child-external-1','',100,100),
	('recognition-physical-2','recognition-parent-invocation','mingming',:job,'recognizing','segment_2',
	 'sha256:recognition-child-request-2','{}','{}','succeeded',1,:physical_digest2,
     '{"problems":[{"problem_id":"recognition-child-2"}]}','recognition-child-external-2','',100,100)
`,
		sql.Named("job", recognitionJob),
		sql.Named("submission", recognitionSubmission),
		sql.Named("dispatch", recognitionDispatch),
		sql.Named("asset_refs", fmt.Sprintf(`["%s"]`, pageAssetID)),
		sql.Named("source_digest", "sha256:recognition-source"),
		sql.Named("owner", recognitionOwner),
		sql.Named("asset", pageAssetID),
		sql.Named("content_digest", contentDigest),
		sql.Named("parent", recognitionParent),
		sql.Named("child1", recognitionChildOne),
		sql.Named("child2", recognitionChildTwo),
		sql.Named("child1_v2", childOneV2Digest),
		sql.Named("child2_v2", childTwoV2Digest),
		sql.Named("receipt", recognitionReceipt),
		sql.Named("request_digest", requestDigest),
		sql.Named("structure_digest", structureDigest),
		sql.Named("action", action),
		sql.Named("request_json", string(requestJSON)),
		sql.Named("affected", fmt.Sprintf(`["%s","%s"]`, recognitionChildOne, recognitionChildTwo)),
		sql.Named("work", workID),
		sql.Named("work_digest", workDigest),
		sql.Named("physical_digest1", strings.Repeat("c", 64)),
		sql.Named("physical_digest2", strings.Repeat("d", 64)),
		sql.Named("parent_request_digest", parentRequestDigest),
		sql.Named("parent_result_digest", parentResultDigest),
	)
	now := time.Now().UTC()
	claim, found, err := store.ClaimProblemSourceReprocessJob(
		context.Background(), "recognition-worker", now, 5*time.Minute,
	)
	if err != nil || !found || claim.WorkID != workID {
		t.Fatalf("claim source recognition fixture: claim=%+v found=%v err=%v", claim, found, err)
	}
	return claim.Lease()
}

// execRecognitionFixtureScript keeps this fixture's parameter binding exact.
// modernc SQLite applies a positional argument list to each statement in a
// multi-statement Exec, so feeding the complete script to one Exec can bind a
// job id as a later PageAsset digest and hide the actual recognition contract.
// This fixture contains no literal question marks; split only its controlled
// SQL statements and bind each statement's own positional slice.
func execRecognitionFixtureScript(t *testing.T, db *sql.DB, script string, args ...any) {
	t.Helper()
	for _, statement := range strings.Split(script, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		statementArgs := make([]any, 0, len(args))
		for _, arg := range args {
			named, ok := arg.(sql.NamedArg)
			if !ok || !recognitionSQLHasNamedParameter(statement, named.Name) {
				continue
			}
			statementArgs = append(statementArgs, named)
		}
		if _, err := db.Exec(statement, statementArgs...); err != nil {
			t.Fatalf("seed source recognition fixture: %v", err)
		}
	}
}

func recognitionSQLHasNamedParameter(statement string, name string) bool {
	token := ":" + name
	for start := 0; ; {
		index := strings.Index(statement[start:], token)
		if index < 0 {
			return false
		}
		end := start + index + len(token)
		if end == len(statement) ||
			!((statement[end] >= 'a' && statement[end] <= 'z') ||
				(statement[end] >= 'A' && statement[end] <= 'Z') ||
				(statement[end] >= '0' && statement[end] <= '9') ||
				statement[end] == '_') {
			return true
		}
		start = end
	}
}

func recognitionInputDigest(requestDigest, problemID string, revision int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", requestDigest, problemID, revision)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func recognitionActionDigest(
	ownerScope string,
	agentName string,
	dispatchID string,
	problemID string,
	action string,
	structureVersion int,
	expectedInputRevision int,
	payload json.RawMessage,
) string {
	raw, _ := json.Marshal(struct {
		OwnerScope            string          `json:"owner_scope"`
		AgentName             string          `json:"agent_name"`
		DispatchID            string          `json:"dispatch_id"`
		ProblemID             string          `json:"problem_id"`
		Action                string          `json:"action"`
		StructureVersion      int             `json:"structure_version"`
		ExpectedInputRevision int             `json:"expected_input_revision"`
		Payload               json.RawMessage `json:"payload"`
	}{
		ownerScope,
		agentName,
		dispatchID,
		problemID,
		action,
		structureVersion,
		expectedInputRevision,
		payload,
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func recognitionStructureDigest(t *testing.T) string {
	t.Helper()
	type member struct {
		ProblemID            string   `json:"problem_id"`
		Ordinal              int      `json:"ordinal"`
		ProblemKind          string   `json:"problem_kind"`
		ParentProblemID      string   `json:"parent_problem_id"`
		SubproblemNo         string   `json:"subproblem_no"`
		SourceNumberPath     []string `json:"source_number_path"`
		DisplayLabel         string   `json:"display_label"`
		SourceSectionPath    []string `json:"source_section_path"`
		SourceSectionLabel   string   `json:"source_section_label"`
		SystemSectionOrdinal int      `json:"system_section_ordinal"`
		SystemDisplayLabel   string   `json:"system_display_label"`
		DependencyGroupID    string   `json:"dependency_group_id"`
	}
	members := []member{
		{
			ProblemID: recognitionParent, Ordinal: 0, ProblemKind: "compound_parent",
			SourceNumberPath: []string{"1"}, DisplayLabel: "1",
			SourceSectionPath: []string{}, DependencyGroupID: "recognition-group",
		},
		{
			ProblemID: recognitionChildOne, Ordinal: 1, ProblemKind: "subproblem",
			ParentProblemID: recognitionParent, SubproblemNo: "1",
			SourceNumberPath: []string{"1", "1"}, DisplayLabel: "1(1)",
			SourceSectionPath: []string{}, DependencyGroupID: "recognition-group",
		},
		{
			ProblemID: recognitionChildTwo, Ordinal: 2, ProblemKind: "subproblem",
			ParentProblemID: recognitionParent, SubproblemNo: "2",
			SourceNumberPath: []string{"1", "2"}, DisplayLabel: "1(2)",
			SourceSectionPath: []string{}, DependencyGroupID: "recognition-group",
		},
	}
	raw, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type recognitionV19Facts struct {
	ProblemOneRaw       string
	ProblemOneCanonical string
	ProblemTwoRaw       string
	ProblemTwoCanonical string
	AttemptOneState     string
	AttemptOneRaw       string
	AttemptOneCanonical string
	AttemptOneBBox      string
	AttemptTwoState     string
	AttemptTwoRaw       string
	AttemptTwoCanonical string
	AttemptTwoBBox      string
}

func readRecognitionV19Facts(t *testing.T, db *sql.DB) recognitionV19Facts {
	t.Helper()
	var facts recognitionV19Facts
	if err := db.QueryRow(`
		SELECT p1.stem_raw,p1.stem_markdown,p2.stem_raw,p2.stem_markdown,
		       a1.answer_state,a1.answer_raw,a1.answer_markdown,a1.bbox_json,
		       a2.answer_state,a2.answer_raw,a2.answer_markdown,a2.bbox_json
		FROM k12_problems p1
		JOIN k12_problems p2
		  ON p2.agent_name=p1.agent_name AND p2.problem_id=?
		JOIN k12_attempts a1
		  ON a1.agent_name=p1.agent_name AND a1.problem_id=p1.problem_id
		JOIN k12_attempts a2
		  ON a2.agent_name=p2.agent_name AND a2.problem_id=p2.problem_id
		WHERE p1.agent_name='mingming' AND p1.problem_id=?`,
		recognitionChildTwo, recognitionChildOne,
	).Scan(
		&facts.ProblemOneRaw, &facts.ProblemOneCanonical,
		&facts.ProblemTwoRaw, &facts.ProblemTwoCanonical,
		&facts.AttemptOneState, &facts.AttemptOneRaw,
		&facts.AttemptOneCanonical, &facts.AttemptOneBBox,
		&facts.AttemptTwoState, &facts.AttemptTwoRaw,
		&facts.AttemptTwoCanonical, &facts.AttemptTwoBBox,
	); err != nil {
		t.Fatal(err)
	}
	return facts
}

func assertRecognitionHeadsAtRevision(
	t *testing.T,
	db *sql.DB,
	commit k12storage.ProblemSourceRecognitionCommit,
) {
	t.Helper()
	for _, item := range commit.Items {
		var memberRevision, attemptRevision int
		var currentDisposition, inputDigest, attemptDigest string
		if err := db.QueryRow(`
			SELECT ir.input_revision,ir.current_disposition,ir.input_digest,
			       sm.input_revision,a.confirmed_version,a.input_digest
			FROM k12_problem_input_revisions ir
			JOIN k12_problem_structure_members sm
			  ON sm.agent_name=ir.agent_name AND sm.submission_id=ir.submission_id
			 AND sm.structure_version=ir.structure_version AND sm.problem_id=ir.problem_id
			JOIN k12_attempts a
			  ON a.agent_name=ir.agent_name AND a.submission_id=ir.submission_id
			 AND a.problem_id=ir.problem_id
			WHERE ir.agent_name='mingming' AND ir.submission_id=?
			  AND ir.structure_version=1 AND ir.problem_id=? AND ir.input_revision=3`,
			recognitionSubmission, item.ProblemID,
		).Scan(
			&item.InputRevision, &currentDisposition, &inputDigest,
			&memberRevision, &attemptRevision, &attemptDigest,
		); err != nil {
			t.Fatal(err)
		}
		if item.InputRevision != 3 || memberRevision != 3 || attemptRevision != 3 ||
			currentDisposition != "current" || inputDigest != item.InputDigest ||
			attemptDigest != item.InputDigest {
			t.Fatalf("result head binding drift for %s: item=%+v member=%d attempt=%d/%q disposition=%s input=%q",
				item.ProblemID, item, memberRevision, attemptRevision, attemptDigest,
				currentDisposition, inputDigest)
		}
		var superseded string
		if err := db.QueryRow(`
			SELECT current_disposition FROM k12_problem_input_revisions
			WHERE agent_name='mingming' AND submission_id=? AND structure_version=1
			  AND problem_id=? AND input_revision=2`,
			recognitionSubmission, item.ProblemID,
		).Scan(&superseded); err != nil {
			t.Fatal(err)
		}
		if superseded != "superseded" {
			t.Fatalf("source revision for %s disposition=%s", item.ProblemID, superseded)
		}
	}
}

func assertRecognitionRowCounts(
	t *testing.T,
	db *sql.DB,
	resultRows int,
	itemRows int,
	physicalRows int,
	resultRevisionRows int,
) {
	t.Helper()
	for table, want := range map[string]int{
		"k12_problem_source_recognition_results":          resultRows,
		"k12_problem_source_recognition_items":            itemRows,
		"k12_problem_source_recognition_physical_results": physicalRows,
	} {
		var got int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s rows=%d, want %d", table, got, want)
		}
	}
	var gotRevisions int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM k12_problem_input_revisions
		WHERE agent_name='mingming' AND submission_id=? AND input_revision=3`,
		recognitionSubmission,
	).Scan(&gotRevisions); err != nil {
		t.Fatal(err)
	}
	if gotRevisions != resultRevisionRows {
		t.Fatalf("result input revision rows=%d, want %d", gotRevisions, resultRevisionRows)
	}
}
