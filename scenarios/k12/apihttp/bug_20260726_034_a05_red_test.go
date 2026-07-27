package apihttp_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

func TestBUG20260726034A05ArchivedHistorySummaryCountsAssessmentResults(t *testing.T) {
	h, _, clock := newWeeklyContractServer(t)
	if rec, _ := do(t, h, http.MethodPut, "/profile-bundle",
		weeklyBundleBody("a05-bundle", 0, 0, 0)); rec.Code != http.StatusOK {
		t.Fatalf("profile-bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, envelope := do(t, h, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"a05-plan-week-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	plan := envelope["plan"].(map[string]any)
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	rec, prepared := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/prepare-output",
		fmt.Sprintf(`{"agent":"mingming","expected_revision":%d,
			"idempotency_key":"a05-prepare"}`, revision))
	if rec.Code != http.StatusCreated {
		t.Fatalf("prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	snapshotID := prepared["snapshot"].(map[string]any)["snapshot_id"].(string)
	itemID := trackItem(t, plan, k12.WeeklySectionTextbookConsolidation)["item_id"].(string)
	rec, attempt := do(t, h, http.MethodPost,
		"/weekly-practice/snapshots/"+snapshotID+"/attempts",
		fmt.Sprintf(`{"agent":"mingming","item_id":%q,
			"student_answer":"19","idempotency_key":"a05-wrong"}`, itemID))
	if rec.Code != http.StatusCreated ||
		attempt["attempt"].(map[string]any)["result"] != k12.WeeklyAttemptWrong {
		t.Fatalf("wrong assessment status=%d body=%v", rec.Code, attempt)
	}

	clock.now += 8 * 86400
	if rec, _ := do(t, h, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"a05-plan-week-2"}`); rec.Code != http.StatusCreated {
		t.Fatalf("next-week plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, history := do(t, h, http.MethodGet,
		"/weekly-practice/plans/history?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", rec.Code, rec.Body.String())
	}
	items, _ := history["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("history items=%v", items)
	}
	summary := items[0].(map[string]any)
	exactKeys(t, summary, "snapshot_id", "artifact_id", "plan_id", "iso_week_year",
		"iso_week_number", "timezone", "local_start_date", "local_end_date",
		"item_count", "correct_count", "wrong_count", "needs_review_count",
		"archived_at")
	if summary["correct_count"] != float64(0) ||
		summary["wrong_count"] != float64(1) ||
		summary["needs_review_count"] != float64(0) {
		t.Fatalf("history assessment counts drifted: %v", summary)
	}
}
