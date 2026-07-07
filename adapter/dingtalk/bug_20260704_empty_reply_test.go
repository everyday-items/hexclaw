package dingtalk

// BUG-20260704：钉钉回复正文为空时，sampleMarkdown 的 text 字段为空，钉钉硬拒
// 「400 miss.param.markdownTotext」→ 答案发不出去（叠加占位撤回后用户一片空白）。
// 空正文来源：推理型模型只产出 <think> 被 StripThinking 剥空、纯工具调用轮无正文、审核截断等。
// 修复：在唯一出站构造点 dingtalkMarkdownMessage 强制 text 非空（与 title 兜底对称）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// 构造层不变量：任意「空/纯空白」正文 → 出站 text 必须非空（否则真机 400）；非空正文原样透传。
func TestDingtalkMarkdownMessage_EmptyContentGetsNonEmptyText_BUG20260704(t *testing.T) {
	for _, content := range []string{"", "   ", "\n\t ", "\r\n"} {
		msg := dingtalkMarkdownMessage(content)
		var p struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		}
		if err := json.Unmarshal([]byte(msg.MsgParam), &p); err != nil {
			t.Fatalf("content=%q: MsgParam 非法 JSON: %v", content, err)
		}
		if strings.TrimSpace(p.Text) == "" {
			t.Errorf("content=%q: 出站 text 仍为空 → 钉钉 sampleMarkdown 会 400 miss.param.markdownTotext", content)
		}
		if strings.TrimSpace(p.Title) == "" {
			t.Errorf("content=%q: title 不应为空", content)
		}
	}

	// 守 B7：非空正文必须原样透传，不被兜底逻辑污染。
	msg := dingtalkMarkdownMessage("你好 **世界**")
	var p struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(msg.MsgParam), &p)
	if p.Text != "你好 **世界**" {
		t.Errorf("非空正文被改动: %q, 期望 %q", p.Text, "你好 **世界**")
	}
}

// 端到端：上游产出空正文时，走生产 handleMessage 仍应发出一条**非空**回复（不 400、不空白）。
func TestHandleMessageEmptyReplyStillSendsNonEmpty_BUG20260704(t *testing.T) {
	a := newTestAdapter()
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: ""}, nil // 模拟推理型模型只出思考、正文被剥空
	}
	fakeAPI := newFakeDingtalkOpenAPI("test-token")
	a.openAPI = fakeAPI

	event := dtEvent{SenderStaffId: "user123", SenderNick: "TestUser"}
	event.Text.Content = "hi"

	a.handleMessage(event)

	calls := fakeAPI.SendCalls()
	if len(calls) != 2 {
		t.Fatalf("占位 + 回复应共 2 条，实际 %d：%+v", len(calls), calls)
	}
	if strings.TrimSpace(calls[1].Text) == "" {
		t.Errorf("空正文回复应走兜底为非空文本，实际 %q（真机会 400）", calls[1].Text)
	}
}
