package apihttp_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

type imageTaskClassifierHTTPStub struct {
	calls  int
	result usecase.ImageTaskClassification
	err    error
}

type imageTaskClassifierHTTPDefinitiveError struct{}

func (imageTaskClassifierHTTPDefinitiveError) Error() string {
	return "provider failed with private detail"
}

func (imageTaskClassifierHTTPDefinitiveError) ProviderResponseStatusCode() int { return 503 }

type imageTaskHTTPWritingOCRStub struct {
	result usecase.ImageTaskWritingOCRResult
}

func (s *imageTaskHTTPWritingOCRStub) RecognizeImageTaskWriting(
	context.Context, []byte,
) (usecase.ImageTaskWritingOCRResult, error) {
	return s.result, nil
}

type imageTaskHTTPFeedbackSolver struct {
	calls *atomic.Int32
}

func (imageTaskHTTPFeedbackSolver) Solve(
	context.Context, string, string, string,
) (usecase.SolveResult, error) {
	return usecase.SolveResult{}, nil
}

func (s *imageTaskHTTPFeedbackSolver) GenerateWorkFeedback(
	context.Context, usecase.WorkFeedbackRequest,
) (usecase.WorkFeedbackOutput, error) {
	s.calls.Add(1)
	return usecase.WorkFeedbackOutput{
		Feedback:   "画面中的人物和小猫位置清楚；建议补充地面上的可见阴影细节。",
		SkillStamp: "art-feedback@1.0.0/test",
	}, nil
}

func (s *imageTaskClassifierHTTPStub) ClassifyImageTask(
	context.Context, usecase.ImageTaskClassificationInput,
) (usecase.ImageTaskClassification, error) {
	s.calls++
	if s.err != nil {
		return usecase.ImageTaskClassification{}, s.err
	}
	if s.result.Intent != "" {
		return s.result, nil
	}
	return usecase.ImageTaskClassification{
		Intent:         k12.ImageTaskIntentArtwork,
		IntentEvidence: []string{"visible crayon illustration"},
		Confidence:     0.99,
	}, nil
}

func TestImageTaskFailedPublicEndpointsProjectSameStructuredFailureKind(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	fixture.classifier.err = imageTaskClassifierHTTPDefinitiveError{}
	rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks",
		createImageTaskBody(fixture.assetID, "message-failed"))
	if rec.Code != http.StatusOK {
		t.Fatalf("create image task: %d %#v", rec.Code, out)
	}
	dispatchID, _ := out["dispatch"].(map[string]any)["dispatch_id"].(string)
	if dispatchID == "" {
		t.Fatalf("missing dispatch identity: %#v", out)
	}

	_, out = waitImageTaskHTTPState(t, fixture, dispatchID, func(dispatch map[string]any) bool {
		return dispatch["status"] == string(k12.ImageTaskStatusFailed)
	})
	failedDispatch := out["dispatch"].(map[string]any)
	const wantFailureKind = "classification_provider_failed"
	if failedDispatch["failure_kind"] != wantFailureKind {
		t.Fatalf("dispatch failure_kind=%#v, want %q; dispatch=%#v",
			failedDispatch["failure_kind"], wantFailureKind, failedDispatch)
	}
	if body, _ := json.Marshal(failedDispatch); strings.Contains(string(body), "private detail") {
		t.Fatalf("dispatch leaked error detail: %s", body)
	}

	rec, result := do(t, fixture.handler, http.MethodGet,
		"/image-tasks/"+dispatchID+"/result?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("result status=%d body=%#v", rec.Code, result)
	}
	if result["status"] != string(k12.ImageTaskStatusFailed) ||
		result["failure_kind"] != wantFailureKind {
		t.Fatalf("result failure projection drift: %#v", result)
	}
	if strings.Contains(rec.Body.String(), "private detail") {
		t.Fatalf("result leaked error detail: %s", rec.Body.String())
	}
}

func TestImageTaskRecoveringPublicEndpointsOmitInternalOutcomeUnknownFailureKind(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	fixture.classifier.err = imageTaskClassifierHTTPDefinitiveError{}
	rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks",
		createImageTaskBody(fixture.assetID, "message-recovering"))
	if rec.Code != http.StatusOK {
		t.Fatalf("create image task: %d %#v", rec.Code, out)
	}
	dispatchID, _ := out["dispatch"].(map[string]any)["dispatch_id"].(string)
	if dispatchID == "" {
		t.Fatalf("missing dispatch identity: %#v", out)
	}
	_, out = waitImageTaskHTTPState(t, fixture, dispatchID, func(dispatch map[string]any) bool {
		return dispatch["status"] == string(k12.ImageTaskStatusFailed)
	})
	if _, err := fixture.db.ExecContext(t.Context(), `
		UPDATE k12_image_task_dispatches
		SET failure_kind = ?, retry_safe = 0
		WHERE dispatch_id = ?`, "classification_outcome_unknown", dispatchID); err != nil {
		t.Fatalf("seed recovering dispatch: %v", err)
	}

	rec, out = do(t, fixture.handler, http.MethodGet,
		"/image-tasks/"+dispatchID+"?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dispatch status=%d body=%#v", rec.Code, out)
	}
	dispatch := out["dispatch"].(map[string]any)
	if _, exists := dispatch["failure_kind"]; exists {
		t.Fatalf("recovering dispatch leaked internal failure_kind: %#v", dispatch)
	}
	progress, _ := dispatch["progress"].(map[string]any)
	if progress["state"] != "recovering" {
		t.Fatalf("recovering dispatch state=%#v, want recovering", progress["state"])
	}

	rec, result := do(t, fixture.handler, http.MethodGet,
		"/image-tasks/"+dispatchID+"/result?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("result status=%d body=%#v", rec.Code, result)
	}
	if _, exists := result["failure_kind"]; exists {
		t.Fatalf("recovering result leaked internal failure_kind: %#v", result)
	}
	dispatch, _ = result["dispatch"].(map[string]any)
	if _, exists := dispatch["failure_kind"]; exists {
		t.Fatalf("recovering result dispatch leaked internal failure_kind: %#v", dispatch)
	}
}

type imageTaskHTTPFixture struct {
	handler       http.Handler
	classifier    *imageTaskClassifierHTTPStub
	ocr           *imageTaskHTTPWritingOCRStub
	coordinator   *usecase.ImageTaskCoordinator
	assetID       string
	feedbackCalls *atomic.Int32
	db            *sql.DB
}

type imageTaskDispatchContract struct {
	DispatchID             string                `json:"dispatch_id"`
	TaskIntent             k12.ImageTaskIntent   `json:"task_intent"`
	Status                 k12.ImageTaskStatus   `json:"status"`
	IntentEvidence         []string              `json:"intent_evidence"`
	IntentConfidence       float64               `json:"intent_confidence"`
	ConfirmationCandidates []k12.ImageTaskIntent `json:"confirmation_candidates"`
	Target                 *struct {
		Type k12.ImageTaskTargetType `json:"type"`
		ID   string                  `json:"id"`
	} `json:"target,omitempty"`
	TargetProjection *struct {
		Kind     string                         `json:"kind"`
		IntakeID string                         `json:"intake_id"`
		WorkType string                         `json:"work_type"`
		Status   k12.CreativeWorkIntakeStatus   `json:"status"`
		Work     *imageTaskCreativeWorkContract `json:"work,omitempty"`
	} `json:"target_projection,omitempty"`
	Progress struct {
		Operation string `json:"operation"`
		State     string `json:"state"`
	} `json:"progress"`
	Version   int   `json:"version"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

type imageTaskCreativeWorkContract struct {
	WorkID      string `json:"work_id"`
	DisplayName string `json:"display_name"`
}

type imageTaskCreateContract struct {
	Created  bool                      `json:"created"`
	Dispatch imageTaskDispatchContract `json:"dispatch"`
}

type imageTaskResultContract struct {
	DispatchID        string              `json:"dispatch_id"`
	TaskIntent        k12.ImageTaskIntent `json:"task_intent"`
	Status            k12.ImageTaskStatus `json:"status"`
	SourceDigest      string              `json:"source_digest"`
	SourceAttachments []struct {
		Digest    string `json:"digest"`
		SizeBytes int    `json:"size_bytes"`
	} `json:"source_attachments"`
	OperationReceipts []struct {
		InvocationID string `json:"invocation_id"`
		Operation    string `json:"operation"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		Status       string `json:"status"`
		Attempt      int    `json:"attempt"`
		ResultDigest string `json:"result_digest"`
	} `json:"operation_receipts"`
	Result *struct {
		Kind    k12.ImageTaskIntent `json:"kind"`
		Payload struct {
			Intake struct {
				IntakeID string                       `json:"intake_id"`
				Status   k12.CreativeWorkIntakeStatus `json:"status"`
			} `json:"intake"`
			Work     *imageTaskCreativeWorkContract `json:"work,omitempty"`
			Feedback *struct {
				GenerationID       string           `json:"generation_id"`
				StructuredFeedback k12.WorkFeedback `json:"structured_feedback"`
				ProjectionMarkdown string           `json:"projection_markdown"`
			} `json:"feedback,omitempty"`
		} `json:"payload"`
	} `json:"result"`
}

type creativeWorkFeedbackHTTPContract struct {
	FeedbackID         string `json:"feedback_id"`
	ProjectionMarkdown string `json:"projection_markdown"`
}

type creativeWorkGenerationHTTPContract struct {
	GenerationID string                            `json:"generation_id"`
	Status       string                            `json:"status"`
	Feedback     *creativeWorkFeedbackHTTPContract `json:"feedback,omitempty"`
}

type creativeWorkHTTPContract struct {
	WorkID          string                              `json:"work_id"`
	InitialFeedback creativeWorkGenerationHTTPContract  `json:"initial_feedback"`
	LatestFeedback  *creativeWorkGenerationHTTPContract `json:"latest_feedback,omitempty"`
}

func newImageTaskHTTPFixture(t *testing.T) imageTaskHTTPFixture {
	t.Helper()
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// ImageTask workers are asynchronous. A plain SQLite :memory: database is
	// connection-local, so keep the fixture on one connection to ensure the
	// worker and HTTP poll observe the same durable schema.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"mingming", "gege"} {
		if _, err := db.Exec(`INSERT INTO agents(name) VALUES(?)`, agent); err != nil {
			t.Fatal(err)
		}
	}
	wired, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
	)
	if err != nil {
		t.Fatal(err)
	}
	assetID, err := assetstore.Save("mingming", raw)
	if err != nil {
		t.Fatal(err)
	}
	classifier := &imageTaskClassifierHTTPStub{}
	ocr := &imageTaskHTTPWritingOCRStub{result: usecase.ImageTaskWritingOCRResult{
		Raw: "我的好爸〔字迹不清〕", CanonicalContent: "我的好爸〔字迹不清〕",
		Confidence: 0.7,
		RiskSegments: []k12.CreativeWorkIntakeOCRRisk{{
			SegmentID: "line-1-word-5", RawText: "〔字迹不清〕",
			Reasons: []string{"illegible"},
		}},
	}}
	feedbackCalls := &atomic.Int32{}
	feedbackDeps := wired.Deps
	feedbackDeps.Solver = &imageTaskHTTPFeedbackSolver{calls: feedbackCalls}
	feedbackDeps.WorkFeedbackRoute = func(
		context.Context, string,
	) (k12.ImageTaskRouteSnapshot, error) {
		return k12.ImageTaskRouteSnapshot{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			Route: "hexclaw-gpt/gpt-5.6-sol", Capability: "vision",
			SelectionSource: "explicit", PolicyVersion: "work-feedback-routing-v1",
			PromptVersion: "art-feedback-v1",
		}, nil
	}
	coordinator := &usecase.ImageTaskCoordinator{
		Records: wired.Records, Classifier: classifier, WritingOCR: ocr,
		WorkFeedback: &feedbackDeps,
		ResolveRoute: func(requested k12.ImageTaskRouteSnapshot) (k12.ImageTaskRouteSnapshot, error) {
			if requested.Provider == "" {
				requested.Provider = "hexclaw-gpt"
			}
			if requested.Model == "" {
				requested.Model = "gpt-5.6-sol"
			}
			requested.Route = requested.Provider + "/" + requested.Model
			requested.Capability = "vision"
			if requested.SelectionSource == "" {
				requested.SelectionSource = "auto"
			}
			requested.PolicyVersion = "image-task-routing-v1"
			requested.PromptVersion = "image-task-classifier-v1"
			return requested, nil
		},
	}
	t.Cleanup(func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := coordinator.Wait(waitCtx); err != nil {
			t.Errorf("drain image task workers: %v", err)
		}
	})
	return imageTaskHTTPFixture{
		handler: apihttp.NewHandler(apihttp.Runtime{
			Views: wired.Registry.Views, Records: wired.Records, Deps: wired.Deps,
			ImageTasks: coordinator,
		}),
		classifier: classifier, ocr: ocr, coordinator: coordinator, assetID: assetID,
		feedbackCalls: feedbackCalls, db: db,
	}
}

func assertJSONExactKeys(t *testing.T, value map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(value))
	for key := range value {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON keys drift: got=%v want=%v value=%#v", got, want, value)
	}
}

func waitImageTaskHTTPState(
	t *testing.T,
	fixture imageTaskHTTPFixture,
	dispatchID string,
	accept func(map[string]any) bool,
) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec, out := do(t, fixture.handler, http.MethodGet,
			"/image-tasks/"+dispatchID+"?agent=mingming", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("poll image task: %d %#v", rec.Code, out)
		}
		dispatch := out["dispatch"].(map[string]any)
		if accept(dispatch) {
			return rec, out
		}
		if time.Now().After(deadline) {
			t.Fatalf("image task did not reach expected state: %#v", dispatch)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func createImageTaskBody(assetID, sourceRef string) string {
	return fmt.Sprintf(`{
		"agent":"mingming","source_session":"session-1","source_kind":"desktop",
		"source_ref":%q,"source_asset_refs":[%q],"message_intent":"请处理",
		"attempt_generation":1,
		"route_request":{"provider":"hexclaw-gpt","model":"gpt-5.6-sol","selection_source":"explicit"}
	}`, sourceRef, assetID)
}

func TestImageTaskPublicSurfaceExactSetAndNoInternalLeak(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks",
		createImageTaskBody(fixture.assetID, "message-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("create image task: %d %#v", rec.Code, out)
	}
	writeK12ImageTaskWireFixture(t, "create.json", rec.Body.Bytes())
	dispatch := out["dispatch"].(map[string]any)
	dispatchID, _ := dispatch["dispatch_id"].(string)
	if dispatchID == "" ||
		dispatch["task_intent"] != string(k12.ImageTaskIntentUnknown) ||
		dispatch["status"] != string(k12.ImageTaskStatusRouting) {
		t.Fatalf("POST must return durable routing identity before provider completion: %#v", dispatch)
	}
	var created imageTaskCreateContract
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create contract: %v", err)
	}
	if !created.Created ||
		created.Dispatch.Target != nil ||
		created.Dispatch.TargetProjection != nil ||
		created.Dispatch.Progress.Operation != "classification" ||
		created.Dispatch.Progress.State != "routing" {
		t.Fatalf("create acceptance contract drift: %#v", created)
	}
	rec, out = waitImageTaskHTTPState(t, fixture, dispatchID, func(dispatch map[string]any) bool {
		progress, _ := dispatch["progress"].(map[string]any)
		return progress["state"] == "feedback_ready"
	})
	writeK12ImageTaskWireFixture(t, "dispatch.json", rec.Body.Bytes())
	var projected struct {
		Dispatch imageTaskDispatchContract `json:"dispatch"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &projected); err != nil {
		t.Fatalf("decode projected contract: %v", err)
	}
	if projected.Dispatch.Target == nil ||
		projected.Dispatch.Target.Type != k12.ImageTaskTargetCreativeWorkIntake ||
		projected.Dispatch.Target.ID == "" ||
		projected.Dispatch.Progress.Operation != "promotion" ||
		projected.Dispatch.Progress.State != "feedback_ready" ||
		projected.Dispatch.TargetProjection == nil ||
		projected.Dispatch.TargetProjection.Kind != "creative" ||
		projected.Dispatch.TargetProjection.Work == nil ||
		projected.Dispatch.TargetProjection.Work.DisplayName != "美术作品" {
		t.Fatalf("projected wire contract drift: %#v", projected)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{
		`"grading_job"`, `"provider"`, `"model"`, `"invocation"`,
		`"request_digest"`, `"failure_kind"`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public facade leaked %q: %s", forbidden, body)
		}
	}

	for _, tc := range []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{http.MethodGet, "/image-tasks/" + dispatchID + "?agent=mingming", "", http.StatusOK},
		{http.MethodGet, "/image-tasks/" + dispatchID + "/result?agent=mingming", "", http.StatusOK},
		{http.MethodPost, "/image-tasks/" + dispatchID + "/confirm", `{"agent":"mingming","version":1}`, http.StatusConflict},
		{http.MethodPost, "/image-tasks/" + dispatchID + "/retry", `{"agent":"mingming","version":1}`, http.StatusConflict},
		{http.MethodPost, "/image-tasks/" + dispatchID + "/cancel", `{"agent":"mingming","version":1}`, http.StatusConflict},
	} {
		rec, out = do(t, fixture.handler, tc.method, tc.path, tc.body)
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d; body=%#v", tc.method, tc.path, rec.Code, tc.want, out)
		}
	}

	rec, _ = do(t, fixture.handler, http.MethodGet,
		"/image-tasks/"+dispatchID+"/result?agent=mingming", "")
	writeK12ImageTaskWireFixture(t, "result.json", rec.Body.Bytes())
	var result imageTaskResultContract
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result contract: %v", err)
	}
	if result.DispatchID != dispatchID ||
		result.TaskIntent != k12.ImageTaskIntentArtwork ||
		result.SourceDigest == "" ||
		len(result.SourceAttachments) != 1 ||
		result.Result == nil ||
		result.Result.Kind != k12.ImageTaskIntentArtwork ||
		result.Result.Payload.Intake.IntakeID == "" ||
		result.Result.Payload.Intake.Status != k12.CreativeWorkIntakePromoted ||
		result.Result.Payload.Work == nil ||
		result.Result.Payload.Work.DisplayName != "美术作品" ||
		result.Result.Payload.Feedback == nil ||
		result.Result.Payload.Feedback.GenerationID == "" ||
		result.Result.Payload.Feedback.ProjectionMarkdown == "" ||
		result.Result.Payload.Feedback.StructuredFeedback.ProjectionMarkdown !=
			result.Result.Payload.Feedback.ProjectionMarkdown {
		t.Fatalf("result wire contract drift: %#v", result)
	}
	rawImage, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
	)
	if err != nil {
		t.Fatal(err)
	}
	imageDigest := sha256.Sum256(rawImage)
	if result.SourceAttachments[0].Digest != "sha256:"+hex.EncodeToString(imageDigest[:]) ||
		result.SourceAttachments[0].SizeBytes != len(rawImage) {
		t.Fatalf("source attachment receipt drift: %#v", result.SourceAttachments)
	}
	var feedbackReceipt *struct {
		InvocationID string `json:"invocation_id"`
		Operation    string `json:"operation"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		Status       string `json:"status"`
		Attempt      int    `json:"attempt"`
		ResultDigest string `json:"result_digest"`
	}
	for index := range result.OperationReceipts {
		if result.OperationReceipts[index].Operation == "work_feedback" {
			feedbackReceipt = &result.OperationReceipts[index]
			break
		}
	}
	if feedbackReceipt == nil ||
		feedbackReceipt.Provider != "hexclaw-gpt" ||
		feedbackReceipt.Model != "gpt-5.6-sol" ||
		feedbackReceipt.Status != "succeeded" ||
		feedbackReceipt.Attempt != 1 ||
		feedbackReceipt.InvocationID == "" ||
		feedbackReceipt.ResultDigest == "" {
		t.Fatalf("work feedback provider receipt drift: %#v", result.OperationReceipts)
	}
	var rawResult map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rawResult); err != nil {
		t.Fatal(err)
	}
	assertJSONExactKeys(t, rawResult,
		"dispatch_id", "operation_receipts", "result", "source_attachments",
		"source_digest", "status", "task_intent")
	rawProjection := rawResult["result"].(map[string]any)
	assertJSONExactKeys(t, rawProjection, "kind", "payload")
	rawPayload := rawProjection["payload"].(map[string]any)
	assertJSONExactKeys(t, rawPayload, "feedback", "intake", "work")
	rawFeedback := rawPayload["feedback"].(map[string]any)
	assertJSONExactKeys(t, rawFeedback,
		"generation_id", "projection_markdown", "structured_feedback")
	rawStructured := rawFeedback["structured_feedback"].(map[string]any)
	assertJSONExactKeys(t, rawStructured,
		"evidence_refs", "feedback_id", "feedback_type",
		"limitations", "observations", "projection_markdown", "source_snapshot",
		"suggestions", "version_id")

	workID := result.Result.Payload.Work.WorkID
	rec, _ = do(t, fixture.handler, http.MethodGet,
		"/creative-works/"+workID+"?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get promoted creative work: %d %s", rec.Code, rec.Body.String())
	}
	var work creativeWorkHTTPContract
	if err := json.Unmarshal(rec.Body.Bytes(), &work); err != nil {
		t.Fatalf("decode creative work: %v", err)
	}
	if work.WorkID != workID ||
		work.LatestFeedback == nil ||
		work.LatestFeedback.Feedback == nil ||
		result.Result.Payload.Feedback.GenerationID != work.InitialFeedback.GenerationID ||
		result.Result.Payload.Feedback.GenerationID != work.LatestFeedback.GenerationID ||
		result.Result.Payload.Feedback.StructuredFeedback.FeedbackID !=
			work.LatestFeedback.Feedback.FeedbackID ||
		result.Result.Payload.Feedback.ProjectionMarkdown !=
			work.LatestFeedback.Feedback.ProjectionMarkdown {
		t.Fatalf("image-task/work feedback identity drift: result=%#v work=%#v", result, work)
	}
	var generationCount int
	if err := fixture.db.QueryRow(`SELECT count(*) FROM k12_work_feedback_generations
		WHERE work_id=?`, workID).Scan(&generationCount); err != nil {
		t.Fatal(err)
	}
	if generationCount != 1 || fixture.feedbackCalls.Load() != 1 {
		t.Fatalf("feedback cardinality generations=%d provider_calls=%d want 1/1",
			generationCount, fixture.feedbackCalls.Load())
	}

	for _, path := range []string{
		"/grading-jobs", "/grading-jobs/internal-id", "/grading-jobs/internal-id/result",
		"/creative-work-ocr-jobs", "/recognize", "/recognize/anchors",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec, out = do(t, fixture.handler, method, path, `{}`)
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("removed path reachable %s %s: %d %#v", method, path, rec.Code, out)
			}
		}
	}
}

func TestImageTaskHTTPRejectsCrossOwnerAssetBeforeClassifier(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	other := strings.Replace(fixture.assetID, "asset://mingming/", "asset://gege/", 1)
	rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks",
		createImageTaskBody(other, "message-cross-owner"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-owner asset got %d want 400: %#v", rec.Code, out)
	}
	if fixture.classifier.calls != 0 {
		t.Fatalf("classifier called before owner fail-close: %d", fixture.classifier.calls)
	}
}

func TestImageTaskHTTPCreativeConfirmationProjectionCarriesExactFrozenInput(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	fixture.classifier.result = usecase.ImageTaskClassification{
		Intent:         k12.ImageTaskIntentWriting,
		IntentEvidence: []string{"continuous handwritten essay"},
		Confidence:     0.98,
	}
	rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks",
		createImageTaskBody(fixture.assetID, "message-writing-risk"))
	if rec.Code != http.StatusOK {
		t.Fatalf("create risky writing: %d %#v", rec.Code, out)
	}
	rawDispatch := out["dispatch"].(map[string]any)
	dispatchID := rawDispatch["dispatch_id"].(string)
	if rawDispatch["status"] != string(k12.ImageTaskStatusRouting) {
		t.Fatalf("POST did not return routing identity: %#v", rawDispatch)
	}
	_, out = waitImageTaskHTTPState(t, fixture, dispatchID, func(dispatch map[string]any) bool {
		target, _ := dispatch["target_projection"].(map[string]any)
		return target["status"] == string(k12.CreativeWorkIntakeAwaitingConfirmation)
	})
	rawDispatch = out["dispatch"].(map[string]any)
	rawTarget := rawDispatch["target_projection"].(map[string]any)
	assertJSONExactKeys(t, rawTarget,
		"canonical_content", "canonical_version", "conflicts",
		"entry_kind", "intake_id", "kind", "promotion_policy",
		"routing_provenance", "status", "work_type")
	if rawTarget["canonical_version"] != float64(1) ||
		rawTarget["canonical_content"] != fixture.ocr.result.CanonicalContent ||
		rawTarget["status"] != string(k12.CreativeWorkIntakeAwaitingConfirmation) {
		t.Fatalf("creative confirmation input drift: %#v", rawTarget)
	}
	conflicts := rawTarget["conflicts"].([]any)
	if len(conflicts) != 1 {
		t.Fatalf("minimum conflict set drift: %#v", conflicts)
	}
	assertJSONExactKeys(t, conflicts[0].(map[string]any),
		"raw_text", "reason", "segment_id")
}

func TestImageTaskHTTPManualCreativeEntryAndCommitExactContract(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	body := fmt.Sprintf(`{
		"agent":"mingming","source_session":"session-1","source_kind":"desktop",
		"source_ref":"manual-art-1","source_asset_refs":[%q],
		"attempt_generation":1,
		"route_request":{"provider":"hexclaw-gpt","model":"gpt-5.6-sol","selection_source":"explicit"},
		"creative_entry":{"kind":"new_work","task_intent":"artwork"}
	}`, fixture.assetID)
	rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual create: %d %#v", rec.Code, out)
	}
	dispatch := out["dispatch"].(map[string]any)
	dispatchID := dispatch["dispatch_id"].(string)
	if fixture.classifier.calls != 0 ||
		dispatch["status"] != string(k12.ImageTaskStatusRouted) {
		t.Fatalf("manual create used classifier or did not route atomically: %#v calls=%d",
			dispatch, fixture.classifier.calls)
	}
	target := dispatch["target_projection"].(map[string]any)
	assertJSONExactKeys(t, target,
		"commit_required", "commit_state", "entry_kind", "intake_id", "kind",
		"promotion_policy", "routing_provenance", "status", "work_type")
	if target["entry_kind"] != "new_work" ||
		target["promotion_policy"] != "explicit_commit" ||
		target["routing_provenance"] != "parent_selected" ||
		target["commit_required"] != true ||
		target["commit_state"] != "pending" {
		t.Fatalf("manual hidden semantics drift: %#v", target)
	}
	confirm := fmt.Sprintf(`{
		"agent":"mingming","version":%v,
		"creative":{"action":"commit","work_title":"彩虹和小猫","task_requirement":"观察色彩与构图",
		"content_markdown":"家长补充说明"}
	}`, dispatch["version"])
	rec, out = do(t, fixture.handler, http.MethodPost,
		"/image-tasks/"+dispatchID+"/confirm", confirm)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual commit: %d %#v", rec.Code, out)
	}
	target = out["dispatch"].(map[string]any)["target_projection"].(map[string]any)
	assertJSONExactKeys(t, target,
		"commit_required", "commit_state", "entry_kind", "intake_id", "kind",
		"promoted_generation_id", "promoted_work_id", "promotion_policy",
		"routing_provenance", "status", "work", "work_type")
	if target["status"] != "promoted" ||
		target["commit_required"] != false ||
		target["commit_state"] != "committed" ||
		target["promoted_work_id"] == "" ||
		target["promoted_generation_id"] == "" {
		t.Fatalf("manual commit projection drift: %#v", target)
	}
	var versionCount int
	if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_creative_work_versions
		WHERE work_record_id=?`, target["promoted_work_id"]).Scan(&versionCount); err != nil {
		t.Fatal(err)
	}
	if versionCount != 0 {
		t.Fatalf("manual commit created %d legacy versions, want 0", versionCount)
	}
}

func TestImageTaskHTTPRejectsMalformedCreativeUnionsBeforeSideEffects(t *testing.T) {
	for _, test := range []struct {
		name          string
		creativeEntry string
	}{
		{name: "revision", creativeEntry: `{"kind":"revision","task_intent":"artwork","work_id":"work-1","base_version_id":"v1"}`},
		{name: "new_work_with_work_id", creativeEntry: `{"kind":"new_work","task_intent":"artwork","work_id":"work-1"}`},
		{name: "new_work_with_base_version_id", creativeEntry: `{"kind":"new_work","task_intent":"artwork","base_version_id":"v1"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newImageTaskHTTPFixture(t)
			tables := []string{
				"k12_image_task_dispatches", "k12_creative_work_intakes",
				"k12_creative_works", "k12_creative_work_versions",
				"k12_work_feedback_generations", "k12_image_task_invocations",
				"outbox_events",
			}
			before := make(map[string]int, len(tables))
			for _, table := range tables {
				var count int
				if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
					t.Fatalf("count %s before malformed create: %v", table, err)
				}
				before[table] = count
			}
			malformedCreate := fmt.Sprintf(`{
				"agent":"mingming","source_session":"session-1","source_kind":"desktop",
				"source_ref":"manual-bad-%s","source_asset_refs":[%q],
				"attempt_generation":1,
				"route_request":{"provider":"hexclaw-gpt","model":"gpt-5.6-sol","selection_source":"explicit"},
				"creative_entry":%s
			}`, test.name, fixture.assetID, test.creativeEntry)
			rec, _ := do(t, fixture.handler, http.MethodPost, "/image-tasks", malformedCreate)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("malformed creative_entry got %d, want 400", rec.Code)
			}
			if fixture.classifier.calls != 0 {
				t.Fatalf("malformed manual union called classifier %d times", fixture.classifier.calls)
			}
			for _, table := range tables {
				var after int
				if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&after); err != nil {
					t.Fatalf("count %s after malformed create: %v", table, err)
				}
				if after != before[table] {
					t.Fatalf("malformed creative_entry changed %s rows: before=%d after=%d",
						table, before[table], after)
				}
			}
		})
	}

	fixture := newImageTaskHTTPFixture(t)
	body := fmt.Sprintf(`{
		"agent":"mingming","source_session":"session-1","source_kind":"desktop",
		"source_ref":"manual-art-mixed","source_asset_refs":[%q],
		"attempt_generation":1,
		"route_request":{"provider":"hexclaw-gpt","model":"gpt-5.6-sol","selection_source":"explicit"},
		"creative_entry":{"kind":"new_work","task_intent":"artwork"}
	}`, fixture.assetID)
	_, out := do(t, fixture.handler, http.MethodPost, "/image-tasks", body)
	dispatch := out["dispatch"].(map[string]any)
	mixedConfirm := fmt.Sprintf(`{
		"agent":"mingming","version":%v,
		"creative":{"action":"commit","canonical_version":1,"canonical_content":"mixed"}
	}`, dispatch["version"])
	rec, _ := do(t, fixture.handler, http.MethodPost,
		"/image-tasks/"+dispatch["dispatch_id"].(string)+"/confirm", mixedConfirm)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("mixed commit/freeze_ocr got %d, want 400", rec.Code)
	}
}

func TestImageTaskHTTPManualWritingClearOCRRequiresFreezeBeforeCommit(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	fixture.ocr.result = usecase.ImageTaskWritingOCRResult{
		Raw: "我的好爸爸", CanonicalContent: "我的好爸爸", Confidence: 0.99,
	}
	body := fmt.Sprintf(`{
		"agent":"mingming","source_session":"session-1","source_kind":"desktop",
		"source_ref":"manual-writing-1","source_asset_refs":[%q],
		"attempt_generation":1,
		"route_request":{"provider":"hexclaw-gpt","model":"gpt-5.6-sol","selection_source":"explicit"},
		"creative_entry":{"kind":"new_work","task_intent":"writing"}
	}`, fixture.assetID)
	rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual writing create: %d %#v", rec.Code, out)
	}
	dispatchID := out["dispatch"].(map[string]any)["dispatch_id"].(string)
	_, out = waitImageTaskHTTPState(t, fixture, dispatchID, func(dispatch map[string]any) bool {
		target, _ := dispatch["target_projection"].(map[string]any)
		return target["status"] == string(k12.CreativeWorkIntakeAwaitingConfirmation)
	})
	dispatch := out["dispatch"].(map[string]any)
	target := dispatch["target_projection"].(map[string]any)
	if _, invented := target["conflicts"]; invented ||
		target["canonical_content"] != "我的好爸爸" ||
		target["commit_required"] != true {
		t.Fatalf("manual clear OCR projection drift: %#v", target)
	}
	freeze := fmt.Sprintf(`{
		"agent":"mingming","version":%v,
		"creative":{"action":"freeze_ocr","canonical_version":1,
		"canonical_content":"我的好爸爸"}
	}`, dispatch["version"])
	rec, out = do(t, fixture.handler, http.MethodPost,
		"/image-tasks/"+dispatchID+"/confirm", freeze)
	if rec.Code != http.StatusOK {
		t.Fatalf("freeze manual OCR: %d %#v", rec.Code, out)
	}
	target = out["dispatch"].(map[string]any)["target_projection"].(map[string]any)
	if target["status"] != "ready" || target["commit_state"] != "pending" {
		t.Fatalf("freeze_ocr did not stop before commit: %#v", target)
	}
}

func TestImageTaskHTTPManualWritingPersistsEverySegmentCorrectionInFrozenEvidence(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	body := fmt.Sprintf(`{
		"agent":"mingming","source_session":"session-1","source_kind":"desktop",
		"source_ref":"manual-writing-corrections","source_asset_refs":[%q],
		"attempt_generation":1,
		"route_request":{"provider":"hexclaw-gpt","model":"gpt-5.6-sol","selection_source":"explicit"},
		"creative_entry":{"kind":"new_work","task_intent":"writing"}
	}`, fixture.assetID)
	rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual writing create: %d %#v", rec.Code, out)
	}
	dispatchID := out["dispatch"].(map[string]any)["dispatch_id"].(string)
	_, out = waitImageTaskHTTPState(t, fixture, dispatchID, func(dispatch map[string]any) bool {
		target, _ := dispatch["target_projection"].(map[string]any)
		return target["status"] == string(k12.CreativeWorkIntakeAwaitingConfirmation)
	})
	dispatch := out["dispatch"].(map[string]any)
	freeze := fmt.Sprintf(`{
		"agent":"mingming","version":%v,
		"creative":{"action":"freeze_ocr","canonical_version":1,
		"canonical_content":"我的好爸爸",
		"segment_corrections":[
			{"segment_id":"line-1-word-5","canonical_text":"爸"}
		]}
	}`, dispatch["version"])
	rec, out = do(t, fixture.handler, http.MethodPost,
		"/image-tasks/"+dispatchID+"/confirm", freeze)
	if rec.Code != http.StatusOK {
		t.Fatalf("freeze corrected OCR: %d %#v", rec.Code, out)
	}
	view, err := fixture.coordinator.Get(context.Background(), "mingming", dispatchID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Creative == nil || view.Creative.OCREvidence == nil ||
		view.Creative.ConfirmationProvenance != k12.CreativeWorkParentCorrected ||
		view.Creative.OCREvidence.CanonicalContent != "我的好爸爸" ||
		len(view.Creative.OCREvidence.SegmentCorrections) != 1 ||
		view.Creative.OCREvidence.SegmentCorrections[0].SegmentID != "line-1-word-5" ||
		view.Creative.OCREvidence.SegmentCorrections[0].CanonicalText != "爸" {
		t.Fatalf("structured segment correction did not reach frozen evidence: %+v", view.Creative)
	}
}

func TestImageTaskHTTPManualWritingRejectsIncompleteOrAmbiguousSegmentCorrections(t *testing.T) {
	tests := []struct {
		name        string
		corrections string
		canonical   string
	}{
		{
			name:        "missing risk segment",
			corrections: `[]`,
			canonical:   "我的好爸爸",
		},
		{
			name: "unknown risk segment",
			corrections: `[
				{"segment_id":"unknown","canonical_text":"爸"}
			]`,
			canonical: "我的好爸爸",
		},
		{
			name: "duplicate risk segment",
			corrections: `[
				{"segment_id":"line-1-word-5","canonical_text":"爸"},
				{"segment_id":"line-1-word-5","canonical_text":"爸爸"}
			]`,
			canonical: "我的好爸爸",
		},
		{
			name: "full canonical disagrees with structured correction",
			corrections: `[
				{"segment_id":"line-1-word-5","canonical_text":"爸"}
			]`,
			canonical: "我的好父亲",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newImageTaskHTTPFixture(t)
			body := fmt.Sprintf(`{
				"agent":"mingming","source_session":"session-1","source_kind":"desktop",
				"source_ref":"manual-writing-invalid-correction","source_asset_refs":[%q],
				"attempt_generation":1,
				"route_request":{"provider":"hexclaw-gpt","model":"gpt-5.6-sol","selection_source":"explicit"},
				"creative_entry":{"kind":"new_work","task_intent":"writing"}
			}`, fixture.assetID)
			rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks", body)
			if rec.Code != http.StatusOK {
				t.Fatalf("manual writing create: %d %#v", rec.Code, out)
			}
			dispatchID := out["dispatch"].(map[string]any)["dispatch_id"].(string)
			_, out = waitImageTaskHTTPState(t, fixture, dispatchID, func(dispatch map[string]any) bool {
				target, _ := dispatch["target_projection"].(map[string]any)
				return target["status"] == string(k12.CreativeWorkIntakeAwaitingConfirmation)
			})
			dispatch := out["dispatch"].(map[string]any)
			freeze := fmt.Sprintf(`{
				"agent":"mingming","version":%v,
				"creative":{"action":"freeze_ocr","canonical_version":1,
				"canonical_content":%q,
				"segment_corrections":%s}
			}`, dispatch["version"], test.canonical, test.corrections)
			rec, _ = do(t, fixture.handler, http.MethodPost,
				"/image-tasks/"+dispatchID+"/confirm", freeze)
			if rec.Code != http.StatusConflict {
				t.Fatalf("invalid corrections got %d, want 409", rec.Code)
			}
			view, err := fixture.coordinator.Get(context.Background(), "mingming", dispatchID)
			if err != nil {
				t.Fatal(err)
			}
			if view.Creative == nil ||
				view.Creative.Status != k12.CreativeWorkIntakeAwaitingConfirmation ||
				view.Creative.OCREvidence == nil ||
				len(view.Creative.OCREvidence.SegmentCorrections) != 0 {
				t.Fatalf("failed correction mutated frozen evidence: %+v", view.Creative)
			}
		})
	}
}

func TestImageTaskHTTPResultHonorsOwnerIsolation(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	rec, out := do(t, fixture.handler, http.MethodPost, "/image-tasks",
		createImageTaskBody(fixture.assetID, "message-isolation"))
	if rec.Code != http.StatusOK {
		t.Fatal(out)
	}
	dispatchID := out["dispatch"].(map[string]any)["dispatch_id"].(string)
	rec, out = do(t, fixture.handler, http.MethodGet,
		"/image-tasks/"+dispatchID+"/result?agent=gege", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-owner result must be 404: %d %#v", rec.Code, out)
	}
}
