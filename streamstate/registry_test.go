package streamstate

import (
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// TestNewRegistry_DefaultTTL 零值 TTL 应使用默认 2 分钟。
func TestNewRegistry_DefaultTTL(t *testing.T) {
	r := NewRegistry(0)
	if r == nil {
		t.Fatal("registry 不应为 nil")
	}
	if r.ttl != 2*time.Minute {
		t.Errorf("期望默认 TTL=2min, 实际=%v", r.ttl)
	}

	r2 := NewRegistry(-1)
	if r2.ttl != 2*time.Minute {
		t.Errorf("负 TTL 应回退到默认, 实际=%v", r2.ttl)
	}

	r3 := NewRegistry(30 * time.Second)
	if r3.ttl != 30*time.Second {
		t.Errorf("显式 TTL 应保留, 实际=%v", r3.ttl)
	}
}

// TestRegistry_StartAppendComplete happy path: Start → Append → Done。
func TestRegistry_StartAppendComplete(t *testing.T) {
	r := NewRegistry(time.Minute)
	r.Start("user1", "sess1", "req1")

	// 初始 pending
	snap, ok := r.GetStreamSnapshot("user1", "req1")
	if !ok {
		t.Fatal("Start 后应可查询到")
	}
	if snap.Status != StatusPending {
		t.Errorf("期望 pending, 实际 %v", snap.Status)
	}

	// Append 一段增量
	s2 := r.Append("req1", &adapter.ReplyChunk{
		Content:   "hello",
		Reasoning: "think",
		Metadata:  map[string]string{"k": "v"},
		ReasoningDisclosure: adapter.ReasoningDisclosure{
			Visibility: adapter.ReasoningVisible,
			Source:     "ollama",
			Dialect:    "message.thinking",
			Provider:   "ollama",
			Model:      "test-model",
		},
	})
	if s2 == nil || s2.Status != StatusStreaming {
		t.Fatalf("append 后应 streaming, 实际 %+v", s2)
	}
	if s2.Content != "hello" || s2.Reasoning != "think" {
		t.Errorf("内容拼接错: %+v", s2)
	}
	if s2.Metadata["k"] != "v" {
		t.Errorf("metadata 未合并: %+v", s2.Metadata)
	}

	// 再次 Append 应累加，并携带 Usage / ToolCall
	usage := &adapter.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}
	tc := []adapter.ToolCall{{ID: "t1", Name: "echo"}}
	s3 := r.Append("req1", &adapter.ReplyChunk{
		Content:   " world",
		Done:      true,
		Usage:     usage,
		ToolCalls: tc,
	})
	if s3 == nil || !s3.Done || s3.Status != StatusCompleted {
		t.Fatalf("Done=true 后应 completed, 实际 %+v", s3)
	}
	if s3.Content != "hello world" {
		t.Errorf("内容累加错: %q", s3.Content)
	}
	if s3.Usage == nil || s3.Usage.TotalTokens != 3 {
		t.Errorf("Usage 缺失: %+v", s3.Usage)
	}
	if len(s3.ToolCalls) != 1 || s3.ToolCalls[0].ID != "t1" {
		t.Errorf("ToolCalls 缺失: %+v", s3.ToolCalls)
	}

	// Done 后 ListActiveStreams 应不再包含
	active := r.ListActiveStreams("user1")
	if len(active) != 0 {
		t.Errorf("completed 流不应出现在 active 列表: %+v", active)
	}
}

// TestRegistry_FailAndCancel 错误/取消状态。
func TestRegistry_FailAndCancel(t *testing.T) {
	r := NewRegistry(time.Minute)

	// Fail 路径
	r.Start("u", "s", "reqF")
	s := r.Fail("reqF", errors.New("boom"))
	if s == nil || s.Status != StatusErrored {
		t.Fatalf("Fail 后应 errored, 实际 %+v", s)
	}
	if s.Metadata["error"] != "boom" {
		t.Errorf("错误信息未写入 Metadata: %+v", s.Metadata)
	}

	// Cancel 路径
	r.Start("u", "s", "reqC")
	s2 := r.Cancel("reqC")
	if s2 == nil || s2.Status != StatusCancelled {
		t.Fatalf("Cancel 后应 cancelled, 实际 %+v", s2)
	}

	// 未知 requestID 应返回 nil
	if r.Fail("nope", errors.New("x")) != nil {
		t.Error("未知 requestID Fail 应返回 nil")
	}
	if r.Cancel("") != nil {
		t.Error("空 requestID Cancel 应返回 nil")
	}
}

// TestRegistry_EdgeCases 边界条件。
func TestRegistry_EdgeCases(t *testing.T) {
	r := NewRegistry(time.Minute)

	// 空 requestID Start 静默忽略
	r.Start("u", "s", "")
	if len(r.ListActiveStreams("u")) != 0 {
		t.Error("空 requestID 不应注册")
	}

	// Append 到不存在的 requestID
	if r.Append("ghost", &adapter.ReplyChunk{Content: "x"}) != nil {
		t.Error("不存在的 requestID Append 应返回 nil")
	}

	// Append nil chunk
	r.Start("u", "s", "r1")
	if r.Append("r1", nil) != nil {
		t.Error("nil chunk 应返回 nil")
	}

	// Start 已存在的 requestID 应重置状态
	r.Append("r1", &adapter.ReplyChunk{Content: "partial"})
	r.Start("u2", "s2", "r1") // 重启
	snap, ok := r.GetStreamSnapshot("u2", "r1")
	if !ok {
		t.Fatal("重启后新 user 应可查")
	}
	if snap.Status != StatusPending {
		t.Errorf("重启后应 pending, 实际 %v", snap.Status)
	}
	if snap.Done {
		t.Error("重启后 Done 应为 false")
	}

	// GetStreamSnapshot 跨 user 不应命中
	if _, ok := r.GetStreamSnapshot("other-user", "r1"); ok {
		t.Error("跨 user 查询不应命中")
	}
}

// TestRegistry_ListActiveStreams_Filter 验证 userID 过滤 + 排序。
func TestRegistry_ListActiveStreams_Filter(t *testing.T) {
	r := NewRegistry(time.Minute)
	r.Start("u1", "s", "a")
	r.Start("u1", "s", "b")
	r.Start("u2", "s", "c")

	list1 := r.ListActiveStreams("u1")
	if len(list1) != 2 {
		t.Errorf("u1 应有 2 条 active, 实际 %d", len(list1))
	}
	list2 := r.ListActiveStreams("u2")
	if len(list2) != 1 || list2[0].RequestID != "c" {
		t.Errorf("u2 过滤错: %+v", list2)
	}

	// 空 userID 返回空
	if len(r.ListActiveStreams("nobody")) != 0 {
		t.Error("未知 user 应返回空")
	}
}

// TestRegistry_CleanupAfterTTL 完成后的条目超过 TTL 被清理。
func TestRegistry_CleanupAfterTTL(t *testing.T) {
	r := NewRegistry(10 * time.Millisecond)
	r.Start("u", "s", "req")
	r.Append("req", &adapter.ReplyChunk{Content: "x", Done: true})

	// 立刻应仍可查（未超 TTL）
	if _, ok := r.GetStreamSnapshot("u", "req"); !ok {
		t.Fatal("刚完成应仍可查")
	}

	time.Sleep(30 * time.Millisecond)

	// 任意操作触发 cleanupLocked
	r.Start("u", "s", "trigger")

	if _, ok := r.GetStreamSnapshot("u", "req"); ok {
		t.Error("超 TTL 应被清理")
	}
}
