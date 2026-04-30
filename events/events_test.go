package events

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func withFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagEventsTransportV1: on,
	})
	return featureflag.WithContext(ctx, flags)
}

func TestEvent_New_FillsTimestamp(t *testing.T) {
	e := New("tool.test", SeverityInfo)
	if e.Type != "tool.test" || e.Severity != SeverityInfo {
		t.Errorf("type/severity 不对")
	}
	if e.Timestamp.IsZero() {
		t.Error("Timestamp 应自动填充")
	}
	if e.Data == nil {
		t.Error("Data 应初始化为 empty map")
	}
}

func TestEvent_With_ChainedKVs(t *testing.T) {
	e := New("x", SeverityWarn).
		With("tool", "shell").
		With("duration_ms", 142).
		WithSource("engine.tool_executor").
		WithTrace("trace-1", "sess-2")

	if e.Data["tool"] != "shell" {
		t.Errorf("tool kv missing")
	}
	if e.Data["duration_ms"] != 142 {
		t.Errorf("int kv missing")
	}
	if e.Source != "engine.tool_executor" {
		t.Errorf("source not set")
	}
	if e.TraceID != "trace-1" || e.SessionID != "sess-2" {
		t.Errorf("trace/session not set")
	}
}

func TestMemorySink_AppendsAndCopies(t *testing.T) {
	s := NewMemorySink()
	s.Submit(context.Background(), New("a", SeverityInfo))
	s.Submit(context.Background(), New("b", SeverityInfo))
	got := s.Events()
	if len(got) != 2 {
		t.Errorf("expected 2 events, got %d", len(got))
	}
	got[0].Type = "tampered"
	internal := s.Events()
	if internal[0].Type == "tampered" {
		t.Error("Events() 应返回副本，外部修改不应影响内部")
	}
	s.Reset()
	if len(s.Events()) != 0 {
		t.Error("Reset 应清空")
	}
}

func TestMemorySink_ConcurrentSafe(t *testing.T) {
	s := NewMemorySink()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Submit(context.Background(), New("a", SeverityInfo))
		}()
	}
	wg.Wait()
	if len(s.Events()) != 50 {
		t.Errorf("expected 50, got %d", len(s.Events()))
	}
}

func TestFileSink_WritesJSONLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	s, err := NewFileSink(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Submit(context.Background(), New("a", SeverityInfo).With("k", 1))
	s.Submit(context.Background(), New("b", SeverityWarn).With("k", 2))
	s.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitLines(data)
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %s", len(lines), data)
	}
	var e Event
	if err := json.Unmarshal(lines[0], &e); err != nil {
		t.Fatal(err)
	}
	if e.Type != "a" {
		t.Errorf("first event Type: got %q", e.Type)
	}
}

func TestFileSink_AfterCloseFails(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFileSink(filepath.Join(dir, "x.jsonl"))
	s.Close()
	if err := s.Submit(context.Background(), New("a", SeverityInfo)); err == nil {
		t.Error("close 后 Submit 应报错")
	}
}

func TestHTTPSink_PostsJSON(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = readAll(r.Body)
		r.Body.Close()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	s := NewHTTPSink(srv.URL, time.Second)
	if err := s.Submit(context.Background(), New("a", SeverityInfo).With("x", 1)); err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("server received nothing")
	}
	var e Event
	if err := json.Unmarshal(got, &e); err != nil {
		t.Fatal(err)
	}
	if e.Type != "a" {
		t.Errorf("type lost in transit")
	}
}

func TestHTTPSink_4xxReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	s := NewHTTPSink(srv.URL, time.Second)
	if err := s.Submit(context.Background(), New("a", SeverityInfo)); err == nil {
		t.Error("4xx 应返回错误")
	}
}

func TestMultiSink_FanOut(t *testing.T) {
	a := NewMemorySink()
	b := NewMemorySink()
	m := NewMultiSink(a, nil, b) // nil 应被跳过
	m.Submit(context.Background(), New("x", SeverityInfo))
	if len(a.Events()) != 1 || len(b.Events()) != 1 {
		t.Errorf("fan-out 应同步进 a/b")
	}
	m.Close()
}

func TestEmit_NoopWhenFlagOff(t *testing.T) {
	mem := NewMemorySink()
	em := NewEmitter(mem, "test")
	ctx := withFlag(WithEmitter(context.Background(), em), false)
	_ = Emit(ctx, New("x", SeverityInfo))
	if len(mem.Events()) != 0 {
		t.Errorf("flag OFF 时不应投递；got %d", len(mem.Events()))
	}
}

func TestEmit_DeliversWhenFlagOn(t *testing.T) {
	mem := NewMemorySink()
	em := NewEmitter(mem, "engine.test")
	ctx := withFlag(WithEmitter(context.Background(), em), true)

	if err := Emit(ctx, New("tool.completed", SeverityInfo).With("tool", "shell")); err != nil {
		t.Fatal(err)
	}
	if len(mem.Events()) != 1 {
		t.Fatalf("expected 1, got %d", len(mem.Events()))
	}
	got := mem.Events()[0]
	if got.Type != "tool.completed" {
		t.Errorf("type lost")
	}
	if got.Source != "engine.test" {
		t.Errorf("expected default Source filled in; got %q", got.Source)
	}
}

func TestEmit_NoOpWhenNoEmitterInCtx(t *testing.T) {
	ctx := withFlag(context.Background(), true)
	if err := Emit(ctx, New("x", SeverityInfo)); err != nil {
		t.Errorf("没注入 emitter 应安静返回；got %v", err)
	}
}

func TestEmit_RejectsEmptyType(t *testing.T) {
	mem := NewMemorySink()
	em := NewEmitter(mem, "x")
	ctx := withFlag(WithEmitter(context.Background(), em), true)
	err := Emit(ctx, Event{Severity: SeverityInfo})
	if err == nil {
		t.Error("空 Type 应被拒绝")
	}
	if len(mem.Events()) != 0 {
		t.Error("空 Type 不应进 Sink")
	}
}

func TestEmitter_NilSinkSafelyDegrades(t *testing.T) {
	em := NewEmitter(nil, "x")
	if err := em.Emit(context.Background(), New("a", SeverityInfo)); err != nil {
		t.Errorf("nil sink 应退化为 Noop；got %v", err)
	}
}

// helpers --------------------------------------------------------------

func splitLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				out = append(out, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}

func readAll(r interface{ Read(p []byte) (n int, err error) }) ([]byte, error) {
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, err
		}
	}
}
