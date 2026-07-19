package dingtalk

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func ensureDingTalkRenderEvidence(reply *adapter.Reply) error {
	if reply == nil {
		return nil
	}
	if (reply.MessageContent == nil) != (reply.RenderManifest == nil) {
		return errors.New("dingtalk: MessageContent and RenderManifest must be supplied together")
	}
	if reply.MessageContent != nil {
		return reply.RenderManifest.ValidateFor(*reply.MessageContent)
	}

	// DingTalk rejects an empty sampleMarkdown body. Keep the canonical render
	// protocol fail-closed for genuinely empty content, but turn the platform's
	// established user-visible fallback into the canonical source before
	// building render evidence. This also keeps the manifest aligned with what
	// dingtalkMarkdownMessage ultimately sends instead of recording an empty
	// successful render.
	if strings.TrimSpace(reply.Content) == "" && len(reply.Attachments) == 0 {
		reply.Content = dingtalkEmptyReplyFallback
	}

	producer := messagecontent.ProducerChat
	locale := "und"
	if reply.Metadata != nil {
		if parsed, ok := messagecontent.ParseProducerKind(reply.Metadata["producer_kind"]); ok {
			producer = parsed
		}
		if value := strings.TrimSpace(reply.Metadata["locale"]); value != "" {
			locale = value
		}
	}

	refs := make([]messagecontent.AttachmentRef, 0, len(reply.Attachments))
	parts := make([]messagecontent.RenderPart, 0, len(reply.Attachments)+1)
	projected := adapter.NormalizeMathText(restoreEscapedMarkdownNewlines(reply.Content))
	parts = append(parts, messagecontent.RenderPart{Kind: messagecontent.PartMarkdown, Text: projected})
	for _, attachment := range reply.Attachments {
		identity := attachment.Data
		if identity == "" {
			identity = attachment.URL
		}
		sum := sha256.Sum256([]byte(identity))
		digest := "sha256:" + hex.EncodeToString(sum[:])
		assetID := "attachment:" + hex.EncodeToString(sum[:])
		mime := strings.TrimSpace(attachment.Mime)
		if mime == "" {
			mime = "application/octet-stream"
		}
		alt := strings.TrimSpace(attachment.Name)
		if alt == "" {
			alt = "attachment"
		}
		refs = append(refs, messagecontent.AttachmentRef{AssetID: assetID, Name: attachment.Name, MIME: mime, Digest: digest, AltText: alt})
		parts = append(parts, messagecontent.RenderPart{Kind: messagecontent.PartArtifact, ArtifactRef: assetID, ArtifactDigest: digest, AltText: alt})
	}

	content, err := messagecontent.New(producer, locale, reply.Content, refs)
	if err != nil {
		return err
	}
	fallback := ""
	if projected != reply.Content {
		fallback = messagecontent.FallbackMathToReadableText
	}
	manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceChannel,
		RendererVersion: "dingtalk-sample-markdown-v1",
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown:    true,
			TeXMath:     false,
			UnicodeMath: true,
			Attachments: len(refs) > 0,
		},
		Parts:          parts,
		FallbackReason: fallback,
	})
	if err != nil {
		return err
	}
	reply.MessageContent = &content
	reply.RenderManifest = &manifest
	return nil
}
