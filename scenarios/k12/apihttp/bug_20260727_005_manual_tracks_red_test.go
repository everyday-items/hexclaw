package apihttp_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type manualTrackCandidateSource struct {
	requests []usecase.WeeklyPracticeCandidateRequest
	failNext bool
}

func (s *manualTrackCandidateSource) GenerateWeeklyPracticeCandidates(
	_ context.Context,
	req usecase.WeeklyPracticeCandidateRequest,
) ([]usecase.WeeklyPracticeCandidate, error) {
	s.requests = append(s.requests, req)
	if s.failNext {
		s.failNext = false
		return nil, fmt.Errorf("injected manual track failure")
	}
	count := req.MaxItems
	if count < 1 {
		count = 1
	}
	out := make([]usecase.WeeklyPracticeCandidate, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, usecase.WeeklyPracticeCandidate{
			SourceKind:       "manual-test",
			GenerationMethod: k12.WeeklyGenerationMethodRuleGenerated,
			SourceRef:        fmt.Sprintf("%s:%d:%d", req.PlanSection, req.ArithmeticMinutes, i),
			PromptMarkdown:   fmt.Sprintf("手动练习 %d", i+1),
			ExpectedAnswer:   fmt.Sprint(i + 1),
			EvidenceRefs:     []string{"manual:test"},
			EstimatedSeconds: 30,
		})
	}
	return out, nil
}

func newManualTrackContractServer(
	t *testing.T,
) (http.Handler, *manualTrackCandidateSource) {
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
	rt, err := assembly.Wire(
		db,
		fakeSolveExec{},
		assembly.WithAccumulationMetadataDeriver(fixedAccumulationMetadataDeriver{}),
		assembly.WithRenderer(fixedPDFRenderer{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	candidates := &manualTrackCandidateSource{}
	rt.Deps.Now = func() int64 { return 1785081600 }
	rt.Deps.WeeklyCurriculum = weeklyCatalogStub{}
	rt.Deps.WeeklyCandidates = candidates
	rt.Deps.WeeklyAssessment = weeklyAssessmentStub{}
	seedBUG20260726034A02Manifest(
		t, db, weeklyBundleManifestID, "desktop-user", "doc-weekly-contract",
		1, "ready_for_confirmation", "",
	)
	return apihttp.NewHandler(apihttp.Runtime{
		Views: rt.Registry.Views, Records: rt.Records, Deps: rt.Deps,
	}), candidates
}

func disabledManualTracksBundleBody(key string) string {
	body := weeklyBundleBody(key, 0, 0, 0)
	body = strings.Replace(body,
		`"textbook_consolidation_enabled":true`,
		`"textbook_consolidation_enabled":false`, 1)
	body = strings.Replace(body,
		`"arithmetic_warmup_enabled":true`,
		`"arithmetic_warmup_enabled":false`, 1)
	return body
}

func ensureManualTrackPlan(
	t *testing.T,
	h http.Handler,
	key string,
) map[string]any {
	t.Helper()
	rec, envelope := do(t, h, http.MethodPost, "/weekly-practice/plans",
		fmt.Sprintf(`{"agent":"mingming","idempotency_key":%q}`, key))
	if rec.Code != http.StatusCreated {
		t.Fatalf("ensure plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	return envelope["plan"].(map[string]any)
}

func manualRecommendation(
	t *testing.T,
	plan map[string]any,
	track string,
) map[string]any {
	t.Helper()
	recommendations, ok := plan["manual_track_recommendations"].(map[string]any)
	if !ok {
		t.Fatalf("plan missing manual_track_recommendations: %v", plan)
	}
	got, ok := recommendations[track].(map[string]any)
	if !ok {
		t.Fatalf("recommendation %s missing: %v", track, recommendations)
	}
	return got
}

func TestBUG20260727005DefaultSettingsAndPlanExposeManualRecommendations(t *testing.T) {
	h, candidates := newManualTrackContractServer(t)

	rec, settings := do(t, h, http.MethodGet,
		"/weekly-practice/settings?agent=other", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, settings, "agent", "revision", "timezone", "due_review_enabled",
		"textbook_consolidation_enabled", "textbook_consolidation_tier",
		"arithmetic_warmup_enabled", "arithmetic_minutes", "created_at", "updated_at")
	if settings["textbook_consolidation_enabled"] != false ||
		settings["textbook_consolidation_tier"] != "standard" ||
		settings["arithmetic_warmup_enabled"] != false ||
		settings["arithmetic_minutes"] != float64(2) {
		t.Fatalf("manual defaults drifted: %v", settings)
	}

	rec, envelope := do(t, h, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"other","idempotency_key":"manual-default-plan"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(candidates.requests) != 0 {
		t.Fatalf("disabled auto prepare called candidate source: %v", candidates.requests)
	}
	plan := envelope["plan"].(map[string]any)
	exactKeys(t, plan, "plan_id", "agent", "revision", "iso_week_year",
		"iso_week_number", "timezone", "week_start", "week_end",
		"local_start_date", "local_end_date", "status", "settings_revision",
		"curriculum_progress_revision", "tracks", "manual_track_recommendations",
		"created_at", "updated_at")

	sync := manualRecommendation(t, plan, "textbook_consolidation")
	exactKeys(t, sync, "availability", "selected_item_count",
		"recommended_item_count", "min_item_count", "max_item_count")
	if sync["availability"] != "setup_required" ||
		sync["selected_item_count"] != float64(5) ||
		sync["recommended_item_count"] != float64(5) ||
		sync["min_item_count"] != float64(1) ||
		sync["max_item_count"] != float64(10) {
		t.Fatalf("sync recommendation=%v", sync)
	}

	arithmetic := manualRecommendation(t, plan, "arithmetic_warmup")
	exactKeys(t, arithmetic, "availability", "selected_item_count",
		"recommended_item_count", "min_item_count", "max_item_count")
	if arithmetic["selected_item_count"] != float64(10) ||
		arithmetic["recommended_item_count"] != float64(10) ||
		arithmetic["min_item_count"] != float64(1) ||
		arithmetic["max_item_count"] != float64(20) {
		t.Fatalf("arithmetic default=%v", arithmetic)
	}
}

func trackBySection(t *testing.T, plan map[string]any, section string) map[string]any {
	t.Helper()
	tracks, ok := plan["tracks"].([]any)
	if !ok {
		t.Fatalf("plan tracks=%#v", plan["tracks"])
	}
	for _, raw := range tracks {
		track, ok := raw.(map[string]any)
		if ok && track["plan_section"] == section {
			return track
		}
	}
	t.Fatalf("track %q not found in %#v", section, tracks)
	return nil
}

func TestBUG20260727005ManualTextbookPrepareUsesItemCountAndIsIdempotent(t *testing.T) {
	h, candidates := newManualTrackContractServer(t)
	if rec, _ := do(t, h, http.MethodPut, "/profile-bundle",
		disabledManualTracksBundleBody("manual-sync-bundle")); rec.Code != http.StatusOK {
		t.Fatalf("profile-bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	plan := ensureManualTrackPlan(t, h, "manual-sync-plan")
	if len(candidates.requests) != 0 {
		t.Fatalf("ensure called supplement source with auto prepare disabled")
	}
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	body := fmt.Sprintf(`{"plan_revision":%d,"item_count":8,
		"idempotency_key":"manual-sync-eight"}`, revision)
	path := "/weekly-practice/plans/" + planID + "/tracks/textbook_consolidation/prepare"

	rec, first := do(t, h, http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, first, "plan", "replayed")
	if len(candidates.requests) != 1 ||
		candidates.requests[0].MaxItems != 8 {
		t.Fatalf("candidate requests=%v want=8", candidates.requests)
	}
	firstPlan := first["plan"].(map[string]any)

	rec, replay := do(t, h, http.MethodPost, path, body)
	if rec.Code != http.StatusOK || replay["replayed"] != true {
		t.Fatalf("replay status=%d body=%v", rec.Code, replay)
	}
	if len(candidates.requests) != 1 ||
		replay["plan"].(map[string]any)["revision"] != firstPlan["revision"] {
		t.Fatalf("replay generated again: requests=%v body=%v", candidates.requests, replay)
	}

	rec, _ = do(t, h, http.MethodPost, path, fmt.Sprintf(
		`{"plan_revision":%d,"item_count":9,"idempotency_key":"manual-sync-eight"}`,
		revision))
	if rec.Code != http.StatusConflict {
		t.Fatalf("same key different item_count status=%d want 409", rec.Code)
	}
}

func TestBUG20260727005ManualTextbookFailurePreservesReadyRevision(t *testing.T) {
	h, candidates := newManualTrackContractServer(t)
	if rec, _ := do(t, h, http.MethodPut, "/profile-bundle",
		disabledManualTracksBundleBody("manual-preserve-bundle")); rec.Code != http.StatusOK {
		t.Fatalf("profile-bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	plan := ensureManualTrackPlan(t, h, "manual-preserve-plan")
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	path := "/weekly-practice/plans/" + planID + "/tracks/textbook_consolidation/prepare"
	rec, first := do(t, h, http.MethodPost, path, fmt.Sprintf(
		`{"plan_revision":%d,"item_count":5,"idempotency_key":"manual-ready"}`,
		revision))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	readyPlan := first["plan"].(map[string]any)
	readyRevision := int(readyPlan["revision"].(float64))
	readyTrack := trackBySection(t, readyPlan, k12.WeeklySectionTextbookConsolidation)
	readyItems := len(readyTrack["items"].([]any))

	candidates.failNext = true
	rec, _ = do(t, h, http.MethodPost, path, fmt.Sprintf(
		`{"plan_revision":%d,"item_count":8,"idempotency_key":"manual-more-fails"}`,
		readyRevision))
	if rec.Code < 400 {
		t.Fatalf("failed generation status=%d want error", rec.Code)
	}
	rec, current := do(t, h, http.MethodGet,
		"/weekly-practice/plans/current?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("current status=%d body=%s", rec.Code, rec.Body.String())
	}
	currentPlan := current["plan"].(map[string]any)
	currentTrack := trackBySection(t, currentPlan, k12.WeeklySectionTextbookConsolidation)
	if int(currentPlan["revision"].(float64)) != readyRevision ||
		currentTrack["status"] != k12.WeeklyTrackReady ||
		len(currentTrack["items"].([]any)) != readyItems {
		t.Fatalf("failed increment replaced ready plan: before=%v after=%v",
			readyPlan, currentPlan)
	}
}

func TestBUG20260727005ArithmeticCreateUsesPerBatchItemCount(t *testing.T) {
	h, candidates := newManualTrackContractServer(t)
	if rec, _ := do(t, h, http.MethodPut, "/profile-bundle",
		disabledManualTracksBundleBody("manual-arithmetic-bundle")); rec.Code != http.StatusOK {
		t.Fatalf("profile-bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	plan := ensureManualTrackPlan(t, h, "manual-arithmetic-plan")
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	path := "/weekly-practice/plans/" + planID + "/arithmetic-batches"
	body := fmt.Sprintf(`{"plan_revision":%d,"item_count":15,
		"idempotency_key":"manual-arithmetic-15"}`, revision)

	rec, first := do(t, h, http.MethodPost, path, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	exactKeys(t, first, "batch", "replayed")
	if len(candidates.requests) != 1 ||
		candidates.requests[0].MaxItems != 15 {
		t.Fatalf("arithmetic request=%v", candidates.requests)
	}

	rec, replay := do(t, h, http.MethodPost, path, body)
	if rec.Code != http.StatusOK || replay["replayed"] != true ||
		len(candidates.requests) != 1 {
		t.Fatalf("replay status=%d body=%v requests=%v", rec.Code, replay, candidates.requests)
	}
	rec, _ = do(t, h, http.MethodPost, path, fmt.Sprintf(
		`{"plan_revision":%d,"item_count":16,"idempotency_key":"manual-arithmetic-15"}`,
		revision))
	if rec.Code != http.StatusConflict {
		t.Fatalf("same key different item_count status=%d want 409", rec.Code)
	}
	for _, itemCount := range []int{0, 21} {
		rec, _ = do(t, h, http.MethodPost, path, fmt.Sprintf(
			`{"plan_revision":%d,"item_count":%d,"idempotency_key":"invalid-%d"}`,
			revision, itemCount, itemCount))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("item_count=%d status=%d want 400", itemCount, rec.Code)
		}
	}
}

func TestBUG20260727005ManualCommandsRejectLegacyOwnershipAndRevisionFields(t *testing.T) {
	h, candidates := newManualTrackContractServer(t)
	plan := ensureManualTrackPlan(t, h, "manual-legacy-fields-plan")
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))

	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "textbook_legacy_tier",
			path: "/weekly-practice/plans/" + planID + "/tracks/textbook_consolidation/prepare",
			body: fmt.Sprintf(
				`{"plan_revision":%d,"item_count":5,"tier":"standard","idempotency_key":"legacy-sync-tier"}`,
				revision),
		},
		{
			name: "textbook_expected_revision",
			path: "/weekly-practice/plans/" + planID + "/tracks/textbook_consolidation/prepare",
			body: fmt.Sprintf(
				`{"plan_revision":%d,"item_count":5,"idempotency_key":"legacy-sync-revision","expected_revision":%d}`,
				revision, revision),
		},
		{
			name: "arithmetic_legacy_minutes",
			path: "/weekly-practice/plans/" + planID + "/arithmetic-batches",
			body: fmt.Sprintf(
				`{"plan_revision":%d,"item_count":10,"minutes":2,"idempotency_key":"legacy-arithmetic-minutes"}`,
				revision),
		},
		{
			name: "arithmetic_expected_revision",
			path: "/weekly-practice/plans/" + planID + "/arithmetic-batches",
			body: fmt.Sprintf(
				`{"plan_revision":%d,"item_count":10,"idempotency_key":"legacy-arithmetic-revision","expected_revision":%d}`,
				revision, revision),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rec, _ := do(t, h, http.MethodPost, test.path, test.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("legacy field status=%d want 400 body=%s",
					rec.Code, rec.Body.String())
			}
		})
	}
	if len(candidates.requests) != 0 {
		t.Fatalf("rejected legacy commands reached candidate source: %v", candidates.requests)
	}
}
