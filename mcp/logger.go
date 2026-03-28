package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ToolCallLog is a single structured log entry for an MCP tool call.
type ToolCallLog struct {
	Timestamp  string `json:"timestamp"`
	Server     string `json:"server"`
	Tool       string `json:"tool"`
	DurationMs int64  `json:"duration_ms"`
	Status     string `json:"status"` // "success" | "error" | "timeout"
	Error      string `json:"error,omitempty"`
	InputSize  int    `json:"input_size"`
	OutputSize int    `json:"output_size"`
}

// Logger writes structured JSON logs for MCP tool calls.
//
// Each MCP server gets its own log file at ~/.hexclaw/logs/mcp/{name}.log.
// Log rotation: max 10MB per file, keep 3 rotated copies.
type Logger struct {
	mu      sync.Mutex
	dir     string
	writers map[string]*logWriter
}

type logWriter struct {
	file    *os.File
	path    string
	size    int64
	maxSize int64
}

const (
	defaultMaxLogSize = 10 * 1024 * 1024 // 10MB
	maxRotatedFiles   = 3
)

// NewLogger creates an MCP logger that writes to the given directory.
func NewLogger(dir string) *Logger {
	os.MkdirAll(dir, 0755)
	return &Logger{
		dir:     dir,
		writers: make(map[string]*logWriter),
	}
}

// Log records a tool call event.
func (l *Logger) Log(entry ToolCallLog) {
	l.mu.Lock()
	defer l.mu.Unlock()

	w, err := l.getWriter(entry.Server)
	if err != nil {
		return
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	data = append(data, '\n')

	n, _ := w.file.Write(data)
	w.size += int64(n)

	if w.size >= w.maxSize {
		l.rotate(w)
	}
}

// LogToolCall is a convenience method to log a tool call with timing.
func (l *Logger) LogToolCall(server, tool string, duration time.Duration, err error, inputSize, outputSize int) {
	status := "success"
	errMsg := ""
	if err != nil {
		status = "error"
		errMsg = err.Error()
	}
	l.Log(ToolCallLog{
		Timestamp:  time.Now().UTC().Format(time.RFC3339Nano),
		Server:     server,
		Tool:       tool,
		DurationMs: duration.Milliseconds(),
		Status:     status,
		Error:      errMsg,
		InputSize:  inputSize,
		OutputSize: outputSize,
	})
}

// ReadLogs returns the last N log entries for a server.
func (l *Logger) ReadLogs(server string, limit int) ([]ToolCallLog, error) {
	path := filepath.Join(l.dir, server+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	lines := splitLines(data)
	if limit > 0 && len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}

	var logs []ToolCallLog
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var entry ToolCallLog
		if json.Unmarshal(line, &entry) == nil {
			logs = append(logs, entry)
		}
	}
	return logs, nil
}

func (l *Logger) getWriter(server string) (*logWriter, error) {
	if w, ok := l.writers[server]; ok {
		return w, nil
	}

	path := filepath.Join(l.dir, server+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}

	info, _ := f.Stat()
	var size int64
	if info != nil {
		size = info.Size()
	}

	w := &logWriter{file: f, path: path, size: size, maxSize: defaultMaxLogSize}
	l.writers[server] = w
	return w, nil
}

func (l *Logger) rotate(w *logWriter) {
	w.file.Close()

	// Shift existing rotated files
	for i := maxRotatedFiles; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", w.path, i)
		newer := fmt.Sprintf("%s.%d", w.path, i-1)
		if i == 1 {
			newer = w.path
		}
		os.Rename(newer, old)
	}

	// Reopen fresh log file
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		delete(l.writers, filepath.Base(w.path))
		return
	}
	w.file = f
	w.size = 0
}

// Close closes all open log files.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, w := range l.writers {
		w.file.Close()
	}
	l.writers = nil
}

func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				lines = append(lines, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}
