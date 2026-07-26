package k12storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
)

func TestCommitPracticeGeneration_CommittedReplayIgnoresStaleCandidateBasket(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	fields := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceMixed,
		Title:      "待打印篮",
		Items: []k12.PracticeItem{{
			ItemID: "item-stable", Subject: "数学", AddedVia: k12.PracticeAddedViaCustom,
			QuestionMarkdown: "1+1=?", ExpectedAnswerMarkdown: "2",
			VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
		}},
	}
	rec, err := k12.NewPracticeSetRecord("mingming", "session-1", fields)
	if err != nil {
		t.Fatal(err)
	}
	job := k12.PracticeGenerationJob{
		GenerationJobID: "pgen-stable", AgentName: "mingming", IdempotencyKey: "same-command",
		RequestDigest: "same-digest", Scope: "week", VariantsPerSource: 1,
		Difficulty: "same", Total: "5", Textbook: "人教版",
		Status: k12.PracticeGenerationCommitted, ResultItemIDs: []string{"item-stable"},
		CreatedAt: 100, UpdatedAt: 100,
	}
	committed, replay, err := store.CommitPracticeGeneration(ctx, rec, -1, job)
	if err != nil || replay {
		t.Fatalf("首次提交: replay=%v err=%v", replay, err)
	}

	// 并发调用可能在首次提交前读过幂等收据、却在提交后才读到篮子；它会基于
	// 过期命令快照再次拼入同一批确定性 item ID。committed 收据已是唯一真相，
	// 存储层必须先收敛到它，不能让输家的候选篮校验覆盖幂等结果。
	staleFields, err := k12.ParsePracticeSetFields(committed.Fields)
	if err != nil {
		t.Fatal(err)
	}
	staleFields.Items = append(staleFields.Items, staleFields.Items[0])
	raw, err := json.Marshal(staleFields)
	if err != nil {
		t.Fatal(err)
	}
	committed.Fields = string(raw)

	got, replay, err := store.CommitPracticeGeneration(ctx, committed, committed.Version, job)
	if err != nil {
		t.Fatalf("committed 幂等重放不得校验输家的过期候选篮: %v", err)
	}
	if !replay || got.RecordID != committed.RecordID {
		t.Fatalf("重放必须返回已提交集合: replay=%v got=%+v", replay, got)
	}
}

func TestPracticeSetRoundTrip_PreservesSingleGenerationProjection(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	fields := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceSingleVariant,
		Title:      "待打印篮",
		Items: []k12.PracticeItem{{
			ItemID:               "item-pending",
			SourceProblemID:      "problem-source",
			SourceMistakeSummary: "4÷0.5=8（小数点）",
			Subject:              "数学",
			AddedVia:             k12.PracticeAddedViaSingleVariant,
			GenerationStatus:     k12.PracticeItemGenerationQueued,
			VerificationStatus:   k12.PracticeItemPending,
			GenerationJobID:      "pgen-pending",
			VariantIndex:         1,
			RequestedDifficulty:  "same",
		}},
	}
	rec, err := k12.NewPracticeSetRecord("mingming", "session-1", fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, rec.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := k12.ParsePracticeSetFields(got.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(roundTrip.Items) != 1 {
		t.Fatalf("items=%d want 1", len(roundTrip.Items))
	}
	item := roundTrip.Items[0]
	if item.SourceMistakeSummary != fields.Items[0].SourceMistakeSummary ||
		item.GenerationStatus != k12.PracticeItemGenerationQueued {
		t.Fatalf("single generation projection lost on round-trip: %+v", item)
	}
}

func TestPracticeGenerationJobRoundTrip_PreservesFrozenSingleSourceSnapshot(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	job := k12.PracticeGenerationJob{
		GenerationJobID:   "pgen-single",
		AgentName:         "mingming",
		IdempotencyKey:    "single:mistake-1:generation-1",
		RequestDigest:     "digest-1",
		Scope:             "single",
		VariantsPerSource: 1,
		Difficulty:        "same",
		Total:             "1",
		Textbook:          "人教版",
		SourceMistakeID:   "mistake-1",
		SourceSummary:     "4÷0.5=8（小数点）",
		RequestSnapshot:   `{"grade":"五年级下"}`,
		RouteSnapshot:     `{"provider":"hexclaw-gpt","model":"gpt-5.6-sol"}`,
		Attempt:           2,
		FailureReason:     "provider unavailable",
		CreatedAt:         100,
		UpdatedAt:         101,
	}
	if err := store.RecordPracticeGenerationFailure(ctx, job); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetPracticeGenerationJob(ctx, job.AgentName, job.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceMistakeID != job.SourceMistakeID ||
		got.SourceSummary != job.SourceSummary ||
		got.RequestSnapshot != job.RequestSnapshot ||
		got.RouteSnapshot != job.RouteSnapshot ||
		got.Attempt != job.Attempt {
		t.Fatalf("frozen single-source job snapshot lost: %+v", got)
	}
}

func singleGenerationFixture(t *testing.T, jobID, commandKey, sourceID string) (*records.AgentRecord, k12.PracticeGenerationJob) {
	t.Helper()
	itemID := "item-" + jobID
	fields := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceSingleVariant,
		Title:      "待打印篮",
		Items: []k12.PracticeItem{{
			ItemID:               itemID,
			SourceProblemID:      "problem-" + sourceID,
			SourceMistakeSummary: "4÷0.5=8（小数点）",
			Subject:              "数学",
			AddedVia:             k12.PracticeAddedViaSingleVariant,
			GenerationStatus:     k12.PracticeItemGenerationQueued,
			VerificationStatus:   k12.PracticeItemPending,
			GenerationJobID:      jobID,
			VariantIndex:         1,
			RequestedDifficulty:  "same",
		}},
	}
	rec, err := k12.NewPracticeSetRecord("mingming", "session-1", fields)
	if err != nil {
		t.Fatal(err)
	}
	job := k12.PracticeGenerationJob{
		GenerationJobID: jobID, AgentName: "mingming", IdempotencyKey: commandKey,
		RequestDigest: "digest-" + sourceID, Scope: "single", VariantsPerSource: 1,
		Difficulty: "same", Total: "1", Textbook: "人教版",
		Status: k12.PracticeGenerationQueued, ResultItemIDs: []string{itemID},
		SourceMistakeID: sourceID, SourceSummary: fields.Items[0].SourceMistakeSummary,
		RequestSnapshot: `{"grade":"五年级下"}`,
		RouteSnapshot:   `{"provider":"hexclaw-gpt","model":"gpt-5.6-sol"}`,
		CreatedAt:       100, UpdatedAt: 100,
	}
	return rec, job
}

func TestBeginSinglePracticeGeneration_AtomicallyCreatesPlaceholderAndJob(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	rec, job := singleGenerationFixture(t, "pgen-atomic", "single:mistake-1:1", "mistake-1")

	stored, accepted, replay, err := store.BeginSinglePracticeGeneration(ctx, rec, -1, job)
	if err != nil || replay {
		t.Fatalf("begin: replay=%v err=%v", replay, err)
	}
	if accepted.GenerationJobID != job.GenerationJobID || stored.RecordID == "" ||
		accepted.ResultSetID != stored.RecordID {
		t.Fatalf("job/placeholder identity not joined: stored=%+v accepted=%+v", stored, accepted)
	}
	var jobs, items int
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_practice_generation_jobs WHERE generation_job_id=?`,
		job.GenerationJobID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_practice_set_items WHERE generation_job_id=?`,
		job.GenerationJobID).Scan(&items); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || items != 1 {
		t.Fatalf("atomic pair jobs=%d items=%d", jobs, items)
	}

	// A second command for the same active source converges to the first durable
	// pair even when the caller generated a different command/job identity.
	duplicateRec, duplicateJob := singleGenerationFixture(
		t, "pgen-duplicate", "single:mistake-1:2", "mistake-1",
	)
	replayedSet, replayedJob, replay, err := store.BeginSinglePracticeGeneration(
		ctx, duplicateRec, -1, duplicateJob,
	)
	if err != nil || !replay {
		t.Fatalf("active-source replay: replay=%v err=%v", replay, err)
	}
	if replayedJob.GenerationJobID != job.GenerationJobID ||
		replayedSet.RecordID != stored.RecordID {
		t.Fatalf("active-source replay drifted: set=%s job=%s", replayedSet.RecordID, replayedJob.GenerationJobID)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM k12_practice_generation_jobs
		WHERE agent_name='mingming' AND source_mistake_id='mistake-1'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("same active source created %d jobs", jobs)
	}
}

func TestBeginSinglePracticeGeneration_InvalidPlaceholderLeavesNoHalfState(t *testing.T) {
	store, db := setup(t)
	ctx := context.Background()
	rec, job := singleGenerationFixture(t, "pgen-invalid", "single:mistake-2:1", "mistake-2")
	rec.Fields = `{"source_kind":"single_variant","title":"待打印篮","items":[{"item_id":"broken"}]}`

	if _, _, _, err := store.BeginSinglePracticeGeneration(ctx, rec, -1, job); err == nil {
		t.Fatal("invalid placeholder unexpectedly accepted")
	}
	var jobs, items int
	_ = db.QueryRow(`SELECT COUNT(*) FROM k12_practice_generation_jobs WHERE generation_job_id=?`,
		job.GenerationJobID).Scan(&jobs)
	_ = db.QueryRow(`SELECT COUNT(*) FROM k12_practice_set_items WHERE generation_job_id=?`,
		job.GenerationJobID).Scan(&items)
	if jobs != 0 || items != 0 {
		t.Fatalf("invalid begin left half state jobs=%d items=%d", jobs, items)
	}
}

func TestAdvanceSinglePracticeGeneration_CommitsVerifiedItemAtomically(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	rec, job := singleGenerationFixture(t, "pgen-finish", "single:mistake-3:1", "mistake-3")
	stored, accepted, _, err := store.BeginSinglePracticeGeneration(ctx, rec, -1, job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceSinglePracticeGeneration(
		ctx, "mingming", accepted.GenerationJobID,
		k12.PracticeGenerationGenerating, 1, k12.PracticeItem{}, "",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceSinglePracticeGeneration(
		ctx, "mingming", accepted.GenerationJobID,
		k12.PracticeGenerationValidating, 1, k12.PracticeItem{}, "",
	); err != nil {
		t.Fatal(err)
	}
	ready := k12.PracticeItem{
		ItemID: accepted.ResultItemIDs[0], SourceProblemID: "problem-mistake-3",
		SourceMistakeSummary: accepted.SourceSummary,
		Subject:              "数学", AddedVia: k12.PracticeAddedViaSingleVariant,
		GenerationStatus: k12.PracticeItemGenerationReady,
		QuestionMarkdown: "5÷0.5=?", ExpectedAnswerMarkdown: "10",
		VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算",
		GenerationJobID: accepted.GenerationJobID, VariantIndex: 1,
		RequestedDifficulty: "same", ActualDifficulty: "same",
	}
	finished, err := store.AdvanceSinglePracticeGeneration(
		ctx, "mingming", accepted.GenerationJobID,
		k12.PracticeGenerationCommitted, 1, ready, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != k12.PracticeGenerationCommitted {
		t.Fatalf("job status=%s", finished.Status)
	}
	got, err := store.Get(ctx, stored.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := k12.ParsePracticeSetFields(got.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields.Items) != 1 || !k12.PracticeItemPublishable(fields.Items[0]) ||
		fields.Items[0].QuestionMarkdown != ready.QuestionMarkdown {
		t.Fatalf("verified item not committed atomically: %+v", fields.Items)
	}
}

func TestAdvanceSinglePracticeGeneration_FailureAndRetryReuseSameJobAndItem(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	rec, job := singleGenerationFixture(t, "pgen-retry", "single:mistake-4:1", "mistake-4")
	stored, accepted, _, err := store.BeginSinglePracticeGeneration(ctx, rec, -1, job)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := store.AdvanceSinglePracticeGeneration(
		ctx, "mingming", accepted.GenerationJobID,
		k12.PracticeGenerationFailed, 1, k12.PracticeItem{}, "provider unavailable",
	)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != k12.PracticeGenerationFailed || failed.FailureReason == "" {
		t.Fatalf("failed job not durable: %+v", failed)
	}
	retried, err := store.AdvanceSinglePracticeGeneration(
		ctx, "mingming", accepted.GenerationJobID,
		k12.PracticeGenerationQueued, 1, k12.PracticeItem{}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if retried.GenerationJobID != accepted.GenerationJobID ||
		retried.ResultItemIDs[0] != accepted.ResultItemIDs[0] ||
		retried.RouteSnapshot != accepted.RouteSnapshot {
		t.Fatalf("retry changed frozen identity/snapshot: %+v", retried)
	}
	got, err := store.Get(ctx, stored.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := k12.ParsePracticeSetFields(got.Fields)
	if fields.Items[0].GenerationStatus != k12.PracticeItemGenerationQueued ||
		fields.Items[0].BlockedReason != "" {
		t.Fatalf("retry did not reset same placeholder: %+v", fields.Items[0])
	}
}

func TestSaveSinglePracticeGenerationOutput_IsImmutablePerAttempt(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	rec, job := singleGenerationFixture(
		t, "pgen-output", "single:mistake-output:1", "mistake-output",
	)
	_, accepted, _, err := store.BeginSinglePracticeGeneration(ctx, rec, -1, job)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AdvanceSinglePracticeGeneration(
		ctx, accepted.AgentName, accepted.GenerationJobID,
		k12.PracticeGenerationGenerating, 1, k12.PracticeItem{}, "",
	); err != nil {
		t.Fatal(err)
	}

	const output = `{"solution":"## 问题\\n5÷0.5=?\\n## 答案\\n10"}`
	saved, err := store.SaveSinglePracticeGenerationOutput(
		ctx, accepted.AgentName, accepted.GenerationJobID, 1, output,
	)
	if err != nil {
		t.Fatal(err)
	}
	if saved.GenerationOutput != output || saved.OutputAttempt != 1 {
		t.Fatalf("generation output checkpoint lost: %+v", saved)
	}
	replayed, err := store.SaveSinglePracticeGenerationOutput(
		ctx, accepted.AgentName, accepted.GenerationJobID, 1, output,
	)
	if err != nil || replayed.GenerationOutput != output {
		t.Fatalf("exact output replay must converge: job=%+v err=%v", replayed, err)
	}
	if _, err = store.SaveSinglePracticeGenerationOutput(
		ctx, accepted.AgentName, accepted.GenerationJobID, 1, `{"solution":"changed"}`,
	); !errors.Is(err, k12storage.ErrPracticeGenerationOutputConflict) {
		t.Fatalf("changed output for same attempt err=%v want immutable conflict", err)
	}
	if _, err = store.SaveSinglePracticeGenerationOutput(
		ctx, accepted.AgentName, accepted.GenerationJobID, 2, output,
	); !errors.Is(err, records.ErrIllegalTransition) {
		t.Fatalf("future output attempt err=%v want illegal transition", err)
	}

	if _, err = store.AdvanceSinglePracticeGeneration(
		ctx, accepted.AgentName, accepted.GenerationJobID,
		k12.PracticeGenerationValidating, 1, k12.PracticeItem{}, "",
	); err != nil {
		t.Fatal(err)
	}
	const validation = `{"solution":"## 解答\\n独立验算\\n## 答案\\n10"}`
	validated, err := store.SaveSinglePracticeValidationOutput(
		ctx, accepted.AgentName, accepted.GenerationJobID, 1, validation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if validated.ValidationOutput != validation || validated.ValidationAttempt != 1 {
		t.Fatalf("validation output checkpoint lost: %+v", validated)
	}
	if _, err = store.SaveSinglePracticeValidationOutput(
		ctx, accepted.AgentName, accepted.GenerationJobID, 1, `{"solution":"changed"}`,
	); !errors.Is(err, k12storage.ErrPracticeGenerationOutputConflict) {
		t.Fatalf("changed validation output err=%v want immutable conflict", err)
	}
}

func TestSinglePracticeGenerationQueries_ProjectLatestAndRecoverable(t *testing.T) {
	store, _ := setup(t)
	ctx := context.Background()
	activeRec, activeJob := singleGenerationFixture(
		t, "pgen-active", "single:mistake-active:1", "mistake-active",
	)
	activeSet, _, _, err := store.BeginSinglePracticeGeneration(ctx, activeRec, -1, activeJob)
	if err != nil {
		t.Fatal(err)
	}
	failedRec, failedJob := singleGenerationFixture(
		t, "pgen-terminal", "single:mistake-failed:1", "mistake-failed",
	)
	activeFields, err := k12.ParsePracticeSetFields(activeSet.Fields)
	if err != nil {
		t.Fatal(err)
	}
	failedFields, err := k12.ParsePracticeSetFields(failedRec.Fields)
	if err != nil {
		t.Fatal(err)
	}
	activeFields.Items = append(activeFields.Items, failedFields.Items[0])
	raw, err := json.Marshal(activeFields)
	if err != nil {
		t.Fatal(err)
	}
	activeSet.Fields = string(raw)
	_, failedAccepted, _, err := store.BeginSinglePracticeGeneration(
		ctx, activeSet, activeSet.Version, failedJob,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AdvanceSinglePracticeGeneration(
		ctx, "mingming", failedAccepted.GenerationJobID,
		k12.PracticeGenerationFailed, 1, k12.PracticeItem{}, "timeout",
	); err != nil {
		t.Fatal(err)
	}
	recoverable, err := store.ListRecoverableSinglePracticeGenerations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recoverable) != 1 || recoverable[0].GenerationJobID != activeJob.GenerationJobID {
		t.Fatalf("recoverable=%+v", recoverable)
	}
	latest, err := store.GetLatestSinglePracticeGeneration(
		ctx, "mingming", "mistake-failed",
	)
	if err != nil {
		t.Fatal(err)
	}
	if latest.GenerationJobID != failedJob.GenerationJobID ||
		latest.Status != k12.PracticeGenerationFailed {
		t.Fatalf("latest=%+v", latest)
	}
}
