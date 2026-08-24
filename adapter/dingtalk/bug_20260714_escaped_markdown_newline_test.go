package dingtalk

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// BUG-20260714：某些自动化/模型链路把 Markdown 正文二次转义成字面量 `\n`，
// sampleMarkdown 不会替调用方反转义，最终钉钉把 `\n` 和后面的 `>`/`#` 全挤在标题行。
func TestBug20260714_MarkdownMessageRestoresEscapedDocumentNewlines(t *testing.T) {
	content := `HexClaw 全链路回归\n\n> 以下正文来自真实模型请求。\n\n# 真实模型与钉钉链路测试\n\n- 云端模型成功`
	reply := &adapter.Reply{Content: content}
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		t.Fatalf("构造钉钉 channel manifest: %v", err)
	}
	projected, err := dingTalkManifestMarkdown(*reply.RenderManifest)
	if err != nil {
		t.Fatalf("读取钉钉 Markdown 投影: %v", err)
	}
	msg := dingtalkMarkdownMessage(projected)

	var payload struct {
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(msg.MsgParam), &payload); err != nil {
		t.Fatalf("解析钉钉 markdown 载荷: %v", err)
	}
	if strings.Contains(payload.Text, `\n`) {
		t.Fatalf("字面量 \\n 仍漏到钉钉正文: %q", payload.Text)
	}
	if !strings.Contains(payload.Text, "\n\n> 以下正文") || !strings.Contains(payload.Text, "\n\n# 真实模型") {
		t.Fatalf("Markdown 段落结构没有恢复: %q", payload.Text)
	}
	if payload.Title != "HexClaw 全链路回归" {
		t.Fatalf("title 不应吞入转义正文，got %q", payload.Title)
	}
}

// 单个字面量 `\n` 很可能是用户讲 JSON/转义本身，不应擅自改写；只有明显的整篇文档
// 二次转义（至少两个换行标记且没有真实换行）才做兼容修复。
func TestBug20260714_MarkdownMessagePreservesSingleLiteralEscape(t *testing.T) {
	content := `JSON 字符串 a\nb 是什么意思？`
	reply := &adapter.Reply{Content: content}
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		t.Fatalf("构造钉钉 channel manifest: %v", err)
	}
	projected, err := dingTalkManifestMarkdown(*reply.RenderManifest)
	if err != nil {
		t.Fatalf("读取钉钉 Markdown 投影: %v", err)
	}
	msg := dingtalkMarkdownMessage(projected)
	var payload map[string]string
	if err := json.Unmarshal([]byte(msg.MsgParam), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["text"] != content {
		t.Fatalf("不应改写用户讨论的单个转义符: %q", payload["text"])
	}
}
