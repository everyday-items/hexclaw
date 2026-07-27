package apihttp_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type deterministicCandidateEngine struct {
	mu   sync.Mutex
	next int
}

func (e *deterministicCandidateEngine) GeneratePracticeVariant(
	context.Context, string, string, string,
) (usecase.SolveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.next++
	value := e.next
	return usecase.SolveResult{Solution: fmt.Sprintf(
		"## 问题\n变式练习 %d\n\n## 解答\n验算过程 %d\n\n## 答案\n答案 %d",
		value, value, value,
	)}, nil
}

func (e *deterministicCandidateEngine) Solve(
	_ context.Context,
	problem, _, _ string,
) (usecase.SolveResult, error) {
	fields := strings.Fields(problem)
	value := fields[len(fields)-1]
	return usecase.SolveResult{Solution: fmt.Sprintf(
		"## 问题\n%s\n\n## 解答\n独立验算\n\n## 答案\n答案 %s",
		problem, value,
	)}, nil
}

type candidateReviewHTTPFixture struct {
	handler http.Handler
	deps    usecase.Deps
	db      *sql.DB
}

func newCandidateReviewHTTPFixture(t *testing.T) candidateReviewHTTPFixture {
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
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('mingming')`); err != nil {
		t.Fatal(err)
	}
	k, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	engine := &deterministicCandidateEngine{}
	k.Deps.Solver = engine
	k.Deps.PracticeVariant = engine
	k.Deps.PracticeGenerationRoute = func(
		_ context.Context,
		requested k12.GradingModelSnapshot,
	) (k12.GradingModelSnapshot, error) {
		if requested.Provider == "" {
			requested.Provider = "test-provider"
			requested.Model = "test-model"
		}
		requested.Route = requested.Provider + "/" + requested.Model
		requested.Capability = "text"
		return requested, nil
	}
	fixed, err := time.Parse(time.RFC3339, "2026-07-27T12:00:00+08:00")
	if err != nil {
		t.Fatal(err)
	}
	k.Deps.Now = func() int64 { return fixed.Unix() }
	return candidateReviewHTTPFixture{
		handler: apihttp.NewHandler(apihttp.Runtime{
			Views: k.Registry.Views, Records: k.Records, Deps: k.Deps,
		}),
		deps: k.Deps,
		db:   db,
	}
}

func seedCandidateReviewMistake(
	t *testing.T,
	fixture candidateReviewHTTPFixture,
	recordID, question string,
) {
	t.Helper()
	record, err := k12.NewMistakeRecord("mingming", "session-1", k12.MistakeFields{
		Subject: "数学", Question: question, KnowledgePoint: "整数加法",
		CanonicalAnswer: "5", ErrorCause: "计算失误",
		EntrySource: k12.MistakeEntryPhoto,
	})
	if err != nil {
		t.Fatal(err)
	}
	record.RecordID = recordID
	due := int64(1_700_000_000)
	record.DueAt = &due
	if _, err := fixture.deps.Records.Put(context.Background(), record); err != nil {
		t.Fatal(err)
	}
}

func TestPracticeCandidateSelectionHTTPGeneratesBatchesAndCommitsOnce(t *testing.T) {
	fixture := newCandidateReviewHTTPFixture(t)
	seedCandidateReviewMistake(t, fixture, "mistake-candidates", "2 + 3 = ?")

	rec, body := do(t, fixture.handler, http.MethodPost,
		"/mistakes/mistake-candidates/practice-candidate-selection",
		`{"agent":"mingming","idempotency_key":"open-1","grade":"五年级下",
		  "textbook":"人教版","provider":"test-provider","model":"test-model"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("open status=%d body=%s", rec.Code, rec.Body.String())
	}
	candidates := body["candidates"].([]any)
	if len(candidates) != 4 {
		t.Fatalf("initial candidates=%d, want original+3", len(candidates))
	}
	if candidates[0].(map[string]any)["candidate_kind"] != k12.PracticeCandidateOriginal {
		t.Fatalf("first candidate=%v, want original", candidates[0])
	}
	for _, raw := range candidates {
		if raw.(map[string]any)["state"] != k12.PracticeCandidateReady {
			t.Fatalf("candidate not ready: %v", raw)
		}
	}
	selectionID := body["selection_id"].(string)
	revision := int(body["revision"].(float64))
	rec, replayedOpen := do(t, fixture.handler, http.MethodPost,
		"/mistakes/mistake-candidates/practice-candidate-selection",
		`{"agent":"mingming","idempotency_key":"open-1","grade":"五年级下",
		  "textbook":"人教版","provider":"test-provider","model":"test-model"}`)
	if rec.Code != http.StatusOK ||
		replayedOpen["selection_id"] != selectionID ||
		len(replayedOpen["candidates"].([]any)) != 4 {
		t.Fatalf("open replay status=%d body=%v", rec.Code, replayedOpen)
	}
	rec, body = do(t, fixture.handler, http.MethodPost,
		"/practice-candidate-selections/"+selectionID+"/batches",
		fmt.Sprintf(`{"agent":"mingming","revision":%d,
			"idempotency_key":"batch-2"}`, revision))
	if rec.Code != http.StatusOK {
		t.Fatalf("batch status=%d body=%s", rec.Code, rec.Body.String())
	}
	candidates = body["candidates"].([]any)
	if len(candidates) != 7 {
		t.Fatalf("after second batch=%d, want original+6", len(candidates))
	}
	revision = int(body["revision"].(float64))
	candidateIDs := make([]string, 0, len(candidates))
	for _, raw := range candidates {
		candidateIDs = append(candidateIDs,
			raw.(map[string]any)["candidate_id"].(string))
	}
	idsJSON, _ := json.Marshal(candidateIDs)
	commitBody := fmt.Sprintf(`{"agent":"mingming","revision":%d,
		"candidate_ids":%s,"idempotency_key":"commit-1"}`, revision, idsJSON)
	rec, body = do(t, fixture.handler, http.MethodPost,
		"/practice-candidate-selections/"+selectionID+"/commit", commitBody)
	if rec.Code != http.StatusOK || int(body["added_count"].(float64)) != 7 {
		t.Fatalf("commit status=%d body=%v", rec.Code, body)
	}
	rec, replay := do(t, fixture.handler, http.MethodPost,
		"/practice-candidate-selections/"+selectionID+"/commit", commitBody)
	if rec.Code != http.StatusOK || replay["replayed"] != true ||
		int(replay["added_count"].(float64)) != 7 {
		t.Fatalf("commit replay status=%d body=%v", rec.Code, replay)
	}
	var total, distinct int
	if err := fixture.db.QueryRow(`SELECT COUNT(*),
		COUNT(DISTINCT normalized_content_hash)
		FROM k12_practice_set_items`).Scan(&total, &distinct); err != nil {
		t.Fatal(err)
	}
	if total != 7 || distinct != 7 {
		t.Fatalf("practice items total=%d distinct=%d, want 7/7", total, distinct)
	}
}

func TestMistakeReviewHTTPDefersSuppressesAndRestoresWithoutMastery(t *testing.T) {
	fixture := newCandidateReviewHTTPFixture(t)
	seedCandidateReviewMistake(t, fixture, "mistake-defer-http", "8 + 7 = ?")
	seedCandidateReviewMistake(t, fixture, "mistake-suppress-http", "9 + 6 = ?")
	plan := k12.WeeklyPracticePlan{
		PlanID: "plan-review-http", AgentName: "mingming",
		ISOWeekYear: 2026, ISOWeekNumber: 31, Timezone: "Asia/Shanghai",
		WeekStart: 1_722_182_400, WeekEnd: 1_722_787_199,
		LocalStartDate: "2026-07-27", LocalEndDate: "2026-08-02",
		Status: k12.WeeklyPlanDraft, SettingsRevision: 0,
		Tracks: []k12.WeeklyPracticeTrack{{
			PlanSection: k12.WeeklySectionDueReview,
			Status:      k12.WeeklyTrackReady,
			Items: []k12.WeeklyPracticeItem{{
				ItemID: "weekly-defer-item", Position: 1,
				PlanSection:    k12.WeeklySectionDueReview,
				SourceKind:     "mistake",
				SourceRef:      "mistake-defer-http",
				PromptMarkdown: "8 + 7 = ?",
			}},
		}},
		CreatedAt: 1_722_182_400, UpdatedAt: 1_722_182_400,
		AnswerKeys: map[string]string{},
	}
	storedPlan, _, err := fixture.deps.Records.UpsertWeeklyPracticePlan(
		context.Background(), plan, "plan-review-command", "plan-review-digest",
	)
	if err != nil {
		t.Fatal(err)
	}
	rec, result := do(t, fixture.handler, http.MethodPost,
		"/mistakes/mistake-defer-http/defer-this-week",
		fmt.Sprintf(`{"agent":"mingming","version":0,
			"plan_id":%q,"plan_revision":%d,"weekly_item_id":"weekly-defer-item",
			"iso_year":2026,"iso_week":31,"idempotency_key":"defer-http"}`,
			storedPlan.PlanID, storedPlan.Revision))
	if rec.Code != http.StatusOK ||
		result["state"] != k12.MistakeReviewDeferredThisWeek {
		t.Fatalf("defer status=%d body=%v", rec.Code, result)
	}
	projected, err := fixture.deps.Records.GetWeeklyPracticePlan(
		context.Background(), "mingming", storedPlan.PlanID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Tracks[0].Items) != 0 {
		t.Fatalf("deferred item remained in current plan: %+v", projected.Tracks[0].Items)
	}
	var status string
	var parentConfirmed, lastRetried int64
	if err := fixture.db.QueryRow(`SELECT status,parent_confirmed_at,last_retried_at
		FROM k12_mistakes WHERE record_id='mistake-defer-http'`).
		Scan(&status, &parentConfirmed, &lastRetried); err != nil {
		t.Fatal(err)
	}
	if status == k12.StatusMastered || parentConfirmed != 0 || lastRetried != 0 {
		t.Fatalf("defer forged mastery status=%s parent=%d retried=%d",
			status, parentConfirmed, lastRetried)
	}

	rec, suppressed := do(t, fixture.handler, http.MethodPost,
		"/mistakes/mistake-suppress-http/suppress",
		`{"agent":"mingming","version":0,"idempotency_key":"suppress-http"}`)
	if rec.Code != http.StatusOK ||
		suppressed["review_state"] != k12.MistakeReviewSuppressed {
		t.Fatalf("suppress status=%d body=%v", rec.Code, suppressed)
	}
	suppressedVersion := int(suppressed["version"].(float64))
	rec, listed := do(t, fixture.handler, http.MethodGet,
		"/mistakes?agent=mingming&status=suppressed", "")
	if rec.Code != http.StatusOK || len(listed["items"].([]any)) != 1 {
		t.Fatalf("suppressed filter status=%d body=%v", rec.Code, listed)
	}
	rec, restored := do(t, fixture.handler, http.MethodPost,
		"/mistakes/mistake-suppress-http/restore-review",
		fmt.Sprintf(`{"agent":"mingming","version":%d,
			"idempotency_key":"restore-http"}`, suppressedVersion))
	if rec.Code != http.StatusOK ||
		restored["review_state"] != k12.MistakeReviewScheduled {
		t.Fatalf("restore status=%d body=%v", rec.Code, restored)
	}
}
