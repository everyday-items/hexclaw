package k12storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

func TestBeginPracticeGenerationJob_IsSourceUniqueAndCreatesNoPublicPlaceholder(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	if err := migrate.Run(ctx, db, []migrate.Migration{
		migrate.K12PracticeGenerationSourcesV86,
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := k12.NewAccumRecord("mingming", "session-1", k12.AccumFields{
		Subject: "语文", EntryType: "好词好句", Content: "桂花香",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.Put(ctx, rec); err != nil || !created {
		t.Fatalf("seed accumulation: created=%v err=%v", created, err)
	}
	var sourceVersion int
	if err := db.QueryRow(`SELECT row_version FROM k12_accumulations
		WHERE record_id=?`, rec.RecordID).Scan(&sourceVersion); err != nil {
		t.Fatal(err)
	}
	job := k12.PracticeGenerationJob{
		GenerationJobID:   "generation-accumulation-1",
		AgentName:         "mingming",
		IdempotencyKey:    "dictation:" + rec.RecordID,
		RequestDigest:     "digest-1",
		Scope:             "single",
		SourceKind:        k12.PracticeGenerationSourceAccumulation,
		SourceID:          rec.RecordID,
		SourceVersion:     sourceVersion,
		SourceSummary:     "桂花香",
		RequestSnapshot:   `{"content":"桂花香"}`,
		RouteSnapshot:     `{"provider":"rule","model":"dictation-format-v1"}`,
		VariantsPerSource: 1,
		Difficulty:        "same",
		Total:             "1",
		ResultItemIDs:     []string{"dictation-generation-accumulation-1"},
		CreatedAt:         10,
		UpdatedAt:         10,
	}
	accepted, created, err := store.BeginPracticeGenerationJob(ctx, job)
	if err != nil || !created {
		t.Fatalf("begin shared source job: created=%v accepted=%+v err=%v",
			created, accepted, err)
	}
	var sets, items, jobs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_practice_sets`).Scan(&sets); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_practice_set_items`).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_practice_generation_jobs
		WHERE source_kind='accumulation' AND source_id=? AND source_version=?`,
		rec.RecordID, sourceVersion).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if sets != 0 || items != 0 || jobs != 1 {
		t.Fatalf("job-only pending wrote sets/items/jobs=%d/%d/%d", sets, items, jobs)
	}

	duplicate := job
	duplicate.GenerationJobID = "generation-accumulation-duplicate"
	duplicate.IdempotencyKey = "dictation:duplicate-command"
	replayed, created, err := store.BeginPracticeGenerationJob(ctx, duplicate)
	if err != nil || created || replayed.GenerationJobID != job.GenerationJobID {
		t.Fatalf("same source identity did not converge: created=%v replay=%+v err=%v",
			created, replayed, err)
	}
	duplicate.RequestDigest = "changed-digest"
	if _, _, err := store.BeginPracticeGenerationJob(ctx, duplicate); err == nil {
		t.Fatal("same source identity accepted a changed frozen request")
	}
}

func TestCommitPracticeGeneration_SourceJobRollsBackJobAndItemTogether(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	if err := migrate.Run(ctx, db, []migrate.Migration{
		migrate.K12PracticeGenerationSourcesV86,
	}); err != nil {
		t.Fatal(err)
	}
	rec, err := k12.NewAccumRecord("mingming", "session-1", k12.AccumFields{
		Subject: "语文", EntryType: "好词好句", Content: "桂花香",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	var sourceVersion int
	if err := db.QueryRow(`SELECT row_version FROM k12_accumulations
		WHERE record_id=?`, rec.RecordID).Scan(&sourceVersion); err != nil {
		t.Fatal(err)
	}
	job := k12.PracticeGenerationJob{
		GenerationJobID: "generation-atomic-1", AgentName: "mingming",
		IdempotencyKey: "dictation:" + rec.RecordID, RequestDigest: "digest-atomic",
		Scope: "single", SourceKind: k12.PracticeGenerationSourceAccumulation,
		SourceID: rec.RecordID, SourceVersion: sourceVersion, SourceSummary: "桂花香",
		RequestSnapshot:   `{"content":"桂花香"}`,
		RouteSnapshot:     `{"provider":"rule","model":"dictation-format-v1"}`,
		VariantsPerSource: 1, Difficulty: "same", Total: "1",
		ResultItemIDs: []string{"dictation-generation-atomic-1"}, CreatedAt: 10, UpdatedAt: 10,
	}
	accepted, _, err := store.BeginPracticeGenerationJob(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err = store.AdvancePracticeGenerationJob(
		ctx, "mingming", accepted.GenerationJobID,
		k12.PracticeGenerationGenerating, 1, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err = store.AdvancePracticeGenerationJob(
		ctx, "mingming", accepted.GenerationJobID,
		k12.PracticeGenerationValidating, 1, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	ready := k12.PracticeItem{
		ItemID: accepted.ResultItemIDs[0], Subject: "语文",
		AddedVia:         k12.PracticeAddedViaAccumulation,
		GenerationStatus: k12.PracticeItemGenerationReady,
		QuestionMarkdown: "默写：桂花香", ExpectedAnswerMarkdown: "桂花香",
		VerificationStatus:    k12.PracticeItemVerified,
		VerificationEvidence:  "字符级比对",
		GenerationJobID:       accepted.GenerationJobID,
		NormalizedContentHash: "hash-atomic-1",
	}
	set, err := k12.NewPracticeSetRecord("mingming", "session-1", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceMixed, Title: "待打印篮",
		Items: []k12.PracticeItem{ready},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER fail_source_job_item_insert
		BEFORE INSERT ON k12_practice_set_items
		BEGIN SELECT RAISE(ABORT,'injected item failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CommitPracticeGeneration(ctx, set, -1, accepted); err == nil {
		t.Fatal("injected item failure unexpectedly committed")
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM k12_practice_generation_jobs
		WHERE generation_job_id=?`, accepted.GenerationJobID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	var setCount, itemCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM k12_practice_sets`).Scan(&setCount)
	_ = db.QueryRow(`SELECT COUNT(*) FROM k12_practice_set_items`).Scan(&itemCount)
	if status != k12.PracticeGenerationValidating || setCount != 0 || itemCount != 0 {
		t.Fatalf("partial commit survived rollback: status=%s sets/items=%d/%d",
			status, setCount, itemCount)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_source_job_item_insert`); err != nil {
		t.Fatal(err)
	}
	stored, replay, err := store.CommitPracticeGeneration(ctx, set, -1, accepted)
	if err != nil || replay || stored.RecordID == "" {
		t.Fatalf("retry atomic commit: replay=%v stored=%+v err=%v", replay, stored, err)
	}
	committed, err := store.GetPracticeGenerationJobByID(
		ctx, "mingming", accepted.GenerationJobID,
	)
	if err != nil || committed.Status != k12.PracticeGenerationCommitted ||
		committed.ResultSetID != stored.RecordID {
		t.Fatalf("shared receipt did not commit with item: %+v err=%v", committed, err)
	}
	fields, err := k12.ParsePracticeSetFields(stored.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields.Items) != 1 || !k12.PracticeItemPublishable(fields.Items[0]) {
		t.Fatalf("committed formal item mismatch: %+v", fields.Items)
	}
}

func TestRemovePracticeItem_RetiresSharedAccumulationJobWithoutLegacyWrite(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	if err := migrate.Run(ctx, db, []migrate.Migration{
		migrate.K12PracticeGenerationSourcesV86,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO k12_practice_generation_jobs(
		generation_job_id,agent_name,idempotency_key,request_digest,scope,
		variants_per_source,difficulty,total,status,result_set_id,result_item_ids_json,
		source_kind,source_id,source_version,created_at,updated_at
	) VALUES(
		'generation-remove','mingming','dictation:remove','digest-remove','single',
		1,'same','1','committed','set-remove','["item-remove"]',
		'accumulation','accumulation-remove',1,10,10
	);`); err != nil {
		t.Fatal(err)
	}
	set, err := k12.NewPracticeSetRecord("mingming", "session-1", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceMixed, Title: "待打印篮",
		Items: []k12.PracticeItem{{
			ItemID: "item-remove", Subject: "语文",
			AddedVia:         k12.PracticeAddedViaAccumulation,
			GenerationStatus: k12.PracticeItemGenerationReady,
			QuestionMarkdown: "默写：桂花香", ExpectedAnswerMarkdown: "桂花香",
			VerificationStatus:   k12.PracticeItemVerified,
			VerificationEvidence: "字符级比对", GenerationJobID: "generation-remove",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	set.RecordID = "set-remove"
	if _, err := store.Put(ctx, set); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(ctx, set.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParsePracticeSetFields(stored.Fields)
	if err != nil {
		t.Fatal(err)
	}
	fields.Items = nil
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemovePracticeItemAndRetireGeneration(
		ctx, "mingming", set.RecordID, "item-remove", "generation-remove",
		string(raw), stored.Version,
	); err != nil {
		t.Fatal(err)
	}
	job, err := store.GetPracticeGenerationJobByID(ctx, "mingming", "generation-remove")
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != k12.PracticeGenerationCommitted || job.RetiredAt == 0 ||
		job.RetiredReason != "removed" {
		t.Fatalf("shared accumulation job not retired for re-add: %+v", job)
	}
	var legacyRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_accumulation_dictation_generations`).Scan(
		&legacyRows,
	); err != nil {
		t.Fatal(err)
	}
	if legacyRows != 0 {
		t.Fatalf("shared removal wrote %d legacy accumulation rows", legacyRows)
	}
	if _, err := store.ReactivatePracticeGenerationJob(
		ctx, "mingming", job.GenerationJobID,
	); err != nil && !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("reactivate shared job: %v", err)
	}
}
