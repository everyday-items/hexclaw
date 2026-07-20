package knowledge

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/resourcegov"
)

type orderedEmbeddingExecutor struct {
	dimension int
	label     string
	order     chan<- string
	active    *atomic.Int32
	peak      *atomic.Int32
}

func (e *orderedEmbeddingExecutor) EmbedForPurpose(
	_ context.Context,
	_ EmbeddingPurpose,
	texts []string,
) ([][]float32, error) {
	current := e.active.Add(1)
	for {
		old := e.peak.Load()
		if current <= old || e.peak.CompareAndSwap(old, current) {
			break
		}
	}
	e.order <- e.label
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = make([]float32, e.dimension)
		vectors[i][0] = 1
	}
	e.active.Add(-1)
	return vectors, nil
}

func TestInteractiveQueryOvertakesBackgroundWorkerOnSharedAccelerator(t *testing.T) {
	workerHarness := newWorkerHarness(t, "background semantic chunk")
	queryHarness := newRevisionSearchHarness(t)
	boot, err := queryHarness.service.EnsureDefaultPolicy(queryHarness.ctx, "owner-1", "default")
	if err != nil {
		t.Fatal(err)
	}
	var corpusUID string
	if err := queryHarness.db.QueryRowContext(queryHarness.ctx, `SELECT corpus_uid
		FROM kb_semantic_corpora WHERE owner_id='owner-1' AND corpus_alias='default'`).Scan(&corpusUID); err != nil {
		t.Fatal(err)
	}
	queryHarness.addLegacyDocument("query-doc", "interactive semantic evidence", nil)
	queryHarness.bindDocument("owner-1", corpusUID, "query-doc")
	queryHarness.seedVisibleRevisionVector(*boot.ActiveRevisionID, corpusUID, "query-doc", []float32{1, 0, 0})

	governor, err := resourcegov.New(resourcegov.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(governor.Close)
	hold, err := governor.Acquire(context.Background(), resourcegov.ResourceAccelerator, resourcegov.PriorityInteractive)
	if err != nil {
		t.Fatal(err)
	}

	order := make(chan string, 2)
	var active atomic.Int32
	var peak atomic.Int32
	backgroundExecutor := &orderedEmbeddingExecutor{
		dimension: 3, label: "worker", order: order, active: &active, peak: &peak,
	}
	queryExecutor := &orderedEmbeddingExecutor{
		dimension: 3, label: "query", order: order, active: &active, peak: &peak,
	}
	now := time.Now().UTC()
	worker := NewSemanticIndexWorker(
		workerHarness.repo,
		&workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{"profile-a": backgroundExecutor}},
		workerConfig(&now, "worker-shared-accelerator", 8),
		WithSemanticWorkerResourceGovernor(governor),
	)
	workerDone := make(chan error, 1)
	go func() {
		_, runErr := worker.RunOnce(workerHarness.ctx)
		workerDone <- runErr
	}()
	waitGovernorQueue(t, governor, resourcegov.ResourceAccelerator, 0, 1)

	searcher := NewSQLiteRevisionSemanticSearcher(
		queryHarness.db, "owner-1", "default",
		&workerExecutorRegistry{executors: map[string]ProfileEmbeddingExecutor{"profile-a": queryExecutor}},
		WithRevisionSearchResourceGovernor(governor),
	)
	queryDone := make(chan error, 1)
	go func() {
		results, ran, searchErr := searcher.Search(queryHarness.ctx, "interactive query", 3, Filter{})
		if searchErr == nil && (!ran || len(results) != 1) {
			searchErr = ErrInvalidEmbeddingResult
		}
		queryDone <- searchErr
	}()
	waitGovernorQueue(t, governor, resourcegov.ResourceAccelerator, 1, 1)
	hold.Release()

	if first := <-order; first != "query" {
		t.Fatalf("first accelerator consumer=%q, want interactive query", first)
	}
	if second := <-order; second != "worker" {
		t.Fatalf("second accelerator consumer=%q, want background worker", second)
	}
	if err := <-queryDone; err != nil {
		t.Fatal(err)
	}
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
	if got := peak.Load(); got > 1 {
		t.Fatalf("query+worker accelerator peak=%d, want <=1", got)
	}
}

func waitGovernorQueue(
	t *testing.T,
	governor *resourcegov.Governor,
	resource resourcegov.Resource,
	interactive, background int,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		metric := governor.Snapshot().Resources[resource]
		if metric.QueuedInteractive == interactive && metric.QueuedBackground == background {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue did not reach interactive=%d background=%d: %+v",
		interactive, background, governor.Snapshot().Resources[resource])
}
