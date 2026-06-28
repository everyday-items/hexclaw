package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/storage"
)

// BUG-20260627 #4：发送时挂载的 skill chip，切到别的会话再回来就没了。
// 根因：用户消息持久化 encodeMessageMetadata 只编码 attachments + documents，
// 漏了随请求 metadata["skills"]（逗号分隔）上送的挂载技能 → 重载后 metadata.skills 缺失，
// 前端 getMessageSkills（仅认数组）取不到 → chip 不显示。
// 不变量：随请求上送的 skills 须落库，并在重载后以**数组**形态还原到 metadata.skills
// （与前端在内存中的 userMeta.skills 形态、与 getMessageSkills 的 Array.isArray 契约一致）。
func TestSaveUserMessage_SkillsSurviveReload_BUG20260627(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()

	sess, err := mgr.GetOrCreate(ctx, &adapter.Message{Platform: adapter.PlatformWeb, UserID: "u1", Content: "seed"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	if err := mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{
		Content: "用这俩角色",
		Metadata: map[string]string{
			"request_id": "req-skill-1",
			"skills":     "前leader,前女友",
		},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}

	msgs, err := store.ListMessages(ctx, sess.ID, 50, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var um *storage.MessageRecord
	for _, m := range msgs {
		if m.Content == "用这俩角色" {
			um = m
		}
	}
	if um == nil {
		t.Fatal("未找到用户消息")
	}

	var meta struct {
		Skills []string `json:"skills"`
	}
	if err := json.Unmarshal([]byte(um.Metadata), &meta); err != nil {
		t.Fatalf("解析重载 metadata 失败: %v (metadata=%q)", err, um.Metadata)
	}
	if len(meta.Skills) != 2 || meta.Skills[0] != "前leader" || meta.Skills[1] != "前女友" {
		t.Fatalf("BUG-20260627 #4: skill chip 重载后丢失。期望 metadata.skills=[前leader 前女友]，实得 %v (metadata=%q)", meta.Skills, um.Metadata)
	}
}

// 留空 skills 不应污染 metadata（不写入空数组键）。
func TestSaveUserMessage_NoSkills_NoSkillKey_BUG20260627(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()

	sess, _ := mgr.GetOrCreate(ctx, &adapter.Message{Platform: adapter.PlatformWeb, UserID: "u1", Content: "seed"})
	if err := mgr.SaveUserMessage(ctx, sess.ID, &adapter.Message{
		Content:  "无技能",
		Metadata: map[string]string{"request_id": "req-noskill"},
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	msgs, _ := store.ListMessages(ctx, sess.ID, 50, 0)
	for _, m := range msgs {
		if m.Content == "无技能" && strings.Contains(m.Metadata, "skills") {
			t.Fatalf("无技能消息不应含 skills 键，metadata=%q", m.Metadata)
		}
	}
}

// 表征测试：助手 reasoning + thinking_duration 走 SaveAssistantReply 已落 metadata，
// 重载后应原样还原（证明 reasoning 持久化本身**不是** bug——若 reasoning 在某会话丢失，
// 根因在「该轮根本没有正常 assistant 回复」即 #1，而非持久化层）。
func TestSaveAssistantReply_ReasoningSurvivesReload_BUG20260627(t *testing.T) {
	mgr, store := newTestManager(t)
	ctx := context.Background()

	sess, _ := mgr.GetOrCreate(ctx, &adapter.Message{Platform: adapter.PlatformWeb, UserID: "u1", Content: "seed"})

	if _, err := mgr.SaveAssistantReply(ctx, sess.ID, "最终回答", AssistantMeta{
		Reasoning:        "让我想想……第一步……第二步……",
		ThinkingDuration: 21,
		Provider:         "siliconflow",
		Model:            "Qwen3",
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
	var meta struct {
		Reasoning        string `json:"reasoning"`
		ThinkingDuration int    `json:"thinking_duration"`
	}
	if err := json.Unmarshal([]byte(am.Metadata), &meta); err != nil {
		t.Fatalf("解析助手 metadata 失败: %v (metadata=%q)", err, am.Metadata)
	}
	if !strings.Contains(meta.Reasoning, "第一步") {
		t.Fatalf("reasoning 重载后丢失，metadata=%q", am.Metadata)
	}
	if meta.ThinkingDuration != 21 {
		t.Fatalf("thinking_duration 重载后丢失，期望 21 实得 %d", meta.ThinkingDuration)
	}
}
