package dingtalk

import (
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
		{name: "block-square-root", markdown: "推导：\n\\[\\sqrt{16} = 4\\]", visible: []string{"√16", "= 4"}},
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
			for _, raw := range []string{"\\frac", "\\sqrt", "\\times", "\\alpha", "\\beta", "\\geq", "\\mathrm", "\\,", "\\[", "\\]"} {
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
