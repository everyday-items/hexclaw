package dingtalk

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func preparedEnvelopeWithCanonicalAlt(t *testing.T, fileName, altText string) adapter.PreparedEnvelope {
	t.Helper()
	const markdown = "## 作品点评\n\n请查看原图。"
	attachment := adapter.Attachment{
		Type: "image",
		Name: fileName,
		Mime: "image/png",
		Data: base64.StdEncoding.EncodeToString(testPNGBytes(t)),
	}
	digest, assetID, err := dingTalkAttachmentIdentity(attachment)
	if err != nil {
		t.Fatalf("构造附件身份: %v", err)
	}
	content, err := messagecontent.New(
		messagecontent.ProducerK12,
		"zh-CN",
		markdown,
		[]messagecontent.AttachmentRef{{
			AssetID: assetID,
			Name:    fileName,
			MIME:    attachment.Mime,
			Digest:  digest,
			AltText: altText,
		}},
	)
	if err != nil {
		t.Fatalf("构造 canonical content: %v", err)
	}
	manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
		Surface: messagecontent.SurfaceChannel,
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown:    true,
			Attachments: true,
		},
		RendererVersion: "dingtalk-sample-markdown-v1",
		Parts: []messagecontent.RenderPart{
			{Kind: messagecontent.PartMarkdown, Text: markdown},
			{
				Kind:           messagecontent.PartArtifact,
				ArtifactRef:    assetID,
				ArtifactDigest: digest,
				AltText:        altText,
			},
		},
	})
	if err != nil {
		t.Fatalf("构造 render manifest: %v", err)
	}
	markdownSum := sha256.Sum256([]byte(markdown))
	return adapter.PreparedEnvelope{Parts: []adapter.DeliveryPart{
		{
			Kind:           messagecontent.PartMarkdown,
			Ordinal:        1,
			Digest:         "sha256:" + hex.EncodeToString(markdownSum[:]),
			Text:           markdown,
			MessageContent: &content,
			RenderManifest: &manifest,
		},
		{
			Kind:               messagecontent.PartArtifact,
			MIME:               attachment.Mime,
			Ordinal:            2,
			Digest:             digest,
			Attachment:         &attachment,
			MessageContent:     &content,
			RenderManifest:     &manifest,
			PreparedResourceID: "@prepared-image",
		},
	}}
}

func TestDingTalkPreparedEnvelopeUsesCanonicalManifestAltText(t *testing.T) {
	tests := []struct {
		name    string
		altText string
		wantAlt string
	}{
		{name: "canonical alt", altText: "作品原图", wantAlt: "作品原图"},
		{name: "blank canonical alt", altText: "   ", wantAlt: "作品原图"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newFakeDingtalkOpenAPI("bound-instance-token")
			client := newDirectReceiptTestAdapter(t)
			client.openAPI = provider

			ack, err := client.SendPreparedEnvelopeWithReceipt(
				context.Background(),
				"parent-user",
				preparedEnvelopeWithCanonicalAlt(t, "upload.png", test.altText),
			)
			if err != nil {
				t.Fatalf("send prepared envelope: %v", err)
			}
			if ack.Status != adapter.DeliveryAccepted || ack.ExternalMessageID == "" {
				t.Fatalf("ack=%+v, want accepted with processQueryKey", ack)
			}
			calls := provider.SendCalls()
			if len(calls) != 1 {
				t.Fatalf("provider send calls=%d, want 1", len(calls))
			}
			if !strings.Contains(calls[0].Text, "!["+test.wantAlt+"](@prepared-image)") {
				t.Fatalf("message=%q, want canonical image alt %q", calls[0].Text, test.wantAlt)
			}
			if strings.Contains(calls[0].Text, "![upload.png]") {
				t.Fatalf("message leaks upload filename instead of canonical alt: %q", calls[0].Text)
			}
		})
	}
}

func TestDingTalkPreparedEnvelopeRejectsUnsafeCanonicalAltBeforeProviderBoundary(t *testing.T) {
	provider := newFakeDingtalkOpenAPI("bound-instance-token")
	client := newDirectReceiptTestAdapter(t)
	client.openAPI = provider

	ack, err := client.SendPreparedEnvelopeWithReceipt(
		context.Background(),
		"parent-user",
		preparedEnvelopeWithCanonicalAlt(
			t,
			"upload.png",
			"作品原图]\n![](@injected-image)",
		),
	)

	if err == nil {
		t.Fatalf("unsafe canonical alt must fail before provider boundary: ack=%+v", ack)
	}
	if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
		t.Fatalf("unsafe canonical alt ack=%+v, want failed without external id", ack)
	}
	if calls := provider.TokenCalls(); calls != 0 {
		t.Fatalf("token calls=%d, want 0", calls)
	}
	if calls := provider.SendCalls(); len(calls) != 0 {
		t.Fatalf("provider send must not start for unsafe canonical alt: %#v", calls)
	}
}
