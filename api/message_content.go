package api

import (
	"encoding/json"
	"strings"

	"github.com/hexagon-codes/hexclaw/adapter"
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
		hydrateMessageRuntimeWire(message)
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

func hydrateMessageRuntimeWire(message *storage.MessageRecord) {
	if message == nil || message.Role != "assistant" {
		return
	}
	var runtimeMeta struct {
		AssistantMessageID  string                          `json:"assistant_message_id"`
		BackendMessageID    string                          `json:"backend_message_id"`
		MessageID           string                          `json:"message_id"`
		ReasoningDisclosure adapter.ReasoningDisclosure     `json:"reasoning_disclosure"`
		RuntimeEvents       []adapter.SequencedRuntimeEvent `json:"runtime_events"`
		LastSequence        uint64                          `json:"last_sequence"`
	}
	raw := message.Metadata
	if raw == "" || raw == "{}" {
		raw = message.Meta
	}
	_ = json.Unmarshal([]byte(raw), &runtimeMeta)
	message.AssistantMessageID = runtimeMeta.AssistantMessageID
	if message.AssistantMessageID == "" {
		message.AssistantMessageID = message.ID
	}
	message.BackendMessageID = runtimeMeta.BackendMessageID
	if message.BackendMessageID == "" {
		message.BackendMessageID = message.AssistantMessageID
	}
	message.MessageID = runtimeMeta.MessageID
	if message.MessageID == "" {
		message.MessageID = message.AssistantMessageID
	}
	message.ReasoningDisclosure = runtimeMeta.ReasoningDisclosure
	if message.ReasoningDisclosure.Visibility == "" {
		message.ReasoningDisclosure.Visibility = adapter.ReasoningNotExposed
	}
	message.RuntimeEvents = runtimeMeta.RuntimeEvents
	if message.RuntimeEvents == nil {
		message.RuntimeEvents = []adapter.SequencedRuntimeEvent{}
	}
	message.LastSequence = runtimeMeta.LastSequence
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
