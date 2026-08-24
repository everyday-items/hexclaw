package engine

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func canonicalProducerContent(producer messagecontent.ProducerKind, markdown, locale string) *messagecontent.MessageContent {
	if strings.TrimSpace(markdown) == "" {
		return nil
	}
	if strings.TrimSpace(locale) == "" {
		locale = "und"
	}
	content, err := messagecontent.New(producer, locale, markdown, nil)
	if err != nil {
		return nil
	}
	return &content
}

func resolveProducerContract(metadata ...map[string]string) (messagecontent.ProducerKind, string, error) {
	producer := messagecontent.ProducerChat
	producerFound := false
	for _, values := range metadata {
		raw := strings.TrimSpace(values["producer_kind"])
		if raw == "" {
			continue
		}
		parsed, ok := messagecontent.ParseProducerKind(raw)
		if !ok {
			return "", "", fmt.Errorf("invalid producer_kind %q", raw)
		}
		producer = parsed
		producerFound = true
		break
	}
	if producerFound {
		switch producer {
		case messagecontent.ProducerTool, messagecontent.ProducerRAG, messagecontent.ProducerReport:
			return "", "", fmt.Errorf("top-level producer_kind %q is unsupported", producer)
		}
	}
	if !producerFound {
		for _, values := range metadata {
			switch strings.TrimSpace(values["source"]) {
			case cronDispatchSource:
				producer = messagecontent.ProducerCron
				producerFound = true
			case webhookDispatchSource:
				producer = messagecontent.ProducerWebhook
				producerFound = true
			case workflowDispatchSource:
				producer = messagecontent.ProducerWorkflow
				producerFound = true
			}
			if producerFound {
				break
			}
		}
	}

	locale := ""
	for _, values := range metadata {
		if value := strings.TrimSpace(values["locale"]); value != "" {
			locale = value
			break
		}
	}
	if locale == "" {
		for _, values := range metadata {
			if value := strings.TrimSpace(values["user_locale"]); value != "" {
				locale = value
				break
			}
		}
	}
	if locale == "" {
		locale = "und"
	}
	return producer, locale, nil
}

func canonicalAttachmentRefs(attachments []adapter.Attachment) []messagecontent.AttachmentRef {
	if len(attachments) == 0 {
		return nil
	}
	refs := make([]messagecontent.AttachmentRef, 0, len(attachments))
	for _, attachment := range attachments {
		payload := canonicalAttachmentPayload(attachment)
		sum := sha256.Sum256(payload)
		hexDigest := hex.EncodeToString(sum[:])
		name := strings.TrimSpace(attachment.Name)
		if name == "" {
			name = "attachment"
		}
		mime := strings.TrimSpace(attachment.Mime)
		if mime == "" {
			mime = "application/octet-stream"
		}
		refs = append(refs, messagecontent.AttachmentRef{
			AssetID: "attachment:" + hexDigest,
			Name:    name,
			MIME:    mime,
			Digest:  "sha256:" + hexDigest,
			AltText: name,
		})
	}
	return refs
}

// canonicalAttachmentPayload 只为合法字节生成物理摘要；无字节来源保留稳定身份，
// URL、路径与非法编码仍由具体出站适配器按各自边界拒绝。
func canonicalAttachmentPayload(attachment adapter.Attachment) []byte {
	encoded := strings.TrimSpace(attachment.Data)
	if encoded != "" {
		if raw, err := base64.StdEncoding.DecodeString(encoded); err == nil && len(raw) > 0 {
			return raw
		}
		return []byte("data:" + encoded)
	}
	if value := strings.TrimSpace(attachment.URL); value != "" {
		return []byte("url:" + value)
	}
	if value := strings.TrimSpace(attachment.ID); value != "" {
		return []byte("id:" + value)
	}
	return []byte("attachment:missing")
}

func canonicalProducerProjection(
	producer messagecontent.ProducerKind,
	markdown, locale string,
	attachments []messagecontent.AttachmentRef,
) (*messagecontent.MessageContent, *messagecontent.RenderManifest, error) {
	content, err := messagecontent.New(producer, locale, markdown, attachments)
	if err != nil {
		return nil, nil, fmt.Errorf("build canonical message content: %w", err)
	}
	parts := make([]messagecontent.RenderPart, 0, len(attachments)+1)
	if strings.TrimSpace(markdown) != "" {
		parts = append(parts, messagecontent.RenderPart{Kind: messagecontent.PartMarkdown, Text: markdown})
	}
	for _, attachment := range attachments {
		parts = append(parts, messagecontent.RenderPart{
			Kind:           messagecontent.PartArtifact,
			ArtifactRef:    attachment.AssetID,
			ArtifactDigest: attachment.Digest,
			AltText:        attachment.AltText,
		})
	}
	manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceDesktop,
		RendererVersion: "engine-markdown-v1",
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown:    true,
			TeXMath:     true,
			MathML:      true,
			Attachments: len(attachments) > 0,
		},
		Parts: parts,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build canonical render manifest: %w", err)
	}
	return &content, &manifest, nil
}

// finalizeProducerReply 是同步回复的唯一顶层生产者协议收口点。
func finalizeProducerReply(reply *adapter.Reply, requestMetadata map[string]string) error {
	if reply == nil {
		return nil
	}
	producer, locale, err := resolveProducerContract(reply.Metadata, requestMetadata)
	if err != nil {
		return err
	}
	content, manifest, err := canonicalProducerProjection(
		producer,
		reply.Content,
		locale,
		canonicalAttachmentRefs(reply.Attachments),
	)
	if err != nil {
		return err
	}
	reply.Metadata = withProducerMetadata(reply.Metadata, producer, locale)
	reply.MessageContent = content
	reply.RenderManifest = manifest
	return nil
}

// finalizeProducerChunk 是流式终态的唯一顶层生产者协议收口点。
func finalizeProducerChunk(chunk *adapter.ReplyChunk, markdown string, requestMetadata map[string]string) error {
	if chunk == nil {
		return nil
	}
	existingMetadata := map[string]string(nil)
	var attachments []messagecontent.AttachmentRef
	if chunk.MessageContent != nil {
		existingMetadata = map[string]string{
			"producer_kind": string(chunk.MessageContent.ProducerKind),
			"locale":        chunk.MessageContent.Locale,
		}
		attachments = append([]messagecontent.AttachmentRef(nil), chunk.MessageContent.Attachments...)
	}
	producer, locale, err := resolveProducerContract(chunk.Metadata, requestMetadata, existingMetadata)
	if err != nil {
		return err
	}
	content, manifest, err := canonicalProducerProjection(producer, markdown, locale, attachments)
	if err != nil {
		return err
	}
	chunk.Metadata = withProducerMetadata(chunk.Metadata, producer, locale)
	chunk.MessageContent = content
	chunk.RenderManifest = manifest
	return nil
}

func withProducerMetadata(metadata map[string]string, producer messagecontent.ProducerKind, locale string) map[string]string {
	result := make(map[string]string, len(metadata)+2)
	for key, value := range metadata {
		result[key] = value
	}
	result["producer_kind"] = string(producer)
	if strings.TrimSpace(locale) == "" {
		locale = "und"
	}
	result["locale"] = locale
	return result
}
