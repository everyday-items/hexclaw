package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// LogFileSink 日志文件持久化 (JSONL + 轮转)
//
// 业界标准做法：运行时日志写入 JSONL 文件，单文件上限后轮转，
// 保留最近 N 份历史文件。用户可提交日志文件反馈 Bug。
//
// 文件路径: ~/.hexclaw/logs/hexclaw.log
// 轮转规则: 单文件 maxSize (默认 10MB)，保留 maxFiles 份 (默认 3)
// 格式: 每行一条 JSON (RFC 7464 JSON Lines)
type LogFileSink struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	size     int64
	maxSize  int64 // 单文件最大字节
	maxFiles int   // 保留历史文件数
}

// LogFileSinkConfig 日志文件配置
type LogFileSinkConfig struct {
	Dir      string // 日志目录 (默认 ~/.hexclaw/logs)
	FileName string // 日志文件名 (默认 hexclaw.log)
	MaxSize  int64  // 单文件最大字节 (默认 10MB)
	MaxFiles int    // 保留历史文件数 (默认 3)
}

// NewLogFileSink 创建日志文件写入器
func NewLogFileSink(cfg LogFileSinkConfig) (*LogFileSink, error) {
	if cfg.Dir == "" {
		home, _ := os.UserHomeDir()
		cfg.Dir = filepath.Join(home, ".hexclaw", "logs")
	}
	if cfg.FileName == "" {
		cfg.FileName = "hexclaw.log"
	}
	if cfg.MaxSize <= 0 {
		cfg.MaxSize = 10 * 1024 * 1024 // 10 MB
	}
	if cfg.MaxFiles <= 0 {
		cfg.MaxFiles = 100
	}

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}

	path := filepath.Join(cfg.Dir, cfg.FileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}

	info, _ := f.Stat()
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	return &LogFileSink{
		file:     f,
		path:     path,
		size:     size,
		maxSize:  cfg.MaxSize,
		maxFiles: cfg.MaxFiles,
	}, nil
}

// logFileEntry JSON Lines 日志条目
type logFileEntry struct {
	Timestamp string         `json:"ts"`
	Level     string         `json:"level"`
	Source    string         `json:"source,omitempty"`
	Message   string         `json:"msg"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// Write 写入一条日志到文件。线程安全。
func (s *LogFileSink) Write(entry LogEntry) {
	data, err := json.Marshal(logFileEntry{
		Timestamp: entry.Timestamp,
		Level:     entry.Level,
		Source:    entry.Source,
		Message:   entry.Message,
		Fields:    entry.Fields,
	})
	if err != nil {
		return
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.file == nil {
		return
	}

	n, err := s.file.Write(data)
	if err != nil {
		return
	}
	s.size += int64(n)

	// 检查是否需要轮转
	if s.size >= s.maxSize {
		s.rotate()
	}
}

// rotate 执行日志轮转
// hexclaw.log → hexclaw.log.1 → hexclaw.log.2 → hexclaw.log.3 (删除)
func (s *LogFileSink) rotate() {
	s.file.Close()

	// 移动历史文件: .3→删, .2→.3, .1→.2, current→.1
	for i := s.maxFiles; i >= 1; i-- {
		src := s.path
		if i > 1 {
			src = fmt.Sprintf("%s.%d", s.path, i-1)
		}
		dst := fmt.Sprintf("%s.%d", s.path, i)

		if i == s.maxFiles {
			os.Remove(dst)
		}
		os.Rename(src, dst)
	}

	// 创建新文件
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		s.file = nil
		return
	}
	s.file = f
	s.size = 0
}

// Close 关闭文件
func (s *LogFileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

// Path 返回当前日志文件路径
func (s *LogFileSink) Path() string {
	return s.path
}

// RotatedFiles 返回所有日志文件路径（当前 + 历史），用于导出/上传
func (s *LogFileSink) RotatedFiles() []string {
	files := []string{s.path}
	for i := 1; i <= s.maxFiles; i++ {
		p := fmt.Sprintf("%s.%d", s.path, i)
		if _, err := os.Stat(p); err == nil {
			files = append(files, p)
		}
	}
	return files
}

// AttachToCollector 将文件写入器挂载到 LogCollector
// 在 LogCollector.Add 中调用 sink.Write 持久化日志
func AttachToCollector(collector *LogCollector, sink *LogFileSink) {
	collector.mu.Lock()
	collector.fileSink = sink
	collector.mu.Unlock()
}
