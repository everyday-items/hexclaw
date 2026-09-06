package knowledge

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/storage/migrate"
)

type gatedUploadReader struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
	reader  io.Reader
}

type failingUploadReader struct {
	reads atomic.Int32
	err   error
}

func (r *failingUploadReader) Read([]byte) (int, error) {
	r.reads.Add(1)
	return 0, r.err
}

func (r *gatedUploadReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
		return r.reader.Read(p)
	case <-time.After(5 * time.Second):
		return 0, errors.New("test upload reader timed out")
	}
}

func TestKnowledgeUploadOperationProjectionHasStableIntentAndExactDurableStates(t *testing.T) {
	db, service, ctx := newAsyncIngestHarness(t)
	_, failedErr := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "upload-intent-stable-v1", Filename: "六上数学.txt",
		MediaType: "text/plain", SizeBytes: int64(len("durable knowledge bytes")),
		Body: strings.NewReader(""), AgentID: "mingming", LearnerID: "learner-1",
	})
	if !errors.Is(failedErr, ErrInvalidDocumentUpload) {
		t.Fatalf("empty upload error=%v, want invalid upload", failedErr)
	}
	failed, err := service.ListUploadOperationsForCorpus(ctx, "desktop-user", "default")
	if err != nil || len(failed) != 1 || failed[0].State != UploadOperationFailed ||
		failed[0].Error != "upload_failed" || failed[0].DocumentID != "" || failed[0].JobID != "" {
		t.Fatalf("unbound failed upload=%+v err=%v", failed, err)
	}
	reader := &gatedUploadReader{
		started: make(chan struct{}), release: make(chan struct{}),
		reader: strings.NewReader("durable knowledge bytes"),
	}
	type result struct {
		accepted CreateDocumentResult
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
			IdempotencyKey: "upload-intent-stable-v1", Filename: "六上数学.txt",
			MediaType: "text/plain", SizeBytes: int64(len("durable knowledge bytes")),
			Body: reader, AgentID: "mingming", LearnerID: "learner-1",
		})
		resultCh <- result{accepted: accepted, err: err}
	}()
	select {
	case <-reader.started:
	case stopped := <-resultCh:
		t.Fatalf("same-key failed upload did not resume receiving: %v", stopped.err)
	case <-time.After(5 * time.Second):
		t.Fatal("same-key failed upload did not start reading")
	}

	assertOne := func(want UploadOperationState) UploadOperationProjection {
		t.Helper()
		operations, err := service.ListUploadOperationsForCorpus(
			context.Background(), "desktop-user", "default",
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(operations) != 1 {
			t.Fatalf("operations=%+v, want exactly one", operations)
		}
		operation := operations[0]
		if operation.State != want {
			t.Fatalf("state=%q, want %q: %+v", operation.State, want, operation)
		}
		return operation
	}

	receiving := assertOne(UploadOperationReceiving)
	if receiving.OperationID != failed[0].OperationID || receiving.Error != "" ||
		receiving.CreatedAt != failed[0].CreatedAt {
		t.Fatalf("failed upload recovery changed stable intent: before=%+v after=%+v", failed[0], receiving)
	}
	if receiving.OperationID == "" || receiving.DisplayName != "六上数学.txt" ||
		receiving.DocumentID != "" || receiving.JobID != "" ||
		receiving.ContentDigest != "" || receiving.Stage != "receiving" || receiving.Terminal {
		t.Fatalf("receiving intent projection drift: %+v", receiving)
	}
	close(reader.release)
	created := <-resultCh
	if created.err != nil {
		t.Fatal(created.err)
	}
	if created.accepted.OperationID != receiving.OperationID ||
		created.accepted.DocumentID == "" || created.accepted.JobID == "" {
		t.Fatalf("accepted stable identity drift: receiving=%+v accepted=%+v",
			receiving, created.accepted)
	}

	pending := assertOne(UploadOperationPendingResponse)
	if pending.OperationID != receiving.OperationID ||
		pending.DocumentID != created.accepted.DocumentID || pending.JobID != created.accepted.JobID ||
		len(pending.ContentDigest) != 64 || pending.Stage != "pending_response" || pending.Terminal {
		t.Fatalf("pending response projection drift: %+v", pending)
	}

	// Same owner/corpus/key/content replays the exact UploadIntent and root job.
	replayed, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "upload-intent-stable-v1", Filename: "六上数学.txt",
		MediaType: "text/plain", SizeBytes: int64(len("durable knowledge bytes")),
		Body: strings.NewReader("durable knowledge bytes"), AgentID: "mingming", LearnerID: "learner-1",
	})
	if err != nil || replayed != created.accepted {
		t.Fatalf("idempotent replay=%+v err=%v, want %+v", replayed, err, created.accepted)
	}
	var operationRows, jobRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM kb_upload_operations`).Scan(&operationRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM kb_knowledge_jobs WHERE kind='ingest'`).Scan(&jobRows); err != nil {
		t.Fatal(err)
	}
	if operationRows != 1 || jobRows != 1 {
		t.Fatalf("replay duplicated durable work: operations=%d jobs=%d", operationRows, jobRows)
	}

	if err := service.MarkUploadResponseDelivered(
		ctx, "another-owner", "default", receiving.OperationID,
	); !errors.Is(err, ErrSemanticIndexNotFound) {
		t.Fatalf("cross-owner acknowledgement error=%v, want not found", err)
	}
	if err := service.MarkUploadResponseDelivered(
		ctx, "desktop-user", "other-corpus", receiving.OperationID,
	); !errors.Is(err, ErrSemanticIndexNotFound) {
		t.Fatalf("cross-corpus acknowledgement error=%v, want not found", err)
	}
	if err := service.MarkUploadResponseDelivered(
		ctx, "desktop-user", "default", receiving.OperationID,
	); err != nil {
		t.Fatal(err)
	}
	queued := assertOne(UploadOperationQueued)
	if queued.Stage != string(JobStageExtracting) || queued.Terminal {
		t.Fatalf("queued projection=%+v", queued)
	}
	// Delivery ACK is an at-least-once client message. A lost 204 response may
	// be retried after restart; the second ACK must be a no-op, not a 404 or a
	// second state transition.
	if err := service.MarkUploadResponseDelivered(
		ctx, "desktop-user", "default", receiving.OperationID,
	); err != nil {
		t.Fatalf("duplicate acknowledgement must be idempotent: %v", err)
	}
	if replayedACK := assertOne(UploadOperationQueued); replayedACK.UpdatedAt != queued.UpdatedAt {
		t.Fatalf("duplicate acknowledgement rewrote the durable projection: first=%+v replay=%+v", queued, replayedACK)
	}

	states := []struct {
		state    UploadOperationState
		terminal bool
	}{
		{UploadOperationRunning, false},
		{UploadOperationRetryWait, false},
		{UploadOperationSucceeded, true},
		{UploadOperationFailed, true},
		{UploadOperationCancelled, true},
	}
	for _, tt := range states {
		now := time.Now().UTC().UnixMilli()
		leaseOwner := ""
		var leaseExpires, heartbeat, nextAttempt, finished any
		if tt.state == UploadOperationRunning {
			leaseOwner, leaseExpires, heartbeat = "worker-1", now+60_000, now
		}
		if tt.state == UploadOperationRetryWait {
			nextAttempt = now + 60_000
		}
		if tt.terminal {
			finished = now
		}
		lastError := ""
		if tt.state == UploadOperationFailed {
			lastError = "extract failed"
		}
		if _, err := db.Exec(`UPDATE kb_knowledge_jobs SET state=?,lease_owner=?,
			lease_expires_at=?,heartbeat_at=?,next_attempt_at=?,finished_at=?,last_error=?,updated_at=?
			WHERE job_id=?`, tt.state, leaseOwner, leaseExpires, heartbeat, nextAttempt,
			finished, lastError, now, created.accepted.JobID); err != nil {
			t.Fatalf("set %s: %v", tt.state, err)
		}
		projection := assertOne(tt.state)
		if projection.Terminal != tt.terminal ||
			(tt.state == UploadOperationFailed && projection.Error != lastError) {
			t.Fatalf("state %s projection=%+v", tt.state, projection)
		}
	}

	// A new service instance reads the same stable row and performs no writes.
	restarted := NewSemanticIndexService(NewSQLiteSemanticIndexRepository(db), &staticEmbeddingResolver{})
	operations, err := restarted.ListUploadOperationsForCorpus(ctx, "desktop-user", "default")
	if err != nil || len(operations) != 1 || operations[0].OperationID != receiving.OperationID ||
		operations[0].State != UploadOperationCancelled {
		t.Fatalf("restart projection=%+v err=%v", operations, err)
	}
	if _, err := restarted.ListUploadOperationsForCorpus(ctx, "another-owner", "default"); !errors.Is(err, ErrSemanticIndexNotFound) {
		t.Fatalf("cross-owner query error=%v, want not found", err)
	}
	if _, err := restarted.ListUploadOperationsForCorpus(ctx, "desktop-user", "other-corpus"); !errors.Is(err, ErrSemanticIndexNotFound) {
		t.Fatalf("cross-corpus query error=%v, want not found", err)
	}
}

func TestConcurrentUploadReplayCannotTerminateCreatorIntent(t *testing.T) {
	_, service, ctx := newAsyncIngestHarness(t)
	body := "creator request owns these bytes"
	_, failedErr := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "concurrent-upload-owner-v1", Filename: "owner.txt",
		MediaType: "text/plain", SizeBytes: int64(len(body)), Body: strings.NewReader(""),
	})
	if !errors.Is(failedErr, ErrInvalidDocumentUpload) {
		t.Fatalf("empty upload error=%v, want invalid upload", failedErr)
	}
	failed, err := service.ListUploadOperationsForCorpus(ctx, "desktop-user", "default")
	if err != nil || len(failed) != 1 || failed[0].State != UploadOperationFailed {
		t.Fatalf("unbound failed upload=%+v err=%v", failed, err)
	}
	creatorReader := &gatedUploadReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		reader:  strings.NewReader(body),
	}
	type createResult struct {
		accepted CreateDocumentResult
		err      error
	}
	creatorResult := make(chan createResult, 1)
	go func() {
		accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
			IdempotencyKey: "concurrent-upload-owner-v1",
			Filename:       "owner.txt",
			MediaType:      "text/plain",
			SizeBytes:      int64(len(body)),
			Body:           creatorReader,
		})
		creatorResult <- createResult{accepted: accepted, err: err}
	}()
	select {
	case <-creatorReader.started:
	case stopped := <-creatorResult:
		t.Fatalf("same-key failed upload did not resume receiving: %v", stopped.err)
	case <-time.After(5 * time.Second):
		t.Fatal("same-key failed upload did not start reading")
	}

	replayReader := &failingUploadReader{err: context.Canceled}
	_, replayErr := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "concurrent-upload-owner-v1",
		Filename:       "owner.txt",
		MediaType:      "text/plain",
		SizeBytes:      int64(len(body)),
		Body:           replayReader,
	})
	if !errors.Is(replayErr, ErrIdempotencyConflict) {
		t.Fatalf("concurrent replay error=%v, want fail-closed idempotency conflict", replayErr)
	}
	if got := replayReader.reads.Load(); got != 0 {
		t.Fatalf("concurrent replay consumed request bytes %d times", got)
	}

	close(creatorReader.release)
	created := <-creatorResult
	if created.err != nil || created.accepted.OperationID != failed[0].OperationID ||
		created.accepted.DocumentID == "" || created.accepted.JobID == "" {
		t.Fatalf("healthy intent creator was fenced by replay: accepted=%+v err=%v", created.accepted, created.err)
	}
	operations, err := service.ListUploadOperationsForCorpus(ctx, "desktop-user", "default")
	if err != nil || len(operations) != 1 ||
		operations[0].State != UploadOperationPendingResponse || operations[0].Terminal {
		t.Fatalf("creator operation projection=%+v err=%v", operations, err)
	}
}

func TestKnowledgeUploadOperationBindsMeasuredSizeWhenMultipartLengthIsUnknown(t *testing.T) {
	_, service, ctx := newAsyncIngestHarness(t)
	body := "multipart streams do not expose a trustworthy per-part length"
	accepted, err := service.CreateDocument(ctx, "desktop-user", "default", CreateDocumentInput{
		IdempotencyKey: "unknown-multipart-size-v1",
		Filename:       "unknown-size.txt",
		MediaType:      "text/plain",
		SizeBytes:      0,
		Body:           strings.NewReader(body),
	})
	if err != nil {
		t.Fatal(err)
	}
	operations, err := service.ListUploadOperationsForCorpus(ctx, "desktop-user", "default")
	if err != nil || len(operations) != 1 {
		t.Fatalf("operations=%+v err=%v", operations, err)
	}
	if operations[0].OperationID != accepted.OperationID ||
		operations[0].SizeBytes != int64(len(body)) || operations[0].SizeBytes == 0 {
		t.Fatalf("measured upload size was not bound to operation: %+v", operations[0])
	}
}

func TestKnowledgeUploadOperationMigrationIsRegisteredAfterV70(t *testing.T) {
	const version = 71
	if migrate.KnowledgeUploadOperationsV71.Version != version {
		t.Fatalf("upload operation migration version drift")
	}
	if len(migrate.All) < version || migrate.All[version-1].Version != version {
		t.Fatalf("Knowledge upload operation migration is not registered in order")
	}
}

func TestKnowledgeUploadOperationStartupFencesOrphanedReceivingWithoutReadSideEffects(t *testing.T) {
	db, _, ctx := newAsyncIngestHarness(t)
	repository := NewSQLiteSemanticIndexRepository(db)
	body := "request body lost with the prior process"
	operation, created, err := repository.BeginUploadOperation(
		ctx, "desktop-user", "default", CreateDocumentInput{
			IdempotencyKey: "orphaned-receiving-v1",
			Filename:       "orphan.txt",
			MediaType:      "text/plain",
			SizeBytes:      int64(len(body)),
			Body:           strings.NewReader(body),
		},
	)
	if err != nil || !created || operation.State != UploadOperationReceiving {
		t.Fatalf("begin operation=%+v created=%v err=%v", operation, created, err)
	}

	restarted := NewSemanticIndexService(NewSQLiteSemanticIndexRepository(db), &staticEmbeddingResolver{})
	if err := restarted.ConfigureDocumentIngest(filepath.Join(t.TempDir(), "restart-objects")); err != nil {
		t.Fatal(err)
	}
	assertCancelled := func() int64 {
		t.Helper()
		operations, err := restarted.ListUploadOperationsForCorpus(ctx, "desktop-user", "default")
		if err != nil || len(operations) != 1 {
			t.Fatalf("operations=%+v err=%v", operations, err)
		}
		got := operations[0]
		if got.OperationID != operation.OperationID || got.State != UploadOperationCancelled ||
			!got.Terminal || got.Error != "sidecar_restarted_before_acceptance" {
			t.Fatalf("orphan projection=%+v", got)
		}
		var updatedAt int64
		if err := db.QueryRow(`SELECT updated_at FROM kb_upload_operations WHERE operation_id=?`,
			operation.OperationID).Scan(&updatedAt); err != nil {
			t.Fatal(err)
		}
		return updatedAt
	}
	firstUpdatedAt := assertCancelled()
	if secondUpdatedAt := assertCancelled(); secondUpdatedAt != firstUpdatedAt {
		t.Fatalf("repeat read mutated projection: first=%d second=%d", firstUpdatedAt, secondUpdatedAt)
	}
}
