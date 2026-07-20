package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/resourcegov"
	k12engineadapter "github.com/hexagon-codes/hexclaw/scenarios/k12/engineadapter"
	_ "modernc.org/sqlite"
)

// This is the cross-chain contract from architecture sections 6.15/6.16: an
// interactive GradingJob recognizer and a background Knowledge ingest consume
// the exact same VLM capacity rather than independent package semaphores.
func TestResourceGovernorSharedByK12RecognitionAndKnowledgeIngest(t *testing.T) {
	cfg := resourcegov.DefaultConfig()
	cfg.Limits[resourcegov.ResourceVLM] = 1
	governor, err := resourcegov.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)

	var active atomic.Int32
	var peak atomic.Int32
	gradingEntered := make(chan struct{})
	releaseGrading := make(chan struct{})
	enter := func() {
		current := active.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				return
			}
		}
	}

	recognizer := k12engineadapter.NewRecognizerAdapter(
		func(ctx context.Context, _ []byte, _ string) (string, error) {
			enter()
			close(gradingEntered)
			select {
			case <-releaseGrading:
			case <-ctx.Done():
				active.Add(-1)
				return "", ctx.Err()
			}
			active.Add(-1)
			return `[{"question":"1+1","student_answer":"2"}]`, nil
		},
		k12engineadapter.WithRecognizerResourceGovernor(governor),
	)

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager := knowledge.NewManager(store, store, nil,
		knowledge.WithSplitter(splitter.NewRecursiveSplitter()),
		knowledge.WithResourceGovernor(governor),
		knowledge.WithCaptioner(knowledge.CaptionerFunc(func(context.Context, []byte, string) (string, error) {
			enter()
			active.Add(-1)
			return "knowledge page", nil
		})),
	)
	processor := NewKnowledgeDocumentIngestProcessor(
		manager,
		WithKnowledgeResourceGovernor(governor),
	)

	gradingDone := make(chan error, 1)
	go func() {
		_, recognizeErr := recognizer.Recognize(context.Background(), []byte("not-a-tall-image"))
		gradingDone <- recognizeErr
	}()
	<-gradingEntered

	image := []byte("persisted-image")
	digest := sha256.Sum256(image)
	path := filepath.Join(t.TempDir(), "page.png")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	knowledgeDone := make(chan error, 1)
	go func() {
		_, prepareErr := processor.Prepare(context.Background(), knowledge.PersistedIngestDocument{
			DocumentID: "doc-1", Filename: "page.png", Extension: ".png",
			MediaType: "image/png", StoragePath: path, SizeBytes: int64(len(image)),
			SHA256: hex.EncodeToString(digest[:]),
		})
		knowledgeDone <- prepareErr
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		metric := governor.Snapshot().Resources[resourcegov.ResourceVLM]
		if metric.InUse == 1 && metric.QueuedBackground == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceVLM]
	if metric.InUse != 1 || metric.QueuedBackground != 1 || metric.QueuedInteractive != 0 {
		t.Fatalf("two production chains did not share priority-aware VLM capacity: %+v", metric)
	}
	close(releaseGrading)
	if err := <-gradingDone; err != nil {
		t.Fatal(err)
	}
	if err := <-knowledgeDone; err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > 1 {
		t.Fatalf("K12+Knowledge underlying VLM peak=%d, want <=1", got)
	}
}

func TestKnowledgeProcessorCancellationReleasesBackgroundCPUPermit(t *testing.T) {
	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	holds := make([]*resourcegov.Permit, 0, 2)
	for i := 0; i < 2; i++ {
		permit, acquireErr := governor.Acquire(
			context.Background(), resourcegov.ResourceCPUHeavy, resourcegov.PriorityInteractive,
		)
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		holds = append(holds, permit)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	manager := knowledge.NewManager(store, store, nil,
		knowledge.WithSplitter(splitter.NewRecursiveSplitter()),
		knowledge.WithResourceGovernor(governor),
	)
	processor := NewKnowledgeDocumentIngestProcessor(
		manager, WithKnowledgeResourceGovernor(governor),
	)
	content := []byte("bounded CPU extraction")
	digest := sha256.Sum256(content)
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, prepareErr := processor.Prepare(ctx, knowledge.PersistedIngestDocument{
			DocumentID: "doc-cpu", Filename: "notes.txt", Extension: ".txt",
			MediaType: "text/plain", StoragePath: path, SizeBytes: int64(len(content)),
			SHA256: hex.EncodeToString(digest[:]),
		})
		done <- prepareErr
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if governor.Snapshot().Resources[resourcegov.ResourceCPUHeavy].QueuedBackground == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := governor.Snapshot().Resources[resourcegov.ResourceCPUHeavy].QueuedBackground; got != 1 {
		t.Fatalf("knowledge extraction did not queue as background: %d", got)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("processor cancellation error=%v", err)
	}
	for _, permit := range holds {
		permit.Release()
	}
	metric := governor.Snapshot().Resources[resourcegov.ResourceCPUHeavy]
	if metric.InUse != 0 || metric.QueuedBackground != 0 {
		t.Fatalf("CPU permit leaked after Knowledge cancellation: %+v", metric)
	}
}
