package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/storage"
)

// 有序内容块持久化 round-trip：经 AssistantMeta.Blocks 上送 → 落 meta.blocks → 重载原样还原，
// 多步 ReAct 切会话/重启后仍按真实交错序渲染（前端 normalizeLoadedMessage 读 metadata.blocks）。
func TestSaveAssistantReply_BlocksSurviveReload(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()
	sess, _ := mgr.GetOrCreate(ctx, &adapter.Message{Platform: adapter.PlatformWeb, UserID: "u1", Content: "seed"})

	if _, err := mgr.SaveAssistantReply(ctx, sess.ID, "空气良，适合外出", AssistantMeta{
		Blocks: []adapter.Block{
			{Type: "text", Text: "先查天气。"},
			{Type: "tool_use", ID: "t1", Name: "weather", Input: "{}"},
			{Type: "tool_result", ToolUseID: "t1", ToolName: "weather", Output: "27°C"},
			{Type: "text", Text: "再查空气。"},
			{Type: "tool_use", ID: "t2", Name: "aqi", Input: "{}"},
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
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
	var meta struct {
		Blocks []adapter.Block `json:"blocks"`
	}
	if err := json.Unmarshal([]byte(am.Metadata), &meta); err != nil {
		t.Fatalf("解析 metadata: %v (%q)", err, am.Metadata)
	}
	if len(meta.Blocks) != 5 {
		t.Fatalf("blocks 重载后丢失：期望 5，实得 %d (%q)", len(meta.Blocks), am.Metadata)
	}
	// 交错序保真 + camelCase 字段（对齐前端 ContentBlock）
	if meta.Blocks[1].Type != "tool_use" || meta.Blocks[1].ID != "t1" {
		t.Fatalf("tool_use 块错位: %+v", meta.Blocks[1])
	}
	if meta.Blocks[3].Type != "text" || meta.Blocks[3].Text != "再查空气。" {
		t.Fatalf("工具间文本块丢失: %+v", meta.Blocks[3])
	}
	if meta.Blocks[2].ToolUseID != "t1" || meta.Blocks[2].ToolName != "weather" {
		t.Fatalf("tool_result camelCase 字段错: %+v", meta.Blocks[2])
	}
}
