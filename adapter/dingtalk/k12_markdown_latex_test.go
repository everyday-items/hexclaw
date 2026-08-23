package dingtalk

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func TestK12DingTalkMarkdownLaTeXProjectionMatrix(t *testing.T) {
	tests := []struct {
		name     string
		markdown string
		visible  []string
	}{
		{name: "heading-list", markdown: "## 辅导要点\n\n- 先审题\n- 再验算", visible: []string{"## 辅导要点", "- 先审题"}},
		{name: "inline-fraction", markdown: "答案是 $\\frac{3}{4} \\times 8 = 6$。", visible: []string{"3/4", "×", "= 6"}},
		{name: "mixed-number", markdown: "第一天修了 $2\\frac{3}{4}$ 千米，第二天多修 $1\\frac{1}{2}$ 千米。", visible: []string{"2 3/4", "1 1/2"}},
		{name: "block-square-root", markdown: "推导：\n\\[\\sqrt{16} = 4\\]", visible: []string{"√16", "= 4"}},
		{name: "aligned-block", markdown: "推导：\\[\\begin{aligned}2x + 1 &= 5 \\\\ 2x &= 4\\end{aligned}\\]", visible: []string{"2x + 1 = 5\n2x = 4"}},
		{name: "scripts-units", markdown: "面积是 $6 \\, \\mathrm{cm}^2$，水是 $H_2O$。", visible: []string{"6 cm²", "H₂O"}},
		{name: "comparison", markdown: "因为 $\\alpha + \\beta \\geq 1$，所以成立。", visible: []string{"α + β ≥ 1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reply := &adapter.Reply{
				Content: tc.markdown,
				Metadata: map[string]string{
					"producer_kind": "k12",
					"locale":        "zh-CN",
				},
			}
			if err := ensureDingTalkRenderEvidence(reply); err != nil {
				t.Fatalf("ensureDingTalkRenderEvidence: %v", err)
			}
			if reply.MessageContent == nil || reply.RenderManifest == nil {
				t.Fatalf("render evidence missing: %#v", reply)
			}
			if reply.MessageContent.ProducerKind != messagecontent.ProducerK12 {
				t.Fatalf("producer = %q", reply.MessageContent.ProducerKind)
			}
			if reply.RenderManifest.CapabilitySnapshot.TeXMath {
				t.Fatal("DingTalk must not claim native TeX support")
			}
			projected := reply.RenderManifest.Parts[0].Text
			for _, want := range tc.visible {
				if !strings.Contains(projected, want) {
					t.Fatalf("projection %q does not contain %q", projected, want)
				}
			}
			for _, raw := range []string{"\\frac", "\\sqrt", "\\times", "\\alpha", "\\beta", "\\geq", "\\mathrm", "\\begin", "\\end", "\\\\", "\\,", "\\[", "\\]", "$"} {
				if strings.Contains(projected, raw) {
					t.Fatalf("raw LaTeX %q leaked into projection %q", raw, projected)
				}
			}
			if err := reply.RenderManifest.ValidateFor(*reply.MessageContent); err != nil {
				t.Fatalf("ValidateFor: %v", err)
			}
		})
	}
}

func TestK12DingTalkDoubleEscapedSolutionUsesSampleMarkdown(t *testing.T) {
	content := "## 解题步骤\n\n1. **列式**：$\\\\frac{3}{4} \\\\times 8 = 6$\n2. **验算**：$6 \\\\div 8 = \\\\frac{3}{4}$"
	message := dingtalkMarkdownMessage(content)
	if message.MsgKey != "sampleMarkdown" {
		t.Fatalf("钉钉解题消息类型 = %q, want sampleMarkdown", message.MsgKey)
	}
	var payload struct {
		Title string `json:"title"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal([]byte(message.MsgParam), &payload); err != nil {
		t.Fatalf("decode sampleMarkdown payload: %v", err)
	}
	want := "## 解题步骤\n\n1. **列式**：3/4 × 8 = 6\n2. **验算**：6 ÷ 8 = 3/4"
	if payload.Text != want {
		t.Fatalf("sampleMarkdown 正文投影错误:\n got  %q\n want %q", payload.Text, want)
	}
	if payload.Title != "解题步骤" {
		t.Fatalf("sampleMarkdown 标题 = %q, want 解题步骤", payload.Title)
	}
}

func TestK12DingTalkMarkdownLaTeXProjectionIsIdempotent(t *testing.T) {
	reply := &adapter.Reply{
		Content:  "复习 $\\frac{1}{2} + \\frac{1}{4} = \\frac{3}{4}$",
		Metadata: map[string]string{"producer_kind": "cron", "locale": "zh-CN"},
	}
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		t.Fatalf("first projection: %v", err)
	}
	firstContentID := reply.MessageContent.ContentID
	firstRenderID := reply.RenderManifest.RenderID
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		t.Fatalf("second projection: %v", err)
	}
	if reply.MessageContent.ContentID != firstContentID || reply.RenderManifest.RenderID != firstRenderID {
		t.Fatalf("projection identity drifted: content=%q render=%q", reply.MessageContent.ContentID, reply.RenderManifest.RenderID)
	}
}
