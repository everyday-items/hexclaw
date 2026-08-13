package apihttp_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type a06CandidateSource struct {
	mu                 sync.Mutex
	arithmeticFailures int
	arithmeticTerminal bool
	textbookFailures   int
	requests           map[string][]usecase.WeeklyPracticeCandidateRequest
}

func (s *a06CandidateSource) GenerateWeeklyPracticeCandidates(
	ctx context.Context,
	req usecase.WeeklyPracticeCandidateRequest,
) ([]usecase.WeeklyPracticeCandidate, error) {
	s.mu.Lock()
	if s.requests == nil {
		s.requests = make(map[string][]usecase.WeeklyPracticeCandidateRequest)
	}
	s.requests[req.PlanSection] = append(s.requests[req.PlanSection], req)
	switch req.PlanSection {
	case k12.WeeklySectionArithmeticWarmup:
		if s.arithmeticFailures > 0 {
			s.arithmeticFailures--
			s.mu.Unlock()
			return nil, errors.New("temporary arithmetic generation failure")
		}
		if s.arithmeticTerminal {
			s.mu.Unlock()
			return []usecase.WeeklyPracticeCandidate{{
				SourceKind: "arithmetic_rule", GenerationMethod: "rule_generated",
				SourceRef: "arith:invalid", PromptMarkdown: "invalid",
				ExpectedAnswer: "", EvidenceRefs: []string{"rule:invalid"},
				EstimatedSeconds: 20,
			}}, nil
		}
	case k12.WeeklySectionTextbookConsolidation:
		if s.textbookFailures > 0 {
			s.textbookFailures--
			s.mu.Unlock()
			return nil, errors.New("temporary textbook generation failure")
		}
	}
	s.mu.Unlock()
	return (weeklyCandidateStub{}).GenerateWeeklyPracticeCandidates(ctx, req)
}

func (s *a06CandidateSource) calls(section string) []usecase.WeeklyPracticeCandidateRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]usecase.WeeklyPracticeCandidateRequest(nil), s.requests[section]...)
}

func newA06Server(
	t *testing.T,
	candidates usecase.WeeklyPracticeCandidateSource,
) (http.Handler, usecase.Deps, *weeklyClock) {
	t.Helper()
	ctx := t.Context()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if migrateErr := migrate.Run(ctx, db, migrate.All); migrateErr != nil {
		t.Fatal(migrateErr)
	}
	if _, execErr := db.ExecContext(ctx, `INSERT INTO agents(name) VALUES('mingming'),('other')`); execErr != nil {
		t.Fatal(execErr)
	}
	delivery := &httpReceiptTransport{send: []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryDelivered, ExternalMessageID: "a06-message-1",
	}}}
	rt, err := assembly.Wire(
		db,
		fakeSolveExec{},
		assembly.WithAccumulationMetadataDeriver(fixedAccumulationMetadataDeriver{}),
		assembly.WithRenderer(fixedPDFRenderer{}),
		assembly.WithDeliveryTransport(delivery),
	)
	if err != nil {
		t.Fatal(err)
	}
	clock := &weeklyClock{now: 1785081600}
	rt.Deps.Now = func() int64 { return clock.now }
	rt.Deps.WeeklyCurriculum = weeklyCatalogStub{}
	rt.Deps.WeeklyCandidates = candidates
	rt.Deps.WeeklyAssessment = weeklyAssessmentStub{}
	seedBUG20260726034A02Manifest(
		t, db, weeklyBundleManifestID, "desktop-user", "doc-weekly-contract",
		1, "ready_for_confirmation", "",
	)
	return apihttp.NewHandler(apihttp.Runtime{
		Views: rt.Registry.Views, Records: rt.Records, Deps: rt.Deps,
	}), rt.Deps, clock
}

func a06CreatePlan(
	t *testing.T,
	h http.Handler,
	key string,
) map[string]any {
	t.Helper()
	if rec, _ := do(t, h, http.MethodPut, "/profile-bundle",
		weeklyBundleBody(key+"-bundle", 0, 0, 0)); rec.Code != http.StatusOK {
		t.Fatalf("profile-bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, envelope := do(t, h, http.MethodPost, "/weekly-practice/plans",
		fmt.Sprintf(`{"agent":"mingming","idempotency_key":%q}`, key+"-plan"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	return envelope["plan"].(map[string]any)
}

func a06Track(t *testing.T, plan map[string]any, section string) map[string]any {
	t.Helper()
	for _, raw := range plan["tracks"].([]any) {
		track := raw.(map[string]any)
		if track["plan_section"] == section {
			return track
		}
	}
	t.Fatalf("missing track %s: %v", section, plan["tracks"])
	return nil
}

func a06Batch(t *testing.T, raw any, wantState string) map[string]any {
	t.Helper()
	batch, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("arithmetic_batch=%T %v want object", raw, raw)
	}
	keys := []string{
		"batch_id", "state", "item_count", "content_digest", "retryable",
		"failure_message", "created_at", "updated_at",
	}
	if wantState == "completed" {
		keys = append(keys, "completed_at")
	}
	exactKeys(t, batch, keys...)
	if batch["state"] != wantState {
		t.Fatalf("batch state=%v want %s; batch=%v", batch["state"], wantState, batch)
	}
	if (wantState == "failed_retryable") != batch["retryable"].(bool) {
		t.Fatalf("retryable drifted for state=%s: %v", wantState, batch)
	}
	return batch
}

func a06CurrentPlan(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	rec, envelope := do(t, h, http.MethodGet,
		"/weekly-practice/plans/current?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("current plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	return envelope["plan"].(map[string]any)
}

func TestBUG20260726034A06TrackDTOArithmeticBatchNullable(t *testing.T) {
	h, _, _ := newWeeklyBundleContractServer(t)
	plan := a06CreatePlan(t, h, "a06-track")
	for _, raw := range plan["tracks"].([]any) {
		track := raw.(map[string]any)
		if _, ok := track["arithmetic_batch"]; !ok {
			t.Errorf("track missing arithmetic_batch key: %v", track)
			continue
		}
		if track["plan_section"] != k12.WeeklySectionArithmeticWarmup &&
			track["arithmetic_batch"] != nil {
			t.Errorf("non-arithmetic track projected batch: %v", track)
		}
	}
	arithmetic := a06Track(t, plan, k12.WeeklySectionArithmeticWarmup)
	if arithmetic["arithmetic_batch"] != nil || len(arithmetic["items"].([]any)) != 0 {
		t.Errorf("plan without explicit batch must project null batch and zero items: %v", arithmetic)
	}
}

func TestBUG20260726034A06CreateCommandExactIdempotency(t *testing.T) {
	h, _, _ := newWeeklyBundleContractServer(t)
	plan := a06CreatePlan(t, h, "a06-create")
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	body := fmt.Sprintf(`{"plan_revision":%d,"item_count":1,
		"idempotency_key":"a06-create-command"}`, revision)
	rec, created := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/arithmetic-batches", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create batch status=%d want 201 body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, created, "batch", "replayed")
	if created["replayed"] != false {
		t.Fatalf("first create replayed=%v", created)
	}
	first := a06Batch(t, created["batch"], "preparing")
	rec, replay := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/arithmetic-batches", body)
	if rec.Code != http.StatusOK || replay["replayed"] != true {
		t.Fatalf("create replay status=%d body=%v", rec.Code, replay)
	}
	if replay["batch"].(map[string]any)["batch_id"] != first["batch_id"] {
		t.Fatalf("create replay minted a new batch: first=%v replay=%v", first, replay)
	}
	rec, _ = do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/arithmetic-batches",
		fmt.Sprintf(`{"plan_revision":%d,"item_count":1,
			"idempotency_key":"a06-create-command"}`, revision+1))
	if rec.Code != http.StatusConflict {
		t.Errorf("same key different digest status=%d want 409", rec.Code)
	}
	rec, _ = do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/arithmetic-batches",
		fmt.Sprintf(`{"plan_revision":%d,"item_count":1,
			"idempotency_key":"a06-create-second"}`, revision))
	if rec.Code != http.StatusConflict {
		t.Errorf("unfinished latest batch allowed a second batch: status=%d", rec.Code)
	}
}

func TestBUG20260726034A06StartAttemptAndLastItemCompleteAtomically(t *testing.T) {
	ctx := t.Context()
	h, deps, _ := newWeeklyBundleContractServer(t)
	plan := a06CreatePlan(t, h, "a06-attempt")
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	rec, created := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/arithmetic-batches",
		fmt.Sprintf(`{"plan_revision":%d,"item_count":1,
			"idempotency_key":"a06-attempt-create"}`, revision))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	batchID := created["batch"].(map[string]any)["batch_id"].(string)
	current := a06CurrentPlan(t, h)
	track := a06Track(t, current, k12.WeeklySectionArithmeticWarmup)
	ready := a06Batch(t, track["arithmetic_batch"], "ready")
	items := track["items"].([]any)
	if len(items) == 0 || ready["item_count"] != float64(len(items)) {
		t.Fatalf("ready batch/items mismatch: batch=%v items=%v", ready, items)
	}
	itemID := items[len(items)-1].(map[string]any)["item_id"].(string)
	startBody := `{"agent":"mingming","idempotency_key":"a06-start"}`
	rec, started := do(t, h, http.MethodPost,
		"/weekly-practice/arithmetic-batches/"+batchID+"/start", startBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, started, "batch", "replayed")
	a06Batch(t, started["batch"], "in_progress")
	rec, replayStart := do(t, h, http.MethodPost,
		"/weekly-practice/arithmetic-batches/"+batchID+"/start", startBody)
	if rec.Code != http.StatusOK || replayStart["replayed"] != true {
		t.Fatalf("start replay status=%d body=%v", rec.Code, replayStart)
	}
	attemptBody := fmt.Sprintf(`{"agent":"mingming","item_id":%q,
		"student_answer":"13","idempotency_key":"a06-last-attempt"}`, itemID)
	rec, attempted := do(t, h, http.MethodPost,
		"/weekly-practice/arithmetic-batches/"+batchID+"/attempts", attemptBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("attempt status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, attempted, "attempt", "replayed")
	exactKeys(t, attempted["attempt"].(map[string]any), "attempt_id", "batch_id",
		"item_id", "assessment_id", "result", "verification_evidence",
		"review_scheduled", "created_at")
	current = a06CurrentPlan(t, h)
	completed := a06Batch(t,
		a06Track(t, current, k12.WeeklySectionArithmeticWarmup)["arithmetic_batch"],
		"completed")
	if completed["batch_id"] != batchID {
		t.Fatalf("last attempt completed a different batch: %v", completed)
	}
	var attemptRows int
	if err := deps.Records.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM k12_weekly_arithmetic_attempts
		WHERE batch_id=? AND item_id=?`, batchID, itemID).Scan(&attemptRows); err != nil {
		t.Fatalf("count durable arithmetic attempts: %v", err)
	}
	if attemptRows != 1 {
		t.Fatalf("durable attempt rows=%d want 1", attemptRows)
	}
	rec, replayAttempt := do(t, h, http.MethodPost,
		"/weekly-practice/arithmetic-batches/"+batchID+"/attempts", attemptBody)
	if rec.Code != http.StatusOK || replayAttempt["replayed"] != true {
		t.Fatalf("attempt replay status=%d body=%v", rec.Code, replayAttempt)
	}
	rec, _ = do(t, h, http.MethodPost,
		"/weekly-practice/arithmetic-batches/"+batchID+"/attempts",
		fmt.Sprintf(`{"agent":"mingming","item_id":%q,
			"student_answer":"12","idempotency_key":"a06-last-attempt"}`, itemID))
	if rec.Code != http.StatusConflict {
		t.Errorf("attempt same key different answer status=%d want 409", rec.Code)
	}
	rec, next := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/arithmetic-batches",
		fmt.Sprintf(`{"plan_revision":%d,"item_count":1,
			"idempotency_key":"a06-next-batch"}`, revision))
	if rec.Code != http.StatusCreated ||
		next["batch"].(map[string]any)["batch_id"] == batchID {
		t.Errorf("completed latest did not allow one new batch: status=%d body=%v", rec.Code, next)
	}
}

func TestBUG20260726034A06RetryReusesBatchAndGenerationCheckpoint(t *testing.T) {
	source := &a06CandidateSource{arithmeticFailures: 1}
	h, _, _ := newA06Server(t, source)
	plan := a06CreatePlan(t, h, "a06-retry")
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	rec, created := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/arithmetic-batches",
		fmt.Sprintf(`{"plan_revision":%d,"item_count":1,
			"idempotency_key":"a06-retry-create"}`, revision))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create failing batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	batchID := created["batch"].(map[string]any)["batch_id"].(string)
	failed := a06Batch(t,
		a06Track(t, a06CurrentPlan(t, h), k12.WeeklySectionArithmeticWarmup)["arithmetic_batch"],
		"failed_retryable")
	if failed["batch_id"] != batchID {
		t.Fatalf("failed projection changed batch: %v", failed)
	}
	retryBody := `{"agent":"mingming","idempotency_key":"a06-retry-command"}`
	rec, retried := do(t, h, http.MethodPost,
		"/weekly-practice/arithmetic-batches/"+batchID+"/retry", retryBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, retried, "batch", "replayed")
	if retried["batch"].(map[string]any)["batch_id"] != batchID {
		t.Fatalf("retry minted a new batch: %v", retried)
	}
	ready := a06Batch(t,
		a06Track(t, a06CurrentPlan(t, h), k12.WeeklySectionArithmeticWarmup)["arithmetic_batch"],
		"ready")
	if ready["batch_id"] != batchID {
		t.Fatalf("retry ready projection changed batch: %v", ready)
	}
	rec, replay := do(t, h, http.MethodPost,
		"/weekly-practice/arithmetic-batches/"+batchID+"/retry", retryBody)
	if rec.Code != http.StatusOK || replay["replayed"] != true {
		t.Fatalf("retry replay status=%d body=%v", rec.Code, replay)
	}
	calls := source.calls(k12.WeeklySectionArithmeticWarmup)
	if len(calls) != 2 || !reflect.DeepEqual(calls[0], calls[1]) {
		t.Fatalf("retry did not reuse frozen generation checkpoint: calls=%v", calls)
	}
}

func TestBUG20260726034A06TerminalBatchRejectsRetry(t *testing.T) {
	source := &a06CandidateSource{arithmeticTerminal: true}
	h, _, _ := newA06Server(t, source)
	plan := a06CreatePlan(t, h, "a06-terminal")
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	rec, created := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/arithmetic-batches",
		fmt.Sprintf(`{"plan_revision":%d,"item_count":1,
			"idempotency_key":"a06-terminal-create"}`, revision))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create terminal batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	batchID := created["batch"].(map[string]any)["batch_id"].(string)
	a06Batch(t,
		a06Track(t, a06CurrentPlan(t, h), k12.WeeklySectionArithmeticWarmup)["arithmetic_batch"],
		"failed_terminal")
	rec, _ = do(t, h, http.MethodPost,
		"/weekly-practice/arithmetic-batches/"+batchID+"/retry",
		`{"agent":"mingming","idempotency_key":"a06-terminal-retry"}`)
	if rec.Code != http.StatusConflict {
		t.Errorf("terminal retry status=%d want 409", rec.Code)
	}
}

func TestBUG20260726034A06StaleTextbookRefreshChangesOnlySyncTrack(t *testing.T) {
	ctx := t.Context()
	h, deps, _ := newWeeklyBundleContractServer(t)
	plan := a06CreatePlan(t, h, "a06-stale")
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	dueBefore := a06Track(t, plan, k12.WeeklySectionDueReview)
	arithmeticBefore := a06Track(t, plan, k12.WeeklySectionArithmeticWarmup)
	tx, err := deps.Records.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	progressResult, err := tx.ExecContext(ctx, `UPDATE k12_curriculum_progress
		SET revision=revision+1,updated_at=updated_at+1
		WHERE agent_name='mingming' AND subject='math'`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("advance persisted progress revision: %v", err)
	}
	progressRows, err := progressResult.RowsAffected()
	if err != nil || progressRows != 1 {
		_ = tx.Rollback()
		t.Fatalf("advance persisted progress rows=%d err=%v want 1", progressRows, err)
	}
	headResult, err := tx.ExecContext(ctx, `UPDATE k12_curriculum_progress_revisions
		SET revision=revision+1,updated_at=updated_at+1
		WHERE agent_name='mingming' AND subject='math'`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("advance persisted progress lifecycle revision: %v", err)
	}
	headRows, err := headResult.RowsAffected()
	if err != nil || headRows != 1 {
		_ = tx.Rollback()
		t.Fatalf("advance progress lifecycle rows=%d err=%v want 1", headRows, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stalePlan := a06CurrentPlan(t, h)
	if a06Track(t, stalePlan, k12.WeeklySectionTextbookConsolidation)["status"] !=
		"stale" {
		t.Fatalf("newer progress did not project stale sync track: %v", stalePlan)
	}
	body := fmt.Sprintf(`{"agent":"mingming","expected_revision":%d,
		"idempotency_key":"a06-stale-refresh"}`, revision)
	rec, refreshed := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+
			"/tracks/textbook_consolidation/refresh", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("stale refresh status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, refreshed, "plan", "replayed")
	nextPlan := refreshed["plan"].(map[string]any)
	if nextPlan["revision"] != float64(revision+1) ||
		a06Track(t, nextPlan, k12.WeeklySectionTextbookConsolidation)["status"] !=
			k12.WeeklyTrackReady {
		t.Fatalf("stale refresh did not create ready revision: %v", nextPlan)
	}
	if !reflect.DeepEqual(dueBefore, a06Track(t, nextPlan, k12.WeeklySectionDueReview)) ||
		!reflect.DeepEqual(arithmeticBefore,
			a06Track(t, nextPlan, k12.WeeklySectionArithmeticWarmup)) {
		t.Fatalf("sync refresh changed unrelated tracks: before=%v/%v after=%v",
			dueBefore, arithmeticBefore, nextPlan["tracks"])
	}
	rec, replay := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+
			"/tracks/textbook_consolidation/refresh", body)
	if rec.Code != http.StatusOK || replay["replayed"] != true {
		t.Fatalf("stale refresh replay status=%d body=%v", rec.Code, replay)
	}
}

func TestBUG20260726034A06FailedTextbookRefreshReusesCheckpoint(t *testing.T) {
	source := &a06CandidateSource{textbookFailures: 1}
	h, _, _ := newA06Server(t, source)
	plan := a06CreatePlan(t, h, "a06-failed-sync")
	track := a06Track(t, plan, k12.WeeklySectionTextbookConsolidation)
	if track["status"] != k12.WeeklyTrackFailed {
		t.Fatalf("fixture did not produce failed sync track: %v", track)
	}
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	dueBefore := a06Track(t, plan, k12.WeeklySectionDueReview)
	arithmeticBefore := a06Track(t, plan, k12.WeeklySectionArithmeticWarmup)
	body := fmt.Sprintf(`{"agent":"mingming","expected_revision":%d,
		"idempotency_key":"a06-failed-refresh"}`, revision)
	rec, refreshed := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+
			"/tracks/textbook_consolidation/refresh", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed refresh status=%d body=%s", rec.Code, rec.Body.String())
	}
	nextPlan := refreshed["plan"].(map[string]any)
	if a06Track(t, nextPlan, k12.WeeklySectionTextbookConsolidation)["status"] !=
		k12.WeeklyTrackReady {
		t.Fatalf("failed refresh did not recover sync track: %v", nextPlan)
	}
	if !reflect.DeepEqual(dueBefore, a06Track(t, nextPlan, k12.WeeklySectionDueReview)) ||
		!reflect.DeepEqual(arithmeticBefore,
			a06Track(t, nextPlan, k12.WeeklySectionArithmeticWarmup)) {
		t.Fatalf("failed refresh changed unrelated tracks: %v", nextPlan["tracks"])
	}
	calls := source.calls(k12.WeeklySectionTextbookConsolidation)
	if len(calls) != 2 || !reflect.DeepEqual(calls[0], calls[1]) {
		t.Fatalf("failed refresh did not reuse generation checkpoint: calls=%v", calls)
	}
}
