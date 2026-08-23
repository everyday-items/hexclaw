package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/api"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/egress"
	"github.com/hexagon-codes/hexclaw/knowledge"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
	"golang.org/x/text/unicode/norm"
)

const (
	semanticLiveTextbookPDFGate = "HEX_SEMANTIC_LIVE_TEXTBOOK_PDF"
	semanticLiveTextbookPDFEnv  = "HEXCLAW_TEXTBOOK_PDF"

	semanticLiveTextbookPDFSHA256 = "657e1547074668dbb50f2bf37f13c20f292127be64c26c5334190aa34d06de83"
	semanticLiveTextbookPDFBytes  = int64(14621452)
	semanticLiveTextbookPDFPages  = 131
	semanticLiveTextbookTimeout   = 15 * time.Minute

	semanticTextbookOraclePage       = 10
	semanticTextbookOracleLength     = 58
	semanticTextbookOracleSHA256     = "7740d0dbfd9cb464d2d22f695d4bf2bcba3bbfe6dc83e0df2a673465f890e0ed"
	semanticTextbookOracleQuery      = "在整数除法中，什么情况下说除数是被除数的因数，被除数是除数的倍数？"
	semanticTextbookMinimumHitScore  = 0.65
	semanticTextbookMaximumDrainRuns = 64
)

var semanticTextbookSparsePages = []int{1, 2, 5, 6, 128, 129, 131}

// TestBUG20260728008And012RealTextbookPDFWithLocalOllama 是显式 opt-in 的真实边界门禁。
// 它只使用临时 SQLite、临时对象目录、调用方提供的教材和已安装的本地 Ollama 模型。
func TestBUG20260728008And012RealTextbookPDFWithLocalOllama(t *testing.T) {
	if strings.TrimSpace(os.Getenv(semanticLiveTextbookPDFGate)) != "1" {
		t.Skip("set HEX_SEMANTIC_LIVE_TEXTBOOK_PDF=1 to run the real textbook PDF semantic-index test")
	}

	fixture := requireSemanticTextbookFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), semanticLiveTextbookTimeout)
	defer cancel()

	// 稀疏扫描页只走测试内确定性 captioner，真实网络边界仅为本地 embedding。
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", strconv.Itoa(semanticLiveTextbookPDFPages))
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")
	t.Setenv(semanticLiveLocalProviderEnv, "")
	laneCfg, plan, provider := semanticLiveLocalPlan(t, ctx, config.DefaultConfig())
	if plan.Model != semanticLiveDefaultLocalModel || !plan.Ollama {
		t.Fatalf("resolved local semantic route model=%q ollama=%t, want %q through Ollama",
			plan.Model, plan.Ollama, semanticLiveDefaultLocalModel)
	}

	counter := &semanticLiveEmbeddingCounter{}
	bundle := buildKnowledgeEmbeddingRuntimeProfiles(
		ctx, laneCfg, &egress.Policy{}, newKnowledgeSemanticRuntimeGate(),
		withKnowledgeEmbeddingHTTPClientObserver(func(providerKey, model string, client *http.Client) {
			if providerKey == plan.Provider && model == plan.Model {
				counter.next = client.Transport
				client.Transport = counter
			}
		}),
	)

	databasePath := filepath.Join(t.TempDir(), "semantic-textbook.db")
	db := openSemanticTextbookDatabase(t, ctx, databasePath)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = db.Close()
		}
	})

	runtime, err := setupKnowledgeSemanticIndex(
		ctx, db, bundle.Resolver, bundle.Registry, "semantic-textbook-live-worker",
	)
	if err != nil {
		t.Fatalf("setup production semantic runtime: %v", err)
	}
	if err := runtime.Service.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "objects")); err != nil {
		t.Fatalf("configure isolated document ingest: %v", err)
	}
	runtime.Service.ConfigureVisionRouteResolver(knowledge.VisionRouteSnapshotResolverFunc(
		func(context.Context) (knowledge.VisionRouteSnapshot, error) {
			return semanticTextbookCaptionerRoute(), nil
		},
	))

	captioner := &semanticTextbookSparsePageCaptioner{}
	manager := semanticTextbookManager(db, captioner)
	runtime.Worker.SetDocumentIngestProcessor(api.NewKnowledgeDocumentIngestProcessor(manager))

	textbook := createSemanticTextbookDocument(t, ctx, runtime.Service, fixture)
	distractors := []struct {
		name string
		body string
	}{
		{name: "go-concurrency.txt", body: "Go uses goroutines for concurrent tasks and channels for communication."},
		{name: "grand-canal.txt", body: "The Grand Canal connects northern and southern China and supported historic transport."},
	}
	distractorDocumentIDs := make([]string, 0, len(distractors))
	for index, distractor := range distractors {
		body := strings.NewReader(distractor.body)
		accepted, err := runtime.Service.CreateDocument(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID,
			knowledge.CreateDocumentInput{
				IdempotencyKey: fmt.Sprintf("semantic-textbook-distractor-%d", index),
				Filename:       distractor.name, MediaType: "text/plain",
				SizeBytes: int64(len(distractor.body)), Body: body,
			})
		if err != nil {
			t.Fatalf("create isolated distractor %q: %v", distractor.name, err)
		}
		distractorDocumentIDs = append(distractorDocumentIDs, accepted.DocumentID)
	}

	drainSemanticTextbookJobs(t, ctx, db, runtime.Worker)
	rootJob, err := runtime.Service.GetJob(ctx, knowledgeDesktopOwnerID, textbook.JobID)
	if err != nil || rootJob.State != knowledge.KnowledgeJobSucceeded {
		t.Fatalf("textbook ingest job state=%q err=%v, want succeeded", rootJob.State, err)
	}
	if got := captioner.Calls(); got != len(semanticTextbookSparsePages) {
		t.Fatalf("sparse-page caption calls=%d, want %d", got, len(semanticTextbookSparsePages))
	}

	policy, err := runtime.Service.GetPolicy(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID)
	if err != nil {
		t.Fatalf("read published embedding policy: %v", err)
	}
	if policy.ActiveRevision == nil || policy.DesiredRevision != nil ||
		policy.ActiveRevision.State != knowledge.VectorIndexReady ||
		policy.ActiveRevision.Profile.ModelName != plan.Model ||
		policy.ActiveRevision.Profile.Dimension != 4096 {
		t.Fatalf("published policy active=%+v desired=%+v, want one ready Qwen 4096 revision",
			policy.ActiveRevision, policy.DesiredRevision)
	}
	activeRevisionID, active, err := runtime.Searcher.ActiveRevisionID(ctx)
	if err != nil || !active || activeRevisionID != policy.ActiveRevision.RevisionID {
		t.Fatalf("authoritative active semantic revision=%q active=%t err=%v, policy revision=%q",
			activeRevisionID, active, err, policy.ActiveRevision.RevisionID)
	}

	documentContent, documentGeneration, pageCount := semanticTextbookDocumentFacts(
		t, ctx, runtime.Service, manager, textbook.DocumentID,
	)
	if pageCount != semanticLiveTextbookPDFPages {
		t.Fatalf("textbook page_count=%d, want %d", pageCount, semanticLiveTextbookPDFPages)
	}
	textbookChunks, textbookVectors := semanticTextbookVectorFacts(
		t, ctx, db, textbook.DocumentID, policy.ActiveRevision.RevisionID,
	)
	if textbookChunks == 0 || textbookVectors != textbookChunks {
		t.Fatalf("textbook chunks=%d vectors=%d, want every chunk vectorized", textbookChunks, textbookVectors)
	}
	documentChunkCounts := []int{textbookChunks}
	for _, documentID := range distractorDocumentIDs {
		document, err := manager.GetDocument(ctx, documentID)
		if err != nil {
			t.Fatalf("read distractor document facts: %v", err)
		}
		if document.ChunkCount <= 0 {
			t.Fatalf("distractor document %q has no chunks", documentID)
		}
		documentChunkCounts = append(documentChunkCounts, document.ChunkCount)
	}

	oraclePage := extractSemanticTextbookOraclePage(t, ctx, fixture)
	results, routeRan, receipt, err := runtime.Searcher.SearchWithReceipt(
		ctx, semanticTextbookOracleQuery, 8, knowledge.Filter{},
	)
	if err != nil {
		t.Fatalf("query real textbook revision: %v", err)
	}
	oracleHit := assertSemanticTextbookOracle(
		t, results, routeRan, receipt, textbook.DocumentID, documentGeneration,
		activeRevisionID, policy.ActiveRevision, documentContent, oraclePage,
	)
	assertSemanticTextbookEmbeddingEvidence(t, counter, plan.Model, documentChunkCounts, 1)

	requestsBeforeRestart := len(counter.snapshot())
	vectorsBeforeRestart := textbookVectors
	if err := db.Close(); err != nil {
		t.Fatalf("close semantic database before restart: %v", err)
	}
	closed = true

	restartedDB := openSemanticTextbookDatabase(t, ctx, databasePath)
	t.Cleanup(func() { _ = restartedDB.Close() })
	restarted, err := setupKnowledgeSemanticIndex(
		ctx, restartedDB, bundle.Resolver, bundle.Registry, "semantic-textbook-restarted-worker",
	)
	if err != nil {
		t.Fatalf("rebuild semantic runtime after restart: %v", err)
	}
	processed, err := restarted.Worker.RunOnce(ctx)
	if err != nil || processed {
		t.Fatalf("restart worker processed=%t err=%v, want no duplicate embedding job", processed, err)
	}
	if got := len(counter.snapshot()); got != requestsBeforeRestart {
		t.Fatalf("restart worker issued %d new embedding requests, want zero", got-requestsBeforeRestart)
	}
	_, vectorsAfterRestart := semanticTextbookVectorFacts(
		t, ctx, restartedDB, textbook.DocumentID, policy.ActiveRevision.RevisionID,
	)
	if vectorsAfterRestart != vectorsBeforeRestart {
		t.Fatalf("restart vectors=%d, want stable %d", vectorsAfterRestart, vectorsBeforeRestart)
	}
	restartedActiveRevisionID, restartedActive, err := restarted.Searcher.ActiveRevisionID(ctx)
	if err != nil || !restartedActive || restartedActiveRevisionID != activeRevisionID {
		t.Fatalf("restarted active semantic revision=%q active=%t err=%v, want %q",
			restartedActiveRevisionID, restartedActive, err, activeRevisionID)
	}

	restartedResults, restartedRouteRan, restartedReceipt, err := restarted.Searcher.SearchWithReceipt(
		ctx, semanticTextbookOracleQuery, 8, knowledge.Filter{},
	)
	if err != nil {
		t.Fatalf("query textbook after restart: %v", err)
	}
	restartedHit := assertSemanticTextbookOracle(
		t, restartedResults, restartedRouteRan, restartedReceipt, textbook.DocumentID,
		documentGeneration, restartedActiveRevisionID, policy.ActiveRevision, documentContent, oraclePage,
	)
	if restartedHit.Chunk.ID != oracleHit.Chunk.ID || restartedReceipt.RevisionID != receipt.RevisionID {
		t.Fatalf("restart hit/revision changed from %q/%q to %q/%q",
			oracleHit.Chunk.ID, receipt.RevisionID, restartedHit.Chunk.ID, restartedReceipt.RevisionID)
	}
	if got := len(counter.snapshot()) - requestsBeforeRestart; got != 0 {
		t.Fatalf("restart query embedding request delta=%d, want zero for the cached query", got)
	}
	if got := counter.chatRequests.Load(); got != 0 {
		t.Fatalf("chat requests=%d, want zero", got)
	}

	t.Logf(
		"real textbook semantic gate passed: pages=%d document_chunks=%v vectors=%d revision=%q oracle_page=%d top_score=%.4f embedding_requests=%d chat_requests=0",
		pageCount, documentChunkCounts, textbookVectors, receipt.RevisionID,
		semanticTextbookOraclePage, oracleHit.VectorScore, len(counter.snapshot()),
	)
	_ = provider
}

func requireSemanticTextbookFixture(t *testing.T) string {
	t.Helper()
	path := strings.TrimSpace(os.Getenv(semanticLiveTextbookPDFEnv))
	if path == "" {
		t.Fatal("HEXCLAW_TEXTBOOK_PDF must point to the authorized 131-page textbook PDF")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve textbook PDF path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat textbook PDF: %v", err)
	}
	if !info.Mode().IsRegular() || info.Size() != semanticLiveTextbookPDFBytes {
		t.Fatalf("textbook fixture regular=%t size=%d, want regular file with %d bytes",
			info.Mode().IsRegular(), info.Size(), semanticLiveTextbookPDFBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open textbook PDF: %v", err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		_ = file.Close()
		t.Fatalf("hash textbook PDF: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close textbook PDF after hashing: %v", err)
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != semanticLiveTextbookPDFSHA256 {
		t.Fatalf("textbook PDF sha256=%q, want the authorized fixture digest", got)
	}
	for _, tool := range []string{"pdfinfo", "pdftotext", "pdftoppm"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("required Poppler tool %q is unavailable", tool)
		}
	}
	command := exec.Command("pdfinfo", path)
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("inspect textbook PDF page count: %v", err)
	}
	pageCount := 0
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "Pages" {
			continue
		}
		pageCount, _ = strconv.Atoi(strings.TrimSpace(value))
		break
	}
	if pageCount != semanticLiveTextbookPDFPages {
		t.Fatalf("textbook PDF page count=%d, want %d", pageCount, semanticLiveTextbookPDFPages)
	}
	return path
}

type semanticTextbookSparsePageCaptioner struct {
	calls atomic.Int32
}

func (c *semanticTextbookSparsePageCaptioner) Caption(
	ctx context.Context,
	image []byte,
	mime string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	call := int(c.calls.Add(1))
	if call > len(semanticTextbookSparsePages) {
		return "", fmt.Errorf("unexpected sparse textbook page caption call %d", call)
	}
	if len(image) == 0 || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/") {
		return "", fmt.Errorf("invalid rendered textbook page for caption call %d", call)
	}
	return fmt.Sprintf("Scanned textbook page %d with cover or publication information.",
		semanticTextbookSparsePages[call-1]), nil
}

func (c *semanticTextbookSparsePageCaptioner) Calls() int {
	return int(c.calls.Load())
}

func semanticTextbookCaptionerRoute() knowledge.VisionRouteSnapshot {
	fingerprint := sha256.Sum256([]byte("semantic-textbook-deterministic-captioner-v1"))
	return knowledge.VisionRouteSnapshot{
		ProviderInstanceID:  "semantic-textbook-captioner",
		ProviderName:        "semantic-textbook-captioner",
		ProviderDisplayName: "Semantic textbook captioner",
		Model:               "deterministic-page-captioner-v1",
		Capabilities:        []string{"vision"},
		Fingerprint:         hex.EncodeToString(fingerprint[:]),
	}.Canonical()
}

func semanticTextbookManager(db *sql.DB, captioner knowledge.Captioner) *knowledge.Manager {
	store := knowledge.NewSQLiteStore(db)
	return knowledge.NewManager(store, store, nil,
		knowledge.WithSplitter(splitter.NewMarkdownSplitter(
			splitter.WithMarkdownChunkSize(400),
			splitter.WithMarkdownChunkOverlap(80),
			splitter.WithHeadersToSplit([]string{"#", "##", "###", "####"}),
			splitter.WithCodeBlockAware(true),
		)),
		knowledge.WithCaptioner(captioner),
	)
}

func createSemanticTextbookDocument(
	t *testing.T,
	ctx context.Context,
	service *knowledge.SemanticIndexService,
	fixture string,
) knowledge.CreateDocumentResult {
	t.Helper()
	file, err := os.Open(fixture)
	if err != nil {
		t.Fatalf("open textbook PDF for ingest: %v", err)
	}
	defer file.Close()
	result, err := service.CreateDocument(ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID,
		knowledge.CreateDocumentInput{
			IdempotencyKey: "semantic-textbook-pdf-v1",
			Filename:       filepath.Base(fixture), MediaType: "application/pdf",
			SizeBytes: semanticLiveTextbookPDFBytes, Body: file,
		})
	if err != nil {
		t.Fatalf("create real textbook document: %v", err)
	}
	if result.DocumentID == "" || result.JobID == "" {
		t.Fatalf("accepted textbook document has incomplete durable identity: %+v", result)
	}
	return result
}

func openSemanticTextbookDatabase(
	t *testing.T,
	ctx context.Context,
	path string,
) *sql.DB {
	t.Helper()
	store, err := sqlitestore.New(path)
	if err != nil {
		t.Fatalf("open isolated semantic database: %v", err)
	}
	if err := store.Init(ctx); err != nil {
		_ = store.Close()
		t.Fatalf("initialize isolated production store: %v", err)
	}
	db := store.DB()
	if err := knowledge.NewSQLiteStore(db).Init(ctx); err != nil {
		_ = store.Close()
		t.Fatalf("initialize isolated Knowledge store: %v", err)
	}
	return db
}

func drainSemanticTextbookJobs(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	worker *knowledge.SemanticIndexWorker,
) {
	t.Helper()
	for run := 0; run < semanticTextbookMaximumDrainRuns; run++ {
		processed, err := worker.RunOnce(ctx)
		if err != nil {
			t.Fatalf("drain production semantic job %d: %v", run+1, err)
		}
		if processed {
			continue
		}
		var unfinished int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_knowledge_jobs
			WHERE state IN ('queued','running','retry_wait')`).Scan(&unfinished); err != nil {
			t.Fatalf("inspect durable semantic queue: %v", err)
		}
		if unfinished != 0 {
			t.Fatalf("semantic worker stopped with %d unfinished jobs", unfinished)
		}
		return
	}
	t.Fatalf("semantic worker did not drain within %d sequential runs", semanticTextbookMaximumDrainRuns)
}

func semanticTextbookDocumentFacts(
	t *testing.T,
	ctx context.Context,
	service *knowledge.SemanticIndexService,
	manager *knowledge.Manager,
	documentID string,
) (string, int64, int) {
	t.Helper()
	document, err := manager.GetDocument(ctx, documentID)
	if err != nil {
		t.Fatalf("read textbook document content: %v", err)
	}
	projection, err := service.GetIngestDocumentProjectionForCorpus(
		ctx, knowledgeDesktopOwnerID, knowledgeDefaultCorpusID, documentID,
	)
	if err != nil {
		t.Fatalf("read textbook ingest projection: %v", err)
	}
	if strings.TrimSpace(document.Content) == "" || projection.DocumentGeneration < 1 ||
		projection.PageCount == nil || projection.TextIndexState != knowledge.TextIndexReady {
		t.Fatalf("textbook projection is incomplete: content_bytes=%d projection=%+v",
			len(document.Content), projection)
	}
	return document.Content, projection.DocumentGeneration, int(*projection.PageCount)
}

func semanticTextbookVectorFacts(
	t *testing.T,
	ctx context.Context,
	db *sql.DB,
	documentID string,
	revisionID string,
) (int, int) {
	t.Helper()
	var chunks int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_chunks WHERE doc_id=?`, documentID).Scan(&chunks); err != nil {
		t.Fatalf("count textbook chunks: %v", err)
	}
	var vectors, minDimension, maxDimension int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MIN(dimension),0),COALESCE(MAX(dimension),0)
		FROM kb_revision_vectors WHERE document_id=? AND revision_id=?`,
		documentID, revisionID).Scan(&vectors, &minDimension, &maxDimension); err != nil {
		t.Fatalf("count textbook revision vectors: %v", err)
	}
	if vectors > 0 && (minDimension != 4096 || maxDimension != 4096) {
		t.Fatalf("textbook vector dimensions=%d..%d, want 4096", minDimension, maxDimension)
	}
	return chunks, vectors
}

func extractSemanticTextbookOraclePage(
	t *testing.T,
	ctx context.Context,
	fixture string,
) string {
	t.Helper()
	page := strconv.Itoa(semanticTextbookOraclePage)
	output, err := exec.CommandContext(ctx, "pdftotext", "-f", page, "-l", page, fixture, "-").Output()
	if err != nil {
		t.Fatalf("extract physical textbook page %d: %v", semanticTextbookOraclePage, err)
	}
	normalized := normalizeSemanticTextbookOracle(string(output))
	normalizedRunes := []rune(normalized)
	matches := 0
	for start := 0; start+semanticTextbookOracleLength <= len(normalizedRunes); start++ {
		span := string(normalizedRunes[start : start+semanticTextbookOracleLength])
		digest := sha256.Sum256([]byte(span))
		if hex.EncodeToString(digest[:]) == semanticTextbookOracleSHA256 {
			matches++
		}
	}
	if matches != 1 {
		pageDigest := sha256.Sum256([]byte(normalized))
		t.Fatalf("physical page %d normalized_length=%d page_sha256=%s oracle_length=%d oracle_sha256=%s matches=%d safe_snippet=%q",
			semanticTextbookOraclePage, utf8.RuneCountInString(normalized),
			hex.EncodeToString(pageDigest[:]), semanticTextbookOracleLength,
			semanticTextbookOracleSHA256, matches, semanticTextbookSafeSnippet(normalized))
	}
	return normalized
}

func semanticTextbookSafeSnippet(value string) string {
	runes := []rune(value)
	if len(runes) <= 16 {
		return value
	}
	return string(runes[:8]) + "..." + string(runes[len(runes)-8:])
}

func normalizeSemanticTextbookOracle(value string) string {
	value = norm.NFKC.String(value)
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == '\ufeff' {
			return -1
		}
		return r
	}, value)
}

func assertSemanticTextbookOracle(
	t *testing.T,
	results []*knowledge.SearchResult,
	routeRan bool,
	receipt *knowledge.QueryEmbeddingReceipt,
	documentID string,
	documentGeneration int64,
	activeRevisionID string,
	active *knowledge.EmbeddingRevisionProjection,
	documentContent string,
	oraclePage string,
) *knowledge.SearchResult {
	t.Helper()
	if !routeRan || receipt == nil || len(results) == 0 {
		t.Fatalf("revision query route_ran=%t receipt_present=%t results=%d, want real semantic evidence",
			routeRan, receipt != nil, len(results))
	}
	hit := results[0]
	if hit == nil || hit.Chunk == nil {
		t.Fatal("top semantic result has no chunk evidence")
	}
	chunk := hit.Chunk
	if chunk.DocID != documentID || chunk.PageStart != semanticTextbookOraclePage ||
		chunk.PageEnd != semanticTextbookOraclePage {
		t.Fatalf("top semantic hit doc=%q pages=%d..%d, want textbook physical page %d",
			chunk.DocID, chunk.PageStart, chunk.PageEnd, semanticTextbookOraclePage)
	}
	if hit.VectorScore < semanticTextbookMinimumHitScore {
		t.Fatalf("textbook top semantic score=%.4f, want at least %.2f",
			hit.VectorScore, semanticTextbookMinimumHitScore)
	}
	normalizedHit := normalizeSemanticTextbookOracle(chunk.Content)
	if normalizedHit == "" || !strings.Contains(oraclePage, normalizedHit) {
		t.Fatalf("top semantic hit is not an exact normalized span of physical page %d",
			semanticTextbookOraclePage)
	}
	if chunk.SourceOffsetStart < 0 || chunk.SourceOffsetEnd <= chunk.SourceOffsetStart ||
		chunk.SourceOffsetEnd > int64(len(documentContent)) {
		t.Fatalf("top semantic hit source span=%d..%d is outside document bytes=%d",
			chunk.SourceOffsetStart, chunk.SourceOffsetEnd, len(documentContent))
	}
	span := documentContent[chunk.SourceOffsetStart:chunk.SourceOffsetEnd]
	if normalizeSemanticTextbookOracle(span) != normalizedHit {
		t.Fatal("top semantic hit content does not match its durable source span")
	}
	if chunk.SourceDigest != semanticLiveTextbookPDFSHA256 || chunk.CitationDigest == "" ||
		chunk.DocumentGeneration != documentGeneration ||
		chunk.SemanticRevisionID != activeRevisionID {
		t.Fatalf("top semantic provenance digest/citation/generation/revision is not source-bound: %+v", chunk)
	}
	queryDigest := sha256.Sum256([]byte(strings.TrimSpace(semanticTextbookOracleQuery)))
	if receipt.RevisionID != activeRevisionID {
		t.Fatalf("query receipt revision=%q, want authoritative active semantic revision %q",
			receipt.RevisionID, activeRevisionID)
	}
	if receipt.QueryDigest != "sha256:"+hex.EncodeToString(queryDigest[:]) {
		t.Fatalf("query receipt digest does not bind the frozen original query")
	}
	if receipt.Operation != "query_embedding" || receipt.Status != "succeeded" ||
		receipt.Model != active.Profile.ModelName || receipt.Dimension != active.Profile.Dimension ||
		receipt.ProfileConfigHash != active.ProfileConfigHash ||
		active.RevisionID != activeRevisionID {
		t.Fatalf("query receipt is not bound to the active source revision: %+v", receipt)
	}
	return hit
}

func assertSemanticTextbookEmbeddingEvidence(
	t *testing.T,
	counter *semanticLiveEmbeddingCounter,
	model string,
	documentChunkCounts []int,
	queryRequests int,
) {
	t.Helper()
	documentInputs, wantDocumentRequests := 0, 0
	for _, chunkCount := range documentChunkCounts {
		if chunkCount <= 0 {
			t.Fatalf("document chunk count must be positive, got %d in %v", chunkCount, documentChunkCounts)
		}
		documentInputs += chunkCount
		wantDocumentRequests += (chunkCount + 1) / 2
	}
	records := counter.snapshot()
	documentRequestCount, observedDocumentInputs, observedQueryRequests := 0, 0, 0
	var previousFinished time.Time
	for _, record := range records {
		if record.model != model || record.httpStatus != http.StatusOK {
			t.Fatalf("embedding record model=%q status=%d, want %q/200",
				record.model, record.httpStatus, model)
		}
		if !previousFinished.IsZero() && record.startedAt.Before(previousFinished) {
			t.Fatal("embedding requests overlapped; the textbook live gate must stay sequential")
		}
		previousFinished = record.finishedAt
		isQuery := len(record.inputValues) == 1 &&
			strings.HasPrefix(record.inputValues[0], bug20260728QwenQueryPrefix)
		if isQuery {
			observedQueryRequests++
			if !record.hasDeadline || record.deadlineRemaining < 55*time.Second ||
				record.deadlineRemaining > 60*time.Second {
				t.Fatalf("query embedding deadline=%v, want production budget about 60s", record.deadlineRemaining)
			}
			continue
		}
		documentRequestCount++
		observedDocumentInputs += len(record.inputValues)
		if len(record.inputValues) == 0 || len(record.inputValues) > 2 {
			t.Fatalf("document embedding batch inputs=%d, want production batch size 1..2",
				len(record.inputValues))
		}
		if !record.hasDeadline || record.deadlineRemaining < 115*time.Second ||
			record.deadlineRemaining > 120*time.Second {
			t.Fatalf("document embedding deadline=%v, want production budget about 120s", record.deadlineRemaining)
		}
	}
	if documentRequestCount != wantDocumentRequests || observedDocumentInputs != documentInputs ||
		observedQueryRequests != queryRequests {
		t.Fatalf("embedding evidence documents=%d requests/%d inputs queries=%d, want %d/%d/%d from per-document chunks %v",
			documentRequestCount, observedDocumentInputs, observedQueryRequests,
			wantDocumentRequests, documentInputs, queryRequests, documentChunkCounts)
	}
	if got := counter.chatRequests.Load(); got != 0 {
		t.Fatalf("chat requests=%d, want zero", got)
	}
}
