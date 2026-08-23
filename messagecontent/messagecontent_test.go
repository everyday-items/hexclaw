package messagecontent

import (
	"strings"
	"testing"
)

func TestProducerKindsExactSet(t *testing.T) {
	want := []ProducerKind{
		ProducerChat,
		ProducerQuickChat,
		ProducerK12,
		ProducerSkill,
		ProducerTool,
		ProducerRAG,
		ProducerReport,
		ProducerCron,
		ProducerWebhook,
		ProducerWorkflow,
	}
	got := ProducerKinds()
	if len(got) != len(want) {
		t.Fatalf("producer kinds = %v, want exact set %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("producer kinds = %v, want exact set %v", got, want)
		}
	}
}

func TestNewCanonicalContentAndDigestValidation(t *testing.T) {
	content, err := New(ProducerK12, "zh-CN", "答案是 $\\frac{3}{4}$。", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if content.ContentVersion != CurrentContentVersion {
		t.Fatalf("content_version = %q", content.ContentVersion)
	}
	if content.ContentID == "" || content.SourceDigest == "" {
		t.Fatalf("content identity is incomplete: %#v", content)
	}
	if err := content.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	tampered := content
	tampered.Markdown = "答案被改了"
	if err := tampered.Validate(); err == nil || !strings.Contains(err.Error(), "source_digest") {
		t.Fatalf("tampered content must fail digest validation, got %v", err)
	}
}

func TestBuildRenderManifestFailsVisibleForUnsupportedMath(t *testing.T) {
	content, err := New(ProducerCron, "zh-CN", "复习：$\\frac{1}{2}$", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manifest, err := BuildManifest(content, RenderRequest{
		Surface:         SurfaceChannel,
		RendererVersion: "plain-v1",
		Capabilities: CapabilitySnapshot{
			Markdown:    false,
			TeXMath:     false,
			UnicodeMath: true,
		},
		Parts:          []RenderPart{{Kind: PartText, Text: "复习：1/2"}},
		FallbackReason: FallbackMathToReadableText,
	})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if manifest.SourceDigest != content.SourceDigest || manifest.FallbackReason == "" {
		t.Fatalf("manifest is not traceable: %#v", manifest)
	}

	_, err = BuildManifest(content, RenderRequest{
		Surface:         SurfaceChannel,
		RendererVersion: "plain-v1",
		Capabilities:    CapabilitySnapshot{},
		Parts:           []RenderPart{{Kind: PartText, Text: content.Markdown}},
	})
	if err == nil {
		t.Fatal("raw LaTeX on an unsupported surface must be rejected")
	}
}

func TestBuildRenderManifestRejectsLeakedLaTeXSpacingCommand(t *testing.T) {
	content, err := New(ProducerK12, "zh-CN", "$12 \\, \\mathrm{cm}$", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = BuildManifest(content, RenderRequest{
		Surface:         SurfaceChannel,
		RendererVersion: "channel-markdown-readable-math-v1",
		Capabilities: CapabilitySnapshot{
			Markdown:    true,
			TeXMath:     false,
			UnicodeMath: true,
		},
		Parts:          []RenderPart{{Kind: PartMarkdown, Text: "12 \\,cm"}},
		FallbackReason: FallbackMathToReadableText,
	})
	if err == nil || !strings.Contains(err.Error(), "raw LaTeX") {
		t.Fatalf("symbol-form spacing command must fail closed, got %v", err)
	}
}

func TestBuildRenderManifestAllowsLiteralLaTeXInsideMarkdownCodeFence(t *testing.T) {
	canonical := "## 解题步骤\n\n答案是 $\\frac{3}{4}$。\n\n```json\n{\"canonical_markdown\":\"$\\\\frac{3}{4}$\"}\n```"
	content, err := New(ProducerK12, "zh-CN", canonical, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	projected := "## 解题步骤\n\n答案是 3/4。\n\n```json\n{\"canonical_markdown\":\"$\\\\frac{3}{4}$\"}\n```"
	manifest, err := BuildManifest(content, RenderRequest{
		Surface:         SurfaceChannel,
		RendererVersion: "channel-markdown-readable-math-v1",
		Capabilities: CapabilitySnapshot{
			Markdown:    true,
			TeXMath:     false,
			UnicodeMath: true,
		},
		Parts:          []RenderPart{{Kind: PartMarkdown, Text: projected}},
		FallbackReason: FallbackMathToReadableText,
	})
	if err != nil {
		t.Fatalf("Markdown 代码围栏内的字面量 LaTeX 不应冒充正文泄漏: %v", err)
	}
	if manifest.Parts[0].Kind != PartMarkdown {
		t.Fatalf("解题消息必须保持 Markdown part: %#v", manifest.Parts)
	}
}

func TestManifestRejectsSourceMismatchAndEmptyProjection(t *testing.T) {
	content, err := New(ProducerWebhook, "en", "**accepted**", nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	manifest, err := BuildManifest(content, RenderRequest{
		Surface:         SurfaceDesktop,
		RendererVersion: "desktop-v1",
		Capabilities:    CapabilitySnapshot{Markdown: true, TeXMath: true},
		Parts:           []RenderPart{{Kind: PartMarkdown, Text: content.Markdown}},
	})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	manifest.SourceDigest = "sha256:wrong"
	if err := manifest.ValidateFor(content); err == nil {
		t.Fatal("manifest with a different source digest must fail")
	}

	_, err = BuildManifest(content, RenderRequest{
		Surface:         SurfaceDesktop,
		RendererVersion: "desktop-v1",
		Capabilities:    CapabilitySnapshot{Markdown: true},
	})
	if err == nil {
		t.Fatal("empty projection must fail visibly")
	}
}

func TestAttachmentsContributeToSourceDigest(t *testing.T) {
	a := []AttachmentRef{{AssetID: "asset-1", MIME: "image/png", Digest: "sha256:a", AltText: "作业照片"}}
	b := []AttachmentRef{{AssetID: "asset-2", MIME: "image/png", Digest: "sha256:b", AltText: "作业照片"}}
	one, err := New(ProducerK12, "zh-CN", "同一正文", a)
	if err != nil {
		t.Fatalf("New a: %v", err)
	}
	two, err := New(ProducerK12, "zh-CN", "同一正文", b)
	if err != nil {
		t.Fatalf("New b: %v", err)
	}
	if one.SourceDigest == two.SourceDigest {
		t.Fatal("attachment identity must participate in source_digest")
	}
}
