package apihttp_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type weeklyCatalogStub struct{}

func (weeklyCatalogStub) LookupWeeklyCurriculum(
	_ context.Context,
	req usecase.WeeklyCurriculumCatalogRequest,
) (k12.CurriculumCatalog, error) {
	return k12.CurriculumCatalog{
		Subject: req.Subject, TextbookBindingID: "binding-rjb-5b",
		TextbookEdition: req.TextbookEdition, TextbookVersion: "2025",
		Title: "数学五年级下册", Volume: req.Volume, PageMin: 1, PageMax: 120,
		Units: []k12.CurriculumCatalogUnit{{
			UnitID: "u1", Title: "第一单元", PageFrom: 10, PageTo: 30,
			Lessons: []k12.CurriculumCatalogLesson{{
				LessonID: "l1", Title: "第一课", PageFrom: 10, PageTo: 20,
			}},
		}},
	}, nil
}

type weeklyCandidateStub struct{}

func (weeklyCandidateStub) GenerateWeeklyPracticeCandidates(
	_ context.Context,
	req usecase.WeeklyPracticeCandidateRequest,
) ([]usecase.WeeklyPracticeCandidate, error) {
	switch req.PlanSection {
	case k12.WeeklySectionTextbookConsolidation:
		return []usecase.WeeklyPracticeCandidate{{
			SourceKind: "textbook_segment", GenerationMethod: "rule_generated",
			SourceRef: "binding-rjb-5b:u1:l1:q1", PromptMarkdown: "同步题：12 + 8 = ?",
			ExpectedAnswer: "20", EvidenceRefs: []string{"segment:u1:l1"},
			EstimatedSeconds: 60,
		}}, nil
	case k12.WeeklySectionArithmeticWarmup:
		return []usecase.WeeklyPracticeCandidate{{
			SourceKind: "arithmetic_rule", GenerationMethod: "rule_generated",
			SourceRef: "arith:20-7", PromptMarkdown: "20 - 7 = ?",
			ExpectedAnswer: "13", EvidenceRefs: []string{"rule:integer-subtraction"},
			EstimatedSeconds: 20,
		}}, nil
	default:
		return nil, fmt.Errorf("unexpected section %s", req.PlanSection)
	}
}

type weeklyAssessmentStub struct{}

func (weeklyAssessmentStub) AssessWeeklyPracticeAnswer(
	_ context.Context,
	req usecase.WeeklyPracticeAnswerRequest,
) (usecase.WeeklyPracticeAnswerAssessment, error) {
	result := k12.WeeklyAttemptWrong
	if req.StudentAnswer == "20" || req.StudentAnswer == "13" {
		result = k12.WeeklyAttemptCorrect
	}
	return usecase.WeeklyPracticeAnswerAssessment{
		AssessmentID: "assessment-" + req.Item.ItemID, Result: result,
		VerificationEvidence: "system_verified:test-assessor",
		Subject:              "数学", KnowledgePoint: "整数计算",
	}, nil
}

type weeklyClock struct{ now int64 }

func newWeeklyContractServer(
	t *testing.T,
) (http.Handler, usecase.Deps, *weeklyClock) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming'),('other')`); err != nil {
		t.Fatal(err)
	}
	delivery := &httpReceiptTransport{send: []usecase.DeliveryTransportAck{{
		Status: k12.DeliveryDelivered, ExternalMessageID: "weekly-message-1",
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
	clock := &weeklyClock{now: 1785081600} // 2026-07-27T00:00:00+08:00
	rt.Deps.Now = func() int64 { return clock.now }
	rt.Deps.WeeklyCurriculum = weeklyCatalogStub{}
	rt.Deps.WeeklyCandidates = weeklyCandidateStub{}
	rt.Deps.WeeklyAssessment = weeklyAssessmentStub{}
	return apihttp.NewHandler(apihttp.Runtime{
		Views: rt.Registry.Views, Records: rt.Records, Deps: rt.Deps,
	}), rt.Deps, clock
}

const weeklyBundleManifestID = "manifest-weekly-contract"

func newWeeklyBundleContractServer(
	t *testing.T,
) (http.Handler, usecase.Deps, *weeklyClock) {
	t.Helper()
	h, deps, clock := newWeeklyContractServer(t)
	seedBUG20260726034A02Manifest(
		t, deps.Records.DB(), weeklyBundleManifestID, "desktop-user",
		"doc-weekly-contract", 1, "ready_for_confirmation", "",
	)
	return h, deps, clock
}

func exactKeys(t *testing.T, got map[string]any, want ...string) {
	t.Helper()
	actual := make([]string, 0, len(got))
	for key := range got {
		actual = append(actual, key)
	}
	sort.Strings(actual)
	sort.Strings(want)
	if fmt.Sprint(actual) != fmt.Sprint(want) {
		t.Fatalf("top-level keys=%v want exact %v; body=%v", actual, want, got)
	}
}

func weeklyBundleBody(key string, profileRevision, progressRevision, settingsRevision int) string {
	return fmt.Sprintf(`{
		"agent":"mingming",
		"idempotency_key":%q,
		"expected_profile_revision":%d,
		"expected_progress_revision":%d,
		"expected_settings_revision":%d,
		"profile":{
			"child_name":"明明",
			"grade_term":"五年级下",
			"subject_textbooks":{
				"math":"人教版",
				"chinese":"统编版",
				"english":"外研版",
				"science":"教科版",
				"information_technology":"浙教版",
				"art":"人美版"
			}
		},
		"curriculum_progress":{
			"subject":"math",
			"textbook_manifest_id":%q,
			"volume":"下册",
			"unit_id":"u1",
			"lesson_id":"l1",
			"page_from":1,
			"page_to":10,
			"evidence_source":"parent_confirmed"
		},
		"weekly_practice_settings":{
			"timezone":"Asia/Shanghai",
			"textbook_consolidation_enabled":true,
			"arithmetic_warmup_enabled":true,
			"arithmetic_minutes":2
		}
	}`, key, profileRevision, progressRevision, settingsRevision, weeklyBundleManifestID)
}

func seedWeeklyDueMistake(t *testing.T, deps usecase.Deps) {
	t.Helper()
	rec, err := k12.NewMistakeRecord("mingming", "weekly-contract", k12.MistakeFields{
		GradeTerm: "五年级下", Subject: "数学", Question: "到期错题：3 × 4 = ?",
		CanonicalAnswer: "12", KnowledgePoint: "整数计算",
		EntrySource: k12.MistakeEntryPhoto,
	})
	if err != nil {
		t.Fatal(err)
	}
	due := int64(1785081500)
	rec.DueAt = &due
	if _, err := deps.Records.Put(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
}

func trackItem(
	t *testing.T,
	plan map[string]any,
	section string,
) map[string]any {
	t.Helper()
	tracks, _ := plan["tracks"].([]any)
	for _, raw := range tracks {
		track, _ := raw.(map[string]any)
		if track["plan_section"] != section {
			continue
		}
		items, _ := track["items"].([]any)
		if len(items) == 0 {
			t.Fatalf("section %s has no item: %v", section, track)
		}
		item, _ := items[0].(map[string]any)
		return item
	}
	t.Fatalf("missing section %s: %v", section, tracks)
	return nil
}

func TestWeeklyPracticeHTTPContract_K12Weekly016To023(t *testing.T) {
	h, deps, clock := newWeeklyBundleContractServer(t)

	rec, catalog := do(t, h, http.MethodGet,
		"/curriculum-catalog?agent=mingming&subject=math&textbook_edition=%E4%BA%BA%E6%95%99%E7%89%88&volume=%E4%B8%8B%E5%86%8C", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, catalog, "agent", "subject", "textbook_binding_id",
		"textbook_edition", "textbook_version", "title", "volume", "page_min",
		"page_max", "units")
	if rec, _ := do(t, h, http.MethodGet,
		"/curriculum-catalog?agent=ghost&subject=math&textbook_edition=x&volume=x", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("catalog unknown owner status=%d want 404", rec.Code)
	}

	rec, progressEnvelope := do(t, h, http.MethodGet,
		"/curriculum-progress?agent=other&subject=math", "")
	if rec.Code != http.StatusOK || progressEnvelope["progress"] != nil ||
		progressEnvelope["revision"] != float64(0) {
		t.Fatalf("unset progress must be 200 null: status=%d body=%v", rec.Code, progressEnvelope)
	}
	exactKeys(t, progressEnvelope, "progress", "revision")
	rec, defaultSettings := do(t, h, http.MethodGet,
		"/weekly-practice/settings?agent=other", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("default settings status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, defaultSettings, "agent", "revision", "timezone",
		"due_review_enabled", "textbook_consolidation_enabled",
		"textbook_consolidation_tier",
		"arithmetic_warmup_enabled", "arithmetic_minutes", "created_at", "updated_at")
	if defaultSettings["revision"] != float64(0) ||
		defaultSettings["due_review_enabled"] != true ||
		defaultSettings["textbook_consolidation_enabled"] != false ||
		defaultSettings["textbook_consolidation_tier"] != "standard" ||
		defaultSettings["arithmetic_warmup_enabled"] != false ||
		defaultSettings["arithmetic_minutes"] != float64(2) ||
		defaultSettings["timezone"] != "Asia/Shanghai" {
		t.Fatalf("settings defaults drifted: %v", defaultSettings)
	}

	bundleBody := weeklyBundleBody("bundle-1", 0, 0, 0)
	rec, bundle := do(t, h, http.MethodPut, "/profile-bundle", bundleBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, bundle, "profile", "curriculum_progress",
		"weekly_practice_settings", "replayed")
	if bundle["replayed"] != false {
		t.Fatalf("first bundle must not replay: %v", bundle)
	}
	bundleProfile := bundle["profile"].(map[string]any)
	exactKeys(t, bundleProfile, "child_name", "grade_term",
		"subject_textbooks", "textbook_edition")
	bundleTextbooks := bundleProfile["subject_textbooks"].(map[string]any)
	exactKeys(t, bundleTextbooks, "math", "chinese", "english", "science",
		"information_technology", "art")
	if bundleProfile["textbook_edition"] != bundleTextbooks["math"] {
		t.Fatalf("bundle scalar is not derived math: %v", bundleProfile)
	}
	rec, replayBundle := do(t, h, http.MethodPut, "/profile-bundle", bundleBody)
	if rec.Code != http.StatusOK || replayBundle["replayed"] != true {
		t.Fatalf("bundle replay status=%d body=%v", rec.Code, replayBundle)
	}
	rec, _ = do(t, h, http.MethodPut, "/profile-bundle",
		weeklyBundleBody("bundle-stale", 0, 0, 0))
	if rec.Code != http.StatusConflict {
		t.Fatalf("three-CAS stale bundle status=%d want 409", rec.Code)
	}
	_, profile := do(t, h, http.MethodGet, "/profile?agent=mingming", "")
	_, persistedProgress := do(t, h, http.MethodGet,
		"/curriculum-progress?agent=mingming&subject=math", "")
	_, persistedSettings := do(t, h, http.MethodGet,
		"/weekly-practice/settings?agent=mingming", "")
	if profile["revision"] != float64(1) ||
		persistedProgress["progress"].(map[string]any)["revision"] != float64(1) ||
		persistedProgress["revision"] != float64(1) ||
		persistedSettings["revision"] != float64(1) {
		t.Fatalf("failed CAS made a partial write: profile=%v progress=%v settings=%v",
			profile, persistedProgress, persistedSettings)
	}

	seedWeeklyDueMistake(t, deps)
	rec, planEnvelope := do(t, h, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"plan-week-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan create status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, planEnvelope, "plan", "replayed")
	plan := planEnvelope["plan"].(map[string]any)
	exactKeys(t, plan, "plan_id", "agent", "revision", "iso_week_year",
		"iso_week_number", "timezone", "week_start", "week_end",
		"local_start_date", "local_end_date", "status", "settings_revision",
		"curriculum_progress_revision", "tracks", "manual_track_recommendations",
		"created_at", "updated_at")
	if len(plan["tracks"].([]any)) != 3 {
		t.Fatalf("plan must expose exact three tracks: %v", plan["tracks"])
	}
	planID := plan["plan_id"].(string)
	planRevision := int(plan["revision"].(float64))
	rec, replayPlan := do(t, h, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"plan-week-1"}`)
	if rec.Code != http.StatusOK || replayPlan["replayed"] != true {
		t.Fatalf("plan replay status=%d body=%v", rec.Code, replayPlan)
	}
	rec, current := do(t, h, http.MethodGet,
		"/weekly-practice/plans/current?agent=mingming", "")
	if rec.Code != http.StatusOK || current["plan"] == nil {
		t.Fatalf("current plan status=%d body=%v", rec.Code, current)
	}
	exactKeys(t, current, "plan")
	rec, noCurrent := do(t, h, http.MethodGet,
		"/weekly-practice/plans/current?agent=other", "")
	if rec.Code != http.StatusOK || noCurrent["plan"] != nil {
		t.Fatalf("missing current must be 200 null: %d %v", rec.Code, noCurrent)
	}
	rec, emptyHistory := do(t, h, http.MethodGet,
		"/weekly-practice/plans/history?agent=mingming", "")
	if rec.Code != http.StatusOK || emptyHistory["next_cursor"] != nil ||
		len(emptyHistory["items"].([]any)) != 0 {
		t.Fatalf("empty history exact contract: status=%d body=%v", rec.Code, emptyHistory)
	}
	exactKeys(t, emptyHistory, "items", "next_cursor")
	if rec, _ := do(t, h, http.MethodGet,
		"/weekly-practice/plans/history?agent=mingming&limit=101", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("history invalid limit status=%d want 400", rec.Code)
	}

	prepareBody := fmt.Sprintf(
		`{"agent":"mingming","expected_revision":%d,"idempotency_key":"prepare-1"}`,
		planRevision)
	rec, prepared := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/prepare-output", prepareBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, prepared, "snapshot", "artifact")
	snapshot := prepared["snapshot"].(map[string]any)
	artifact := prepared["artifact"].(map[string]any)
	exactKeys(t, artifact, "artifact_id", "source_kind", "source_ref", "title",
		"source_digest", "format", "render_contract_version", "content_type",
		"byte_digest", "byte_size")
	snapshotID := snapshot["snapshot_id"].(string)
	artifactID := artifact["artifact_id"].(string)
	rec, replayPrepared := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/prepare-output", prepareBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("prepare replay status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, replayPrepared, "snapshot", "artifact")
	if replayPrepared["snapshot"].(map[string]any)["snapshot_id"] != snapshotID ||
		replayPrepared["artifact"].(map[string]any)["byte_digest"] != artifact["byte_digest"] {
		t.Fatalf("prepare replay drifted: first=%v replay=%v", prepared, replayPrepared)
	}
	rec, fetchedSnapshot := do(t, h, http.MethodGet,
		"/weekly-practice/snapshots/"+snapshotID+"?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot GET status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, fetchedSnapshot, "snapshot_id", "artifact_id", "plan_id", "plan_revision",
		"agent", "iso_week_year", "iso_week_number", "timezone", "week_start",
		"week_end", "local_start_date", "local_end_date", "settings_revision",
		"curriculum_progress_revision", "tracks", "render_version",
		"snapshot_digest", "created_at")
	if rec, _ := do(t, h, http.MethodGet,
		"/weekly-practice/snapshots/"+snapshotID+"?agent=other", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("snapshot cross-owner status=%d want 404", rec.Code)
	}

	printBody := fmt.Sprintf(
		`{"agent":"mingming","idempotency_key":"weekly-print-1","artifact_id":%q}`,
		artifactID)
	rec, printEnvelope := do(t, h, http.MethodPost, "/print-jobs", printBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("artifact print status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, printEnvelope, "print_job", "replayed")
	if printEnvelope["print_job"].(map[string]any)["artifact_id"] != artifactID {
		t.Fatalf("print job did not reuse artifact: %v", printEnvelope)
	}
	rec, _ = do(t, h, http.MethodPost, "/print-jobs", fmt.Sprintf(
		`{"agent":"mingming","idempotency_key":"weekly-print-bad","artifact_id":%q,"canonical_markdown":"x"}`,
		artifactID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mixed print variants status=%d want 400", rec.Code)
	}

	rec, sent := do(t, h, http.MethodPost,
		"/weekly-practice/snapshots/"+snapshotID+"/send",
		`{"agent":"mingming","idempotency_key":"send-1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, sent, "batch_id", "agent_name", "object_kind", "object_id",
		"dedupe_key", "content_digest", "status", "created_at", "updated_at",
		"receipts")

	syncItem := trackItem(t, plan, k12.WeeklySectionTextbookConsolidation)
	itemID := syncItem["item_id"].(string)
	attemptPath := "/weekly-practice/snapshots/" + snapshotID + "/attempts"
	attemptBody := fmt.Sprintf(
		`{"agent":"mingming","item_id":%q,"student_answer":"19","idempotency_key":"attempt-1"}`,
		itemID)
	rec, attemptEnvelope := do(t, h, http.MethodPost, attemptPath, attemptBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("attempt status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, attemptEnvelope, "attempt", "replayed")
	exactKeys(t, attemptEnvelope["attempt"].(map[string]any), "attempt_id",
		"snapshot_id", "item_id", "assessment_id", "result",
		"verification_evidence", "mistake_record_id", "review_scheduled", "created_at")
	rec, replayAttempt := do(t, h, http.MethodPost, attemptPath, attemptBody)
	if rec.Code != http.StatusOK || replayAttempt["replayed"] != true {
		t.Fatalf("attempt replay status=%d body=%v", rec.Code, replayAttempt)
	}
	rec, _ = do(t, h, http.MethodPost, attemptPath, fmt.Sprintf(
		`{"agent":"mingming","item_id":%q,"student_answer":"18","idempotency_key":"attempt-1"}`,
		itemID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("attempt key/different answer status=%d want 409", rec.Code)
	}

	rec, setsBeforeSave := do(t, h, http.MethodGet,
		"/practice-sets?agent=mingming", "")
	if rec.Code != http.StatusOK || len(setsBeforeSave["items"].([]any)) != 0 {
		t.Fatalf("prepare/send/attempt must not create PracticeSet: %v", setsBeforeSave)
	}
	saveBody := fmt.Sprintf(
		`{"agent":"mingming","expected_revision":%d,"idempotency_key":"save-1"}`,
		planRevision)
	rec, saved := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/save-to-practice-set", saveBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("save status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, saved, "receipt", "replayed")
	exactKeys(t, saved["receipt"].(map[string]any), "save_receipt_id", "plan_id",
		"plan_revision", "snapshot_id", "practice_set_id", "created_at")
	rec, replaySave := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/save-to-practice-set", saveBody)
	if rec.Code != http.StatusOK || replaySave["replayed"] != true {
		t.Fatalf("save replay status=%d body=%v", rec.Code, replaySave)
	}

	clock.now += 8 * 86400
	rec, _ = do(t, h, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"plan-week-2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("next-week plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, history := do(t, h, http.MethodGet,
		"/weekly-practice/plans/history?agent=mingming&limit=1", "")
	if rec.Code != http.StatusOK || len(history["items"].([]any)) != 1 {
		t.Fatalf("archived history status=%d body=%v", rec.Code, history)
	}
	exactKeys(t, history, "items", "next_cursor")
	exactKeys(t, history["items"].([]any)[0].(map[string]any), "snapshot_id",
		"artifact_id", "plan_id", "iso_week_year", "iso_week_number", "timezone",
		"local_start_date", "local_end_date", "item_count", "correct_count",
		"wrong_count", "needs_review_count", "archived_at")
}
