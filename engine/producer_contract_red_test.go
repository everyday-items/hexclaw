package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/skill"
)

type producerContractSkill struct{}

func (*producerContractSkill) Name() string        { return "producer_contract_skill" }
func (*producerContractSkill) Description() string { return "producer contract test skill" }
func (*producerContractSkill) Match(content string) bool {
	return strings.HasPrefix(content, "/producer-contract-skill")
}
func (*producerContractSkill) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: "skill contract answer"}, nil
}
func (*producerContractSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("producer_contract_skill", "producer contract test skill", nil)
}

func TestProducerContract_SyncAndStreamTopLevelPair(t *testing.T) {
	registry := skill.NewRegistry()
	if err := registry.Register(&producerContractSkill{}); err != nil {
		t.Fatalf("register skill: %v", err)
	}
	eng := newEngineWithProviderAndSkills(t, &usageLessStreamProvider{}, registry)

	tests := []struct {
		name       string
		wantKind   messagecontent.ProducerKind
		wantLocale string
		message    func(suffix string) *adapter.Message
	}{
		{
			name:       "chat defaults only when producer is missing",
			wantKind:   messagecontent.ProducerChat,
			wantLocale: "und",
			message: func(suffix string) *adapter.Message {
				return &adapter.Message{
					ID: "producer-chat-" + suffix, Platform: adapter.PlatformAPI,
					UserID: "producer-user", ChatID: "producer-chat-" + suffix,
					Content: "chat producer " + suffix,
				}
			},
		},
		{
			name:       "quick chat",
			wantKind:   messagecontent.ProducerQuickChat,
			wantLocale: "zh-CN",
			message: func(suffix string) *adapter.Message {
				return &adapter.Message{
					ID: "producer-quick-" + suffix, Platform: adapter.PlatformAPI,
					UserID: "producer-user", ChatID: "producer-quick-" + suffix,
					Content: "quick chat producer " + suffix,
					Metadata: map[string]string{
						"producer_kind": string(messagecontent.ProducerQuickChat),
						"locale":        "zh-CN",
					},
				}
			},
		},
		{
			name:       "cron",
			wantKind:   messagecontent.ProducerCron,
			wantLocale: "und",
			message: func(suffix string) *adapter.Message {
				msg := NewCronDispatchMessage("producer-user", "producer-cron-"+suffix, "job-"+suffix, "cron producer "+suffix)
				msg.ID = "producer-cron-" + suffix
				return msg
			},
		},
		{
			name:       "webhook derives producer from dispatch source",
			wantKind:   messagecontent.ProducerWebhook,
			wantLocale: "en-US",
			message: func(suffix string) *adapter.Message {
				return &adapter.Message{
					ID: "producer-webhook-" + suffix, Platform: adapter.PlatformAPI,
					UserID: "producer-user", ChatID: "producer-webhook-" + suffix,
					Content: "webhook producer " + suffix,
					Metadata: map[string]string{
						"source":     webhookDispatchSource,
						"webhook_id": "webhook-" + suffix,
						"locale":     "en-US",
					},
				}
			},
		},
		{
			name:       "workflow",
			wantKind:   messagecontent.ProducerWorkflow,
			wantLocale: "zh-CN",
			message: func(suffix string) *adapter.Message {
				return &adapter.Message{
					ID: "producer-workflow-" + suffix, Platform: adapter.PlatformAPI,
					UserID: "producer-user", ChatID: "producer-workflow-" + suffix,
					Content: "workflow producer " + suffix,
					Metadata: map[string]string{
						"source":        workflowDispatchSource,
						"workflow_id":   "workflow-" + suffix,
						"producer_kind": string(messagecontent.ProducerWorkflow),
						"locale":        "zh-CN",
					},
				}
			},
		},
		{
			name:       "skill",
			wantKind:   messagecontent.ProducerSkill,
			wantLocale: "zh-CN",
			message: func(suffix string) *adapter.Message {
				return &adapter.Message{
					ID: "producer-skill-" + suffix, Platform: adapter.PlatformAPI,
					UserID: "producer-user", ChatID: "producer-skill-" + suffix,
					Content:  "/producer-contract-skill " + suffix,
					Metadata: map[string]string{"user_locale": "zh-CN"},
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name+"/sync", func(t *testing.T) {
			reply, err := eng.Process(context.Background(), tt.message("sync"))
			if err != nil {
				t.Fatalf("Process: %v", err)
			}
			assertProducerContractPair(t, reply.MessageContent, reply.RenderManifest, tt.wantKind, tt.wantLocale, reply.Content)
		})

		t.Run(tt.name+"/stream", func(t *testing.T) {
			chunks, err := eng.ProcessStream(context.Background(), tt.message("stream"))
			if err != nil {
				t.Fatalf("ProcessStream: %v", err)
			}
			var complete strings.Builder
			var terminal *adapter.ReplyChunk
			for chunk := range chunks {
				complete.WriteString(chunk.Content)
				if chunk.Done {
					terminal = chunk
				}
			}
			if terminal == nil {
				t.Fatal("stream terminal chunk is required")
			}
			if terminal.Error != nil {
				t.Fatalf("stream terminal error: %v", terminal.Error)
			}
			assertProducerContractPair(
				t,
				terminal.MessageContent,
				terminal.RenderManifest,
				tt.wantKind,
				tt.wantLocale,
				complete.String(),
			)
		})
	}
}

func TestProducerContract_BuildReplyMetadataPreservesProducerAndLocale(t *testing.T) {
	metadata := buildReplyMetadata(map[string]string{
		"producer_kind": string(messagecontent.ProducerWebhook),
		"locale":        "zh-CN",
	}, "provider", "model", "assistant-id")
	if got := metadata["producer_kind"]; got != string(messagecontent.ProducerWebhook) {
		t.Fatalf("producer_kind = %q, want %q", got, messagecontent.ProducerWebhook)
	}
	if got := metadata["locale"]; got != "zh-CN" {
		t.Fatalf("locale = %q, want zh-CN", got)
	}

	defaults := buildReplyMetadata(nil, "provider", "model", "assistant-id")
	if got := defaults["producer_kind"]; got != string(messagecontent.ProducerChat) {
		t.Fatalf("default producer_kind = %q, want %q", got, messagecontent.ProducerChat)
	}
	if got := defaults["locale"]; got != "und" {
		t.Fatalf("default locale = %q, want und", got)
	}
}

func TestProducerContract_SyncAttachmentUsesSameCanonicalRefsAndDigest(t *testing.T) {
	raw := []byte("legal image bytes")
	digestBytes := sha256.Sum256(raw)
	wantDigest := "sha256:" + hex.EncodeToString(digestBytes[:])
	reply := &adapter.Reply{
		Content: "image answer",
		Attachments: []adapter.Attachment{{
			Type: "image",
			Name: "answer.png",
			Mime: "image/png",
			Data: base64.StdEncoding.EncodeToString(raw),
		}},
	}

	if err := finalizeProducerReply(reply, map[string]string{
		"producer_kind": string(messagecontent.ProducerChat),
		"locale":        "zh-CN",
	}); err != nil {
		t.Fatalf("finalize producer reply: %v", err)
	}
	assertProducerContractPair(t, reply.MessageContent, reply.RenderManifest, messagecontent.ProducerChat, "zh-CN", reply.Content)
	if len(reply.MessageContent.Attachments) != 1 {
		t.Fatalf("canonical attachment refs = %d, want 1", len(reply.MessageContent.Attachments))
	}
	ref := reply.MessageContent.Attachments[0]
	if ref.Digest != wantDigest {
		t.Fatalf("attachment digest = %q, want %q", ref.Digest, wantDigest)
	}
	if ref.AssetID != "attachment:"+strings.TrimPrefix(wantDigest, "sha256:") {
		t.Fatalf("attachment asset_id = %q", ref.AssetID)
	}
	if ref.Name != "answer.png" || ref.MIME != "image/png" || ref.AltText != "answer.png" {
		t.Fatalf("canonical attachment ref mismatch: %+v", ref)
	}
	if !reply.RenderManifest.CapabilitySnapshot.Attachments {
		t.Fatal("attachment capability must be recorded")
	}
	var artifact *messagecontent.RenderPart
	for i := range reply.RenderManifest.Parts {
		if reply.RenderManifest.Parts[i].Kind == messagecontent.PartArtifact {
			artifact = &reply.RenderManifest.Parts[i]
			break
		}
	}
	if artifact == nil {
		t.Fatal("attachment artifact render part is required")
	}
	if artifact.ArtifactRef != ref.AssetID || artifact.ArtifactDigest != ref.Digest || artifact.AltText != ref.AltText {
		t.Fatalf("artifact part and canonical ref differ: artifact=%+v ref=%+v", *artifact, ref)
	}
}

func TestProducerContract_NestedToolAndRAGDoNotPromoteTopLevelProducer(t *testing.T) {
	toolContent, err := messagecontent.New(messagecontent.ProducerTool, "und", "tool output", nil)
	if err != nil {
		t.Fatalf("tool content: %v", err)
	}
	ragContent, err := messagecontent.New(messagecontent.ProducerRAG, "und", "rag hit", nil)
	if err != nil {
		t.Fatalf("rag content: %v", err)
	}
	reply := &adapter.Reply{
		Content: "top-level chat answer",
		ToolCalls: []adapter.ToolCall{{
			ID: "tool-1", Name: "lookup", MessageContent: &toolContent,
		}},
		KnowledgeHits: []adapter.KnowledgeHit{{
			DocTitle: "doc", Content: "rag hit", MessageContent: &ragContent,
		}},
	}

	if err := finalizeProducerReply(reply, nil); err != nil {
		t.Fatalf("finalize producer reply: %v", err)
	}
	assertProducerContractPair(t, reply.MessageContent, reply.RenderManifest, messagecontent.ProducerChat, "und", reply.Content)
	if reply.ToolCalls[0].MessageContent.ProducerKind != messagecontent.ProducerTool {
		t.Fatalf("nested tool producer changed: %q", reply.ToolCalls[0].MessageContent.ProducerKind)
	}
	if reply.KnowledgeHits[0].MessageContent.ProducerKind != messagecontent.ProducerRAG {
		t.Fatalf("nested RAG producer changed: %q", reply.KnowledgeHits[0].MessageContent.ProducerKind)
	}
}

func TestProducerContract_ToolRAGAndReportAreNotTopLevelRoutes(t *testing.T) {
	for _, producer := range []messagecontent.ProducerKind{
		messagecontent.ProducerTool,
		messagecontent.ProducerRAG,
		messagecontent.ProducerReport,
	} {
		t.Run(string(producer), func(t *testing.T) {
			_, _, err := resolveProducerContract(map[string]string{"producer_kind": string(producer)})
			if err == nil {
				t.Fatalf("top-level producer %q must be rejected", producer)
			}
		})
	}
}

func TestProducerContract_StreamTerminalPreservesCanonicalAttachmentRefs(t *testing.T) {
	ref := messagecontent.AttachmentRef{
		AssetID: "attachment:stream-image",
		Name:    "stream.png",
		MIME:    "image/png",
		Digest:  "sha256:stream-image",
		AltText: "stream.png",
	}
	existing, err := messagecontent.New(messagecontent.ProducerQuickChat, "zh-CN", "partial", []messagecontent.AttachmentRef{ref})
	if err != nil {
		t.Fatalf("existing stream content: %v", err)
	}
	terminal := &adapter.ReplyChunk{
		Done:           true,
		MessageContent: &existing,
		Metadata: map[string]string{
			"producer_kind": string(messagecontent.ProducerQuickChat),
			"locale":        "zh-CN",
		},
	}

	if err := finalizeProducerChunk(terminal, "complete stream answer", nil); err != nil {
		t.Fatalf("finalize producer chunk: %v", err)
	}
	assertProducerContractPair(t, terminal.MessageContent, terminal.RenderManifest, messagecontent.ProducerQuickChat, "zh-CN", "complete stream answer")
	if len(terminal.MessageContent.Attachments) != 1 || terminal.MessageContent.Attachments[0] != ref {
		t.Fatalf("stream attachment refs changed: %+v", terminal.MessageContent.Attachments)
	}
	if !terminal.RenderManifest.CapabilitySnapshot.Attachments {
		t.Fatal("stream attachment capability must be recorded")
	}
	if len(terminal.RenderManifest.Parts) != 2 || terminal.RenderManifest.Parts[1].Kind != messagecontent.PartArtifact {
		t.Fatalf("stream artifact projection mismatch: %+v", terminal.RenderManifest.Parts)
	}
	artifact := terminal.RenderManifest.Parts[1]
	if artifact.ArtifactRef != ref.AssetID || artifact.ArtifactDigest != ref.Digest || artifact.AltText != ref.AltText {
		t.Fatalf("stream artifact and canonical ref differ: artifact=%+v ref=%+v", artifact, ref)
	}
}

func assertProducerContractPair(
	t *testing.T,
	content *messagecontent.MessageContent,
	manifest *messagecontent.RenderManifest,
	wantKind messagecontent.ProducerKind,
	wantLocale, wantMarkdown string,
) {
	t.Helper()
	if content == nil || manifest == nil {
		t.Fatalf("canonical pair is required: content=%v manifest=%v", content != nil, manifest != nil)
	}
	if content.ProducerKind != wantKind {
		t.Fatalf("producer_kind = %q, want %q", content.ProducerKind, wantKind)
	}
	if content.Locale != wantLocale {
		t.Fatalf("locale = %q, want %q", content.Locale, wantLocale)
	}
	if content.Markdown != wantMarkdown {
		t.Fatalf("markdown = %q, want %q", content.Markdown, wantMarkdown)
	}
	if err := manifest.ValidateFor(*content); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}
}
