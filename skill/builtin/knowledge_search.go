package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/skill"
)

// KnowledgeSearcher is the read-path subset of knowledge.Manager
// (narrow interface for test injection).
type KnowledgeSearcher interface {
	SearchWithFilter(ctx context.Context, query string, topK int, filter knowledge.Filter) ([]knowledge.SearchHit, error)
}

const (
	knowledgeSearchDefaultTopK = 5
	knowledgeSearchMaxTopK     = 20
)

// KnowledgeSearchSkill lets the Agent explicitly retrieve from the local
// knowledge base with optional metadata filters (source / source_type / date).
//
// Distinct from the engine's automatic RAG injection (which retrieves the whole
// KB for every turn): this is an opt-in tool the LLM calls when it wants
// *scoped* recall — e.g. "only my uploaded PDFs", "only what I saved last week".
type KnowledgeSearchSkill struct {
	kb KnowledgeSearcher
}

func NewKnowledgeSearchSkill(kb KnowledgeSearcher) *KnowledgeSearchSkill {
	return &KnowledgeSearchSkill{kb: kb}
}

func (s *KnowledgeSearchSkill) Name() string { return "knowledge_search" }

func (s *KnowledgeSearchSkill) Description() string {
	return "Search the local knowledge base, optionally filtered by source, source type, or creation date"
}

// Match always defers to the LLM tool-calling path; no keyword fast path.
func (s *KnowledgeSearchSkill) Match(string) bool { return false }

func (s *KnowledgeSearchSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("knowledge_search",
		"Search the user's local knowledge base and return the most relevant passages. "+
			"Use this for SCOPED retrieval when the user constrains by where it came from or when it was added "+
			"(e.g. \"search only my uploaded files\", \"what did I save about X last week\"). "+
			"Optional filters narrow results: source_types, sources, created_after/created_before. "+
			"Omit all filters to search the whole knowledge base.",
		&llm.Schema{
			Type: "object",
			Properties: map[string]*llm.Schema{
				"query": {
					Type:        "string",
					Description: "The search query / question (required).",
				},
				"top_k": {
					Type:        "integer",
					Description: fmt.Sprintf("Max passages to return (default %d, max %d).", knowledgeSearchDefaultTopK, knowledgeSearchMaxTopK),
				},
				"source_types": {
					Type:        "array",
					Description: "Only return passages from documents of these types (any-of).",
					Items:       &llm.Schema{Type: "string", Enum: []any{"manual", "upload", "url", "file", "agent"}},
				},
				"sources": {
					Type:        "array",
					Description: "Only return passages whose document source exactly matches one of these (any-of), e.g. a URL or \"upload:report.pdf\".",
					Items:       &llm.Schema{Type: "string"},
				},
				"created_after": {
					Type:        "string",
					Description: "Only documents created on/after this date. RFC3339 or YYYY-MM-DD.",
				},
				"created_before": {
					Type:        "string",
					Description: "Only documents created on/before this date. RFC3339 or YYYY-MM-DD.",
				},
			},
			Required: []string{"query"},
		})
}

func (s *KnowledgeSearchSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	if s.kb == nil {
		return nil, fmt.Errorf("knowledge base is not enabled; cannot search (enable Knowledge in settings)")
	}

	query := firstStringArg(args, "query", "q")
	if query == "" {
		return nil, fmt.Errorf("query must not be empty")
	}

	topK := intArg(args, "top_k", 0)
	if topK <= 0 {
		topK = knowledgeSearchDefaultTopK
	}
	if topK > knowledgeSearchMaxTopK {
		topK = knowledgeSearchMaxTopK
	}

	createdAfter, err := knowledge.ParseFilterDate(firstStringArg(args, "created_after"))
	if err != nil {
		return nil, fmt.Errorf("created_after: %w", err)
	}
	createdBefore, err := knowledge.ParseFilterDate(firstStringArg(args, "created_before"))
	if err != nil {
		return nil, fmt.Errorf("created_before: %w", err)
	}

	filter := knowledge.Filter{
		Sources:       stringSliceArg(args, "sources"),
		SourceTypes:   stringSliceArg(args, "source_types"),
		CreatedAfter:  createdAfter,
		CreatedBefore: createdBefore,
	}

	hits, err := s.kb.SearchWithFilter(ctx, query, topK, filter)
	if err != nil {
		return nil, fmt.Errorf("knowledge base search failed: %w", err)
	}

	return &skill.Result{
		Content:  formatKnowledgeSearchHits(query, hits),
		Data:     hits,
		Metadata: map[string]string{"hits": fmt.Sprintf("%d", len(hits))},
	}, nil
}

// formatKnowledgeSearchHits renders hits as a compact, LLM-readable block.
func formatKnowledgeSearchHits(query string, hits []knowledge.SearchHit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("No matching passages found in the knowledge base for %q (with the given filters).", query)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d passage(s) in the knowledge base:\n\n", len(hits))
	for i, h := range hits {
		fmt.Fprintf(&sb, "[%d] %s", i+1, h.DocTitle)
		if h.Source != "" {
			fmt.Fprintf(&sb, " · %s", h.Source)
		}
		if st, ok := h.Metadata["source_type"].(string); ok && st != "" {
			fmt.Fprintf(&sb, " · %s", st)
		}
		sb.WriteString("\n")
		sb.WriteString(h.Content)
		sb.WriteString("\n\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// stringSliceArg extracts a []string from an LLM tool argument that may arrive
// as []any (JSON array), []string, or a single string. Blank entries are dropped.
func stringSliceArg(args map[string]any, key string) []string {
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	var out []string
	switch t := v.(type) {
	case []string:
		out = t
	case []any:
		out = make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	case string:
		out = []string{t}
	default:
		return nil
	}
	cleaned := out[:0]
	for _, s := range out {
		if strings.TrimSpace(s) != "" {
			cleaned = append(cleaned, s)
		}
	}
	return cleaned
}
