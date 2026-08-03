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
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type problemSourceActionSeed struct {
	fixture    imageTaskHTTPFixture
	dispatchID string
	jobID      string
	problemID  string
}

func seedProblemSourceActionHTTP(t *testing.T) problemSourceActionSeed {
	return seedProblemSourceActionHTTPWithSnapshot(t, func(pageAssetID string) (k12.ProblemAttemptSnapshot, string) {
		const (
			problemID = "problem-source-action"
			attemptID = "attempt-source-action"
		)
		return k12.ProblemAttemptSnapshot{
			Problems: []k12.Problem{{
				ProblemID: problemID, AgentName: "mingming", SubmissionID: "submission-source-action",
				PageAssetID: pageAssetID, Ordinal: 0,
				ProblemKind: k12.ProblemKindStandalone, Subject: "数学",
				StemRaw: "1+1=", StemMarkdown: "1+1=", ConfirmationRequired: true,
				ConfirmationReasons: []string{"source_unclear"}, CanonicalVersion: 1,
			}},
			Attempts: []k12.Attempt{{
				AttemptID: attemptID, AgentName: "mingming", SubmissionID: "submission-source-action",
				ProblemID: problemID, AnswerState: "present", AnswerRaw: "2", AnswerMarkdown: "2",
				BBox:             &k12.AttemptBBox{X: 0.1, Y: 0.2, W: 0.3, H: 0.1},
				ConfirmedVersion: 1, InputDigest: "sha256:source-action-input",
			}},
		}, problemID
	})
}

func seedGroupedProblemSourceActionHTTP(t *testing.T) problemSourceActionSeed {
	return seedProblemSourceActionHTTPWithSnapshot(t, func(pageAssetID string) (k12.ProblemAttemptSnapshot, string) {
		const (
			parentID = "problem-source-parent"
			child1ID = "problem-source-child-1"
			child2ID = "problem-source-child-2"
		)
		return k12.ProblemAttemptSnapshot{
			Problems: []k12.Problem{
				{
					ProblemID: parentID, AgentName: "mingming", SubmissionID: "submission-source-action",
					PageAssetID: pageAssetID, Ordinal: 0, ProblemKind: k12.ProblemKindCompoundParent,
					Subject: "数学", StemRaw: "读图回答两个小题", StemMarkdown: "读图回答两个小题",
					CanonicalVersion: 1,
				},
				{
					ProblemID: child1ID, AgentName: "mingming", SubmissionID: "submission-source-action",
					PageAssetID: pageAssetID, Ordinal: 1, ProblemKind: k12.ProblemKindSubproblem,
					ParentProblemID: parentID, SubproblemNo: "1", Subject: "数学",
					StemRaw: "第一问", StemMarkdown: "第一问", ConfirmationRequired: true,
					ConfirmationReasons: []string{"source_unclear"}, CanonicalVersion: 1,
				},
				{
					ProblemID: child2ID, AgentName: "mingming", SubmissionID: "submission-source-action",
					PageAssetID: pageAssetID, Ordinal: 2, ProblemKind: k12.ProblemKindSubproblem,
					ParentProblemID: parentID, SubproblemNo: "2", Subject: "数学",
					StemRaw: "第二问", StemMarkdown: "第二问", ConfirmationRequired: true,
					ConfirmationReasons: []string{"source_unclear"}, CanonicalVersion: 1,
				},
			},
			Attempts: []k12.Attempt{
				{
					AttemptID: "attempt-source-child-1", AgentName: "mingming",
					SubmissionID: "submission-source-action", ProblemID: child1ID,
					AnswerState: "present", AnswerRaw: "11", AnswerMarkdown: "11",
					BBox:             &k12.AttemptBBox{X: 0.1, Y: 0.2, W: 0.3, H: 0.1},
					ConfirmedVersion: 1, InputDigest: "sha256:source-child-1-input",
				},
				{
					AttemptID: "attempt-source-child-2", AgentName: "mingming",
					SubmissionID: "submission-source-action", ProblemID: child2ID,
					AnswerState: "present", AnswerRaw: "22", AnswerMarkdown: "22",
					BBox:             &k12.AttemptBBox{X: 0.5, Y: 0.6, W: 0.3, H: 0.1},
					ConfirmedVersion: 1, InputDigest: "sha256:source-child-2-input",
				},
			},
		}, child1ID
	})
}

func seedProblemSourceActionHTTPWithSnapshot(
	t *testing.T,
	build func(pageAssetID string) (k12.ProblemAttemptSnapshot, string),
) problemSourceActionSeed {
	t.Helper()
	fixture := newImageTaskHTTPFixture(t)
	ctx := context.Background()
	const (
		dispatchID   = "dispatch-source-action"
		submissionID = "submission-source-action"
	)
	assetAgent, assetFile, err := assetstore.Parse(fixture.assetID)
	if err != nil {
		t.Fatalf("parse source PageAsset fixture: %v", err)
	}
	assetBytes, _, err := assetstore.Read(assetAgent, assetFile)
	if err != nil {
		t.Fatalf("read source PageAsset fixture: %v", err)
	}
	ready, err := (&usecase.PageAssetRepository{Records: fixture.coordinator.Records}).Persist(
		ctx,
		usecase.DefaultLocalOwnerScope,
		assetAgent,
		assetBytes,
	)
	if err != nil || ready.Metadata.PageAssetID != fixture.assetID {
		t.Fatalf("prepare ready source PageAsset fixture: ready=%#v err=%v", ready, err)
	}
	snapshot, problemID := build(fixture.assetID)

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
			?,'sha256:source-action','请批改','completed_homework',
			'["test"]',1,'[]','routed','homework_submission',?,'{}','invocation-source-action',
			'{}','dispatch-source-action-key','sha256:dispatch-source-action',1,0,'',1,100,100
		)`, dispatchID, mustJSONSourceActionTest(t, []string{fixture.assetID}), submissionID); err != nil {
		t.Fatalf("seed image dispatch: %v", err)
	}
	if _, err := fixture.db.ExecContext(ctx, `
		INSERT INTO k12_homework_submissions (
			submission_id,dispatch_id,agent_name,learner_id,source_kind,source_ref,
			source_asset_refs_json,task_intent,status,grading_job_id,idempotency_key,
			version,created_at,updated_at
		) VALUES (
			?,?,'mingming','learner-source-action','desktop','message-source-action',
			?,'completed_homework',
			'awaiting_confirmation',?,'submission-source-action-key',1,100,100
		)`, submissionID, dispatchID, mustJSONSourceActionTest(t, []string{fixture.assetID}), job.Record.RecordID); err != nil {
		t.Fatalf("seed homework submission: %v", err)
	}
	if err := fixture.coordinator.Records.PutProblemAttemptSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("seed problem/attempt: %v", err)
	}
	return problemSourceActionSeed{
		fixture: fixture, dispatchID: dispatchID,
		jobID: job.Record.RecordID, problemID: problemID,
	}
}

func mustJSONSourceActionTest(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
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

func TestBUG_20260726_031_ProblemSourceActionDigestUsesCommittedPayloadSemantics(t *testing.T) {
	seed := seedProblemSourceActionHTTP(t)
	const padded = `{
		"action":"correct_text",
		"structure_version":1,
		"expected_input_revision":1,
		"payload":{
			"question_canonical_markdown":"  2+3=  ",
			"answer_canonical_markdown":"  5  "
		}
	}`
	const normalized = `{
		"action":"correct_text",
		"structure_version":1,
		"expected_input_revision":1,
		"payload":{
			"question_canonical_markdown":"2+3=",
			"answer_canonical_markdown":"5"
		}
	}`
	firstRec, first := postProblemSourceAction(
		t, seed.fixture.handler, seed.dispatchID, seed.problemID,
		"semantic-payload-replay", padded,
	)
	replayRec, replay := postProblemSourceAction(
		t, seed.fixture.handler, seed.dispatchID, seed.problemID,
		"semantic-payload-replay", normalized,
	)
	if firstRec.Code != http.StatusOK || replayRec.Code != http.StatusOK ||
		!reflect.DeepEqual(first, replay) {
		t.Fatalf("behaviorally identical payload did not replay: first=%d %#v replay=%d %#v",
			firstRec.Code, first, replayRec.Code, replay)
	}
	var receipts, work int
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_problem_source_action_receipts
		WHERE agent_name='mingming' AND job_id=?`, seed.jobID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := seed.fixture.db.QueryRow(`
		SELECT COUNT(*) FROM k12_problem_source_reprocess_jobs
		WHERE agent_name='mingming' AND job_id=?`, seed.jobID).Scan(&work); err != nil {
		t.Fatal(err)
	}
	if receipts != 1 || work != 1 {
		t.Fatalf("semantic replay duplicated durable work: receipts=%d work=%d", receipts, work)
	}
}

func TestBUG_20260726_031_ProblemSourceActionSeparatesMalformedAndSemanticPayloadErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "missing payload is malformed",
			body: `{"action":"skip","structure_version":1,"expected_input_revision":1}`,
			want: http.StatusBadRequest,
		},
		{
			name: "unknown payload field is malformed",
			body: `{"action":"skip","structure_version":1,"expected_input_revision":1,"payload":{"extra":true}}`,
			want: http.StatusBadRequest,
		},
		{
			name: "wrong payload scalar type is malformed",
			body: `{"action":"correct_text","structure_version":1,"expected_input_revision":1,"payload":{"question_canonical_markdown":7}}`,
			want: http.StatusBadRequest,
		},
		{
			name: "empty typed correction is semantic",
			body: `{"action":"correct_text","structure_version":1,"expected_input_revision":1,"payload":{"question_canonical_markdown":"  "}}`,
			want: http.StatusUnprocessableEntity,
		},
		{
			name: "negative typed region is semantic",
			body: `{"action":"select_region","structure_version":1,"expected_input_revision":1,"payload":{"page_asset_id":"asset://mingming/x.png","region":{"x":-1,"y":0,"width":1,"height":1}}}`,
			want: http.StatusUnprocessableEntity,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seed := seedProblemSourceActionHTTP(t)
			rec, out := postProblemSourceAction(
				t, seed.fixture.handler, seed.dispatchID, seed.problemID,
				"payload-error-contract", tc.body,
			)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%#v", rec.Code, tc.want, out)
			}
			var receipts, work int
			if err := seed.fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_problem_source_action_receipts`).Scan(&receipts); err != nil {
				t.Fatal(err)
			}
			if err := seed.fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_problem_source_reprocess_jobs`).Scan(&work); err != nil {
				t.Fatal(err)
			}
			if receipts != 0 || work != 0 {
				t.Fatalf("invalid payload wrote receipt/work: %d/%d", receipts, work)
			}
		})
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
	if _, err := seed.fixture.db.Exec(`
		INSERT INTO k12_image_task_owner_scopes (
			dispatch_id,owner_scope,agent_name,created_at
		) VALUES (?,'guardian-1','mingming',100)`, seed.dispatchID); err != nil {
		t.Fatalf("seed durable dispatch owner scope: %v", err)
	}
	remoteHandler := func(
		authenticatedOwner string,
		authorize func(context.Context, string, string) error,
	) http.Handler {
		return apihttp.NewHandler(apihttp.Runtime{
			Records:       seed.fixture.coordinator.Records,
			ImageTasks:    seed.fixture.coordinator,
			PrincipalMode: "remote",
			AuthenticatedOwnerScope: func(context.Context) (string, error) {
				return authenticatedOwner, nil
			},
			AuthorizeAgentScope: authorize,
		})
	}

	rec, out := postProblemSourceAction(t, remoteHandler("attacker", func(
		context.Context,
		string,
		string,
	) error {
		t.Fatal("cross-owner target must be hidden before Agent authorization")
		return nil
	}),
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

	rec, out = postProblemSourceAction(t, remoteHandler("guardian-1", func(
		_ context.Context,
		owner string,
		agent string,
	) error {
		return fmt.Errorf("owner %q lacks command permission for agent %q", owner, agent)
	}), seed.dispatchID, seed.problemID, "forbidden-agent-skip", validSkipSourceActionBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("same-owner command denial status=%d want 403 body=%#v", rec.Code, out)
	}

	rec, out = postProblemSourceAction(t, remoteHandler("guardian-1", nil),
		seed.dispatchID, seed.problemID, "missing-authorizer-skip", validSkipSourceActionBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing remote authorizer status=%d want 403 body=%#v", rec.Code, out)
	}

	rec, out = postProblemSourceAction(t, remoteHandler("guardian-1", func(
		_ context.Context,
		owner string,
		agent string,
	) error {
		if owner != "guardian-1" || agent != "mingming" {
			return fmt.Errorf("unexpected authorization scope %q -> %q", owner, agent)
		}
		return nil
	}),
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
