package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ToolMetric is a single tool call measurement.
type ToolMetric struct {
	Timestamp  string `json:"ts"`
	Tool       string `json:"tool"`
	DurationMs int64  `json:"ms"`
	Status     string `json:"status"` // success | error | timeout | cached
	Cached     bool   `json:"cached,omitempty"`
}

// ToolMetricsCollector records tool call metrics to a JSONL file.
type ToolMetricsCollector struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// NewToolMetricsCollector creates a metrics collector.
func NewToolMetricsCollector(dir string) (*ToolMetricsCollector, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "tools.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &ToolMetricsCollector{file: f, path: path}, nil
}

// Record appends a metric entry.
func (c *ToolMetricsCollector) Record(tool string, duration time.Duration, status string, cached bool) {
	entry := ToolMetric{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Tool:       tool,
		DurationMs: duration.Milliseconds(),
		Status:     status,
		Cached:     cached,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	data = append(data, '\n')

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file != nil {
		c.file.Write(data)
	}
}

// ToolStats is an aggregate view of a single tool's metrics.
type ToolStats struct {
	Tool        string  `json:"tool"`
	CallCount   int     `json:"call_count"`
	SuccessRate float64 `json:"success_rate"`
	AvgLatency  float64 `json:"avg_latency_ms"`
	CachedCount int     `json:"cached_count"`
}

// ReadStats reads the JSONL file and returns per-tool aggregate stats.
//
// limit controls the maximum number of tools returned (sorted by call count desc).
// If limit <= 0, all tools are returned.
func (c *ToolMetricsCollector) ReadStats(limit int) ([]ToolStats, error) {
	c.mu.Lock()
	path := c.path
	c.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	type agg struct {
		calls   int
		success int
		cached  int
		totalMs int64
	}
	m := make(map[string]*agg)

	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var entry ToolMetric
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		a, ok := m[entry.Tool]
		if !ok {
			a = &agg{}
			m[entry.Tool] = a
		}
		a.calls++
		a.totalMs += entry.DurationMs
		if entry.Status == "success" {
			a.success++
		}
		if entry.Cached {
			a.cached++
		}
	}

	stats := make([]ToolStats, 0, len(m))
	for name, a := range m {
		rate := 0.0
		if a.calls > 0 {
			rate = float64(a.success) / float64(a.calls) * 100
		}
		avg := 0.0
		if a.calls > 0 {
			avg = float64(a.totalMs) / float64(a.calls)
		}
		stats = append(stats, ToolStats{
			Tool:        name,
			CallCount:   a.calls,
			SuccessRate: rate,
			AvgLatency:  avg,
			CachedCount: a.cached,
		})
	}

	// Sort by call count descending
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].CallCount > stats[j].CallCount
	})

	if limit > 0 && len(stats) > limit {
		stats = stats[:limit]
	}
	return stats, nil
}

// Close flushes and closes the metrics file.
func (c *ToolMetricsCollector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file != nil {
		c.file.Close()
		c.file = nil
	}
}
