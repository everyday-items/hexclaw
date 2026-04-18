package trace

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTraceGoAndDetach_Fix_v0_3_12_H7 回归测试：
// 修复前业务代码中 `go func() { ... context.Background() ... }()` 模式让异步任务
// 完全脱离 trace 链路；Session/MCP/Skill 调用失败时日志里没 trace_id，排障靠猜。
// 修复后 trace.Go() 提供：Detach（保留 logger 链路）+ panic recover + 统一日志。
func TestTraceGoAndDetach_Fix_v0_3_12_H7(t *testing.T) {
	t.Run("before_fix_behavior_context_background_loses_logger", func(t *testing.T) {
		var buf bytes.Buffer
		h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
		logger := slog.New(h).With("session", "sess-001")
		_ = WithLogger(context.Background(), logger)

		// 修复前模式：goroutine 里用 context.Background() → logger 丢失
		done := make(chan struct{})
		go func() {
			L(context.Background()).Info("async log without trace")
			close(done)
		}()
		<-done

		if strings.Contains(buf.String(), "sess-001") {
			t.Error("修复前，子 goroutine 用 Background 不该继承 session_id")
		}
		t.Logf("修复前：async 日志无 session_id，排障困难")
	})

	t.Run("after_fix_detach_preserves_logger", func(t *testing.T) {
		var buf bytes.Buffer
		h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
		logger := slog.New(h).With("session", "sess-002")
		parent := WithLogger(context.Background(), logger)

		detached := Detach(parent)
		L(detached).Info("after detach")

		if !strings.Contains(buf.String(), "sess-002") {
			t.Error("Detach 后 logger 应保留 session_id")
		}
	})

	t.Run("after_fix_detach_preserves_all_other_values", func(t *testing.T) {
		// F4 修复验证：原实现只保留 logger 丢了其他 Values；
		// 修复后应保留所有 ctx.Value（user_id / tenant_id / 任意 key）
		type userKey struct{}
		type tenantKey struct{}
		parent := context.WithValue(context.Background(), userKey{}, "alice")
		parent = context.WithValue(parent, tenantKey{}, "school-A")

		detached := Detach(parent)

		if got := detached.Value(userKey{}); got != "alice" {
			t.Errorf("Detach 后 user_id 丢失：got=%v want=alice", got)
		}
		if got := detached.Value(tenantKey{}); got != "school-A" {
			t.Errorf("Detach 后 tenant_id 丢失：got=%v want=school-A", got)
		}
	})

	t.Run("after_fix_detach_drops_cancellation", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		detached := Detach(parent)

		cancel() // 父 ctx 取消

		select {
		case <-detached.Done():
			t.Error("Detach 后的 ctx 不应被父 cancel 传染")
		case <-time.After(10 * time.Millisecond):
			// 预期：detached 仍然是活的
		}
	})

	t.Run("after_fix_go_preserves_logger_in_goroutine", func(t *testing.T) {
		var buf bytes.Buffer
		var mu sync.Mutex
		h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
		logger := slog.New(h).With("session", "sess-go")
		parent := WithLogger(context.Background(), logger)

		done := make(chan struct{})
		Go(parent, "test-task", func(ctx context.Context) {
			mu.Lock()
			defer mu.Unlock()
			L(ctx).Info("inside goroutine")
			close(done)
		})
		<-done

		mu.Lock()
		out := buf.String()
		mu.Unlock()

		if !strings.Contains(out, "sess-go") {
			t.Errorf("goroutine 内日志应含 session：%s", out)
		}
	})

	t.Run("after_fix_go_recovers_panic", func(t *testing.T) {
		var buf bytes.Buffer
		var mu sync.Mutex
		handler := &syncHandler{h: slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}), mu: &mu}
		logger := slog.New(handler).With("session", "sess-panic")
		parent := WithLogger(context.Background(), logger)

		done := make(chan struct{})
		Go(parent, "panic-task", func(ctx context.Context) {
			defer close(done)
			panic("boom")
		})
		<-done
		time.Sleep(5 * time.Millisecond) // 等 recover 日志刷盘

		mu.Lock()
		out := buf.String()
		mu.Unlock()

		if !strings.Contains(out, "panic recovered") {
			t.Errorf("panic 应被 recover 并记录：%s", out)
		}
		if !strings.Contains(out, "panic-task") {
			t.Errorf("日志应含 task 名：%s", out)
		}
		if !strings.Contains(out, "boom") {
			t.Errorf("日志应含 panic 值：%s", out)
		}
	})

	t.Run("after_fix_recover_helper_captures_error", func(t *testing.T) {
		var buf bytes.Buffer
		var mu sync.Mutex
		handler := &syncHandler{h: slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}), mu: &mu}
		logger := slog.New(handler).With("session", "sess-rec")
		ctx := WithLogger(context.Background(), logger)

		var capturedErr error
		func() {
			defer Recover(ctx, "skill.exec")(&capturedErr)
			panic(errors.New("skill crashed"))
		}()

		if capturedErr == nil {
			t.Fatal("Recover 应把 panic 转换为 error 写入 *err")
		}
		if !strings.Contains(capturedErr.Error(), "skill.exec") {
			t.Errorf("error 应含 task 名：%v", capturedErr)
		}

		mu.Lock()
		out := buf.String()
		mu.Unlock()
		if !strings.Contains(out, "sess-rec") {
			t.Errorf("日志应含 session：%s", out)
		}
	})

	t.Run("after_fix_recover_non_error_panic_value", func(t *testing.T) {
		var capturedErr error
		func() {
			defer Recover(context.Background(), "task")(&capturedErr)
			panic("string value")
		}()
		if capturedErr == nil || !strings.Contains(capturedErr.Error(), "string value") {
			t.Errorf("string panic 应被转为 error：%v", capturedErr)
		}
	})

	t.Run("after_fix_recover_no_panic_does_nothing", func(t *testing.T) {
		var capturedErr error
		func() {
			defer Recover(context.Background(), "task")(&capturedErr)
		}()
		if capturedErr != nil {
			t.Errorf("无 panic 不应产生 error：%v", capturedErr)
		}
	})
}

// syncHandler 包装 slog.Handler 加锁，避免 test 中 buf 写入竞争
type syncHandler struct {
	h  slog.Handler
	mu *sync.Mutex
}

func (s *syncHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return s.h.Enabled(ctx, lvl)
}
func (s *syncHandler) Handle(ctx context.Context, r slog.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.h.Handle(ctx, r)
}
func (s *syncHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &syncHandler{h: s.h.WithAttrs(attrs), mu: s.mu}
}
func (s *syncHandler) WithGroup(name string) slog.Handler {
	return &syncHandler{h: s.h.WithGroup(name), mu: s.mu}
}
