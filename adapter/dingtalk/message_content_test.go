package dingtalk

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func TestDingTalkReplyBuildsHonestCanonicalProjection(t *testing.T) {
	reply := &adapter.Reply{
		Content:  "## 结果\n\n$\\frac{3}{4} \\times 8 = 6$，且 $\\alpha + \\beta$。",
		Metadata: map[string]string{"producer_kind": "skill", "locale": "zh-CN"},
	}
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		t.Fatalf("ensureDingTalkRenderEvidence: %v", err)
	}
	if reply.MessageContent == nil || reply.MessageContent.ProducerKind != messagecontent.ProducerSkill {
		t.Fatalf("canonical content = %#v", reply.MessageContent)
	}
	if reply.RenderManifest == nil || reply.RenderManifest.Surface != messagecontent.SurfaceChannel {
		t.Fatalf("render manifest = %#v", reply.RenderManifest)
	}
	if reply.RenderManifest.CapabilitySnapshot.TeXMath || !reply.RenderManifest.CapabilitySnapshot.UnicodeMath {
		t.Fatalf("DingTalk capability snapshot lies: %#v", reply.RenderManifest.CapabilitySnapshot)
	}
	visible := reply.RenderManifest.Parts[0].Text
	for _, raw := range []string{`\\frac`, `\\times`, `\\alpha`, `\\beta`, "$"} {
		if strings.Contains(visible, raw) {
			t.Fatalf("raw LaTeX leaked into successful DingTalk projection: %q", visible)
		}
	}
	if err := reply.RenderManifest.ValidateFor(*reply.MessageContent); err != nil {
		t.Fatalf("ValidateFor: %v", err)
	}
}

func TestDingTalkReplyRejectsMismatchedExistingRenderEvidence(t *testing.T) {
	content, _ := messagecontent.New(messagecontent.ProducerChat, "zh-CN", "正文", nil)
	other, _ := messagecontent.New(messagecontent.ProducerChat, "zh-CN", "另一正文", nil)
	manifest, _ := messagecontent.BuildManifest(other, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceChannel,
		RendererVersion: "test",
		Capabilities:    messagecontent.CapabilitySnapshot{Markdown: true, TeXMath: true},
		Parts:           []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: other.Markdown}},
	})
	reply := &adapter.Reply{Content: content.Markdown, MessageContent: &content, RenderManifest: &manifest}
	if err := ensureDingTalkRenderEvidence(reply); err == nil {
		t.Fatal("mismatched content/manifest must fail closed")
	}
}

func TestDingTalkEmptyReplyCanonicalizesVisibleFallbackBeforeRenderEvidence(t *testing.T) {
	reply := &adapter.Reply{Content: " \n\t "}
	if err := ensureDingTalkRenderEvidence(reply); err != nil {
		t.Fatalf("ensureDingTalkRenderEvidence: %v", err)
	}
	if reply.Content != dingtalkEmptyReplyFallback {
		t.Fatalf("content = %q, want visible fallback %q", reply.Content, dingtalkEmptyReplyFallback)
	}
	if reply.MessageContent == nil || reply.MessageContent.Markdown != dingtalkEmptyReplyFallback {
		t.Fatalf("canonical content = %#v", reply.MessageContent)
	}
	if reply.RenderManifest == nil || len(reply.RenderManifest.Parts) != 1 || reply.RenderManifest.Parts[0].Text != dingtalkEmptyReplyFallback {
		t.Fatalf("render manifest = %#v", reply.RenderManifest)
	}
	if err := reply.RenderManifest.ValidateFor(*reply.MessageContent); err != nil {
		t.Fatalf("ValidateFor: %v", err)
	}
}
