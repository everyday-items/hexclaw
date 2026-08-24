package k12storage_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestCurrentCreativeWorkCreateAtomicallyInstallsInitialGenerationWithoutLegacyVersionWrite(t *testing.T) {
	store, db := setup(t)
	rec, err := k12.NewCreativeWorkRecord("mingming", "session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting,
	})
	if err != nil {
		t.Fatal(err)
	}
	initial, created, err := store.CreateCreativeWorkWithInitialGeneration(
		context.Background(),
		rec,
		"auto:request-1",
		"sha256:request-1",
		k12.CreativeWorkSourceSnapshot{
			WorkType:        k12.WorkTypeWriting,
			ContentMarkdown: "桂花落在青石板上。",
		},
	)
	if err != nil || !created {
		t.Fatalf("create current work: created=%v err=%v", created, err)
	}
	if initial.GenerationID == "" || initial.WorkID != rec.RecordID ||
		initial.GenerationNo != 1 || initial.Status != k12.WorkFeedbackQueued {
		t.Fatalf("initial generation incomplete: %+v", initial)
	}

	var initialID, latestID, state string
	var rowVersion int
	if err := db.QueryRow(`SELECT initial_feedback_generation_id,
		latest_feedback_generation_id, feedback_state, row_version
		FROM k12_creative_works WHERE record_id=?`, rec.RecordID).
		Scan(&initialID, &latestID, &state, &rowVersion); err != nil {
		t.Fatal(err)
	}
	if initialID != initial.GenerationID || latestID != "" ||
		state != k12.WorkFeedbackQueued || rowVersion != 1 {
		t.Fatalf("root pointers initial=%q latest=%q state=%q version=%d",
			initialID, latestID, state, rowVersion)
	}
	var legacyVersions int
	if err := db.QueryRow(`SELECT count(*) FROM k12_creative_work_versions
		WHERE work_record_id=?`, rec.RecordID).Scan(&legacyVersions); err != nil {
		t.Fatal(err)
	}
	if legacyVersions != 0 {
		t.Fatalf("current create wrote %d legacy versions", legacyVersions)
	}
}

func TestCurrentCreativeWorkCreateUsesCommandReceiptNotSourceDedupe(t *testing.T) {
	store, db := setup(t)
	create := func(commandKey, requestDigest, content string) (
		*records.AgentRecord,
		k12.WorkFeedbackGeneration,
		bool,
		error,
	) {
		rec, err := k12.NewCreativeWorkRecord(
			"mingming",
			"session",
			k12.CreativeWorkFields{WorkType: k12.WorkTypeWriting},
		)
		if err != nil {
			t.Fatal(err)
		}
		generation, created, err := store.CreateCreativeWorkWithInitialGeneration(
			context.Background(),
			rec,
			commandKey,
			requestDigest,
			k12.CreativeWorkSourceSnapshot{
				WorkType:        k12.WorkTypeWriting,
				ContentMarkdown: content,
			},
		)
		return rec, generation, created, err
	}

	first, firstGeneration, created, err := create(
		"save-work-1", "sha256:same", "同一篇正文",
	)
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	second, secondGeneration, created, err := create(
		"save-work-2", "sha256:same", "同一篇正文",
	)
	if err != nil || !created {
		t.Fatalf("independent save: created=%v err=%v", created, err)
	}
	if first.RecordID == second.RecordID ||
		firstGeneration.GenerationID == secondGeneration.GenerationID {
		t.Fatalf(
			"separate save commands must create independent works: first=%s/%s second=%s/%s",
			first.RecordID, firstGeneration.GenerationID,
			second.RecordID, secondGeneration.GenerationID,
		)
	}

	replayed, replayGeneration, created, err := create(
		"save-work-1", "sha256:same", "同一篇正文",
	)
	if err != nil || created ||
		replayed.RecordID != first.RecordID ||
		replayGeneration.GenerationID != firstGeneration.GenerationID {
		t.Fatalf(
			"same command replay drift: created=%v work=%s generation=%s err=%v",
			created, replayed.RecordID, replayGeneration.GenerationID, err,
		)
	}
	if _, _, _, err := create(
		"save-work-1", "sha256:changed", "改过的正文",
	); !errors.Is(err, k12storage.ErrCurrentCommandConflict) {
		t.Fatalf("same command with changed digest must conflict, got %v", err)
	}

	for table, want := range map[string]int{
		"k12_creative_works":            2,
		"k12_work_feedback_generations": 2,
		"k12_current_create_receipts":   2,
	} {
		var got int
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s count=%d want=%d", table, got, want)
		}
	}
}

func TestWorkFeedbackInitialRetryReusesGenerationAndFailedRegenerationPreservesLatest(t *testing.T) {
	store, _ := setup(t)
	rec, err := k12.NewCreativeWorkRecord("mingming", "session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting,
	})
	if err != nil {
		t.Fatal(err)
	}
	initial, _, err := store.CreateCreativeWorkWithInitialGeneration(
		context.Background(), rec, "auto:req", "sha256:req",
		k12.CreativeWorkSourceSnapshot{
			WorkType: k12.WorkTypeWriting, ContentMarkdown: "原文",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FailWorkFeedbackGeneration(
		context.Background(), "mingming", initial.GenerationID, "provider_down",
	); err != nil {
		t.Fatal(err)
	}
	retried, created, err := store.PrepareWorkFeedbackGeneration(
		context.Background(), "mingming", rec.RecordID, "retry-command", "sha256:retry",
	)
	if err != nil {
		t.Fatal(err)
	}
	if created || retried.GenerationID != initial.GenerationID || retried.Attempt != 1 {
		t.Fatalf("initial retry must reuse generation: created=%v got=%+v initial=%+v",
			created, retried, initial)
	}

	feedback := k12.WorkFeedback{
		FeedbackID: "feedback-1", VersionID: "internal-generation",
		FeedbackType: k12.WorkTypeWriting,
		EvidenceRefs: []string{"content-ref:sha256:test"},
		Observations: []k12.WorkFeedbackObservation{{
			Dimension: "expression", Evidence: "原文有可见细节",
		}},
		SourceSnapshot: k12.WorkFeedbackSourceSnapshot{
			Source: k12.FeedbackSourceAI, MethodRef: "test", Capability: "evidence_based_feedback",
		},
		Limitations: "仅依据原文。", Suggestions: []string{"补充一个声音细节。"},
	}
	feedback.ProjectionMarkdown = k12.ProjectWorkFeedbackMarkdown(feedback)
	if _, err := store.CompleteWorkFeedbackGeneration(
		context.Background(), "mingming", retried.GenerationID, feedback,
	); err != nil {
		t.Fatal(err)
	}
	second, created, err := store.PrepareWorkFeedbackGeneration(
		context.Background(), "mingming", rec.RecordID, "regenerate-1", "sha256:regenerate-1",
	)
	if err != nil || !created || second.GenerationNo != 2 {
		t.Fatalf("prepare regeneration: created=%v generation=%+v err=%v", created, second, err)
	}
	if _, err := store.FailWorkFeedbackGeneration(
		context.Background(), "mingming", second.GenerationID, "provider_down",
	); err != nil {
		t.Fatal(err)
	}
	state, err := store.GetCreativeWorkGenerationState(
		context.Background(), "mingming", rec.RecordID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.Latest == nil || state.Latest.GenerationID != initial.GenerationID ||
		state.Initial == nil || state.Initial.Status != k12.WorkFeedbackSucceeded {
		t.Fatalf("failed regeneration replaced latest: %+v", state)
	}
}

func TestLegacyAccumulationDictationWriterIsFrozenAfterV86(t *testing.T) {
	store, db := setup(t)
	rec, err := k12.NewAccumRecord("mingming", "session", k12.AccumFields{
		Subject: "语文", EntryType: "好词好句", Content: "桂花香",
	})
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Put(context.Background(), rec)
	if err != nil || !created {
		t.Fatalf("put accumulation: created=%v err=%v", created, err)
	}
	_, _, err = store.PrepareAccumulationDictationGeneration(
		context.Background(), "mingming", rec.RecordID, "dictation-1",
		"sha256:dictation-1", `{"content":"桂花香","full_dictation":false}`,
	)
	if err == nil {
		t.Fatal("legacy accumulation generation writer remained writable after V86")
	}
	var legacyRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_accumulation_dictation_generations
		WHERE accumulation_id=?`, rec.RecordID).Scan(&legacyRows); err != nil {
		t.Fatal(err)
	}
	if legacyRows != 0 {
		t.Fatalf("rejected legacy writer left rows=%d", legacyRows)
	}
}

func TestCurrentWorkAndAccumulationDeleteAreCASIdempotentTombstones(t *testing.T) {
	store, _ := setup(t)
	work, err := k12.NewCreativeWorkRecord("mingming", "session", k12.CreativeWorkFields{
		WorkType: k12.WorkTypeWriting,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateCreativeWorkWithInitialGeneration(
		context.Background(), work, "auto:delete-work", "sha256:work",
		k12.CreativeWorkSourceSnapshot{
			WorkType: k12.WorkTypeWriting, ContentMarkdown: "原文",
		},
	); err != nil {
		t.Fatal(err)
	}
	accum, err := k12.NewAccumRecord("mingming", "session", k12.AccumFields{
		Subject: "语文", EntryType: "好词好句", Content: "积累",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), accum); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		kind string
		id   string
	}{
		{kind: "creative_work", id: work.RecordID},
		{kind: "accumulation", id: accum.RecordID},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			if _, err := store.TombstoneCurrentObject(
				context.Background(), "mingming", tc.kind, tc.id, 9, "wrong-version",
			); !errors.Is(err, records.ErrVersionConflict) {
				t.Fatalf("stale CAS err=%v", err)
			}
			first, err := store.TombstoneCurrentObject(
				context.Background(), "mingming", tc.kind, tc.id, 1, "delete-command",
			)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := store.TombstoneCurrentObject(
				context.Background(), "mingming", tc.kind, tc.id, 1, "delete-command",
			)
			if err != nil || replay != first {
				t.Fatalf("delete replay unstable: first=%+v replay=%+v err=%v", first, replay, err)
			}
			if first.RowVersion != 2 || first.DeletedAt == 0 {
				t.Fatalf("delete receipt incomplete: %+v", first)
			}
			if _, err := store.Get(context.Background(), tc.id); !errors.Is(err, records.ErrNotFound) {
				t.Fatalf("tombstoned object remained queryable: %v", err)
			}
		})
	}
}

func TestTombstoneDeleteDoesNotCascadeSharedAccumulationGenerationJob(t *testing.T) {
	store, db := setup(t)
	rec, err := k12.NewAccumRecord("mingming", "session", k12.AccumFields{
		Subject: "语文", EntryType: "好词好句", Content: "积累",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	job := k12.PracticeGenerationJob{
		GenerationJobID: "generation-1", AgentName: "mingming",
		IdempotencyKey: "dictation:" + rec.RecordID + ":v1",
		RequestDigest:  "sha256:dictation", Scope: "single",
		SourceKind: k12.PracticeGenerationSourceAccumulation,
		SourceID:   rec.RecordID, SourceVersion: 1,
		SourceSummary:     "积累",
		VariantsPerSource: 1, Difficulty: "same", Total: "1",
		Status: k12.PracticeGenerationQueued, ResultItemIDs: []string{"practice-item-1"},
		RequestSnapshot: `{"accumulation_id":"` + rec.RecordID + `","source_version":1}`,
		RouteSnapshot:   `{"provider":"rule","model":"dictation-format-v1"}`,
		CreatedAt:       100, UpdatedAt: 100,
	}
	if _, _, err := store.BeginPracticeGenerationJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TombstoneCurrentObject(
		context.Background(), "mingming", "accumulation", rec.RecordID, 1, "delete",
	); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM k12_practice_generation_jobs
		WHERE generation_job_id='generation-1' AND source_kind='accumulation'
		  AND source_id=?`, rec.RecordID).
		Scan(&count); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("delete cascaded shared accumulation generation, count=%d", count)
	}
}
