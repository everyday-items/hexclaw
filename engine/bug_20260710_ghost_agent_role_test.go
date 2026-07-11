package engine

// BUG-20260710：role/pinned_agent 显式指向的 agent 不存在（已删除）时，引擎静默回落
// 默认助理「小蟹」人设继续作答——真机取证：K12 辅导会话（实例已删的孤儿会话）里问
// 「介绍下你」，回复是小蟹自我介绍，前端仍渲染辅导皮肤 → 双端呈现撕裂、身份欺骗式降级。
//
// 期望行为（本测试断言，未修复时 FAIL 即证明 bug）：显式指定的 agent 查无此人 →
// 明确报错（含「不存在/已删除」语义），**不调用 LLM**、不冒充默认助理。
// 注意区分三态：role 为空=默认助理（合法）；role=内置角色（合法）；role=查无此人（本 bug）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

func TestBUG20260710_GhostAgentRole_FailsLoud_NotXiaoxie(t *testing.T) {
	var llmCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			llmCalls.Add(1)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"我是小蟹"},"done":true}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	eng := newWarmupTestEngine(t, map[string]config.LLMProviderConfig{
		"Ollama (本地)": {BaseURL: srv.URL + "/v1", Model: "m"},
	}, "Ollama (本地)")

	msg := &adapter.Message{
		ID:       "ghost-1",
		Platform: adapter.PlatformAPI,
		ChatID:   "c1",
		UserID:   "u1",
		Content:  "介绍下你",
		Metadata: map[string]string{
			"role":         "k12-tutor-DELETED00", // 显式指定但查无此 agent
			"pinned_agent": "k12-tutor-DELETED00",
		},
		Timestamp: time.Now(),
	}
	reply, err := eng.Process(context.Background(), msg)

	// 期望：fail-loud——error 或 reply 明确告知 agent 不存在/已删除；绝不冒充默认助理作答
	loud := false
	if err != nil && (strings.Contains(err.Error(), "不存在") || strings.Contains(err.Error(), "已删除")) {
		loud = true
	}
	if reply != nil && (strings.Contains(reply.Content, "不存在") || strings.Contains(reply.Content, "已删除")) {
		loud = true
	}
	if !loud {
		t.Fatalf("显式 role 查无此 agent 应明确报错，实际 err=%v reply=%q（静默回落小蟹=身份欺骗式降级）",
			err, replyContent(reply))
	}
	if got := llmCalls.Load(); got != 0 {
		t.Fatalf("查无此 agent 不应调用 LLM 冒充默认助理，实际调用 %d 次", got)
	}
}

// 对照：role 为空 = 默认助理，合法路径不受影响（LLM 正常被调用）
func TestBUG20260710_EmptyRole_DefaultAssistantStillWorks(t *testing.T) {
	var llmCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			llmCalls.Add(1)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"你好"},"done":true}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	eng := newWarmupTestEngine(t, map[string]config.LLMProviderConfig{
		"Ollama (本地)": {BaseURL: srv.URL + "/v1", Model: "m"},
	}, "Ollama (本地)")

	msg := &adapter.Message{
		ID: "ok-1", Platform: adapter.PlatformAPI, ChatID: "c2", UserID: "u1",
		Content: "你好", Metadata: map[string]string{"pinned_agent": "default"}, Timestamp: time.Now(),
	}
	reply, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("空 role 默认助理路径不应报错: %v", err)
	}
	if reply == nil || strings.TrimSpace(reply.Content) == "" {
		t.Fatal("默认助理应正常作答")
	}
}

func TestBUG20260710_PinnedOnlyGhostAgentFailsLoud(t *testing.T) {
	eng := newEngineWithProvider(t, newCountingProvider())
	msg := &adapter.Message{Metadata: map[string]string{
		"pinned_agent": "k12-tutor-DELETED-only-pin",
	}}
	if err := eng.guardExplicitRoleExists(msg); err == nil ||
		(!strings.Contains(err.Error(), "不存在") && !strings.Contains(err.Error(), "已删除")) {
		t.Fatalf("pinned_agent-only ghost must fail loud before routing, got %v", err)
	}
	if err := eng.guardExplicitRoleExists(&adapter.Message{Metadata: map[string]string{
		"pinned_agent": "default",
	}}); err != nil {
		t.Fatalf("default pin remains valid: %v", err)
	}
}

func replyContent(r *adapter.Reply) string {
	if r == nil {
		return "<nil>"
	}
	return r.Content
}
