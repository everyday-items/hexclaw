package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const MaxKnowledgeDocumentBytes int64 = 200 << 20

var (
	ErrDocumentIngestUnavailable     = errors.New("knowledge: asynchronous document ingest unavailable")
	ErrDocumentTooLarge              = errors.New("knowledge: document exceeds upload limit")
	ErrInvalidDocumentUpload         = errors.New("knowledge: invalid document upload")
	ErrIdempotencyConflict           = errors.New("knowledge: idempotency key reused with different payload")
	ErrInvalidDocumentRetry          = errors.New("knowledge: invalid document retry")
	ErrDocumentRetryRequiresReupload = errors.New("knowledge: cancelled or deleted document must be uploaded again")
	ErrDocumentRetryNotAllowed       = errors.New("knowledge: document has no failed indexing job to retry")
)

type TextIndexState string

const (
	TextIndexPending  TextIndexState = "pending"
	TextIndexBuilding TextIndexState = "building"
	TextIndexReady    TextIndexState = "ready"
	TextIndexFailed   TextIndexState = "failed"
)

// CreateDocumentInput is the streaming command accepted by the Knowledge
// application service. Owner and corpus are trusted request-scope arguments,
// never caller-controlled fields in this value.
type CreateDocumentInput struct {
	IdempotencyKey string
	Filename       string
	MediaType      string
	SizeBytes      int64
	Body           io.Reader
	AgentID        string
	LearnerID      string
	Subject        string
	Grade          string
	VisionRoute    *VisionRouteSnapshot

	// UploadOperationID is assigned by SemanticIndexService before bytes are
	// persisted. It is an internal command correlation identity, not a caller-
	// controlled field.
	UploadOperationID string
}

type CreateDocumentResult struct {
	OperationID      string           `json:"operation_id"`
	DocumentID       string           `json:"document_id"`
	JobID            string           `json:"job_id"`
	TextIndexState   TextIndexState   `json:"text_index_state"`
	VectorIndexState VectorIndexState `json:"vector_index_state"`
}

// UploadOperationState is the complete renderer-visible lifecycle of one
// accepted upload intent. The pre-job states are durable facts owned by the
// upload ledger; once bound, the asynchronous job is authoritative.
type UploadOperationState string

const (
	UploadOperationReceiving       UploadOperationState = "receiving"
	UploadOperationPendingResponse UploadOperationState = "pending_response"
	UploadOperationQueued          UploadOperationState = "queued"
	UploadOperationRunning         UploadOperationState = "running"
	UploadOperationRetryWait       UploadOperationState = "retry_wait"
	UploadOperationSucceeded       UploadOperationState = "succeeded"
	UploadOperationFailed          UploadOperationState = "failed"
	UploadOperationCancelled       UploadOperationState = "cancelled"
)

// UploadOperationProjection is the read-only durable recovery contract. Owner
// and corpus UID remain private so transport adapters cannot accidentally
// expose authorization scope or use them as caller-provided routing fields.
type UploadOperationProjection struct {
	OperationID   string               `json:"operation_id"`
	OwnerID       string               `json:"-"`
	CorpusID      string               `json:"corpus_id"`
	DocumentID    string               `json:"document_id,omitempty"`
	JobID         string               `json:"job_id,omitempty"`
	DisplayName   string               `json:"display_name"`
	MediaType     string               `json:"media_type"`
	SizeBytes     int64                `json:"size_bytes"`
	ContentDigest string               `json:"content_digest,omitempty"`
	State         UploadOperationState `json:"state"`
	Stage         string               `json:"stage"`
	Terminal      bool                 `json:"terminal"`
	Error         string               `json:"error,omitempty"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

type IngestBlob struct {
	SHA256      string
	StoragePath string
	SizeBytes   int64
	MediaType   string
}

type PersistedIngestDocument struct {
	JobID             string
	DocumentID        string
	OwnerID           string
	CorpusUID         string
	CorpusAlias       string
	ContentGeneration int64
	Filename          string
	Extension         string
	MediaType         string
	SizeBytes         int64
	SHA256            string
	StoragePath       string
	AgentID           string
	LearnerID         string
	Subject           string
	Grade             string
	VisionRoute       *VisionRouteSnapshot
}

type KnowledgeDocumentProjection struct {
	DocumentID         string                `json:"document_id"`
	DocumentGeneration int64                 `json:"document_generation"`
	OwnerID            string                `json:"owner_id"`
	CorpusID           string                `json:"corpus_id"`
	Filename           string                `json:"filename"`
	MediaType          string                `json:"media_type"`
	SizeBytes          int64                 `json:"size_bytes"`
	SHA256             string                `json:"sha256"`
	AgentID            string                `json:"agent_id,omitempty"`
	LearnerID          string                `json:"learner_id,omitempty"`
	Subject            string                `json:"subject,omitempty"`
	Grade              string                `json:"grade,omitempty"`
	PageCount          *int64                `json:"page_count,omitempty"`
	TextIndexState     TextIndexState        `json:"text_index_state"`
	Warnings           []string              `json:"warnings"`
	SourceSpans        []SourceSpan          `json:"source_spans,omitempty"`
	OCRPageReceipts    []OCRPageRouteReceipt `json:"ocr_page_route_receipts"`
}

// SourceSpan is a durable coordinate back to the immutable uploaded source.
// Page numbers are one-based and offsets are byte offsets in the canonical
// extracted text. Legacy chunks may only carry SourceDigest; zero page/offset
// values mean that older data cannot honestly provide finer coordinates.
type SourceSpan struct {
	PageStart         int    `json:"page_start,omitempty"`
	PageEnd           int    `json:"page_end,omitempty"`
	SourceDigest      string `json:"source_digest,omitempty"`
	SourceOffsetStart int64  `json:"source_offset_start,omitempty"`
	SourceOffsetEnd   int64  `json:"source_offset_end,omitempty"`
}

// IngestPageCheckpoint is one completed extraction artifact. Content is
// stored so a replacement worker can continue at the next page without
// repeating a VLM call. ContentDigest is filled and verified by the durable
// repository; callers may omit it when committing a newly produced page.
type IngestPageCheckpoint struct {
	PageNumber        int
	PagesTotal        int64
	SourceDigest      string
	ExtractionMode    string
	Content           string
	ContentDigest     string
	SourceOffsetStart int64
	SourceOffsetEnd   int64
	OCRRouteReceipt   *OCRRouteReceipt
}

// OCRRouteReceipt 是一次视觉转写调用返回的脱敏执行事实。
// Provider/Model 来自实际冻结路由，Fake 必须由执行适配器如实返回。
type OCRRouteReceipt struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Operation string `json:"operation"`
	Status    string `json:"status"`
	Fake      bool   `json:"fake"`
}

// OCRPageInvocationStatus 是逐页 OCR 调用的耐久状态。running/outcome_unknown
// 只能由同一调用的恢复/对账路径处理，不能盲目再次调用 Provider。
type OCRPageInvocationStatus string

const (
	OCRPageInvocationStatusPrepared       OCRPageInvocationStatus = "prepared"
	OCRPageInvocationStatusRunning        OCRPageInvocationStatus = "running"
	OCRPageInvocationStatusSucceeded      OCRPageInvocationStatus = "succeeded"
	OCRPageInvocationStatusFailed         OCRPageInvocationStatus = "failed"
	OCRPageInvocationStatusOutcomeUnknown OCRPageInvocationStatus = "outcome_unknown"
)

var ErrOCRPageInvocationOutcomeUnknown = errors.New("knowledge: OCR page invocation outcome unknown")
var ErrOCRPageInvocationLedgerUnavailable = errors.New("knowledge: OCR page invocation ledger unavailable")

// OCRPageInvocationClaim 是调用前冻结的逐页身份；JobID 由持有的 JobLease 提供，
// 防止调用方用另一个任务覆盖同一页的事实。
type OCRPageInvocationClaim struct {
	PageNumber    int
	PagesTotal    int64
	SourceDigest  string
	RequestDigest string
	Provider      string
	Model         string
}

// OCRPageInvocation 是 OCR 调用的内部耐久投影。Content 只用于同一任务恢复页检查点，
// 不进入公开接口或日志；RouteReceipt 仍需通过既有冻结路由校验。
type OCRPageInvocation struct {
	InvocationID  string
	JobID         string
	PageNumber    int
	PagesTotal    int64
	SourceDigest  string
	RequestDigest string
	Provider      string
	Model         string
	Operation     string
	Status        OCRPageInvocationStatus
	Content       string
	ContentDigest string
	RouteReceipt  OCRRouteReceipt
	LeaseEpoch    int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Fresh         bool `json:"-"`
}

// OCRPageInvocationResult 是 Provider 成功后的最小结果；保存成功事实与内容摘要
// 后，下一次 worker 可直接复用，不会再次触发相同 VLM 请求。
type OCRPageInvocationResult struct {
	Content      string
	RouteReceipt OCRRouteReceipt
}

// OCRPageInvocationProgress 是 IngestPageProgress 的可选耐久扩展。旧的文本页与
// 外部测试实现无需实现该接口，只有实际 OCR 页在调用 VLM 前后使用它。
type OCRPageInvocationProgress interface {
	ClaimOCRPageInvocation(context.Context, JobLease, time.Time, OCRPageInvocationClaim) (OCRPageInvocation, error)
	SaveOCRPageInvocation(context.Context, JobLease, time.Time, OCRPageInvocation, OCRPageInvocationResult) error
}

// OCRPageInvocationContextProgress 是 worker 已绑定租约后的适配面；调用方无需
// 直接接触 lease epoch，避免以空租约执行账本写入。
type OCRPageInvocationContextProgress interface {
	ClaimOCRPageInvocationContext(context.Context, OCRPageInvocationClaim) (OCRPageInvocation, error)
	SaveOCRPageInvocationContext(context.Context, OCRPageInvocation, OCRPageInvocationResult) error
}

// OCRPageInvocationOutcomeMarker 在 Provider 未返回可验证结果时把调用停在
// outcome_unknown，恢复路径只能先对账，不能直接再次调用。
type OCRPageInvocationOutcomeMarker interface {
	MarkOCRPageInvocationOutcomeUnknown(context.Context, JobLease, time.Time, OCRPageInvocation, string) error
}

type OCRPageInvocationContextOutcomeMarker interface {
	MarkOCRPageInvocationOutcomeUnknownContext(context.Context, OCRPageInvocation, string) error
}

const (
	OCRRouteOperationPDFPage = "knowledge_pdf_page_ocr"
	OCRRouteStatusSucceeded  = "succeeded"
)

// OCRPageRouteReceipt 把执行事实绑定到不可变源页与转写内容摘要。
// 公开投影不包含页面原文、图片字节、凭据或 Provider 外部请求 ID。
type OCRPageRouteReceipt struct {
	PageNumber    int    `json:"page_number"`
	PagesTotal    int64  `json:"pages_total"`
	SourceDigest  string `json:"source_digest"`
	ContentDigest string `json:"content_digest"`
	OCRRouteReceipt
}

// IngestPageProgress is the lease-fenced page protocol exposed to resumable
// parsers. Each CommitPage transaction atomically writes the page artifact and
// the job's pages_done/pages_total projection.
type IngestPageProgress interface {
	SetPageTotal(context.Context, string, int64) error
	LoadCompletedPages(context.Context, string, int64) ([]IngestPageCheckpoint, error)
	CommitPage(context.Context, IngestPageCheckpoint) error
}

// PreparedIngestDocument is the parser/splitter output passed to the durable
// repository. It deliberately carries no vectors: text publication and the
// optional revision-scoped embedding child job are separate atomic stages.
type PreparedIngestDocument struct {
	Document  *Document
	Chunks    []*Chunk
	PageCount int64
	Warnings  []string
}

// DocumentIngestProcessor performs CPU/IO-heavy parsing, OCR and chunking off
// the HTTP request path. Implementations must honor context cancellation; the
// worker keeps the durable lease alive while Prepare is running.
type DocumentIngestProcessor interface {
	Prepare(context.Context, PersistedIngestDocument) (PreparedIngestDocument, error)
}

// ResumableDocumentIngestProcessor is an optional extension. Existing small
// document processors remain source compatible; the worker prefers this path
// whenever implemented.
type ResumableDocumentIngestProcessor interface {
	DocumentIngestProcessor
	PrepareResumable(context.Context, PersistedIngestDocument, IngestPageProgress) (PreparedIngestDocument, error)
}

type documentIngestWorkerRepository interface {
	DocumentIngestRepository
	GetIngestDocumentForCorpusUID(context.Context, string, string, string) (PersistedIngestDocument, error)
	SaveJobProgress(context.Context, JobLease, time.Time, JobProgressUpdate) error
	CompleteIngestDocument(context.Context, JobLease, time.Time, PreparedIngestDocument) error
}

type jobScopedDocumentIngestRepository interface {
	GetIngestDocumentForJob(context.Context, string, string, string, string) (PersistedIngestDocument, error)
}

type DocumentIngestRepository interface {
	CreateIngestDocument(context.Context, string, string, CreateDocumentInput, IngestBlob) (CreateDocumentResult, error)
	RetryIngestDocument(context.Context, string, string, string, string) (CreateDocumentResult, error)
	GetIngestDocument(context.Context, string, string) (PersistedIngestDocument, error)
	GetIngestDocumentProjection(context.Context, string, string) (KnowledgeDocumentProjection, error)
	ListIngestBlobPaths(context.Context) ([]string, error)
	IsIngestBlobPathReferenced(context.Context, string) (bool, error)
}

type visionRouteRetryRepository interface {
	RetryIngestDocumentWithVisionRoute(
		context.Context, string, string, string, string, *VisionRouteSnapshot,
	) (CreateDocumentResult, error)
}

type localIngestBlobStore struct {
	root    string
	locksMu sync.Mutex
	locks   map[string]*ingestObjectPathLock
}

type ingestObjectPathLock struct {
	mu   sync.Mutex
	refs int
}

func newLocalIngestBlobStore(root string) (*localIngestBlobStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, ErrDocumentIngestUnavailable
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("knowledge: resolve ingest object root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("knowledge: create ingest object root: %w", err)
	}
	return &localIngestBlobStore{root: abs, locks: map[string]*ingestObjectPathLock{}}, nil
}

func (s *localIngestBlobStore) Persist(
	ctx context.Context,
	ownerID, corpusID string,
	input CreateDocumentInput,
) (IngestBlob, func(), error) {
	if err := validateSemanticScope(ownerID, corpusID); err != nil {
		return IngestBlob{}, nil, err
	}
	filename, extension, mediaType, err := validateCreateDocumentInput(input)
	if err != nil {
		return IngestBlob{}, nil, err
	}
	tmpDir := filepath.Join(s.root, ".incoming")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return IngestBlob{}, nil, fmt.Errorf("knowledge: create ingest staging directory: %w", err)
	}
	tmp, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		return IngestBlob{}, nil, fmt.Errorf("knowledge: create ingest staging file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()

	hasher := sha256.New()
	prefix := &prefixCapture{limit: 16}
	limited := io.LimitReader(contextReader{ctx: ctx, reader: input.Body}, MaxKnowledgeDocumentBytes+1)
	written, copyErr := io.Copy(io.MultiWriter(tmp, hasher, prefix), limited)
	if copyErr != nil {
		_ = tmp.Close()
		return IngestBlob{}, nil, fmt.Errorf("knowledge: stream upload: %w", copyErr)
	}
	if written == 0 {
		_ = tmp.Close()
		return IngestBlob{}, nil, fmt.Errorf("%w: empty file", ErrInvalidDocumentUpload)
	}
	if written > MaxKnowledgeDocumentBytes {
		_ = tmp.Close()
		return IngestBlob{}, nil, ErrDocumentTooLarge
	}
	if input.SizeBytes > 0 && input.SizeBytes != written {
		_ = tmp.Close()
		return IngestBlob{}, nil, fmt.Errorf("%w: declared bytes=%d actual=%d", ErrInvalidDocumentUpload, input.SizeBytes, written)
	}
	if extension == ".pdf" && !strings.HasPrefix(string(prefix.bytes), "%PDF-") {
		_ = tmp.Close()
		return IngestBlob{}, nil, fmt.Errorf("%w: PDF magic mismatch", ErrInvalidDocumentUpload)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return IngestBlob{}, nil, fmt.Errorf("knowledge: fsync staged upload: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return IngestBlob{}, nil, fmt.Errorf("knowledge: close staged upload: %w", err)
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	scopeDigest := hashStrings(strings.TrimSpace(ownerID), strings.TrimSpace(corpusID))
	scopeDir := filepath.Join(s.root, scopeDigest[:24])
	bucket := filepath.Join(scopeDir, digest[:2])
	if err := os.MkdirAll(bucket, 0o700); err != nil {
		return IngestBlob{}, nil, fmt.Errorf("knowledge: create ingest object bucket: %w", err)
	}
	finalPath := filepath.Join(bucket, digest)
	release := s.acquireObjectPath(finalPath)
	locked := true
	defer func() {
		if locked {
			release()
		}
	}()
	if err := os.Rename(tmpPath, finalPath); err != nil {
		info, statErr := os.Stat(finalPath)
		if statErr != nil || info.Size() != written {
			return IngestBlob{}, nil, fmt.Errorf("knowledge: publish ingest object: %w", err)
		}
	}
	if err := syncDirectory(bucket); err != nil {
		return IngestBlob{}, nil, err
	}
	if err := syncDirectory(scopeDir); err != nil {
		return IngestBlob{}, nil, err
	}
	if err := syncDirectory(s.root); err != nil {
		return IngestBlob{}, nil, err
	}
	if err := syncDirectory(tmpDir); err != nil {
		return IngestBlob{}, nil, err
	}
	_ = filename // validated here; repository persists the original value.
	locked = false
	return IngestBlob{SHA256: digest, StoragePath: finalPath, SizeBytes: written, MediaType: mediaType}, release, nil
}

func (s *localIngestBlobStore) acquireObjectPath(path string) func() {
	clean := filepath.Clean(path)
	s.locksMu.Lock()
	entry := s.locks[clean]
	if entry == nil {
		entry = &ingestObjectPathLock{}
		s.locks[clean] = entry
	}
	entry.refs++
	s.locksMu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		s.locksMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(s.locks, clean)
		}
		s.locksMu.Unlock()
	}
}

func (s *localIngestBlobStore) RemoveManagedObject(path string) error {
	clean := filepath.Clean(path)
	if !isManagedIngestObjectPath(s.root, clean) {
		return fmt.Errorf("knowledge: refusing to remove unmanaged ingest object")
	}
	if err := os.Remove(clean); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("knowledge: remove unreferenced ingest object: %w", err)
	}
	return syncDirectory(filepath.Dir(clean))
}

func validateCreateDocumentInput(input CreateDocumentInput) (string, string, string, error) {
	key := strings.TrimSpace(input.IdempotencyKey)
	if key == "" || len(key) > 256 {
		return "", "", "", fmt.Errorf("%w: Idempotency-Key is required", ErrInvalidDocumentUpload)
	}
	if input.Body == nil {
		return "", "", "", fmt.Errorf("%w: file is required", ErrInvalidDocumentUpload)
	}
	filename := strings.TrimSpace(input.Filename)
	if filename == "" || filename != filepath.Base(filename) || filename == "." {
		return "", "", "", fmt.Errorf("%w: unsafe filename", ErrInvalidDocumentUpload)
	}
	extension := strings.ToLower(filepath.Ext(filename))
	if !allowedKnowledgeUploadExtensions[extension] {
		return "", "", "", fmt.Errorf("%w: unsupported extension %s", ErrInvalidDocumentUpload, extension)
	}
	mediaType := strings.TrimSpace(strings.Split(input.MediaType, ";")[0])
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = mediaTypeForKnowledgeExtension(extension)
	}
	if input.SizeBytes < 0 || input.SizeBytes > MaxKnowledgeDocumentBytes {
		return "", "", "", ErrDocumentTooLarge
	}
	return filename, extension, mediaType, nil
}

var allowedKnowledgeUploadExtensions = map[string]bool{
	".txt": true, ".md": true, ".csv": true, ".json": true,
	".doc": true, ".docx": true, ".pptx": true, ".pdf": true,
	".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true,
}

func mediaTypeForKnowledgeExtension(extension string) string {
	switch extension {
	case ".pdf":
		return "application/pdf"
	case ".txt", ".md", ".csv", ".json":
		return "text/plain"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

type prefixCapture struct {
	limit int
	bytes []byte
}

func (w *prefixCapture) Write(p []byte) (int, error) {
	if remaining := w.limit - len(w.bytes); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		w.bytes = append(w.bytes, p[:remaining]...)
	}
	return len(p), nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("knowledge: open ingest object directory: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("knowledge: fsync ingest object directory: %w", err)
	}
	return nil
}

// PruneOrphans removes only files whose path exactly matches this store's
// managed layout and is absent from the committed SQLite blob set. Symlinks,
// unknown files and paths outside root are never touched.
func (s *localIngestBlobStore) PruneOrphans(referenced []string) error {
	keep := make(map[string]struct{}, len(referenced))
	for _, path := range referenced {
		clean := filepath.Clean(path)
		if isManagedIngestObjectPath(s.root, clean) {
			keep[clean] = struct{}{}
		}
	}
	dirtyDirectories := map[string]struct{}{}
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		clean := filepath.Clean(path)
		rel, err := filepath.Rel(s.root, clean)
		if err != nil {
			return err
		}
		parts := strings.Split(rel, string(filepath.Separator))
		staleIncoming := len(parts) == 2 && parts[0] == ".incoming"
		managedOrphan := isManagedIngestObjectPath(s.root, clean)
		if !staleIncoming && !managedOrphan {
			return nil
		}
		if _, referenced := keep[clean]; referenced {
			return nil
		}
		if err := os.Remove(clean); err != nil {
			return fmt.Errorf("knowledge: remove orphan ingest object: %w", err)
		}
		dirtyDirectories[filepath.Dir(clean)] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	for directory := range dirtyDirectories {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func isManagedIngestObjectPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || len(parts[0]) != 24 || len(parts[1]) != 2 || len(parts[2]) != 64 {
		return false
	}
	for _, value := range parts {
		if _, err := hex.DecodeString(value); err != nil {
			return false
		}
	}
	return parts[1] == parts[2][:2]
}

func hashStrings(parts ...string) string {
	var h hash.Hash = sha256.New()
	for _, part := range parts {
		_, _ = io.WriteString(h, part)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func documentTimeNow() time.Time { return time.Now().UTC() }
