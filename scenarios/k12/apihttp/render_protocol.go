package apihttp

import "github.com/hexagon-codes/hexclaw/messagecontent"

// k12RenderProjection is the single K12 HTTP projection boundary. Domain
// facts stay structured; user-facing Markdown is wrapped in the same
// MessageContent/RenderManifest contract used by chat, history and channels.
func k12RenderProjection(producer messagecontent.ProducerKind, locale, markdown string) (*messagecontent.MessageContent, *messagecontent.RenderManifest) {
	content, err := messagecontent.New(producer, locale, markdown, nil)
	if err != nil {
		return nil, nil
	}
	manifest, err := messagecontent.BuildManifest(content, messagecontent.RenderRequest{
		Surface:         messagecontent.SurfaceK12,
		RendererVersion: "k12-markdown-v1",
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
