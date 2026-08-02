package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/storage/migrate"
	_ "modernc.org/sqlite"
)

const (
	bug20260726MathFivePDFSHA  = "657e1547074668dbb50f2bf37f13c20f292127be64c26c5334190aa34d06de83"
	bug20260726MathFivePDFSize = int64(14_621_452)
)

type bug20260726EmbeddingResolver struct{}

func (bug20260726EmbeddingResolver) Resolve(
	context.Context,
	string,
	string,
	knowledge.EmbeddingSelection,
) (knowledge.EmbeddingProfileSnapshot, error) {
	return knowledge.EmbeddingProfileSnapshot{}, knowledge.ErrProfileUnavailable
}

func (bug20260726EmbeddingResolver) Catalog(
	context.Context,
	string,
	string,
) (knowledge.EmbeddingProfileCatalog, error) {
	return knowledge.EmbeddingProfileCatalog{}, nil
}

type bug20260726MutableVisionRoute struct {
	mu    sync.Mutex
	route knowledge.VisionRouteSnapshot
}

func (r *bug20260726MutableVisionRoute) FreezeDefaultVisionRoute(
	context.Context,
) (knowledge.VisionRouteSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.route, nil
}

func (r *bug20260726MutableVisionRoute) set(route knowledge.VisionRouteSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.route = route
}

type bug20260726KnowledgeHarness struct {
	ctx        context.Context
	db         *sql.DB
	repository *knowledge.SQLiteSemanticIndexRepository
	service    *knowledge.SemanticIndexService
	manager    *knowledge.Manager
	route      *bug20260726MutableVisionRoute
	objectRoot string
	http       *httptest.Server

	captionMu     sync.Mutex
	captionRoutes []knowledge.VisionRouteSnapshot
}

func newBug20260726KnowledgeHarness(
	t *testing.T,
	route knowledge.VisionRouteSnapshot,
) *bug20260726KnowledgeHarness {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "knowledge.db")+
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })

	baseStore := knowledge.NewSQLiteStore(db)
	if err := baseStore.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Run(ctx, db, []migrate.Migration{
		migrate.KnowledgeIndexV23,
		migrate.KnowledgeIngestV24,
		migrate.KnowledgeIngestGenerationsV26,
		migrate.KnowledgeDocumentScopeV27,
		migrate.KnowledgeIngestCheckpointV28,
		migrate.KnowledgeIngestExecutionV46,
		migrate.KnowledgeUploadOperationsV71,
	}); err != nil {
		t.Fatal(err)
	}

	repository := knowledge.NewSQLiteSemanticIndexRepository(db)
	if _, err := repository.BindLegacyDefaultCorpus(ctx, "desktop-user", "default"); err != nil {
		t.Fatal(err)
	}
	service := knowledge.NewSemanticIndexService(repository, bug20260726EmbeddingResolver{})
	objectRoot := filepath.Join(t.TempDir(), "objects")
	if err := service.ConfigureDocumentIngest(objectRoot); err != nil {
		t.Fatal(err)
	}
	routeResolver := &bug20260726MutableVisionRoute{route: route}
	service.ConfigureVisionRouteResolver(routeResolver)

	harness := &bug20260726KnowledgeHarness{
		ctx: ctx, db: db, repository: repository, service: service,
		route: routeResolver, objectRoot: objectRoot,
	}
	scopedStore := knowledge.NewSQLiteStore(
		db,
		knowledge.WithSQLiteSemanticMutations("desktop-user", "default"),
	)
	harness.manager = knowledge.NewManager(
		scopedStore,
		scopedStore,
		nil,
		knowledge.WithSplitter(splitter.NewMarkdownSplitter(
			splitter.WithMarkdownChunkSize(400),
			splitter.WithMarkdownChunkOverlap(80),
		)),
		knowledge.WithCaptioner(knowledge.CaptionerFunc(func(
			ctx context.Context,
			image []byte,
			mime string,
		) (string, error) {
			snapshot, ok := knowledge.VisionRouteSnapshotFromContext(ctx)
			if !ok {
				return "", errors.New("BUG-20260726-024: VLM call lost frozen route snapshot")
			}
			if len(image) == 0 || mime != "image/png" {
				return "", fmt.Errorf("invalid local VLM input bytes=%d mime=%q", len(image), mime)
			}
			harness.captionMu.Lock()
			harness.captionRoutes = append(harness.captionRoutes, snapshot)
			call := len(harness.captionRoutes)
			harness.captionMu.Unlock()
			return fmt.Sprintf("local fake VLM page %d arithmetic lesson", call), nil
		})),
	)
	harness.http = newBug20260726KnowledgeHTTPServer(t, harness.manager, service)
	return harness
}

func newBug20260726KnowledgeHTTPServer(
	t *testing.T,
	manager *knowledge.Manager,
	service *knowledge.SemanticIndexService,
) *httptest.Server {
	t.Helper()
	server := NewServer(config.DefaultConfig(), nil, nil, nil)
	server.SetKnowledgeBase(manager)
	server.SetSemanticIndexService(service)
	httpServer := httptest.NewServer(server.routes())
	t.Cleanup(httpServer.Close)
	return httpServer
}

func (h *bug20260726KnowledgeHarness) captionRouteSnapshots() []knowledge.VisionRouteSnapshot {
	h.captionMu.Lock()
	defer h.captionMu.Unlock()
	return append([]knowledge.VisionRouteSnapshot(nil), h.captionRoutes...)
}

func bug20260726MathFiveFixture(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("real 14.6MB/131-page PDF boundary")
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	repositoryRoot := filepath.Dir(filepath.Dir(sourceFile))
	fixture := filepath.Join(
		filepath.Dir(repositoryRoot),
		"hexclaw-docs",
		"test",
		"\u4e49\u52a1\u6559\u80b2\u6559\u79d1\u4e66\u00b7\u6570\u5b66\u4e94\u5e74\u7ea7\u4e0b\u518c.pdf",
	)
	file, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		t.Fatal(err)
	}
	gotSHA := hex.EncodeToString(digest.Sum(nil))
	if info.Size() != bug20260726MathFivePDFSize || gotSHA != bug20260726MathFivePDFSHA {
		t.Fatalf("fixture bytes=%d sha256=%s want=%d/%s",
			info.Size(), gotSHA, bug20260726MathFivePDFSize, bug20260726MathFivePDFSHA)
	}
	return fixture
}

func bug20260726PostPDF(
	t *testing.T,
	baseURL string,
	fixture string,
	idempotencyKey string,
) knowledge.CreateDocumentResult {
	t.Helper()
	file, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("corpus_id", "default"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("subject", "\u6570\u5b66"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("grade", "\u4e94\u5e74\u7ea7\u4e0b"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", filepath.Base(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(part, file); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/knowledge/documents", &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var result knowledge.CreateDocumentResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted || result.DocumentID == "" || result.JobID == "" {
		t.Fatalf("upload status=%d result=%+v", resp.StatusCode, result)
	}
	return result
}

func bug20260726Route(
	providerID string,
	providerName string,
	displayName string,
	model string,
	capabilities ...string,
) knowledge.VisionRouteSnapshot {
	return knowledge.VisionRouteSnapshot{
		ProviderInstanceID: providerID, ProviderName: providerName,
		ProviderDisplayName: displayName, Model: model, Capabilities: capabilities,
	}.Canonical()
}

func bug20260726AssertPersistedRoute(
	t *testing.T,
	db *sql.DB,
	jobID string,
	want knowledge.VisionRouteSnapshot,
) {
	t.Helper()
	var got knowledge.VisionRouteSnapshot
	var capabilitiesJSON string
	if err := db.QueryRow(`SELECT provider_instance_id,provider_name,provider_display_name,
		model,capabilities_json,selection_fingerprint
		FROM kb_ingest_execution_snapshots WHERE job_id=?`, jobID).Scan(
		&got.ProviderInstanceID,
		&got.ProviderName,
		&got.ProviderDisplayName,
		&got.Model,
		&capabilitiesJSON,
		&got.Fingerprint,
	); err != nil {
		t.Fatal(err)
	}
	if err := got.UnmarshalCapabilitiesJSON(capabilitiesJSON); err != nil {
		t.Fatal(err)
	}
	want = want.Canonical()
	if got.ProviderInstanceID != want.ProviderInstanceID ||
		got.ProviderName != want.ProviderName ||
		got.ProviderDisplayName != want.ProviderDisplayName ||
		got.Model != want.Model ||
		got.Fingerprint != want.Fingerprint {
		t.Fatalf("job %s route=%+v want=%+v", jobID, got, want)
	}
}

func bug20260726RunIngest(
	h *bug20260726KnowledgeHarness,
) (bool, error) {
	worker := knowledge.NewSemanticIndexWorker(
		h.repository,
		nil,
		knowledge.SemanticIndexWorkerConfig{
			OwnerID: "desktop-user", CorpusID: "default",
			WorkerID: "bug-20260726-ingest", BatchSize: 64,
			LeaseDuration: 10 * time.Minute, RetryDelay: time.Second,
		},
	)
	worker.SetDocumentIngestProcessor(NewKnowledgeDocumentIngestProcessor(h.manager))
	return worker.RunOnce(h.ctx)
}

func TestBUG20260726024RealPDFConsumesFrozenDefaultVisionRoute(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	fixture := bug20260726MathFiveFixture(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", "2")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")

	frozen := bug20260726Route(
		"provider-hexclaw-gpt", "hexclaw-gpt", "HexClaw-GPT", "gpt-5.6-sol",
		"text", "vision",
	)
	h := newBug20260726KnowledgeHarness(t, frozen)
	accepted := bug20260726PostPDF(t, h.http.URL, fixture, "bug-20260726-024-frozen-route")
	bug20260726AssertPersistedRoute(t, h.db, accepted.JobID, frozen)

	h.route.set(bug20260726Route(
		"provider-ollama", "ollama", "Ollama (local)", "qwen3-vl",
		"text", "vision",
	))
	worked, err := bug20260726RunIngest(h)
	if err != nil || !worked {
		t.Fatalf("real PDF ingest worked=%v err=%v", worked, err)
	}

	calls := h.captionRouteSnapshots()
	if len(calls) != 7 {
		t.Fatalf("real PDF fake VLM calls=%d want=7", len(calls))
	}
	for index, route := range calls {
		route = route.Canonical()
		if route.ProviderInstanceID != frozen.ProviderInstanceID ||
			route.ProviderName != frozen.ProviderName ||
			route.Model != frozen.Model ||
			route.Fingerprint != frozen.Fingerprint {
			t.Fatalf("VLM call %d route=%+v want frozen=%+v", index+1, route, frozen)
		}
	}
	bug20260726AssertPersistedRoute(t, h.db, accepted.JobID, frozen)

	job, err := h.service.GetJob(h.ctx, "desktop-user", accepted.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != knowledge.KnowledgeJobSucceeded || job.PagesDone == nil ||
		job.PagesTotal == nil || *job.PagesDone != 131 || *job.PagesTotal != 131 {
		t.Fatalf("real PDF terminal job=%+v", job)
	}
	var documents, sources, jobs, nonReadySegments int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM kb_documents WHERE id=?`, accepted.DocumentID).
		Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM kb_ingest_document_sources WHERE document_id=?`,
		accepted.DocumentID).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM kb_knowledge_jobs WHERE document_id=?`,
		accepted.DocumentID).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM kb_ingest_segments
		WHERE document_id=? AND state<>'ready'`, accepted.DocumentID).Scan(&nonReadySegments); err != nil {
		t.Fatal(err)
	}
	if documents != 1 || sources != 1 || jobs != 1 || nonReadySegments != 0 {
		t.Fatalf("real PDF duplicate/residue documents=%d sources=%d jobs=%d non_ready_segments=%d",
			documents, sources, jobs, nonReadySegments)
	}
}

func TestBUG20260726024RealPDFVisionPreflightMakesZeroModelCalls(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	fixture := bug20260726MathFiveFixture(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")

	textOnly := bug20260726Route(
		"provider-hexclaw-gpt", "hexclaw-gpt", "HexClaw-GPT",
		"gpt-5.3-codex-spark", "text",
	)
	h := newBug20260726KnowledgeHarness(t, textOnly)
	accepted := bug20260726PostPDF(t, h.http.URL, fixture, "bug-20260726-024-preflight")
	worked, runErr := bug20260726RunIngest(h)
	if !worked || !errors.Is(runErr, knowledge.ErrVisionModelRequired) {
		t.Fatalf("preflight worked=%v err=%v", worked, runErr)
	}
	if calls := len(h.captionRouteSnapshots()); calls != 0 {
		t.Fatalf("vision preflight made %d model call(s), want zero", calls)
	}
	job, err := h.service.GetJob(h.ctx, "desktop-user", accepted.JobID)
	if err != nil {
		t.Fatal(err)
	}
	wantPages := []int{1, 2, 5, 6, 128, 129, 131}
	if job.State != knowledge.KnowledgeJobFailed || job.Failure == nil ||
		job.Failure.Code != knowledge.VisionModelRequiredFailureCode ||
		job.Failure.ProviderDisplayName != "HexClaw-GPT" ||
		job.Failure.Model != "gpt-5.3-codex-spark" ||
		fmt.Sprint(job.Failure.AffectedPages) != fmt.Sprint(wantPages) {
		t.Fatalf("structured preflight failure=%+v job_state=%s", job.Failure, job.State)
	}
	var chunks, fts int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM kb_chunks WHERE doc_id=?`, accepted.DocumentID).
		Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM kb_chunks_fts`).Scan(&fts); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 || fts != 0 {
		t.Fatalf("preflight published partial text chunks=%d fts=%d", chunks, fts)
	}
	bug20260726AssertPersistedRoute(t, h.db, accepted.JobID, textOnly)
}

func bug20260726PostCancel(t *testing.T, baseURL string, jobID string) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodPost,
		baseURL+"/api/v1/knowledge/jobs/"+jobID+"/cancel",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("cancel status=%d body=%s", resp.StatusCode, raw)
	}
}

func bug20260726DeleteDocument(
	t *testing.T,
	baseURL string,
	documentID string,
	idempotencyKey string,
) {
	t.Helper()
	req, err := http.NewRequest(
		http.MethodDelete,
		baseURL+"/api/v1/knowledge/documents/"+documentID,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Idempotency-Key", idempotencyKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete status=%d body=%s", resp.StatusCode, raw)
	}
}

func TestBUG20260726026RealPDFDeleteFromDocumentRootConvergesWithoutOrphans(t *testing.T) {
	fixture := bug20260726MathFiveFixture(t)
	route := bug20260726Route(
		"provider-hexclaw-gpt", "hexclaw-gpt", "HexClaw-GPT", "gpt-5.6-sol",
		"text", "vision",
	)
	h := newBug20260726KnowledgeHarness(t, route)
	accepted := bug20260726PostPDF(t, h.http.URL, fixture, "bug-20260726-026-delete")

	var sourcePath string
	if err := h.db.QueryRow(`SELECT blob.storage_path
		FROM kb_ingest_document_sources source JOIN kb_ingest_blobs blob
		  ON blob.owner_id=source.owner_id AND blob.corpus_uid=source.corpus_uid
		 AND blob.sha256=source.blob_sha256
		WHERE source.document_id=?`, accepted.DocumentID).Scan(&sourcePath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sourcePath); err != nil {
		t.Fatalf("managed source missing before cleanup: %v", err)
	}

	bug20260726PostCancel(t, h.http.URL, accepted.JobID)
	result, err := h.db.Exec(`DELETE FROM kb_semantic_document_bindings WHERE document_id=?`,
		accepted.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		t.Fatalf("missing-resource fixture removed bindings=%d want=1", affected)
	}

	const deleteKey = "bug-20260726-026-delete-replay"
	bug20260726DeleteDocument(t, h.http.URL, accepted.DocumentID, deleteKey)
	bug20260726DeleteDocument(t, h.http.URL, accepted.DocumentID, deleteKey)

	gcWorker := knowledge.NewSemanticIndexWorker(
		h.repository,
		nil,
		knowledge.SemanticIndexWorkerConfig{
			OwnerID: "desktop-user", CorpusID: "default",
			WorkerID: "bug-20260726-gc", BatchSize: 64,
			LeaseDuration: time.Minute, RetryDelay: time.Second,
		},
	)
	worked, err := gcWorker.RunOnce(h.ctx)
	if err != nil || !worked {
		t.Fatalf("GC worked=%v err=%v", worked, err)
	}
	bug20260726DeleteDocument(t, h.http.URL, accepted.DocumentID, deleteKey)

	if _, err := os.Stat(sourcePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed source survived cleanup: %v", err)
	}
	for name, query := range map[string]string{
		"documents":           `SELECT COUNT(*) FROM kb_documents WHERE id=?`,
		"chunks":              `SELECT COUNT(*) FROM kb_chunks WHERE doc_id=?`,
		"fts":                 `SELECT COUNT(*) FROM kb_chunks_fts`,
		"bindings":            `SELECT COUNT(*) FROM kb_semantic_document_bindings WHERE document_id=?`,
		"generations":         `SELECT COUNT(*) FROM kb_semantic_document_generations WHERE document_id=?`,
		"revision_documents":  `SELECT COUNT(*) FROM kb_revision_documents WHERE document_id=?`,
		"vectors":             `SELECT COUNT(*) FROM kb_revision_vectors WHERE document_id=?`,
		"jobs":                `SELECT COUNT(*) FROM kb_knowledge_jobs`,
		"job_checkpoints":     `SELECT COUNT(*) FROM kb_job_stage_checkpoints`,
		"page_checkpoints":    `SELECT COUNT(*) FROM kb_ingest_page_checkpoints`,
		"execution_snapshots": `SELECT COUNT(*) FROM kb_ingest_execution_snapshots`,
		"segments":            `SELECT COUNT(*) FROM kb_ingest_segments`,
		"job_failures":        `SELECT COUNT(*) FROM kb_job_failures`,
		"sources":             `SELECT COUNT(*) FROM kb_ingest_document_sources WHERE document_id=?`,
		"blobs":               `SELECT COUNT(*) FROM kb_ingest_blobs`,
		"batch_manifests":     `SELECT COUNT(*) FROM kb_embedding_batch_manifests`,
		"batch_chunks":        `SELECT COUNT(*) FROM kb_embedding_batch_chunks`,
	} {
		var count int
		args := []any{}
		switch name {
		case "documents", "chunks", "bindings", "generations",
			"revision_documents", "vectors", "sources":
			args = append(args, accepted.DocumentID)
		}
		if err := h.db.QueryRow(query, args...).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if count != 0 {
			t.Errorf("cleanup left %s=%d", name, count)
		}
	}

	restartedRepository := knowledge.NewSQLiteSemanticIndexRepository(h.db)
	restartedService := knowledge.NewSemanticIndexService(
		restartedRepository,
		bug20260726EmbeddingResolver{},
	)
	if err := restartedService.ConfigureDocumentIngest(h.objectRoot); err != nil {
		t.Fatal(err)
	}
	restartedStore := knowledge.NewSQLiteStore(
		h.db,
		knowledge.WithSQLiteSemanticMutations("desktop-user", "default"),
	)
	restartedManager := knowledge.NewManager(restartedStore, restartedStore, nil)
	restartedHTTP := newBug20260726KnowledgeHTTPServer(t, restartedManager, restartedService)
	bug20260726DeleteDocument(t, restartedHTTP.URL, accepted.DocumentID, deleteKey)
}
