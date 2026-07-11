package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

// replyMetaTestSkill 是产出 reply-safe 结构化元数据（record）+ 非白名单内部元数据的测试 skill。
type replyMetaTestSkill struct{ meta map[string]string }

func (s *replyMetaTestSkill) Name() string        { return "meta_skill" }
func (s *replyMetaTestSkill) Description() string { return "emits metadata" }
func (s *replyMetaTestSkill) Match(_ string) bool { return false }
func (s *replyMetaTestSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition(s.Name(), "test", nil)
}

type replyMetaProvider struct{}

func (p *replyMetaProvider) Name() string { return "test" }
func (p *replyMetaProvider) Complete(_ context.Context, req llm.CompletionRequest) (*llm.CompletionResponse, error) {
	for _, msg := range req.Messages {
		if msg.Role == llm.RoleTool {
			return &llm.CompletionResponse{Content: "saved"}, nil
		}
	}
	return &llm.CompletionResponse{ToolCalls: []llm.ToolCall{{ID: "meta-call", Name: "meta_skill", Arguments: "{}"}}}, nil
}
func (p *replyMetaProvider) Stream(context.Context, llm.CompletionRequest) (*llm.Stream, error) {
	return nil, fmt.Errorf("stream unused")
}
func (p *replyMetaProvider) Models() []llm.ModelInfo                { return nil }
func (p *replyMetaProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }
func (s *replyMetaTestSkill) Execute(_ context.Context, _ map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: "ok", Metadata: s.meta}, nil
}

// BUG-1 plumb：skill.Result.Metadata 在 ToolExecutor 边界被丢弃（只回传 Content），
// 导致 record chip 元数据永远到不了 reply。本测试钉死通用 plumb：ToolExecutor 执行 skill
// 时把 reply-safe 白名单键（record）收进 ctx sink；applyToolReplyMeta 落到 msg.Metadata；
// 非白名单键（internal）不得泄漏。
func TestBUG1_ToolExecutorStampsReplySafeMetaToSink(t *testing.T) {
	reg := skill.NewRegistry()
	if err := reg.Register(&replyMetaTestSkill{meta: map[string]string{
		"record":   `{"collection":"错题本","fields":{"question":"3.8×3"},"status":"new"}`,
		"internal": "should-not-leak",
	}}); err != nil {
		t.Fatal(err)
	}
	exec := NewToolExecutor(reg, nil)

	ctx := withToolReplyMetaSink(context.Background())
	if _, err := exec.Execute(ctx, "meta_skill", nil); err != nil {
		t.Fatal(err)
	}

	msg := &adapter.Message{}
	applyToolReplyMeta(ctx, msg)

	if got := msg.Metadata["record"]; got == "" {
		t.Errorf("record 应透传到 msg.Metadata, got meta=%v", msg.Metadata)
	}
	if _, leaked := msg.Metadata["internal"]; leaked {
		t.Errorf("非白名单内部元数据不应泄漏到 reply, meta=%v", msg.Metadata)
	}
}

// 无 sink 的 ctx（旧路径 / 无工具）下 stamp/apply 必须安全 no-op，不 panic。
func TestBUG1_ReplyMetaSinkAbsentIsNoOp(t *testing.T) {
	stampToolReplyMeta(context.Background(), map[string]string{"record": "x"})
	msg := &adapter.Message{}
	applyToolReplyMeta(context.Background(), msg)
	if len(msg.Metadata) != 0 {
		t.Errorf("无 sink 应 no-op, meta=%v", msg.Metadata)
	}
}

// buildReplyMetadata 必须把 record 键转发进最终回复元数据（前端消费点）。
func TestBUG1_BuildReplyMetadataForwardsRecord(t *testing.T) {
	in := map[string]string{"record": `{"collection":"错题本"}`, "role": "mingming"}
	out := buildReplyMetadata(in, "prov", "model", "msg-1")
	if out["record"] != in["record"] {
		t.Errorf("buildReplyMetadata 应转发 record, got %v", out)
	}
}

func TestBUG1_RecordMetadataSurvivesProcessSaveReload(t *testing.T) {
	record := `{"collection":"mistake","status":"new"}`
	reg := skill.NewRegistry()
	if err := reg.Register(&replyMetaTestSkill{meta: map[string]string{
		"record": record, "internal": "must-not-persist",
	}}); err != nil {
		t.Fatal(err)
	}
	eng := newEngineWithProviderAndSkills(t, &replyMetaProvider{}, reg)
	eng.SetToolCollector(NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))
	msg := &adapter.Message{
		ID: "reply-meta-save", Platform: adapter.PlatformAPI,
		UserID: "u-meta", ChatID: "c-meta", Content: "run metadata tool",
	}
	reply, err := eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if reply.Metadata["record"] != record {
		t.Fatalf("live reply lost record metadata: %v", reply.Metadata)
	}
	messages, err := eng.store.ListMessages(context.Background(), msg.SessionID, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, persisted := range messages {
		if persisted.Role != "assistant" {
			continue
		}
		if !strings.Contains(persisted.Metadata, `"record"`) || !strings.Contains(persisted.Metadata, "mistake") {
			t.Fatalf("record metadata vanished after save/reload: %q", persisted.Metadata)
		}
		if strings.Contains(persisted.Metadata, "must-not-persist") {
			t.Fatalf("non-whitelisted metadata leaked to persistence: %q", persisted.Metadata)
		}
		return
	}
	t.Fatal("assistant message not persisted")
}
