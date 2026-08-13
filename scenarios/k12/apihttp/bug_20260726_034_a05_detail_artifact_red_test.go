package apihttp_test

import (
	"fmt"
	"net/http"
	"testing"
)

type a05RequestLedger struct {
	next  http.Handler
	calls []string
}

func (l *a05RequestLedger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	l.calls = append(l.calls, r.Method+" "+r.URL.Path)
	l.next.ServeHTTP(w, r)
}

func prepareArchivedWeeklyArtifact(
	t *testing.T,
) (*a05RequestLedger, string, string) {
	t.Helper()
	h, _, clock := newWeeklyBundleContractServer(t)
	ledger := &a05RequestLedger{next: h}
	if rec, _ := do(t, ledger, http.MethodPut, "/profile-bundle",
		weeklyBundleBody("a05-artifact-bundle", 0, 0, 0)); rec.Code != http.StatusOK {
		t.Fatalf("profile-bundle status=%d body=%s", rec.Code, rec.Body.String())
	}
	rec, envelope := do(t, ledger, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"a05-artifact-plan"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plan status=%d body=%s", rec.Code, rec.Body.String())
	}
	plan := envelope["plan"].(map[string]any)
	planID := plan["plan_id"].(string)
	revision := int(plan["revision"].(float64))
	rec, prepared := do(t, ledger, http.MethodPost,
		"/weekly-practice/plans/"+planID+"/prepare-output",
		fmt.Sprintf(`{"agent":"mingming","expected_revision":%d,
			"idempotency_key":"a05-artifact-prepare"}`, revision))
	if rec.Code != http.StatusCreated {
		t.Fatalf("prepare status=%d body=%s", rec.Code, rec.Body.String())
	}
	snapshot := prepared["snapshot"].(map[string]any)
	artifact := prepared["artifact"].(map[string]any)
	snapshotID := snapshot["snapshot_id"].(string)
	artifactID := artifact["artifact_id"].(string)
	if artifactID == "" {
		t.Fatal("prepared artifact_id is empty")
	}
	clock.now += 8 * 86400
	if rec, _ := do(t, ledger, http.MethodPost, "/weekly-practice/plans",
		`{"agent":"mingming","idempotency_key":"a05-artifact-next-week"}`); rec.Code != http.StatusCreated {
		t.Fatalf("archive trigger status=%d body=%s", rec.Code, rec.Body.String())
	}
	return ledger, snapshotID, artifactID
}

func TestBUG20260726034A05DetailArtifactHistoryAndSnapshotShareArtifactID(t *testing.T) {
	ledger, snapshotID, artifactID := prepareArchivedWeeklyArtifact(t)
	rec, history := do(t, ledger, http.MethodGet,
		"/weekly-practice/plans/history?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history status=%d body=%s", rec.Code, rec.Body.String())
	}
	items := history["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("history items=%v", items)
	}
	summary := items[0].(map[string]any)
	if summary["snapshot_id"] != snapshotID || summary["artifact_id"] != artifactID {
		t.Errorf("history immutable artifact binding=%v want snapshot=%s artifact=%s",
			summary, snapshotID, artifactID)
	}
	rec, snapshot := do(t, ledger, http.MethodGet,
		"/weekly-practice/snapshots/"+snapshotID+"?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rec.Code, rec.Body.String())
	}
	if snapshot["artifact_id"] != artifactID {
		t.Errorf("snapshot artifact_id=%v want %s; snapshot=%v",
			snapshot["artifact_id"], artifactID, snapshot)
	}
}

func TestBUG20260726034A05DetailArtifactViewUsesOnlySnapshotGET(t *testing.T) {
	ledger, snapshotID, artifactID := prepareArchivedWeeklyArtifact(t)
	ledger.calls = nil
	rec, snapshot := do(t, ledger, http.MethodGet,
		"/weekly-practice/snapshots/"+snapshotID+"?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("view snapshot status=%d body=%s", rec.Code, rec.Body.String())
	}
	if snapshot["artifact_id"] != artifactID {
		t.Errorf("view resolved artifact_id=%v want %s", snapshot["artifact_id"], artifactID)
	}
	wantCall := "GET /weekly-practice/snapshots/" + snapshotID
	if len(ledger.calls) != 1 || ledger.calls[0] != wantCall {
		t.Errorf("view network ledger=%v want exactly [%s]", ledger.calls, wantCall)
	}
	for _, call := range ledger.calls {
		if call == "POST /weekly-practice/plans/"+snapshotID+"/prepare-output" {
			t.Errorf("history view invoked prepare-output: %v", ledger.calls)
		}
	}
	rec, _ = do(t, ledger, http.MethodGet,
		"/weekly-practice/snapshots/"+snapshotID+"?agent=other", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-owner snapshot status=%d want 404 body=%s",
			rec.Code, rec.Body.String())
	}
}
