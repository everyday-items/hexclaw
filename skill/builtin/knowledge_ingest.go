package builtin

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/skill"
)

// KnowledgeIngestor is the write-path subset of knowledge.Manager
// (narrow interface for test injection).
type KnowledgeIngestor interface {
	AddDocument(ctx context.Context, title, content, source string) (*knowledge.Document, error)
}

// ingestTitleMaxRunes caps the title derived from content when none is given.
const ingestTitleMaxRunes = 24

// KnowledgeIngestSkill lets the Agent persist text into the local knowledge base.
//
// It is the final link of the "collect → organize → ingest" loop: without it,
// the Agent can only claim "saved to knowledge base" in its reply while nothing
// is actually persisted (LLM hallucinated success).
type KnowledgeIngestSkill struct {
	kb KnowledgeIngestor
}

func NewKnowledgeIngestSkill(kb KnowledgeIngestor) *KnowledgeIngestSkill {
	return &KnowledgeIngestSkill{kb: kb}
}

func (s *KnowledgeIngestSkill) Name() string { return "knowledge_ingest" }

func (s *KnowledgeIngestSkill) Description() string {
	return "Write text content into the local knowledge base for later retrieval"
}

// Match always defers to the LLM tool-calling path; no keyword fast path.
func (s *KnowledgeIngestSkill) Match(string) bool { return false }

func (s *KnowledgeIngestSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("knowledge_ingest",
		"Write text content into the local knowledge base (persisted and indexed for retrieval), "+
			"so it can be referenced in later conversations. You MUST call this tool whenever the user "+
			"asks to save/add/archive content into the knowledge base, or a scheduled task requires "+
			"archiving collected results — merely replying \"saved to knowledge base\" without calling "+
			"this tool persists nothing.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"title": {
					Type:        "string",
					Description: "Document title, a short summary of the topic, e.g. \"2026-06-11 tech news digest\"",
				},
				"content": {
					Type:        "string",
					Description: "Full text content to ingest (required)",
				},
				"source": {
					Type:        "string",
					Description: "Where the content originally came from: a URL or a document/file name. OMIT this field entirely when the content was produced in this conversation — do not invent source labels.",
				},
			},
			Required: []string{"title", "content"},
		})
}

func (s *KnowledgeIngestSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	if s.kb == nil {
		return nil, fmt.Errorf("knowledge base is not enabled; cannot ingest (enable Knowledge in settings)")
	}

	content := firstStringArg(args, "content", "text")
	if content == "" {
		return nil, fmt.Errorf("content must not be empty")
	}
	title := firstStringArg(args, "title")
	if title == "" {
		title = deriveIngestTitle(content)
	} else {
		// Collapse whitespace/newlines: titles are single-line labels.
		title = strings.Join(strings.Fields(title), " ")
	}
	source := firstStringArg(args, "source")
	if source == "" {
		source = "agent"
	}

	doc, err := s.kb.AddDocument(ctx, title, content, source)
	if err != nil {
		return nil, fmt.Errorf("failed to write to knowledge base: %w", err)
	}

	return &skill.Result{
		Content: fmt.Sprintf("Saved to knowledge base: \"%s\" (document ID: %s, source: %s)", doc.Title, doc.ID, source),
		Metadata: map[string]string{
			"doc_id": doc.ID,
			"source": source,
		},
	}, nil
}

// deriveIngestTitle derives a default title from the first line of content.
func deriveIngestTitle(content string) string {
	line := strings.TrimSpace(content)
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	if utf8.RuneCountInString(line) <= ingestTitleMaxRunes {
		return line
	}
	return string([]rune(line)[:ingestTitleMaxRunes])
}
