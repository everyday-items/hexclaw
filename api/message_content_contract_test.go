package api

import (
	"encoding/json"
	"testing"

	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/storage"
)

func TestChatResponseCarriesCanonicalMessageContent(t *testing.T) {
	content := canonicalChatContent("$\\frac{3}{4}$", map[string]string{
		"producer_kind": "quick_chat",
		"locale":        "zh-CN",
	})
	if content == nil || content.ProducerKind != messagecontent.ProducerQuickChat {
		t.Fatalf("canonical content = %#v", content)
	}
	_, manifest := canonicalRenderProjection(content.ProducerKind, content.Locale, content.Markdown, messagecontent.SurfaceDesktop, "desktop-v1")
	payload, err := json.Marshal(ChatResponse{Reply: content.Markdown, SessionID: "s1", MessageContent: content, RenderManifest: manifest})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := got["message_content"]; !ok {
		t.Fatalf("response omitted message_content: %s", payload)
	}
	if _, ok := got["render_manifest"]; !ok {
		t.Fatalf("response omitted render_manifest: %s", payload)
	}
}

func TestHydrateHistoryMessageContentKeepsLegacyStorageReadable(t *testing.T) {
	messages := []*storage.MessageRecord{{
		ID:          "m1",
		Role:        "assistant",
		Content:     "## answer",
		ContentType: "text",
		Metadata:    `{"producer_kind":"rag","locale":"en"}`,
	}}
	hydrateMessageContents(messages)
	if messages[0].MessageContent == nil {
		t.Fatal("text history must be hydrated with canonical content")
	}
	if messages[0].MessageContent.ProducerKind != messagecontent.ProducerRAG {
		t.Fatalf("producer = %q", messages[0].MessageContent.ProducerKind)
	}
	if err := messages[0].MessageContent.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if messages[0].RenderManifest == nil {
		t.Fatal("text history must carry a traceable RenderManifest")
	}
	if messages[0].RenderManifest.Surface != messagecontent.SurfaceHistory {
		t.Fatalf("history surface = %q", messages[0].RenderManifest.Surface)
	}
	if err := messages[0].RenderManifest.ValidateFor(*messages[0].MessageContent); err != nil {
		t.Fatalf("manifest ValidateFor: %v", err)
	}
}

func TestHydrateHistorySkipsOpaqueMultimodalJSON(t *testing.T) {
	messages := []*storage.MessageRecord{{
		ID:          "m2",
		Content:     `[{"type":"image"}]`,
		ContentType: "multimodal_json",
	}}
	hydrateMessageContents(messages)
	if messages[0].MessageContent != nil {
		t.Fatal("opaque multimodal JSON must remain on the legacy compatibility path")
	}
}
