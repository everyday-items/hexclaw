package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/skill"
)

type semanticIndexServiceStub struct {
	getPolicyFn       func(context.Context, string, string) (knowledge.EmbeddingPolicyProjection, error)
	applyFn           func(context.Context, string, string, int64, knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error)
	getJobFn          func(context.Context, string, string) (knowledge.KnowledgeJob, error)
	cancelJobFn       func(context.Context, string, string) (knowledge.KnowledgeJob, error)
	getScopedJobFn    func(context.Context, string, string, string) (knowledge.KnowledgeJob, error)
	cancelScopedJobFn func(context.Context, string, string, string) (knowledge.KnowledgeJob, error)
	applyCalls        int
}

func (s *semanticIndexServiceStub) GetPolicy(ctx context.Context, ownerID, corpusID string) (knowledge.EmbeddingPolicyProjection, error) {
	return s.getPolicyFn(ctx, ownerID, corpusID)
}

func (s *semanticIndexServiceStub) ApplyPolicy(ctx context.Context, ownerID, corpusID string, expectedVersion int64, selection knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
	s.applyCalls++
	return s.applyFn(ctx, ownerID, corpusID, expectedVersion, selection)
}

func (s *semanticIndexServiceStub) GetJob(ctx context.Context, ownerID, jobID string) (knowledge.KnowledgeJob, error) {
	return s.getJobFn(ctx, ownerID, jobID)
}

func (s *semanticIndexServiceStub) CancelJob(ctx context.Context, ownerID, jobID string) (knowledge.KnowledgeJob, error) {
	return s.cancelJobFn(ctx, ownerID, jobID)
}

func (s *semanticIndexServiceStub) GetJobForCorpus(ctx context.Context, ownerID, corpusID, jobID string) (knowledge.KnowledgeJob, error) {
	if s.getScopedJobFn != nil {
		return s.getScopedJobFn(ctx, ownerID, corpusID, jobID)
	}
	return s.getJobFn(ctx, ownerID, jobID)
}

func (s *semanticIndexServiceStub) CancelJobForCorpus(ctx context.Context, ownerID, corpusID, jobID string) (knowledge.KnowledgeJob, error) {
	if s.cancelScopedJobFn != nil {
		return s.cancelScopedJobFn(ctx, ownerID, corpusID, jobID)
	}
	return s.cancelJobFn(ctx, ownerID, jobID)
}

func newSemanticIndexHTTPServer(t *testing.T, svc semanticIndexAPI) *httptest.Server {
	t.Helper()
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetSemanticIndexService(svc)
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return ts
}

func TestKnowledgeAPIPrincipalCannotBeForgedAndUnsupportedCorpusFailsClosed(t *testing.T) {
	var gotOwner, gotCorpus string
	policyCalls := 0
	stub := &semanticIndexServiceStub{
		getPolicyFn: func(_ context.Context, ownerID, corpusID string) (knowledge.EmbeddingPolicyProjection, error) {
			policyCalls++
			gotOwner, gotCorpus = ownerID, corpusID
			return knowledge.EmbeddingPolicyProjection{}, nil
		},
		applyFn: func(context.Context, string, string, int64, knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
			return knowledge.ApplyPolicyResult{}, nil
		},
		getJobFn: func(context.Context, string, string) (knowledge.KnowledgeJob, error) {
			return knowledge.KnowledgeJob{}, nil
		},
	}
	stub.cancelJobFn = stub.getJobFn
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetSemanticIndexService(stub)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/knowledge/corpora/default/embedding-policy?user_id=forged-remote-user", nil)
	req.SetPathValue("corpus_id", "default")
	rec := httptest.NewRecorder()
	srv.handleGetKnowledgeEmbeddingPolicy(rec, req)
	if rec.Code != http.StatusOK || gotOwner != "desktop-user" || gotCorpus != "default" {
		t.Fatalf("status=%d scope=%q/%q", rec.Code, gotOwner, gotCorpus)
	}

	policyCalls = 0
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/knowledge/corpora/another-child/embedding-policy?user_id=desktop-user", nil)
	req.SetPathValue("corpus_id", "another-child")
	rec = httptest.NewRecorder()
	srv.handleGetKnowledgeEmbeddingPolicy(rec, req)
	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnprocessableEntity || payload["code"] != "knowledge_scope_unsupported" || policyCalls != 0 {
		t.Fatalf("unsupported corpus status=%d payload=%v calls=%d", rec.Code, payload, policyCalls)
	}
}

func TestKnowledgeJobEndpointsRequireDefaultOwnerAndCorpus(t *testing.T) {
	var gotOwner, gotCorpus, gotJob string
	getCalls, cancelCalls := 0, 0
	stub := &semanticIndexServiceStub{
		getPolicyFn: func(context.Context, string, string) (knowledge.EmbeddingPolicyProjection, error) {
			return knowledge.EmbeddingPolicyProjection{}, nil
		},
		applyFn: func(context.Context, string, string, int64, knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
			return knowledge.ApplyPolicyResult{}, nil
		},
		getJobFn: func(context.Context, string, string) (knowledge.KnowledgeJob, error) {
			return knowledge.KnowledgeJob{JobID: "job-other", CorpusUID: "corpus-other"}, nil
		},
		cancelJobFn: func(context.Context, string, string) (knowledge.KnowledgeJob, error) {
			return knowledge.KnowledgeJob{JobID: "job-other", CorpusUID: "corpus-other"}, nil
		},
		getScopedJobFn: func(_ context.Context, ownerID, corpusID, jobID string) (knowledge.KnowledgeJob, error) {
			getCalls++
			gotOwner, gotCorpus, gotJob = ownerID, corpusID, jobID
			return knowledge.KnowledgeJob{}, knowledge.ErrSemanticIndexNotFound
		},
		cancelScopedJobFn: func(_ context.Context, ownerID, corpusID, jobID string) (knowledge.KnowledgeJob, error) {
			cancelCalls++
			gotOwner, gotCorpus, gotJob = ownerID, corpusID, jobID
			return knowledge.KnowledgeJob{}, knowledge.ErrSemanticIndexNotFound
		},
	}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetSemanticIndexService(stub)

	for _, testCase := range []struct {
		method string
		path   string
		setID  func(*http.Request)
		handle http.HandlerFunc
	}{
		{http.MethodGet, "/api/v1/knowledge/jobs/job-other?user_id=forged", func(r *http.Request) { r.SetPathValue("job_id", "job-other") }, srv.handleGetKnowledgeJob},
		{http.MethodPost, "/api/v1/knowledge/jobs/job-other/cancel?user_id=forged", func(r *http.Request) { r.SetPathValue("job_id", "job-other") }, srv.handleCancelKnowledgeJob},
	} {
		req := httptest.NewRequest(testCase.method, testCase.path, nil)
		testCase.setID(req)
		rec := httptest.NewRecorder()
		testCase.handle(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status=%d body=%s", testCase.method, rec.Code, rec.Body.String())
		}
	}
	if getCalls != 1 || cancelCalls != 1 || gotOwner != "desktop-user" || gotCorpus != "default" || gotJob != "job-other" {
		t.Fatalf("scoped job calls get=%d cancel=%d args=%q/%q/%q", getCalls, cancelCalls, gotOwner, gotCorpus, gotJob)
	}
}

func TestSemanticIndexPolicyHTTPContract(t *testing.T) {
	profileID := "cloud-fast"
	done, total := int64(135), int64(225)
	stub := &semanticIndexServiceStub{}
	stub.getPolicyFn = func(_ context.Context, ownerID, corpusID string) (knowledge.EmbeddingPolicyProjection, error) {
		if ownerID != "desktop-user" || corpusID != "default" {
			t.Fatalf("scope = %q/%q", ownerID, corpusID)
		}
		profile := knowledge.EmbeddingProfile{
			ProfileID: profileID, ModelName: "text-embedding-3-small",
			ProviderID: "openai-compatible", ProviderName: "OpenAI 兼容",
			Location: knowledge.ProviderLocation("cloud"), Capability: "embedding",
			Dimension: 1536, Availability: knowledge.ProfileAvailability("connected"),
		}
		return knowledge.EmbeddingPolicyProjection{
			PolicyVersion: 7,
			Selection:     knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionKind("auto")},
			ActiveRevision: &knowledge.EmbeddingRevisionProjection{
				RevisionID: "rev-a", State: knowledge.VectorIndexState("ready"),
				ProfileConfigHash: "profile-hash-a", Profile: profile,
			},
			IndexingActivity: knowledge.IndexingActivity{
				State: knowledge.IndexingActivityState("building"), ProcessingDocuments: 1,
				ChunksDone: &done, ChunksTotal: &total,
			},
			AvailableProfiles: []knowledge.EmbeddingProfile{profile},
			Recommendation:    &knowledge.EmbeddingRecommendation{ProfileID: &profileID, ReasonCode: "large_document", ReasonText: "大文件优先批处理"},
			CatalogVersion:    3,
		}, nil
	}
	stub.applyFn = func(_ context.Context, ownerID, corpusID string, version int64, selection knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
		if ownerID != "desktop-user" || corpusID != "default" || version != 7 || string(selection.Kind) != "disabled" {
			t.Fatalf("apply args = %q/%q version=%d selection=%+v", ownerID, corpusID, version, selection)
		}
		return knowledge.ApplyPolicyResult{PolicyVersion: 8, Selection: selection}, nil
	}
	stub.getJobFn = func(context.Context, string, string) (knowledge.KnowledgeJob, error) {
		return knowledge.KnowledgeJob{}, nil
	}
	stub.cancelJobFn = stub.getJobFn
	ts := newSemanticIndexHTTPServer(t, stub)

	resp, err := http.Get(ts.URL + "/api/v1/knowledge/corpora/default/embedding-policy?user_id=desktop-user")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d", resp.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	active := payload["active_revision"].(map[string]any)
	if active["profile"].(map[string]any)["model_name"] != "text-embedding-3-small" {
		t.Fatalf("active revision must carry immutable nested profile: %+v", active)
	}
	if active["profile_config_hash"] != "profile-hash-a" {
		t.Fatalf("active revision must carry immutable profile hash: %+v", active)
	}
	activity := payload["indexing_activity"].(map[string]any)
	if activity["chunks_done"] != float64(135) || activity["chunks_total"] != float64(225) {
		t.Fatalf("activity must use persisted chunk progress: %+v", activity)
	}

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/v1/knowledge/corpora/default/embedding-policy:apply?user_id=desktop-user",
		strings.NewReader(`{"expected_policy_version":7,"selection":{"kind":"disabled"}}`))
	req.Header.Set("Content-Type", "application/json")
	applyResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer applyResp.Body.Close()
	if applyResp.StatusCode != http.StatusOK {
		t.Fatalf("apply status=%d", applyResp.StatusCode)
	}
}

func TestSemanticIndexApplyRejectsMalformedUnionBeforeService(t *testing.T) {
	stub := &semanticIndexServiceStub{
		getPolicyFn: func(context.Context, string, string) (knowledge.EmbeddingPolicyProjection, error) {
			return knowledge.EmbeddingPolicyProjection{}, nil
		},
		applyFn: func(context.Context, string, string, int64, knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
			return knowledge.ApplyPolicyResult{}, nil
		},
		getJobFn: func(context.Context, string, string) (knowledge.KnowledgeJob, error) {
			return knowledge.KnowledgeJob{}, nil
		},
	}
	stub.cancelJobFn = stub.getJobFn
	badBodies := []string{
		`{"expected_policy_version":1,"selection":{"kind":"auto","profile_id":"x"}}`,
		`{"expected_policy_version":1,"selection":{"kind":"profile"}}`,
		`{"expected_policy_version":1,"selection":{"kind":"disabled","location":"local"}}`,
		`{"expected_policy_version":1,"selection":{"kind":"auto"},"mode":"cloud"}`,
	}
	for _, body := range badBodies {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/corpora/default/embedding-policy:apply?user_id=desktop-user", strings.NewReader(body))
		req.SetPathValue("corpus_id", "default")
		rec := httptest.NewRecorder()
		srv := NewServer(config.DefaultConfig(), nil, nil, nil)
		srv.SetSemanticIndexService(stub)
		srv.handleApplyKnowledgeEmbeddingPolicy(rec, req)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
	if stub.applyCalls != 0 {
		t.Fatalf("malformed unions reached service %d times", stub.applyCalls)
	}
}

func TestSemanticIndexJobHTTPContractAndErrorMapping(t *testing.T) {
	stub := &semanticIndexServiceStub{}
	stub.getPolicyFn = func(context.Context, string, string) (knowledge.EmbeddingPolicyProjection, error) {
		return knowledge.EmbeddingPolicyProjection{}, nil
	}
	stub.applyFn = func(context.Context, string, string, int64, knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
		return knowledge.ApplyPolicyResult{}, knowledge.ErrPolicyVersionConflict
	}
	stub.getJobFn = func(_ context.Context, ownerID, jobID string) (knowledge.KnowledgeJob, error) {
		if ownerID != "desktop-user" || jobID != "job-1" {
			t.Fatalf("get job scope=%q id=%q", ownerID, jobID)
		}
		return knowledge.KnowledgeJob{JobID: jobID}, nil
	}
	stub.cancelJobFn = func(_ context.Context, ownerID, jobID string) (knowledge.KnowledgeJob, error) {
		if ownerID != "desktop-user" || jobID != "job-1" {
			t.Fatalf("cancel job scope=%q id=%q", ownerID, jobID)
		}
		return knowledge.KnowledgeJob{JobID: jobID, CancelRequested: true}, nil
	}
	ts := newSemanticIndexHTTPServer(t, stub)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/knowledge/jobs/job-1?user_id=desktop-user"},
		{http.MethodPost, "/api/v1/knowledge/jobs/job-1/cancel?user_id=desktop-user"},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+tc.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("%s %s status=%d", tc.method, tc.path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	req, _ := http.NewRequest(http.MethodPost,
		ts.URL+"/api/v1/knowledge/corpora/default/embedding-policy:apply?user_id=desktop-user",
		strings.NewReader(`{"expected_policy_version":1,"selection":{"kind":"auto"}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("version conflict status=%d", resp.StatusCode)
	}
}

func TestSemanticIndexValidationErrorsAreClientErrors(t *testing.T) {
	for _, semanticErr := range []error{
		knowledge.ErrInvalidSelection,
		knowledge.ErrInvalidEmbeddingProfile,
		knowledge.ErrProfileUnavailable,
	} {
		rec := httptest.NewRecorder()
		writeSemanticIndexError(rec, semanticErr)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("error %v status=%d, want %d", semanticErr, rec.Code, http.StatusUnprocessableEntity)
		}
	}
	rec := httptest.NewRecorder()
	writeSemanticIndexError(rec, knowledge.ErrSemanticIndexNotFound)
	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusNotFound || payload["code"] != "semantic_index_not_found" {
		t.Fatalf("not-found contract status=%d payload=%v", rec.Code, payload)
	}
}

func newAuthenticatedSemanticIndexApplyRequest(t *testing.T, corpusID, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequestWithContext(
		skill.WithAuthenticatedUser(t.Context(), "authenticated-owner"),
		http.MethodPost,
		"/api/v1/knowledge/corpora/"+corpusID+"/embedding-policy:apply?user_id=forged-query-owner",
		strings.NewReader(body),
	)
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("corpus_id", corpusID)
	return req
}

func TestSemanticIndexApplyUsesAuthenticatedOwnerAndRejectsForgedBodyUser(t *testing.T) {
	var gotOwner, gotCorpus string
	var gotVersion int64
	var gotSelection knowledge.EmbeddingSelection
	stub := &semanticIndexServiceStub{
		applyFn: func(_ context.Context, ownerID, corpusID string, expectedVersion int64, selection knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
			gotOwner, gotCorpus = ownerID, corpusID
			gotVersion, gotSelection = expectedVersion, selection
			return knowledge.ApplyPolicyResult{}, nil
		},
	}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetSemanticIndexService(stub)

	rec := httptest.NewRecorder()
	srv.handleApplyKnowledgeEmbeddingPolicy(rec, newAuthenticatedSemanticIndexApplyRequest(t, "default",
		`{"expected_policy_version":17,"selection":{"kind":"profile","profile_id":"failed-explicit-profile"}}`))
	if rec.Code != http.StatusOK || gotOwner != "authenticated-owner" || gotCorpus != "default" ||
		gotVersion != 17 || gotSelection != (knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionProfile, ProfileID: "failed-explicit-profile"}) {
		t.Fatalf("valid apply status=%d owner=%q corpus=%q version=%d selection=%+v", rec.Code, gotOwner, gotCorpus, gotVersion, gotSelection)
	}

	rec = httptest.NewRecorder()
	srv.handleApplyKnowledgeEmbeddingPolicy(rec, newAuthenticatedSemanticIndexApplyRequest(t, "default",
		`{"expected_policy_version":17,"selection":{"kind":"profile","profile_id":"failed-explicit-profile"},"user_id":"forged-body-owner"}`))
	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest || stub.applyCalls != 1 || !strings.Contains(payload["error"], `unknown field "user_id"`) {
		t.Fatalf("forged body status=%d payload=%v apply_calls=%d", rec.Code, payload, stub.applyCalls)
	}
}

func TestSemanticIndexApplyRejectsNonDefaultCorpusBeforeService(t *testing.T) {
	stub := &semanticIndexServiceStub{
		applyFn: func(context.Context, string, string, int64, knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
			t.Fatal("unsupported corpus must not reach semantic-index service")
			return knowledge.ApplyPolicyResult{}, nil
		},
	}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetSemanticIndexService(stub)

	rec := httptest.NewRecorder()
	srv.handleApplyKnowledgeEmbeddingPolicy(rec, newAuthenticatedSemanticIndexApplyRequest(t, "non-default",
		`{"expected_policy_version":17,"selection":{"kind":"disabled"}}`))
	var payload map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusUnprocessableEntity || payload["code"] != "knowledge_scope_unsupported" || stub.applyCalls != 0 {
		t.Fatalf("unsupported corpus status=%d payload=%v apply_calls=%d", rec.Code, payload, stub.applyCalls)
	}
}

func TestSemanticIndexApplyExplicitProfileFailureIsScopedAndSanitized(t *testing.T) {
	const internalError = "sqlite: private provider failure"
	var gotOwner, gotCorpus string
	var gotVersion int64
	var gotSelection knowledge.EmbeddingSelection
	stub := &semanticIndexServiceStub{
		applyFn: func(_ context.Context, ownerID, corpusID string, expectedVersion int64, selection knowledge.EmbeddingSelection) (knowledge.ApplyPolicyResult, error) {
			gotOwner, gotCorpus = ownerID, corpusID
			gotVersion, gotSelection = expectedVersion, selection
			return knowledge.ApplyPolicyResult{}, errors.New(internalError)
		},
	}
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetSemanticIndexService(stub)

	rec := httptest.NewRecorder()
	srv.handleApplyKnowledgeEmbeddingPolicy(rec, newAuthenticatedSemanticIndexApplyRequest(t, "default",
		`{"expected_policy_version":23,"selection":{"kind":"profile","profile_id":"failed-explicit-profile"}}`))
	rawBody := rec.Body.String()
	var payload map[string]string
	if err := json.NewDecoder(strings.NewReader(rawBody)).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusInternalServerError || payload["code"] != "semantic_index_internal" ||
		payload["error"] != "semantic index temporarily unavailable" || strings.Contains(rawBody, internalError) {
		t.Fatalf("failed explicit apply status=%d payload=%v", rec.Code, payload)
	}
	if gotOwner != "authenticated-owner" || gotCorpus != "default" || gotVersion != 23 ||
		gotSelection != (knowledge.EmbeddingSelection{Kind: knowledge.EmbeddingSelectionProfile, ProfileID: "failed-explicit-profile"}) {
		t.Fatalf("failed explicit apply args owner=%q corpus=%q version=%d selection=%+v", gotOwner, gotCorpus, gotVersion, gotSelection)
	}
}
