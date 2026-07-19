package api

import (
	"encoding/json"
	"strings"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/storage"
)

func canonicalChatContent(markdown string, metadata map[string]string) *messagecontent.MessageContent {
	if strings.TrimSpace(markdown) == "" {
		return nil
	}
	producer := messagecontent.ProducerChat
	if parsed, ok := messagecontent.ParseProducerKind(metadata["producer_kind"]); ok {
		producer = parsed
	}
	locale := strings.TrimSpace(metadata["locale"])
	if locale == "" {
		locale = "und"
	}
	content, err := messagecontent.New(producer, locale, markdown, nil)
	if err != nil {
		return nil
	}
	return &content
}

func canonicalRenderProjection(producer messagecontent.ProducerKind, locale, markdown string, surface messagecontent.Surface, rendererVersion string) (*messagecontent.MessageContent, *messagecontent.RenderManifest) {
	content, err := messagecontent.New(producer, locale, markdown, nil)
	if err != nil {
		return nil, nil
	}
	manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
		Surface:         surface,
		RendererVersion: rendererVersion,
		Capabilities: messagecontent.CapabilitySnapshot{
			Markdown: true,
			TeXMath:  true,
			MathML:   true,
		},
		Parts: []messagecontent.RenderPart{{Kind: messagecontent.PartMarkdown, Text: markdown}},
	})
	if err != nil {
		return nil, nil
	}
	return &content, &manifest
}

func hydrateMessageContents(messages []*storage.MessageRecord) {
	for _, message := range messages {
		if message == nil || strings.TrimSpace(message.Content) == "" {
			continue
		}
		if message.ContentType != "" && message.ContentType != "text" {
			continue
		}
		metadata := make(map[string]string)
		mergeStringMetadata(metadata, message.Metadata)
		mergeStringMetadata(metadata, message.Meta)
		if message.MessageContent == nil {
			message.MessageContent = canonicalChatContent(message.Content, metadata)
		}
		if message.MessageContent != nil && message.RenderManifest == nil {
			_, message.RenderManifest = canonicalRenderProjection(
				message.MessageContent.ProducerKind,
				message.MessageContent.Locale,
				message.MessageContent.Markdown,
				messagecontent.SurfaceHistory,
				"history-markdown-v1",
			)
		}
	}
}

func mergeStringMetadata(target map[string]string, raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	var values map[string]any
	if json.Unmarshal([]byte(raw), &values) != nil {
		return
	}
	for key, value := range values {
		if text, ok := value.(string); ok {
			target[key] = text
		}
	}
}
