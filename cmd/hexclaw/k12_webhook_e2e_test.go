package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	platformapi "github.com/hexagon-codes/hexclaw/api"
	k12 "github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	"github.com/hexagon-codes/hexclaw/webhook"

	_ "modernc.org/sqlite"
)

type k12WebhookSolveStub struct{}

func (k12WebhookSolveStub) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: "unused", Metadata: map[string]string{}}, nil
}

func newK12WebhookRuntime(t *testing.T) *assembly.K12 {
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
	if _, err := db.Exec(`INSERT INTO agents(name) VALUES('kid-agent')`); err != nil {
		t.Fatal(err)
	}
	runtime, err := assembly.Wire(db, k12WebhookSolveStub{})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func TestK12WebhookTextSubmissionUsesGradingJobApplicationCommand(t *testing.T) {
	runtime := newK12WebhookRuntime(t)
	snapshot := func() k12.GradingModelSnapshot {
		return k12.GradingModelSnapshot{Provider: "test", Model: "test-model", Capability: "vision"}
	}
	runDir := t.TempDir()
	grading := usecase.NewGradingOrchestrator(runtime.Deps, snapshot, usecase.WithGradingRunDir(runDir))
	app := k12WebhookApplication{
		deps: runtime.Deps, grading: grading, snapshot: snapshot,
	}
	event := webhook.K12Dispatch{
		ReceiptID: "receipt-1", BindingID: "binding-1", EventID: "delivery-1",
		EventType: webhook.K12EventSubmissionRequested,
		AgentID:   "kid-agent", LearnerID: "kid-learner",
		Payload: json.RawMessage(`{"text":"12 和 18 的最大公约数是多少？","subject":"数学","source_session":"parent-chat"}`),
	}
	result, err := app.handle(context.Background(), event)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Reference, "grading_job:") {
		t.Fatalf("reference=%q", result.Reference)
	}
	jobID := strings.TrimPrefix(result.Reference, "grading_job:")
	job, err := runtime.Deps.GetGradingJob(context.Background(), "kid-agent", jobID)
	if err != nil {
		t.Fatalf("webhook did not create a real GradingJob: %v", err)
	}
	if job.Fields.SourceKind != "webhook" || job.Fields.IdempotencyKey == "" || job.Record.SourceSession != "parent-chat" {
		t.Fatalf("GradingJob lost webhook semantics: record=%+v fields=%+v", job.Record, job.Fields)
	}
	if job.Fields.SubmissionID != "webhook-receipt:receipt-1" {
		t.Fatalf("text payload must point at its durable webhook submission, got %q", job.Fields.SubmissionID)
	}
	if job.Record.Status != k12.GradingStageAwaitingConfirmation || job.Fields.AnchorState != k12.GradingAnchorDegraded {
		t.Fatalf("text submission stayed an empty queued Job: status=%s fields=%+v", job.Record.Status, job.Fields)
	}
	if result.Status != webhook.K12ReceiptSucceeded {
		t.Fatalf("text command did not report its real domain terminal: %+v", result)
	}

	// Text is already trusted normalized input, but it must still enter the same
	// typed DD-010~012 facts as photo recognition. A status-only Job leaves the
	// confirmation UI empty and cannot be continued safely.
	typed, err := runtime.Deps.Records.GetProblemAttemptSnapshot(context.Background(), "kid-agent", job.Fields.SubmissionID)
	if err != nil {
		t.Fatalf("text webhook did not persist typed Problem/Attempt: %v", err)
	}
	if len(typed.Problems) != 1 || len(typed.Attempts) != 1 {
		t.Fatalf("typed text submission=%+v, want one Problem and one Attempt", typed)
	}
	problem, attempt := typed.Problems[0], typed.Attempts[0]
	if problem.ProblemKind != k12.ProblemKindStandalone ||
		problem.StemRaw != "12 和 18 的最大公约数是多少？" ||
		problem.StemMarkdown != problem.StemRaw || problem.Subject != "数学" {
		t.Fatalf("typed Problem lost trusted text facts: %+v", problem)
	}
	if attempt.ProblemID != problem.ProblemID || attempt.AnswerState != "blank" ||
		attempt.AnswerRaw != "" || attempt.AnswerMarkdown != "" || attempt.ConfirmedVersion != 0 {
		t.Fatalf("text submission must create an independent blank Attempt: %+v", attempt)
	}

	// The production GET/confirm surface recovers the typed facts through a
	// text-specific run. It persists run metadata but never invents an image.
	h := apihttp.NewHandler(apihttp.Runtime{
		Views: runtime.Registry.Views, Records: runtime.Records, Deps: runtime.Deps, Grading: grading,
	})
	getReq := httptest.NewRequest(http.MethodGet, "/grading-jobs/"+jobID+"?agent=kid-agent", nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		body, _ := io.ReadAll(getRec.Result().Body)
		t.Fatalf("GET text GradingJob status=%d body=%s", getRec.Code, body)
	}
	var getBody struct {
		Job struct {
			RecognizedQuestions []struct {
				ProblemID   string `json:"problem_id"`
				AnswerState string `json:"answer_state"`
			} `json:"recognized_questions"`
		} `json:"job"`
	}
	if err := json.NewDecoder(getRec.Result().Body).Decode(&getBody); err != nil {
		t.Fatal(err)
	}
	if len(getBody.Job.RecognizedQuestions) != 1 ||
		getBody.Job.RecognizedQuestions[0].ProblemID != problem.ProblemID ||
		getBody.Job.RecognizedQuestions[0].AnswerState != "blank" {
		t.Fatalf("GET GradingJob did not expose typed text question: %+v", getBody.Job.RecognizedQuestions)
	}

	confirmBody := strings.NewReader(`{"agent":"kid-agent","question_corrections":[{"problem_id":"` + problem.ProblemID + `","confirmed":true}]}`)
	confirmReq := httptest.NewRequest(http.MethodPost, "/grading-jobs/"+jobID+"/confirm", confirmBody)
	confirmReq.Header.Set("Content-Type", "application/json")
	confirmRec := httptest.NewRecorder()
	h.ServeHTTP(confirmRec, confirmReq)
	if confirmRec.Code != http.StatusOK {
		body, _ := io.ReadAll(confirmRec.Result().Body)
		t.Fatalf("confirm text GradingJob status=%d body=%s", confirmRec.Code, body)
	}
	var confirmResult map[string]any
	if err := json.NewDecoder(confirmRec.Result().Body).Decode(&confirmResult); err != nil {
		t.Fatal(err)
	}
	if confirmResult["stage"] != k12.GradingStageAssessing && confirmResult["stage"] != k12.GradingStageRendering &&
		confirmResult["stage"] != k12.GradingStageProjecting && confirmResult["stage"] != k12.GradingStageCompleted {
		t.Fatalf("confirmed text GradingJob was not accepted by the text worker: %+v", confirmResult)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err = runtime.Deps.GetGradingJob(context.Background(), "kid-agent", jobID)
		if err == nil && job.Record.Status == k12.GradingStageCompleted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || job.Record.Status != k12.GradingStageCompleted {
		t.Fatalf("text worker did not reach completed: job=%+v err=%v", job, err)
	}
	resultReq := httptest.NewRequest(http.MethodGet, "/grading-jobs/"+jobID+"/result?agent=kid-agent", nil)
	resultRec := httptest.NewRecorder()
	h.ServeHTTP(resultRec, resultReq)
	if resultRec.Code != http.StatusOK {
		body, _ := io.ReadAll(resultRec.Result().Body)
		t.Fatalf("GET text GradingJob result status=%d body=%s", resultRec.Code, body)
	}
	var resultBody struct {
		Result struct {
			Markdown string `json:"markdown"`
			Items    []any  `json:"items"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resultRec.Result().Body).Decode(&resultBody); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(resultBody.Result.Markdown) == "" || len(resultBody.Result.Items) != 1 {
		t.Fatalf("text result is not readable: %+v", resultBody.Result)
	}
	if _, statErr := os.Stat(filepath.Join(runDir, jobID, "run.json")); statErr != nil {
		t.Fatalf("text run metadata was not persisted: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(runDir, jobID, "image.bin")); !os.IsNotExist(statErr) {
		t.Fatalf("text run must not fabricate a persisted image: err=%v", statErr)
	}
	confirmed, err := runtime.Deps.Records.GetProblemAttemptSnapshot(context.Background(), "kid-agent", job.Fields.SubmissionID)
	if err != nil || len(confirmed.Attempts) != 1 || confirmed.Attempts[0].ConfirmedVersion != 1 ||
		confirmed.Attempts[0].InputDigest == "" {
		t.Fatalf("text confirmation did not version typed Attempt: snapshot=%+v err=%v", confirmed, err)
	}

	// A repeated confirm is an illegal state transition. It must not mutate the
	// already-frozen typed facts before returning the conflict.
	duplicateConfirm := httptest.NewRequest(http.MethodPost, "/grading-jobs/"+jobID+"/confirm",
		strings.NewReader(`{"agent":"kid-agent","question_corrections":[{"problem_id":"`+problem.ProblemID+`","confirmed":true}]}`))
	duplicateConfirm.Header.Set("Content-Type", "application/json")
	duplicateRec := httptest.NewRecorder()
	h.ServeHTTP(duplicateRec, duplicateConfirm)
	if duplicateRec.Code != http.StatusConflict {
		body, _ := io.ReadAll(duplicateRec.Result().Body)
		t.Fatalf("duplicate text confirm status=%d body=%s", duplicateRec.Code, body)
	}
	afterDuplicate, err := runtime.Deps.Records.GetProblemAttemptSnapshot(context.Background(), "kid-agent", job.Fields.SubmissionID)
	if err != nil || len(afterDuplicate.Attempts) != 1 || afterDuplicate.Attempts[0].ConfirmedVersion != 1 {
		t.Fatalf("rejected duplicate confirm mutated typed facts: snapshot=%+v err=%v", afterDuplicate, err)
	}

	// The same stable delivery reaches the same existing Job through the usecase
	// idempotency key; the adapter does not create a parallel orchestration path.
	retry, err := app.handle(context.Background(), event)
	if err != nil || retry.Reference != result.Reference {
		t.Fatalf("stable delivery retry=%+v err=%v, want %+v", retry, err, result)
	}
}

func TestK12WebhookPracticeReturnBatchesAreAtomic(t *testing.T) {
	runtime := newK12WebhookRuntime(t)
	ctx := context.Background()
	setID, _, err := runtime.Deps.CreatePracticeSet(ctx, "kid-agent", "webhook-practice", k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceManual, Title: "Webhook 回传卷",
		Items: []k12.PracticeItem{
			{ItemID: "q1", QuestionMarkdown: "1+1=?", ExpectedAnswerMarkdown: "2", VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
			{ItemID: "q2", QuestionMarkdown: "2+2=?", ExpectedAnswerMarkdown: "4", VerificationStatus: k12.PracticeItemVerified, VerificationEvidence: "独立验算"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	finalized, _, err := runtime.Deps.FinalizeBasket(ctx, "kid-agent", setID, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	raw, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	assetID, err := assetstore.Save("kid-agent", raw)
	if err != nil {
		t.Fatal(err)
	}
	app := k12WebhookApplication{deps: runtime.Deps}
	_, err = app.handle(ctx, webhook.K12Dispatch{
		ReceiptID: "receipt-return", BindingID: "binding-return", EventID: "delivery-return",
		EventType: webhook.K12EventPracticeReturnRequested, AgentID: "kid-agent", LearnerID: "kid-learner",
		Payload: json.RawMessage(`{"paper_no":"` + finalized.Fields.PaperNo + `","return_assets":[` +
			`{"return_id":"return-valid","asset_ref":"` + assetID + `","item_ids":["q1"]},` +
			`{"return_id":"return-invalid","asset_ref":"` + assetID + `","item_ids":["ghost"]}]}`),
	})
	if err == nil {
		t.Fatal("invalid second batch must reject the whole webhook command")
	}
	after, getErr := runtime.Deps.GetPracticeSet(ctx, "kid-agent", setID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if after.Record.Status != k12.PracticeStatusAssigned || len(after.Fields.ReturnAssets) != 0 {
		t.Fatalf("multi-batch failure partially committed: status=%s assets=%+v", after.Record.Status, after.Fields.ReturnAssets)
	}
}

type capturedK12WorkflowRunner struct {
	id, version, input, agent, learner, triggerKey string
	runID                                          string
	retrySafe                                      bool
	err                                            error
}

func (r *capturedK12WorkflowRunner) RunK12WorkflowFromWebhookDispatch(
	_ context.Context, id, version, input, agent, learner, triggerKey string,
) (string, bool, error) {
	r.id, r.version, r.input, r.agent, r.learner, r.triggerKey = id, version, input, agent, learner, triggerKey
	if r.runID == "" {
		r.runID = "run-1"
	}
	return r.runID, r.retrySafe, r.err
}

func TestK12WebhookWorkflowUsesBoundOwnerAndVersionedWorkflowCommand(t *testing.T) {
	runner := &capturedK12WorkflowRunner{}
	app := k12WebhookApplication{workflows: runner}
	result, err := app.handle(context.Background(), webhook.K12Dispatch{
		BindingID: "binding-workflow", EventID: "delivery-workflow", EventType: webhook.K12EventWorkflowRunRequested,
		AgentID: "kid-agent", LearnerID: "kid-learner",
		Payload: json.RawMessage(`{"workflow_id":"wf-homework","workflow_version":"v3","input":"生成本周复习"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reference != "workflow_run:run-1" {
		t.Fatalf("reference=%q", result.Reference)
	}
	if result.Status != webhook.K12ReceiptSucceeded {
		t.Fatalf("workflow Receipt must wait for the real terminal: %+v", result)
	}
	if runner.id != "wf-homework" || runner.version != "v3" || runner.input != "生成本周复习" ||
		runner.agent != "kid-agent" || runner.learner != "kid-learner" ||
		runner.triggerKey != "k12-webhook:binding-workflow:delivery-workflow" {
		t.Fatalf("workflow command did not receive trusted dispatch: %+v", runner)
	}
}

func TestK12WebhookWorkflowPropagatesRetryEvidenceAndOutcomeUnknown(t *testing.T) {
	dispatch := webhook.K12Dispatch{
		BindingID: "binding-workflow", EventID: "delivery-workflow-failed",
		EventType: webhook.K12EventWorkflowRunRequested, AgentID: "kid-agent", LearnerID: "kid-learner",
		Payload: json.RawMessage(`{"workflow_id":"wf-homework","workflow_version":"v3"}`),
	}
	local := &capturedK12WorkflowRunner{runID: "run-local", retrySafe: true, err: errors.New("local node validation")}
	result, err := (k12WebhookApplication{workflows: local}).handle(context.Background(), dispatch)
	if err == nil || !result.RetrySafe || result.Reference != "workflow_run:run-local" {
		t.Fatalf("local workflow failure result=%+v err=%v", result, err)
	}

	unknown := &capturedK12WorkflowRunner{
		runID: "run-unknown", err: fmt.Errorf("%w: provider disconnected", platformapi.ErrK12WorkflowOutcomeUnknown),
	}
	result, err = (k12WebhookApplication{workflows: unknown}).handle(context.Background(), dispatch)
	if !errors.Is(err, webhook.ErrK12OutcomeUnknown) || result.RetrySafe || result.Reference != "workflow_run:run-unknown" {
		t.Fatalf("unknown workflow failure result=%+v err=%v", result, err)
	}
}

func TestInstallK12WebhookHandlerRecoversAcceptedDispatchAfterRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "k12-webhook-startup.db")
	db1, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db1.SetMaxOpenConns(1)
	if _, err := db1.Exec(migrate.K12WebhooksV18DDL); err != nil {
		db1.Close()
		t.Fatal(err)
	}
	mgr1 := webhook.NewManager(db1)
	if err := mgr1.Init(ctx); err != nil {
		db1.Close()
		t.Fatal(err)
	}
	binding, _, err := mgr1.CreateK12Binding(ctx, webhook.K12BindingInput{
		Name: "startup-recover", AgentID: "kid-agent", LearnerID: "kid-learner",
		AllowedEvents: []webhook.K12EventType{webhook.K12EventSubmissionRequested},
		CreatedBy:     "parent-1", Enabled: true,
	})
	if err != nil {
		db1.Close()
		t.Fatal(err)
	}
	dispatch := webhook.K12Dispatch{
		ReceiptID: "receipt-startup-recover", BindingID: binding.BindingID,
		EventID: "event-startup-recover", EventType: webhook.K12EventSubmissionRequested,
		AgentID: binding.AgentID, LearnerID: binding.LearnerID,
		Payload: json.RawMessage(`{"text":"recover at composition root"}`),
	}
	dispatchJSON, err := json.Marshal(dispatch)
	if err != nil {
		db1.Close()
		t.Fatal(err)
	}
	now := time.Now().UTC().UnixNano()
	if _, err := db1.Exec(`INSERT INTO k12_webhook_receipts
      (receipt_id,binding_id,event_id,event_type,payload_digest,status,reference,failure_kind,dispatch_json,created_at,updated_at)
      VALUES(?,?,?,?,?,?,?,?,?,?,?)`, dispatch.ReceiptID, dispatch.BindingID, dispatch.EventID,
		dispatch.EventType, "digest", webhook.K12ReceiptAccepted, "", "", string(dispatchJSON), now, now); err != nil {
		db1.Close()
		t.Fatal(err)
	}
	if err := db1.Close(); err != nil {
		t.Fatal(err)
	}

	db2, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	db2.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db2.Close() })
	mgr2 := webhook.NewManager(db2)
	if err := mgr2.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	recovered, err := installK12WebhookHandler(ctx, mgr2, func(_ context.Context, got webhook.K12Dispatch) (webhook.K12DispatchResult, error) {
		calls.Add(1)
		if got.EventID != dispatch.EventID {
			t.Errorf("recovered event=%q want=%q", got.EventID, dispatch.EventID)
		}
		return webhook.K12DispatchResult{Reference: "grading_job:startup", Status: webhook.K12ReceiptSucceeded}, nil
	})
	if err != nil || recovered != 1 {
		t.Fatalf("install recovery=%d err=%v", recovered, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		receipt, getErr := mgr2.GetK12Receipt(ctx, dispatch.ReceiptID)
		if getErr == nil && receipt.Status == webhook.K12ReceiptSucceeded {
			if receipt.Reference != "grading_job:startup" || calls.Load() != 1 {
				t.Fatalf("receipt=%+v calls=%d", receipt, calls.Load())
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("startup recovery did not dispatch exactly once; calls=%d", calls.Load())
}
