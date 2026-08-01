package apihttp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type problemSourceActionSeed struct {
	fixture    imageTaskHTTPFixture
	dispatchID string
	jobID      string
	problemID  string
}

func seedProblemSourceActionHTTP(t *testing.T) problemSourceActionSeed {
	t.Helper()
	fixture := newImageTaskHTTPFixture(t)
	ctx := context.Background()
	const (
		dispatchID   = "dispatch-source-action"
		submissionID = "submission-source-action"
		problemID    = "problem-source-action"
		attemptID    = "attempt-source-action"
	)

	deps := usecase.Deps{Records: fixture.coordinator.Records}
	policy := k12.ApprovedRecognizingRequestPolicy()
	job, _, err := deps.CreateGradingJob(ctx, "mingming", "session-source-action",
		usecase.CreateGradingJobInput{
			SubmissionID:     submissionID,
			SourceKind:       "desktop",
			SourceKey:        "source-action",
			ConfirmedVersion: 1,
			ModelSnapshot: k12.GradingModelSnapshot{
				Provider:                 "hexclaw-gpt",
				Model:                    k12.RecognizingPolicyModel,
				Route:                    "hexclaw-gpt/" + k12.RecognizingPolicyModel,
				RecognizingRequestPolicy: policy,
			},
		})
	if err != nil {
		t.Fatalf("seed grading job: %v", err)
	}

	if _, err := fixture.db.ExecContext(ctx, `
		INSERT INTO k12_image_task_dispatches (
			dispatch_id,agent_name,learner_id,source_kind,source_ref,source_session_id,
			source_asset_refs_json,source_digest,message_intent,task_intent,
			intent_evidence_json,intent_confidence,confirmation_candidates_json,status,
			target_object_type,target_object_id,classification_route_snapshot_json,
			classification_invocation_id,route_policy_snapshot_json,idempotency_key,
			request_digest,attempt_generation,retry_safe,failure_kind,version,created_at,updated_at
		) VALUES (
			?,'mingming','learner-source-action','desktop','message-source-action','session-source-action',
			'["asset://mingming/source-action.png"]','sha256:source-action','请批改','completed_homework',
			'["test"]',1,'[]','routed','homework_submission',?,'{}','invocation-source-action',
			'{}','dispatch-source-action-key','sha256:dispatch-source-action',1,0,'',1,100,100
		)`, dispatchID, submissionID); err != nil {
		t.Fatalf("seed image dispatch: %v", err)
	}
	if _, err := fixture.db.ExecContext(ctx, `
		INSERT INTO k12_homework_submissions (
			submission_id,dispatch_id,agent_name,learner_id,source_kind,source_ref,
			source_asset_refs_json,task_intent,status,grading_job_id,idempotency_key,
			version,created_at,updated_at
		) VALUES (
			?,?,'mingming','learner-source-action','desktop','message-source-action',
			'["asset://mingming/source-action.png"]','completed_homework',
			'awaiting_confirmation',?,'submission-source-action-key',1,100,100
		)`, submissionID, dispatchID, job.Record.RecordID); err != nil {
		t.Fatalf("seed homework submission: %v", err)
	}
	if err := fixture.coordinator.Records.PutProblemAttemptSnapshot(ctx,
		k12.ProblemAttemptSnapshot{
			Problems: []k12.Problem{{
				ProblemID: problemID, AgentName: "mingming", SubmissionID: submissionID,
				PageAssetID: "asset://mingming/source-action.png", Ordinal: 0,
				ProblemKind: k12.ProblemKindStandalone, Subject: "数学",
				StemRaw: "1+1=", StemMarkdown: "1+1=", ConfirmationRequired: true,
				ConfirmationReasons: []string{"source_unclear"}, CanonicalVersion: 1,
			}},
			Attempts: []k12.Attempt{{
				AttemptID: attemptID, AgentName: "mingming", SubmissionID: submissionID,
				ProblemID: problemID, AnswerState: "unclear", ConfirmedVersion: 1,
				InputDigest: "sha256:source-action-input",
			}},
		}); err != nil {
		t.Fatalf("seed problem/attempt: %v", err)
	}
	return problemSourceActionSeed{
		fixture: fixture, dispatchID: dispatchID,
		jobID: job.Record.RecordID, problemID: problemID,
	}
}

func postProblemSourceAction(
	t *testing.T,
	handler http.Handler,
	dispatchID, problemID, idempotencyKey, body string,
) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost,
		"/image-tasks/"+dispatchID+"/problems/"+problemID+"/source-actions",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

const validSkipSourceActionBody = `{
	"action":"skip",
	"structure_version":1,
	"expected_input_revision":1,
	"payload":{}
}`

func TestBUG_20260726_031_ProblemSourceActionValidSkipReturnsReceiptAndSnapshot(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	rec, out := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "skip-command-1", validSkipSourceActionBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid skip = %d, want 200; body=%#v", rec.Code, out)
	}
	if out["command_receipt_id"] == "" ||
		out["input_revision"].(float64) != 1 ||
		out["progressive_snapshot"] == nil {
		t.Fatalf("skip response missing durable receipt/revision/snapshot: %#v", out)
	}
	var assessmentCount int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_grading_assessment_items
		WHERE agent_name='mingming' AND job_id=? AND problem_id=?`,
		seed.jobID, seed.problemID).Scan(&assessmentCount); err != nil {
		t.Fatal(err)
	}
	if assessmentCount != 0 {
		t.Fatalf("skip must not create assessment/model side effects: %d", assessmentCount)
	}
	var mistakeCount, reviewCount, learningEventCount int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_mistakes WHERE agent_name='mingming'`,
	).Scan(&mistakeCount); err != nil {
		t.Fatal(err)
	}
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_mistakes
		WHERE agent_name='mingming' AND due_at IS NOT NULL`,
	).Scan(&reviewCount); err != nil {
		t.Fatal(err)
	}
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM outbox_events
		WHERE agent_name='mingming' AND event_type='k12.mistake.recorded'`,
	).Scan(&learningEventCount); err != nil {
		t.Fatal(err)
	}
	if mistakeCount != 0 || reviewCount != 0 || learningEventCount != 0 {
		t.Fatalf(
			"skip polluted learning facts: mistakes=%d reviews=%d learning_events=%d",
			mistakeCount, reviewCount, learningEventCount,
		)
	}
}

func TestBUG_20260726_031_ProblemSourceActionSameCommand100ConcurrentOneReceipt(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	const requests = 100
	type response struct {
		code int
		body map[string]any
	}
	results := make(chan response, requests)
	var wg sync.WaitGroup
	wg.Add(requests)
	for i := 0; i < requests; i++ {
		go func() {
			defer wg.Done()
			rec, out := postProblemSourceAction(t, seed.fixture.handler,
				seed.dispatchID, seed.problemID, "skip-command-100", validSkipSourceActionBody)
			results <- response{code: rec.Code, body: out}
		}()
	}
	wg.Wait()
	close(results)

	receiptID := ""
	for result := range results {
		if result.code != http.StatusOK {
			t.Fatalf("concurrent skip = %d, want 200; body=%#v", result.code, result.body)
		}
		current, _ := result.body["command_receipt_id"].(string)
		if current == "" {
			t.Fatalf("concurrent skip missing receipt: %#v", result.body)
		}
		if receiptID == "" {
			receiptID = current
		} else if current != receiptID {
			t.Fatalf("same command produced multiple receipts: %q != %q", current, receiptID)
		}
	}
}

func TestBUG_20260726_031_ProblemSourceAction100DistinctCommandsHaveOneCASWinner(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	const requests = 100
	type response struct {
		code int
		body map[string]any
	}
	results := make(chan response, requests)
	var wg sync.WaitGroup
	wg.Add(requests)
	for i := 0; i < requests; i++ {
		i := i
		go func() {
			defer wg.Done()
			rec, out := postProblemSourceAction(t, seed.fixture.handler,
				seed.dispatchID, seed.problemID,
				fmt.Sprintf("skip-cas-%03d", i), validSkipSourceActionBody)
			results <- response{code: rec.Code, body: out}
		}()
	}
	wg.Wait()
	close(results)

	successes, conflicts := 0, 0
	for result := range results {
		switch result.code {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("concurrent distinct skip = %d, want 200/409; body=%#v",
				result.code, result.body)
		}
	}
	if successes != 1 || conflicts != requests-1 {
		t.Fatalf("CAS winners=%d conflicts=%d, want 1/%d",
			successes, conflicts, requests-1)
	}
	var currentSkips, commandReceipts int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_problem_skip_receipts
		WHERE agent_name='mingming' AND job_id=? AND problem_id=?
		  AND current_disposition='current'`,
		seed.jobID, seed.problemID,
	).Scan(&currentSkips); err != nil {
		t.Fatal(err)
	}
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_problem_source_action_receipts
		WHERE agent_name='mingming' AND job_id=? AND problem_id=?`,
		seed.jobID, seed.problemID,
	).Scan(&commandReceipts); err != nil {
		t.Fatal(err)
	}
	if currentSkips != 1 || commandReceipts != 1 {
		t.Fatalf("CAS durable rows: current_skips=%d command_receipts=%d, want 1/1",
			currentSkips, commandReceipts)
	}
}

func TestBUG_20260726_031_ProblemSourceActionIdempotentReplayAndDigestConflict(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	firstRec, first := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "skip-command-replay", validSkipSourceActionBody)
	replayRec, replay := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "skip-command-replay", validSkipSourceActionBody)
	if firstRec.Code != http.StatusOK || replayRec.Code != http.StatusOK ||
		!reflect.DeepEqual(first, replay) {
		t.Fatalf("same key+digest must replay exact 200: first=%d %#v replay=%d %#v",
			firstRec.Code, first, replayRec.Code, replay)
	}
	conflictRec, conflict := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "skip-command-replay", `{
			"action":"resume",
			"structure_version":1,
			"expected_input_revision":1,
			"payload":{}
		}`)
	if conflictRec.Code != http.StatusConflict {
		t.Fatalf("same key+different digest = %d, want 409; body=%#v",
			conflictRec.Code, conflict)
	}
}

func TestBUG_20260726_031_ProblemSourceActionRejectsStaleAndMismatchedScope(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	for _, tc := range []struct {
		name     string
		dispatch string
		problem  string
		body     string
		want     int
	}{
		{
			name: "stale structure", dispatch: seed.dispatchID, problem: seed.problemID,
			body: `{"action":"skip","structure_version":2,"expected_input_revision":1,"payload":{}}`,
			want: http.StatusConflict,
		},
		{
			name: "stale input revision", dispatch: seed.dispatchID, problem: seed.problemID,
			body: `{"action":"skip","structure_version":1,"expected_input_revision":2,"payload":{}}`,
			want: http.StatusConflict,
		},
		{
			name: "dispatch problem mismatch", dispatch: seed.dispatchID, problem: "problem-from-other-dispatch",
			body: validSkipSourceActionBody, want: http.StatusNotFound,
		},
		{
			name: "identity pollution", dispatch: seed.dispatchID, problem: seed.problemID,
			body: `{
				"action":"skip","structure_version":1,"expected_input_revision":1,
				"agent":"gege","owner":"other-owner","payload":{}
			}`,
			want: http.StatusBadRequest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, out := postProblemSourceAction(t, seed.fixture.handler,
				tc.dispatch, tc.problem, "scope-"+tc.name, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("%s = %d, want %d; body=%#v", tc.name, rec.Code, tc.want, out)
			}
		})
	}
}

func TestBUG_20260726_031_ProblemSourceActionUsesTrustedRemotePrincipal(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	remoteHandler := func(authenticatedOwner string) http.Handler {
		return apihttp.NewHandler(apihttp.Runtime{
			Records:       seed.fixture.coordinator.Records,
			ImageTasks:    seed.fixture.coordinator,
			PrincipalMode: "remote",
			AuthenticatedOwnerScope: func(context.Context) (string, error) {
				return authenticatedOwner, nil
			},
		})
	}

	rec, out := postProblemSourceAction(t, remoteHandler("gege"),
		seed.dispatchID, seed.problemID, "cross-agent-skip", validSkipSourceActionBody)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-agent principal status=%d want 404 body=%#v", rec.Code, out)
	}
	var skipCount, commandCount int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_problem_skip_receipts
		WHERE job_id=? AND problem_id=?`,
		seed.jobID, seed.problemID,
	).Scan(&skipCount); err != nil {
		t.Fatal(err)
	}
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_problem_source_action_receipts
		WHERE job_id=? AND problem_id=?`,
		seed.jobID, seed.problemID,
	).Scan(&commandCount); err != nil {
		t.Fatal(err)
	}
	if skipCount != 0 || commandCount != 0 {
		t.Fatalf("cross-agent principal wrote skip=%d command=%d", skipCount, commandCount)
	}

	rec, out = postProblemSourceAction(t, remoteHandler("mingming"),
		seed.dispatchID, seed.problemID, "trusted-agent-skip", validSkipSourceActionBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("trusted remote principal status=%d want 200 body=%#v", rec.Code, out)
	}
}

func TestBUG_20260726_031_ProblemSourceActionResumeSupersedesSkipWithNewRevision(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	skipRec, skip := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "skip-before-resume", validSkipSourceActionBody)
	if skipRec.Code != http.StatusOK {
		t.Fatalf("skip before resume = %d; body=%#v", skipRec.Code, skip)
	}
	resumeRec, resumed := postProblemSourceAction(t, seed.fixture.handler,
		seed.dispatchID, seed.problemID, "resume-after-skip", `{
			"action":"resume",
			"structure_version":1,
			"expected_input_revision":1,
			"payload":{}
		}`)
	if resumeRec.Code != http.StatusOK ||
		resumed["input_revision"].(float64) != 2 ||
		resumed["progressive_snapshot"] == nil {
		t.Fatalf("resume must create revision 2 snapshot: status=%d body=%#v",
			resumeRec.Code, resumed)
	}
	var current, superseded int
	if err := seed.fixture.db.QueryRow(`
		SELECT
			SUM(CASE WHEN current_disposition='current' THEN 1 ELSE 0 END),
			SUM(CASE WHEN current_disposition='superseded' THEN 1 ELSE 0 END)
		FROM k12_problem_skip_receipts
		WHERE agent_name='mingming' AND job_id=? AND problem_id=?`,
		seed.jobID, seed.problemID).Scan(&current, &superseded); err != nil {
		t.Fatal(err)
	}
	if current != 0 || superseded != 1 {
		t.Fatalf("resume must supersede the one skip head: current=%d superseded=%d",
			current, superseded)
	}
}
