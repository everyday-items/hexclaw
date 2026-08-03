package knowledge

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/hexclaw/localinfer"
)

// RetrievalLane identifies a privacy-safe execution lane. Query text, document
// identifiers, and source paths are intentionally never recorded.
type RetrievalLane string

const (
	RetrievalLaneVector RetrievalLane = "vector"
	RetrievalLaneFTS    RetrievalLane = "fts"
	RetrievalLaneLike   RetrievalLane = "like"
)

// RetrievalLaneMetrics is a process-lifetime aggregate suitable for a local
// Desktop diagnostics endpoint. TotalLatencyMS supports rate and latency
// monitoring without retaining per-request data.
type RetrievalLaneMetrics struct {
	Calls          uint64  `json:"calls"`
	Hits           uint64  `json:"hits"`
	Empty          uint64  `json:"empty"`
	Errors         uint64  `json:"errors"`
	Fallbacks      uint64  `json:"fallbacks"`
	TotalLatencyMS float64 `json:"total_latency_ms"`
	HitRate        float64 `json:"hit_rate"`
	FallbackRate   float64 `json:"fallback_rate"`
}

type RetrievalMetricsSnapshot struct {
	Vector         RetrievalLaneMetrics       `json:"vector"`
	FTS            RetrievalLaneMetrics       `json:"fts"`
	Like           RetrievalLaneMetrics       `json:"like"`
	Rerank         RerankMetrics              `json:"rerank"`
	LocalInference localinfer.MetricsSnapshot `json:"local_inference"`
}

type RerankSkipReason string

const (
	RerankSkipDisabled        RerankSkipReason = "disabled"
	RerankSkipInsufficient    RerankSkipReason = "insufficient_candidates"
	RerankSkipNoExecutor      RerankSkipReason = "no_executor"
	RerankSkipExecutionFailed RerankSkipReason = "execution_failed"
	RerankSkipEmptyResult     RerankSkipReason = "empty_result"
)

var allRerankSkipReasons = [...]RerankSkipReason{
	RerankSkipDisabled,
	RerankSkipInsufficient,
	RerankSkipNoExecutor,
	RerankSkipExecutionFailed,
	RerankSkipEmptyResult,
}

type RerankMetrics struct {
	Configured uint64                      `json:"configured"`
	Eligible   uint64                      `json:"eligible"`
	Executed   uint64                      `json:"executed"`
	Succeeded  uint64                      `json:"succeeded"`
	Failed     uint64                      `json:"failed"`
	Skipped    map[RerankSkipReason]uint64 `json:"skipped"`
}

type rerankMetricsCollector struct {
	mu      sync.Mutex
	metrics RerankMetrics
}

func (c *rerankMetricsCollector) observe(
	configured, eligible, executed, succeeded bool,
	reason RerankSkipReason,
) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if configured {
		c.metrics.Configured++
	}
	if eligible {
		c.metrics.Eligible++
	}
	if executed {
		c.metrics.Executed++
	}
	if succeeded {
		c.metrics.Succeeded++
	} else if executed {
		c.metrics.Failed++
	}
	if reason != "" {
		if c.metrics.Skipped == nil {
			c.metrics.Skipped = make(map[RerankSkipReason]uint64, len(allRerankSkipReasons))
		}
		c.metrics.Skipped[reason]++
	}
}

func (c *rerankMetricsCollector) snapshot() RerankMetrics {
	result := RerankMetrics{Skipped: make(map[RerankSkipReason]uint64, len(allRerankSkipReasons))}
	if c == nil {
		return result
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result.Configured = c.metrics.Configured
	result.Eligible = c.metrics.Eligible
	result.Executed = c.metrics.Executed
	result.Succeeded = c.metrics.Succeeded
	result.Failed = c.metrics.Failed
	for _, reason := range allRerankSkipReasons {
		result.Skipped[reason] = c.metrics.Skipped[reason]
	}
	return result
}

type retrievalLaneCounter struct {
	calls       atomic.Uint64
	hits        atomic.Uint64
	empty       atomic.Uint64
	errors      atomic.Uint64
	fallbacks   atomic.Uint64
	latencyNano atomic.Int64
}

type retrievalMetricsCollector struct {
	vector retrievalLaneCounter
	fts    retrievalLaneCounter
	like   retrievalLaneCounter
}

func (m *retrievalMetricsCollector) counter(lane RetrievalLane) *retrievalLaneCounter {
	switch lane {
	case RetrievalLaneVector:
		return &m.vector
	case RetrievalLaneFTS:
		return &m.fts
	case RetrievalLaneLike:
		return &m.like
	default:
		return nil
	}
}

func (m *retrievalMetricsCollector) observe(
	lane RetrievalLane,
	duration time.Duration,
	resultCount int,
	err error,
	fallback bool,
) {
	if m == nil {
		return
	}
	counter := m.counter(lane)
	if counter == nil {
		return
	}
	counter.calls.Add(1)
	counter.latencyNano.Add(duration.Nanoseconds())
	if fallback {
		counter.fallbacks.Add(1)
	}
	if err != nil {
		counter.errors.Add(1)
		return
	}
	if resultCount > 0 {
		counter.hits.Add(1)
		return
	}
	counter.empty.Add(1)
}

func retrievalLaneMetricsSnapshot(counter *retrievalLaneCounter) RetrievalLaneMetrics {
	if counter == nil {
		return RetrievalLaneMetrics{}
	}
	calls := counter.calls.Load()
	metrics := RetrievalLaneMetrics{
		Calls:          calls,
		Hits:           counter.hits.Load(),
		Empty:          counter.empty.Load(),
		Errors:         counter.errors.Load(),
		Fallbacks:      counter.fallbacks.Load(),
		TotalLatencyMS: float64(counter.latencyNano.Load()) / float64(time.Millisecond),
	}
	if calls > 0 {
		metrics.HitRate = float64(metrics.Hits) / float64(calls)
		metrics.FallbackRate = float64(metrics.Fallbacks) / float64(calls)
	}
	return metrics
}

func (m *retrievalMetricsCollector) snapshot() RetrievalMetricsSnapshot {
	if m == nil {
		return RetrievalMetricsSnapshot{}
	}
	return RetrievalMetricsSnapshot{
		Vector: retrievalLaneMetricsSnapshot(&m.vector),
		FTS:    retrievalLaneMetricsSnapshot(&m.fts),
		Like:   retrievalLaneMetricsSnapshot(&m.like),
	}
}

// RetrievalMetricsSnapshot returns local aggregate diagnostics. It is safe to
// call concurrently with active retrieval and never includes query text.
func (m *Manager) RetrievalMetricsSnapshot() RetrievalMetricsSnapshot {
	if m == nil {
		return RetrievalMetricsSnapshot{}
	}
	snapshot := m.retrievalMetrics.snapshot()
	snapshot.Rerank = m.rerankMetrics.snapshot()
	if m.localInference != nil {
		snapshot.LocalInference = m.localInference.Snapshot()
	} else {
		snapshot.LocalInference = localinfer.MetricsSnapshot{
			Operations: map[localinfer.Operation]localinfer.OperationMetrics{},
		}
	}
	return snapshot
}

type retrievalMetricsContextKey struct{}

func withRetrievalMetrics(ctx context.Context, metrics *retrievalMetricsCollector) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, retrievalMetricsContextKey{}, metrics)
}

func observeRetrievalLane(
	ctx context.Context,
	lane RetrievalLane,
	duration time.Duration,
	resultCount int,
	err error,
	fallback bool,
) {
	if ctx == nil {
		return
	}
	metrics, _ := ctx.Value(retrievalMetricsContextKey{}).(*retrievalMetricsCollector)
	metrics.observe(lane, duration, resultCount, err, fallback)
}
