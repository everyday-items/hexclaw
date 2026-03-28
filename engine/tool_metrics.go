package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
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

// Close flushes and closes the metrics file.
func (c *ToolMetricsCollector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.file != nil {
		c.file.Close()
		c.file = nil
	}
}
