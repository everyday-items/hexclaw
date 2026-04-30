package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// Sink 是事件投递器。Submit 不应阻塞调用方过久 —— 慢 Sink 应内部异步化。
type Sink interface {
	// Submit 提交一个事件。返回非 nil error 时 Emitter 会记 fallback 日志，
	// 但不会向调用方传播（事件投递必须 best-effort）。
	Submit(ctx context.Context, e Event) error
	// Close 优雅关闭：刷盘 / 清空队列。可重复调用，幂等。
	Close() error
}

// NoopSink 丢弃所有事件。用于 flag OFF 或测试桩。
type NoopSink struct{}

// NewNoopSink 创建一个 NoopSink。
func NewNoopSink() *NoopSink                                            { return &NoopSink{} }
func (n *NoopSink) Submit(_ context.Context, _ Event) error             { return nil }
func (n *NoopSink) Close() error                                        { return nil }

// MemorySink 把事件累计到内存里，用于测试 / 本地调试。线程安全。
type MemorySink struct {
	mu     sync.Mutex
	events []Event
}

// NewMemorySink 创建一个空的 MemorySink。
func NewMemorySink() *MemorySink { return &MemorySink{} }

// Submit 追加事件。
func (m *MemorySink) Submit(_ context.Context, e Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
	return nil
}

// Close noop。
func (m *MemorySink) Close() error { return nil }

// Events 返回截至当前的事件副本。
func (m *MemorySink) Events() []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Event, len(m.events))
	copy(out, m.events)
	return out
}

// Reset 清空事件。
func (m *MemorySink) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = nil
}

// FileSink 把事件以 JSON Lines 写入文件。每次 Submit 一行。
type FileSink struct {
	mu sync.Mutex
	f  *os.File
}

// NewFileSink 打开（或创建）path 对应的文件以 append 模式。失败返回 error。
func NewFileSink(path string) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("events: open sink file %q: %w", path, err)
	}
	return &FileSink{f: f}, nil
}

// Submit 写一行 JSONL。
func (s *FileSink) Submit(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return fmt.Errorf("events: file sink already closed")
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("events: marshal: %w", err)
	}
	data = append(data, '\n')
	_, err = s.f.Write(data)
	return err
}

// Close 关闭文件。多次调用是安全的。
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// HTTPSink POST 单条事件 JSON 到远端 collector。
//
// 设计取舍：
//   - 同步 POST：实现简单；不阻塞由调用方决定是否在 goroutine 里调 Emit
//   - 不带本地 buffer / retry：远端不可达时 Submit 返回 error，Emitter 会记录 fallback
//     不会丢失现有 logger 输出。要做高可靠投递的留给 v0.4.1+ 的 BatchSink
type HTTPSink struct {
	url    string
	client *http.Client
}

// NewHTTPSink 构造一个 HTTPSink。timeout <= 0 时取 5s。
func NewHTTPSink(url string, timeout time.Duration) *HTTPSink {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPSink{
		url:    url,
		client: &http.Client{Timeout: timeout},
	}
}

// Submit POST 一条事件。
func (s *HTTPSink) Submit(ctx context.Context, e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("events: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("events: http post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("events: http %d", resp.StatusCode)
	}
	return nil
}

// Close noop（HTTPSink 无持久状态）。
func (s *HTTPSink) Close() error { return nil }

// MultiSink 把事件 fan-out 到多个底层 Sink。任一 Sink Submit 失败不影响其它。
// Close 会顺序调用底层每个 Sink.Close。
type MultiSink struct {
	sinks []Sink
}

// NewMultiSink 把多个 sink 组合成一个。nil sink 会被跳过。
func NewMultiSink(sinks ...Sink) *MultiSink {
	out := &MultiSink{sinks: make([]Sink, 0, len(sinks))}
	for _, s := range sinks {
		if s != nil {
			out.sinks = append(out.sinks, s)
		}
	}
	return out
}

// Submit 把事件分发到所有 sink；汇总错误（用最后一个非 nil error 上抛）。
func (m *MultiSink) Submit(ctx context.Context, e Event) error {
	var lastErr error
	for _, s := range m.sinks {
		if err := s.Submit(ctx, e); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// Close 关闭所有底层 sink。
func (m *MultiSink) Close() error {
	var lastErr error
	for _, s := range m.sinks {
		if err := s.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}
