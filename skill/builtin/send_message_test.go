package builtin

// 送达 Skill 测试（§11.10 后：纯发送器，审批由 engine 统一安全闸前置，Skill 不自管）：
//   - 经 sender 发送一次且仅一次（记录调用）。
//   - sender 失败（如凭证未配）→ 明确上抛。
//   - 缺参 / 无 sender → 明确报错。
//   - ToolDefinition Required = ["channel","target","content"]。

import (
	"context"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// recordingSender 记录每次 Send 调用。
type recordingSender struct {
	calls []sendCall
	err   error
}
type sendCall struct{ channel, target, content string }

func (r *recordingSender) Send(_ context.Context, channel, target, content string, _ []adapter.Attachment) error {
	r.calls = append(r.calls, sendCall{channel, target, content})
	return r.err
}

func TestSend_SendsExactlyOnce(t *testing.T) {
	sender := &recordingSender{}
	s := NewSendMessageSkill(sender)

	res, err := s.Execute(context.Background(), map[string]any{
		"channel": "feishu", "target": "g1", "content": "digest",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("send must go through sender exactly once, got %d", len(sender.calls))
	}
	got := sender.calls[0]
	if got.channel != "feishu" || got.target != "g1" || got.content != "digest" {
		t.Fatalf("sender got wrong args: %+v", got)
	}
	if res.Metadata["channel"] != "feishu" {
		t.Fatalf("result metadata channel mismatch: %v", res.Metadata)
	}
}

func TestSend_ChannelError_Surfaces(t *testing.T) {
	sender := &recordingSender{err: fmt.Errorf("smtp auth failed")}
	s := NewSendMessageSkill(sender)
	_, err := s.Execute(context.Background(), map[string]any{
		"channel": "email", "target": "a@b.com", "content": "x",
	})
	if err == nil {
		t.Fatalf("sender error (e.g. missing credentials) must surface, not be swallowed")
	}
}

func TestSend_MissingArgs_Errors(t *testing.T) {
	s := NewSendMessageSkill(&recordingSender{})
	if _, err := s.Execute(context.Background(), map[string]any{"channel": "feishu", "content": "x"}); err == nil {
		t.Fatalf("missing target must error")
	}
}

func TestSend_NoSender_Errors(t *testing.T) {
	s := NewSendMessageSkill(nil)
	if _, err := s.Execute(context.Background(), map[string]any{
		"channel": "feishu", "target": "g1", "content": "x",
	}); err == nil {
		t.Fatalf("nil sender must error clearly")
	}
}

func TestSend_ToolDef_Required(t *testing.T) {
	s := NewSendMessageSkill(nil)
	req := s.ToolDefinition().Function.Parameters.Required
	if len(req) != 3 {
		t.Fatalf("Required must be [channel target content], got %v", req)
	}
}
