package apihttp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

func prepareA03Snapshot(
	t *testing.T,
	h http.Handler,
) (attemptPath, attemptBody, snapshotID string) {
	t.Helper()
	if rec, _ := do(t, h, http.MethodPut, "/profile-bundle",
		weeklyBundleBody("a03-bundle", 0, 0, 0)); rec.Code != http.StatusOK {
		t.Fatalf("profile-bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, envelope := do(t, h, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"a03-plan"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	plan := envelope["plan"].(map[string]any)
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	rec, prepared := do(t, h, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/prepare-output",
		fmt.Sprintf(`{"agent":"mingming","expected_revision":%d,
			"idempotency_key":"a03-prepare"}`, revision))
	if rec.Code != http.StatusCreated {
		t.Fatalf("prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	snapshotID = prepared["snapshot"].(map[string]any)["snapshot_id"].(string)
	itemID := trackItem(t, plan, k12.WeeklySectionTextbookConsolidation)["item_id"].(string)
	attemptPath = "/weekly-practice/snapshots/" + snapshotID + "/attempts"
	attemptBody = fmt.Sprintf(`{"agent":"mingming","item_id":%q,
		"student_answer":"19","idempotency_key":"a03-attempt"}`, itemID)
	return attemptPath, attemptBody, snapshotID
}

func TestBUG20260726034A03FormalAssemblyProvidesAssessorOrFailsClosed(t *testing.T) {
	_, deps, _ := newWeeklyContractServer(t)
	rt, err := assembly.Wire(
		deps.Records.DB(),
		fakeSolveExec{},
		assembly.WithAccumulationMetadataDeriver(fixedAccumulationMetadataDeriver{}),
		assembly.WithRenderer(fixedPDFRenderer{}),
	)
	if err != nil {
		return
	}
	if rt.Deps.WeeklyAssessment == nil {
		t.Fatal("formal K12 assembly started without WeeklyPracticeAnswerAssessor")
	}
}

func TestBUG20260726034A03MissingAssessorFailsClosedWithoutPseudoAttempt(t *testing.T) {
	setupHandler, deps, _ := newWeeklyBundleContractServer(t)
	path, body, snapshotID := prepareA03Snapshot(t, setupHandler)
	deps.WeeklyAssessment = nil
	h := apihttp.NewHandler(apihttp.Runtime{Records: deps.Records, Deps: deps})

	rec, out := do(t, h, http.MethodPost, path, body)
	if rec.Code < http.StatusBadRequest {
		t.Errorf("missing assessor returned pseudo-success status=%d body=%v", rec.Code, out)
	}
	if attempt, ok := out["attempt"].(map[string]any); ok &&
		attempt["verification_evidence"] == "assessment_unavailable" {
		t.Errorf("missing assessor persisted forbidden assessment_unavailable attempt: %v", attempt)
	}
	var count int
	if err := deps.Records.DB().QueryRow(
		`SELECT COUNT(*) FROM k12_weekly_practice_attempts WHERE snapshot_id=?`,
		snapshotID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("missing assessor wrote attempts=%d want 0", count)
	}
}

type a03SlowWrongAssessor struct {
	calls atomic.Int64
}

func (a *a03SlowWrongAssessor) AssessWeeklyPracticeAnswer(
	_ context.Context,
	req usecase.WeeklyPracticeAnswerRequest,
) (usecase.WeeklyPracticeAnswerAssessment, error) {
	a.calls.Add(1)
	time.Sleep(75 * time.Millisecond)
	return usecase.WeeklyPracticeAnswerAssessment{
		AssessmentID:         "assessment-a03-" + req.Item.ItemID,
		Result:               k12.WeeklyAttemptWrong,
		VerificationEvidence: "system_verified:a03",
		Subject:              "数学",
		KnowledgePoint:       "整数计算",
	}, nil
}

type a03HTTPResult struct {
	code int
	body map[string]any
	err  error
}

func TestBUG20260726034A03ConcurrentReplayAndRestartUseOneAssessmentAndEffect(t *testing.T) {
	setupHandler, deps, _ := newWeeklyBundleContractServer(t)
	path, body, snapshotID := prepareA03Snapshot(t, setupHandler)
	assessor := &a03SlowWrongAssessor{}
	deps.WeeklyAssessment = assessor
	h := apihttp.NewHandler(apihttp.Runtime{Records: deps.Records, Deps: deps})

	const concurrency = 20
	start := make(chan struct{})
	results := make(chan a03HTTPResult, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			var decoded map[string]any
			err := json.Unmarshal(rec.Body.Bytes(), &decoded)
			results <- a03HTTPResult{code: rec.Code, body: decoded, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	attemptIDs := map[string]struct{}{}
	assessmentIDs := map[string]struct{}{}
	mistakeIDs := map[string]struct{}{}
	created, replayed := 0, 0
	for result := range results {
		if result.err != nil {
			t.Errorf("decode response: %v", result.err)
			continue
		}
		if result.code != http.StatusCreated && result.code != http.StatusOK {
			t.Errorf("concurrent status=%d body=%v", result.code, result.body)
			continue
		}
		if result.code == http.StatusCreated {
			created++
		} else {
			replayed++
		}
		attempt, ok := result.body["attempt"].(map[string]any)
		if !ok {
			t.Errorf("missing attempt: %v", result.body)
			continue
		}
		attemptID, _ := attempt["attempt_id"].(string)
		assessmentID, _ := attempt["assessment_id"].(string)
		mistakeID, _ := attempt["mistake_record_id"].(string)
		if attemptID == "" || assessmentID == "" || mistakeID == "" ||
			attempt["result"] != k12.WeeklyAttemptWrong ||
			attempt["review_scheduled"] != true {
			t.Errorf("incomplete committed attempt/effect: %v", attempt)
		}
		attemptIDs[attemptID] = struct{}{}
		assessmentIDs[assessmentID] = struct{}{}
		mistakeIDs[mistakeID] = struct{}{}
	}
	if created != 1 || replayed != concurrency-1 {
		t.Errorf("statuses created=%d replayed=%d", created, replayed)
	}
	if calls := assessor.calls.Load(); calls != 1 {
		t.Errorf("physical assessor calls=%d want 1", calls)
	}
	if len(attemptIDs) != 1 || len(assessmentIDs) != 1 || len(mistakeIDs) != 1 {
		t.Errorf("identities attempts=%v assessments=%v mistakes=%v",
			attemptIDs, assessmentIDs, mistakeIDs)
	}
	var stored int
	if err := deps.Records.DB().QueryRow(
		`SELECT COUNT(*) FROM k12_weekly_practice_attempts WHERE snapshot_id=?`,
		snapshotID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Errorf("stored attempts=%d want 1", stored)
	}

	restarted := apihttp.NewHandler(apihttp.Runtime{Records: deps.Records, Deps: deps})
	rec, out := do(t, restarted, http.MethodPost, path, body)
	if rec.Code != http.StatusOK || out["replayed"] != true {
		t.Errorf("restart replay status=%d body=%v", rec.Code, out)
	}
	if calls := assessor.calls.Load(); calls != 1 {
		t.Errorf("restart replay repeated assessor; calls=%d", calls)
	}
}
