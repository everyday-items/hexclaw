package api

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func TestK12RenderContractBindsCanonicalMathToSourceDigest(t *testing.T) {
	content, manifest := canonicalRenderProjection(
		messagecontent.ProducerK12,
		"zh-CN",
		"## 批改结果\n\n答案是 $\\frac{3}{4} \\times 8 = 6$。",
		messagecontent.SurfaceK12,
		"k12-markdown-v1",
	)
	if content == nil || manifest == nil {
		t.Fatal("K12 projection must return MessageContent and RenderManifest together")
	}
	if content.ProducerKind != messagecontent.ProducerK12 {
		t.Fatalf("producer = %q", content.ProducerKind)
	}
	if content.SourceDigest != manifest.SourceDigest || content.ContentID != manifest.ContentID {
		t.Fatalf("projection is not source-bound: content=%#v manifest=%#v", content, manifest)
	}
	if !manifest.CapabilitySnapshot.Markdown || !manifest.CapabilitySnapshot.TeXMath || !manifest.CapabilitySnapshot.MathML {
		t.Fatalf("K12 desktop capability snapshot = %#v", manifest.CapabilitySnapshot)
	}
	if err := manifest.ValidateFor(*content); err != nil {
		t.Fatalf("ValidateFor: %v", err)
	}
}

func TestK12RenderContractRejectsEmptyOrUnknownProducer(t *testing.T) {
	if content := canonicalChatContent("", map[string]string{"producer_kind": "k12"}); content != nil {
		t.Fatalf("empty successful content = %#v", content)
	}
	content := canonicalChatContent("正文", map[string]string{
		"producer_kind": "unknown",
		"locale":        "zh-CN",
	})
	if content == nil {
		t.Fatal("legacy producer fallback must remain readable")
	}
	if content.ProducerKind != messagecontent.ProducerChat {
		t.Fatalf("unknown producer must not escape the closed vocabulary: %q", content.ProducerKind)
	}
}
