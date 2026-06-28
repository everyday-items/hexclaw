package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/storage"
)

// P1 bug：助手消息的 tool_calls 不持久化 → 切会话/重启重载后工具卡蒸发。
// 根因：SaveAssistantReply 的 metaMap 只写 reasoning/thinking_duration/provider/model/agent_name，
// 漏写 tool_calls（storage.MessageRecord.Meta 的 schema 注释早已声明应存 tool_calls）。
// 真模型已端到端取证（live tool_calls=1，重载 meta={} → 0）。
//
// 不变量：经 AssistantMeta.ToolCalls 上送的工具调用须落进 meta.tool_calls，重载后原样还原
// （含 status/duration_ms，与 live wire 形状一致；前端 normalizeLoadedMessage 读 metadata.tool_calls 重建卡片）。
func TestSaveAssistantReply_ToolCallsSurviveReload(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()

	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{Platform: adapter.PlatformWeb, UserID: "u1", Content: "seed"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if _, err := mgr.SaveAssistantReply(ctx, sess.ID, "杭州今天 27°C", AssistantMeta{
		Provider: "硅基流动",
		Model:    "Qwen/Qwen3.6-35B-A3B",
		ToolCalls: []adapter.ToolCall{{
			ID:         "call_abc",
			Name:       "weather",
			Arguments:  `{"location":"杭州"}`,
			Result:     "🌍 杭州 27°C",
			Status:     "success",
			DurationMs: 1234,
		}},
	}); err != nil {
		t.Fatalf("save assistant: %v", err)
	}

	msgs, _ := store.ListMessages(ctx, sess.ID, 50, 0)
	var am *storage.MessageRecord
	for _, m := range msgs {
		if m.Role == "assistant" {
			am = m
		}
	}
	if am == nil {
		t.Fatal("未找到助手消息")
	}

	// 模拟前端 normalizeLoadedMessage：从重载的 metadata JSON 取 tool_calls。
	var meta struct {
		ToolCalls []adapter.ToolCall `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(am.Metadata), &meta); err != nil {
		t.Fatalf("解析重载 metadata 失败: %v (metadata=%q)", err, am.Metadata)
	}
	if len(meta.ToolCalls) != 1 {
		t.Fatalf("P1 bug: tool_calls 重载后丢失。期望 1，实得 %d (metadata=%q)", len(meta.ToolCalls), am.Metadata)
	}
	tc := meta.ToolCalls[0]
	if tc.Name != "weather" || tc.ID != "call_abc" {
		t.Fatalf("tool_call 字段错位: %+v", tc)
	}
	if !strings.Contains(tc.Result, "27°C") {
		t.Fatalf("tool_call.result 丢失: %q", tc.Result)
	}
	// status / duration_ms 同样随之落库（与 live wire 形状一致）。
	if tc.Status != "success" {
		t.Fatalf("status 未持久化: %q", tc.Status)
	}
	if tc.DurationMs != 1234 {
		t.Fatalf("duration_ms 未持久化: %d", tc.DurationMs)
	}
}

// 无工具调用时不写 tool_calls 键（不污染 meta）。
func TestSaveAssistantReply_NoToolCalls_NoKey(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()
	sess, _ := mgr.GetOrCreate(ctx, &adapter.Message{Platform: adapter.PlatformWeb, UserID: "u1", Content: "seed"})

	if _, err := mgr.SaveAssistantReply(ctx, sess.ID, "纯文本回答", AssistantMeta{Model: "x"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	msgs, _ := store.ListMessages(ctx, sess.ID, 50, 0)
	for _, m := range msgs {
		if m.Role == "assistant" && strings.Contains(m.Metadata, "tool_calls") {
			t.Fatalf("无工具调用不应含 tool_calls 键，metadata=%q", m.Metadata)
		}
	}
}
