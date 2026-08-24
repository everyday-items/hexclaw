package engine

import (
	"strings"

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

func canonicalProducerProjection(producer messagecontent.ProducerKind, markdown, locale string) (*messagecontent.MessageContent, *messagecontent.RenderManifest) {
	content := canonicalProducerContent(producer, markdown, locale)
	if content == nil {
		return nil, nil
	}
	manifest, err := messagecontent.BuildManifest(*content, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceDesktop,
		RendererVersion: "engine-markdown-v1",
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
	return content, &manifest
}

func withProducerMetadata(metadata map[string]string, producer messagecontent.ProducerKind, locale string) map[string]string {
	result := make(map[string]string, len(metadata)+2)
	for key, value := range metadata {
		result[key] = value
	}
	result["producer_kind"] = string(producer)
	if strings.TrimSpace(locale) != "" {
		result["locale"] = locale
	}
	return result
}
