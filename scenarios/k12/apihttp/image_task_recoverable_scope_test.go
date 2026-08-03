package apihttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type recoverableImageTaskContract struct {
	DispatchID        string              `json:"dispatch_id"`
	SourceSessionID   string              `json:"source_session_id"`
	SourceMessageID   string              `json:"source_message_id"`
	AttemptGeneration int                 `json:"attempt_generation"`
	Version           int                 `json:"version"`
	Stage             string              `json:"stage"`
	Status            k12.ImageTaskStatus `json:"status"`
	ProjectionReady   bool                `json:"projection_ready"`
	Terminal          bool                `json:"terminal"`
}

type blockingRecoveryGrading struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *blockingRecoveryGrading) StartPhotoGradingJob(
	context.Context,
	usecase.StartPhotoGradingInput,
) (usecase.GradingJobView, bool, error) {
	return usecase.GradingJobView{}, false, nil
}

func (g *blockingRecoveryGrading) ConfirmPhotoGradingJob(
	context.Context,
	string,
	usecase.ConfirmPhotoGradingInput,
) (usecase.GradingJobView, bool, error) {
	return usecase.GradingJobView{}, false, nil
}

func (*blockingRecoveryGrading) StartAsync(string) bool { return false }

func (g *blockingRecoveryGrading) ImageTaskHomeworkProjection(
	ctx context.Context,
	_, _ string,
) (usecase.ImageTaskHomeworkProjection, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.release:
		return usecase.ImageTaskHomeworkProjection{Stage: "running"}, nil
	case <-ctx.Done():
		return usecase.ImageTaskHomeworkProjection{}, ctx.Err()
	}
}

func TestRecoverableImageTasksRequireOwnerAgentSessionAndNeverDispatchWork(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	create := func(session, message string, generation int) usecase.ImageTaskView {
		t.Helper()
		view, created, err := fixture.coordinator.Create(context.Background(), usecase.CreateImageTaskInput{
			OwnerScope: "owner-a", AgentName: "mingming", LearnerID: "mingming",
			SourceKind: k12.ImageTaskSourceDesktop, SourceRef: message,
			SourceSessionID: session, SourceAssetRefs: []string{fixture.assetID},
			AttemptGeneration: generation,
			RouteRequest: k12.ImageTaskRouteSnapshot{
				Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", SelectionSource: "explicit",
			},
		})
		if err != nil || !created {
			t.Fatalf("seed image task: created=%v err=%v", created, err)
		}
		return view
	}
	want := create("session-a", "message-a", 2)
	_ = create("session-b", "message-b", 1)

	authorizeCalls := 0
	newHandler := func(owner string) http.Handler {
		return apihttp.NewHandler(apihttp.Runtime{
			Records: fixture.coordinator.Records, ImageTasks: fixture.coordinator,
			PrincipalMode: "remote",
			AuthenticatedOwnerScope: func(context.Context) (string, error) {
				if owner == "" {
					return "", errors.New("no authenticated principal")
				}
				return owner, nil
			},
			AuthorizeAgentScope: func(_ context.Context, gotOwner, agent string) error {
				authorizeCalls++
				if gotOwner != "owner-a" || agent != "mingming" {
					return errors.New("scope not found")
				}
				return nil
			},
		})
	}
	handler := newHandler("owner-a")
	rec, body := do(t, handler, http.MethodGet,
		"/image-tasks/recoverable?agent=mingming&session=session-a", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("recover projection status=%d body=%#v", rec.Code, body)
	}
	var first struct {
		Items []recoverableImageTaskContract `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 1 {
		t.Fatalf("session projection=%+v, want one item", first.Items)
	}
	item := first.Items[0]
	if item.DispatchID != want.Dispatch.DispatchID || item.SourceSessionID != "session-a" ||
		item.SourceMessageID != "message-a" || item.AttemptGeneration != 2 ||
		item.Version != want.Dispatch.Version || item.Stage != "routing" ||
		item.Status != k12.ImageTaskStatusRouting || item.ProjectionReady || item.Terminal {
		t.Fatalf("recovery contract drift: %+v", item)
	}
	beforeCalls := fixture.classifier.calls
	var beforeDispatches, beforeInvocations int
	if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_dispatches`).Scan(&beforeDispatches); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_invocations`).Scan(&beforeInvocations); err != nil {
		t.Fatal(err)
	}

	// A fresh handler represents a Web/Sidecar projection owner after restart.
	// Repeating the read must return identical stable identities and must not
	// call StartAsync/Run or insert any dispatch/invocation.
	restarted := newHandler("owner-a")
	rec, _ = do(t, restarted, http.MethodGet,
		"/image-tasks/recoverable?agent=mingming&session=session-a", "")
	var second struct {
		Items []recoverableImageTaskContract `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("restart query identity drift: first=%+v second=%+v", first, second)
	}
	var afterDispatches, afterInvocations int
	_ = fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_dispatches`).Scan(&afterDispatches)
	_ = fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_invocations`).Scan(&afterInvocations)
	if fixture.classifier.calls != beforeCalls || afterDispatches != beforeDispatches ||
		afterInvocations != beforeInvocations {
		t.Fatalf("query dispatched work: classifier=%d->%d rows=(%d,%d)->(%d,%d)",
			beforeCalls, fixture.classifier.calls, beforeDispatches, beforeInvocations,
			afterDispatches, afterInvocations)
	}

	for _, tc := range []struct {
		name    string
		handler http.Handler
		path    string
		want    int
	}{
		{"missing principal", newHandler(""), "/image-tasks/recoverable?agent=mingming&session=session-a", http.StatusUnauthorized},
		{"cross owner", newHandler("owner-b"), "/image-tasks/recoverable?agent=mingming&session=session-a", http.StatusNotFound},
		{"cross agent", newHandler("owner-a"), "/image-tasks/recoverable?agent=gege&session=session-a", http.StatusNotFound},
		{"missing session", newHandler("owner-a"), "/image-tasks/recoverable?agent=mingming", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := do(t, tc.handler, http.MethodGet, tc.path, "")
			if rec.Code != tc.want {
				t.Fatalf("status=%d, want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if authorizeCalls == 0 {
		t.Fatal("owner-to-agent authorization was bypassed")
	}
}

func TestRemoteImageTaskCommandsAndQueriesAllEnforceOwnerAgentBindingBeforeMutation(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	seed, created, err := fixture.coordinator.Create(
		context.Background(),
		usecase.CreateImageTaskInput{
			OwnerScope: "owner-a", AgentName: "mingming", LearnerID: "mingming",
			SourceKind: k12.ImageTaskSourceDesktop, SourceRef: "scope-seed",
			SourceSessionID: "scope-session", SourceAssetRefs: []string{fixture.assetID},
			AttemptGeneration: 1,
			RouteRequest: k12.ImageTaskRouteSnapshot{
				Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", SelectionSource: "explicit",
			},
		},
	)
	if err != nil || !created {
		t.Fatalf("seed image task: created=%v err=%v", created, err)
	}
	denied := apihttp.NewHandler(apihttp.Runtime{
		Records: fixture.coordinator.Records, ImageTasks: fixture.coordinator,
		PrincipalMode: "remote",
		AuthenticatedOwnerScope: func(context.Context) (string, error) {
			return "attacker", nil
		},
		AuthorizeAgentScope: func(context.Context, string, string) error {
			return errors.New("scope not found")
		},
	})
	missingAuthorizer := apihttp.NewHandler(apihttp.Runtime{
		Records: fixture.coordinator.Records, ImageTasks: fixture.coordinator,
		PrincipalMode: "remote",
		AuthenticatedOwnerScope: func(context.Context) (string, error) {
			return "owner-a", nil
		},
	})
	unauthenticated := apihttp.NewHandler(apihttp.Runtime{
		Records: fixture.coordinator.Records, ImageTasks: fixture.coordinator,
		PrincipalMode: "remote",
		AuthenticatedOwnerScope: func(context.Context) (string, error) {
			return "", errors.New("missing principal")
		},
		AuthorizeAgentScope: func(context.Context, string, string) error { return nil },
	})

	var beforeDispatches, beforeInvocations int
	if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_dispatches`).Scan(&beforeDispatches); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_invocations`).Scan(&beforeInvocations); err != nil {
		t.Fatal(err)
	}
	createBody := createImageTaskBody(fixture.assetID, "attacker-create")
	versionBody := `{"agent":"mingming","version":1}`
	for _, handler := range []struct {
		name string
		h    http.Handler
		want int
	}{
		{name: "denied", h: denied, want: http.StatusNotFound},
		{name: "missing authorizer", h: missingAuthorizer, want: http.StatusNotFound},
		{name: "missing principal", h: unauthenticated, want: http.StatusUnauthorized},
	} {
		t.Run(handler.name, func(t *testing.T) {
			for _, request := range []struct {
				method string
				path   string
				body   string
			}{
				{http.MethodPost, "/image-tasks", createBody},
				{http.MethodGet, "/image-tasks/" + seed.Dispatch.DispatchID + "?agent=mingming", ""},
				{http.MethodGet, "/image-tasks/" + seed.Dispatch.DispatchID + "/result?agent=mingming", ""},
				{http.MethodPost, "/image-tasks/" + seed.Dispatch.DispatchID + "/confirm", versionBody},
				{http.MethodPost, "/image-tasks/" + seed.Dispatch.DispatchID + "/retry", versionBody},
				{http.MethodPost, "/image-tasks/" + seed.Dispatch.DispatchID + "/cancel", versionBody},
			} {
				rec, _ := do(t, handler.h, request.method, request.path, request.body)
				if rec.Code != handler.want {
					t.Fatalf("%s %s status=%d want=%d body=%s", request.method, request.path, rec.Code, handler.want, rec.Body.String())
				}
			}
		})
	}
	var afterDispatches, afterInvocations int
	_ = fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_dispatches`).Scan(&afterDispatches)
	_ = fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_invocations`).Scan(&afterInvocations)
	if afterDispatches != beforeDispatches || afterInvocations != beforeInvocations {
		t.Fatalf("unauthorized image task request mutated ledgers: dispatch=%d->%d invocation=%d->%d",
			beforeDispatches, afterDispatches, beforeInvocations, afterInvocations)
	}
}

func TestRemoteImageTaskDispatchScopeIsFrozenAcrossAgentTransfer(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	create := func(ownerScope, sourceRef string) usecase.ImageTaskView {
		t.Helper()
		view, created, err := fixture.coordinator.Create(
			context.Background(),
			usecase.CreateImageTaskInput{
				OwnerScope: ownerScope, AgentName: "mingming", LearnerID: "mingming",
				SourceKind: k12.ImageTaskSourceDesktop, SourceRef: sourceRef,
				SourceSessionID: "transferred-session", SourceAssetRefs: []string{fixture.assetID},
				AttemptGeneration: 1,
				RouteRequest: k12.ImageTaskRouteSnapshot{
					Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", SelectionSource: "explicit",
				},
			},
		)
		if err != nil || !created {
			t.Fatalf("seed image task: owner=%q created=%v err=%v", ownerScope, created, err)
		}
		return view
	}
	ownerA := create("owner-a", "owner-a-history")
	ownerB := create("owner-b", "owner-b-current")
	legacy := create("", "legacy-without-frozen-owner")

	// Simulate an Agent transfer: owner-b is now allowed to address the Agent,
	// but that current grant must not confer access to owner-a's historical
	// dispatch or to a legacy dispatch whose immutable owner cannot be proven.
	handler := apihttp.NewHandler(apihttp.Runtime{
		Records: fixture.coordinator.Records, ImageTasks: fixture.coordinator,
		PrincipalMode: "remote",
		AuthenticatedOwnerScope: func(context.Context) (string, error) {
			return "owner-b", nil
		},
		AuthorizeAgentScope: func(context.Context, string, string) error { return nil },
	})

	var beforeDispatches, beforeInvocations int
	if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_dispatches`).Scan(&beforeDispatches); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_invocations`).Scan(&beforeInvocations); err != nil {
		t.Fatal(err)
	}
	versionBody := `{"agent":"mingming","version":1}`
	for _, dispatchID := range []string{ownerA.Dispatch.DispatchID, legacy.Dispatch.DispatchID} {
		for _, request := range []struct {
			method string
			path   string
			body   string
		}{
			{http.MethodGet, "/image-tasks/" + dispatchID + "?agent=mingming", ""},
			{http.MethodGet, "/image-tasks/" + dispatchID + "/result?agent=mingming", ""},
			{http.MethodPost, "/image-tasks/" + dispatchID + "/confirm", versionBody},
			{http.MethodPost, "/image-tasks/" + dispatchID + "/retry", versionBody},
			{http.MethodPost, "/image-tasks/" + dispatchID + "/cancel", versionBody},
		} {
			rec, _ := do(t, handler, request.method, request.path, request.body)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s status=%d want=404 body=%s", request.method, request.path, rec.Code, rec.Body.String())
			}
		}
	}

	rec, body := do(t, handler, http.MethodGet,
		"/image-tasks/recoverable?agent=mingming&session=transferred-session", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("recover projection status=%d body=%#v", rec.Code, body)
	}
	var payload struct {
		Items []recoverableImageTaskContract `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 || payload.Items[0].DispatchID != ownerB.Dispatch.DispatchID {
		t.Fatalf("owner-scoped recovery leaked historical dispatches: %+v", payload.Items)
	}
	rec, _ = do(t, handler, http.MethodGet,
		"/image-tasks/"+ownerB.Dispatch.DispatchID+"?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("current owner's dispatch status=%d body=%s", rec.Code, rec.Body.String())
	}

	var afterDispatches, afterInvocations int
	_ = fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_dispatches`).Scan(&afterDispatches)
	_ = fixture.db.QueryRow(`SELECT COUNT(*) FROM k12_image_task_invocations`).Scan(&afterInvocations)
	if afterDispatches != beforeDispatches || afterInvocations != beforeInvocations {
		t.Fatalf("cross-owner requests mutated ledgers: dispatch=%d->%d invocation=%d->%d",
			beforeDispatches, afterDispatches, beforeInvocations, afterInvocations)
	}
}

func TestRecoverableImageTasksUseOneAuthoritativeMutableDispatchSnapshot(t *testing.T) {
	fixture := newImageTaskHTTPFixture(t)
	create := func(message string) usecase.ImageTaskView {
		t.Helper()
		view, created, err := fixture.coordinator.Create(context.Background(), usecase.CreateImageTaskInput{
			AgentName: "mingming", LearnerID: "mingming",
			SourceKind: k12.ImageTaskSourceDesktop, SourceRef: message,
			SourceSessionID: "session-torn-read", SourceAssetRefs: []string{fixture.assetID},
			AttemptGeneration: 1,
			RouteRequest: k12.ImageTaskRouteSnapshot{
				Provider: "hexclaw-gpt", Model: "gpt-5.6-sol", SelectionSource: "explicit",
			},
		})
		if err != nil || !created {
			t.Fatalf("seed image task: created=%v err=%v", created, err)
		}
		return view
	}
	blocker := create("message-blocker")
	target := create("message-target")

	// The first row blocks inside ImageTasks.Get after the recovery list query
	// has materialized both old dispatch snapshots. This makes the otherwise
	// narrow list->Get update window deterministic without production hooks.
	if _, err := fixture.db.Exec(`UPDATE k12_image_task_dispatches
		SET task_intent='completed_homework',status='routed',
		    target_object_type='homework_submission',target_object_id='submission-blocker',
		    created_at=1,updated_at=1,version=version+1
		WHERE dispatch_id=?`, blocker.Dispatch.DispatchID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`INSERT INTO k12_homework_submissions
		(submission_id,dispatch_id,agent_name,learner_id,source_kind,source_ref,
		 source_asset_refs_json,task_intent,status,grading_job_id,idempotency_key,
		 version,created_at,updated_at)
		VALUES('submission-blocker',?,'mingming','mingming','desktop','message-blocker',
		 '[]','completed_homework','processing','grading-blocker','submission-blocker',1,1,1)`,
		blocker.Dispatch.DispatchID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.db.Exec(`UPDATE k12_image_task_dispatches
		SET created_at=2,updated_at=2 WHERE dispatch_id=?`, target.Dispatch.DispatchID); err != nil {
		t.Fatal(err)
	}

	gate := &blockingRecoveryGrading{entered: make(chan struct{}), release: make(chan struct{})}
	fixture.coordinator.Grading = gate
	handler := apihttp.NewHandler(apihttp.Runtime{
		Records: fixture.coordinator.Records, ImageTasks: fixture.coordinator,
	})
	requestContext, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	req := httptest.NewRequest(http.MethodGet,
		"/image-tasks/recoverable?agent=mingming&session=session-torn-read", nil).
		WithContext(requestContext)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	select {
	case <-gate.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery query did not enter controlled list-to-Get window")
	}
	if _, err := fixture.db.Exec(`UPDATE k12_image_task_dispatches
		SET status='cancelled',version=version+1,updated_at=3 WHERE dispatch_id=?`,
		target.Dispatch.DispatchID); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery query did not finish")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("recovery status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Items []recoverableImageTaskContract `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, item := range payload.Items {
		if item.DispatchID != target.Dispatch.DispatchID {
			continue
		}
		if item.Status != k12.ImageTaskStatusCancelled ||
			item.Version != target.Dispatch.Version+1 || item.Stage != "cancelled" ||
			!item.ProjectionReady || !item.Terminal {
			t.Fatalf("torn recovery projection: %+v", item)
		}
		return
	}
	t.Fatalf("target dispatch missing from recovery projection: %+v", payload.Items)
}
