package dingtalk

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"

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
		if err := reply.MessageContent.Validate(); err != nil {
			return err
		}
		if err := reply.RenderManifest.ValidateFor(*reply.MessageContent); err != nil {
			return err
		}
		if err := validateDingTalkAttachmentProjection(
			*reply.MessageContent,
			*reply.RenderManifest,
			reply.Attachments,
		); err != nil {
			return err
		}
		if dingTalkCompatibleManifest(*reply.RenderManifest) {
			visible, err := dingTalkManifestMarkdown(*reply.RenderManifest)
			if err != nil {
				return err
			}
			return validateDingTalkVisibleContent(visible)
		}
	}

	// 钉钉会拒绝空的 sampleMarkdown 正文；先把既有可见兜底写回规范内容，
	// 确保渲染清单与最终发送内容一致。
	if strings.TrimSpace(reply.Content) == "" && len(reply.Attachments) == 0 {
		reply.Content = dingtalkEmptyReplyFallback
	}
	producer := messagecontent.ProducerChat
	locale := "und"
	if reply.MessageContent != nil {
		producer = reply.MessageContent.ProducerKind
		locale = reply.MessageContent.Locale
	} else if reply.Metadata != nil {
		if parsed, ok := messagecontent.ParseProducerKind(reply.Metadata["producer_kind"]); ok {
			producer = parsed
		}
		if value := strings.TrimSpace(reply.Metadata["locale"]); value != "" {
			locale = value
		}
	}

	sourceMarkdown := reply.Content
	if reply.MessageContent != nil {
		sourceMarkdown = reply.MessageContent.Markdown
	}
	refs := make([]messagecontent.AttachmentRef, 0, len(reply.Attachments))
	parts := make([]messagecontent.RenderPart, 0, len(reply.Attachments)+1)
	projected := adapter.NormalizeMathText(restoreEscapedMarkdownNewlines(sourceMarkdown))
	if err := validateDingTalkVisibleContent(projected); err != nil {
		return err
	}
	parts = append(parts, messagecontent.RenderPart{Kind: messagecontent.PartMarkdown, Text: projected})
	for _, attachment := range reply.Attachments {
		if !adapter.IsImageAttachment(attachment) {
			return errors.New("DingTalk only supports image attachments")
		}
		digest, assetID, err := dingTalkAttachmentIdentity(attachment)
		if err != nil {
			return err
		}
		mime := strings.TrimSpace(attachment.Mime)
		if mime == "" {
			mime = "application/octet-stream"
		}
		alt := safeDingTalkAttachmentName(attachment.Name)
		refs = append(refs, messagecontent.AttachmentRef{AssetID: assetID, Name: alt, MIME: mime, Digest: digest, AltText: alt})
		parts = append(parts, messagecontent.RenderPart{Kind: messagecontent.PartArtifact, ArtifactRef: assetID, ArtifactDigest: digest, AltText: alt})
	}

	content, err := messagecontent.New(producer, locale, sourceMarkdown, refs)
	if err != nil {
		return err
	}
	fallback := ""
	if projected != sourceMarkdown {
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

func dingTalkManifestMarkdown(manifest messagecontent.RenderManifest) (string, error) {
	var visible string
	count := 0
	for _, part := range manifest.Parts {
		if part.Kind != messagecontent.PartMarkdown {
			continue
		}
		visible = part.Text
		count++
	}
	if count != 1 || strings.TrimSpace(visible) == "" {
		return "", errors.New("dingtalk: render manifest must contain exactly one visible markdown part")
	}
	return visible, nil
}

func validateDingTalkAttachmentProjection(
	content messagecontent.MessageContent,
	manifest messagecontent.RenderManifest,
	attachments []adapter.Attachment,
) error {
	if len(content.Attachments) != len(attachments) {
		return errors.New("dingtalk: canonical attachments do not match reply attachments")
	}
	if manifest.CapabilitySnapshot.Attachments != (len(attachments) > 0) {
		return errors.New("dingtalk: attachment capability does not match reply attachments")
	}
	artifacts := make([]messagecontent.RenderPart, 0, len(attachments))
	for _, part := range manifest.Parts {
		if part.Kind == messagecontent.PartArtifact {
			artifacts = append(artifacts, part)
		}
	}
	if len(artifacts) != len(attachments) {
		return errors.New("dingtalk: render manifest artifact parts do not match reply attachments")
	}
	for i, attachment := range attachments {
		if !adapter.IsImageAttachment(attachment) {
			return errors.New("DingTalk only supports image attachments")
		}
		digest, _, err := dingTalkAttachmentIdentity(attachment)
		if err != nil {
			return err
		}
		mime := strings.TrimSpace(attachment.Mime)
		if mime == "" {
			mime = "application/octet-stream"
		}
		name := safeDingTalkAttachmentName(attachment.Name)
		ref := content.Attachments[i]
		if ref.Digest != digest || ref.MIME != mime || ref.Name != name || strings.TrimSpace(ref.AssetID) == "" {
			return fmt.Errorf("dingtalk: canonical attachment %d does not match reply bytes", i)
		}
		part := artifacts[i]
		if part.ArtifactRef != ref.AssetID || part.ArtifactDigest != ref.Digest || part.AltText != ref.AltText {
			return fmt.Errorf("dingtalk: render artifact %d does not match canonical attachment", i)
		}
	}
	return nil
}

func dingTalkAttachmentIdentity(attachment adapter.Attachment) (string, string, error) {
	encoded := strings.TrimSpace(attachment.Data)
	if comma := strings.IndexByte(encoded, ','); strings.HasPrefix(strings.ToLower(encoded), "data:") && comma >= 0 {
		encoded = encoded[comma+1:]
	}
	if encoded == "" {
		return "", "", errors.New("DingTalk image attachment bytes are required")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 {
		return "", "", errors.New("DingTalk image attachment bytes are invalid")
	}
	sum := sha256.Sum256(raw)
	hexDigest := hex.EncodeToString(sum[:])
	return "sha256:" + hexDigest, "attachment:" + hexDigest, nil
}

func dingTalkCompatibleManifest(manifest messagecontent.RenderManifest) bool {
	if manifest.Surface != messagecontent.SurfaceChannel ||
		!manifest.CapabilitySnapshot.Markdown ||
		manifest.CapabilitySnapshot.TeXMath {
		return false
	}
	for _, part := range manifest.Parts {
		if part.Kind == messagecontent.PartMarkdown {
			return true
		}
	}
	return false
}

func validateDingTalkVisibleContent(content string) error {
	lower := strings.ToLower(content)
	for _, marker := range []string{"asset://", "file://", "blob:", "data:", "/api/k12/assets/"} {
		if strings.Contains(lower, marker) {
			return errors.New("DingTalk visible content contains an internal asset reference")
		}
	}
	if containsDingTalkLocalPath(lower) {
		return errors.New("DingTalk visible content contains a local file path")
	}
	return nil
}

func containsDingTalkLocalPath(content string) bool {
	runes := []rune(content)
	for i, value := range runes {
		boundary := i == 0 || dingTalkReferenceBoundary(runes[i-1])
		if boundary && i+2 < len(runes) && value == '\\' && runes[i+1] == '\\' && !unicode.IsSpace(runes[i+2]) {
			return true
		}
		if boundary && i+1 < len(runes) && value == '/' && runes[i+1] != '/' && !unicode.IsSpace(runes[i+1]) {
			return true
		}
		if i+2 >= len(runes) {
			continue
		}
		if ((value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')) &&
			runes[i+1] == ':' && (runes[i+2] == '\\' || runes[i+2] == '/') &&
			boundary {
			return true
		}
	}
	return false
}

func dingTalkReferenceBoundary(value rune) bool {
	return unicode.IsSpace(value) || strings.ContainsRune("\"'([{<=:，。；、：", value)
}
