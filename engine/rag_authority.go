package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

const (
	knowledgeEvidenceSchema = "hexclaw.knowledge_evidence.v1"
	knowledgeEvidenceTrust  = "untrusted_document"
)

// knowledgeEvidenceBlock is a data-only envelope. Every string in Items is
// JSON encoded by the host; retrieved content therefore cannot close the
// program-owned frame or become a sibling instruction block.
type knowledgeEvidenceBlock struct {
	Schema string                  `json:"schema"`
	Trust  string                  `json:"trust"`
	Items  []knowledgeEvidenceItem `json:"items"`
}

type knowledgeEvidenceItem struct {
	DocumentID       string  `json:"document_id"`
	DocumentTitle    string  `json:"document_title,omitempty"`
	Source           string  `json:"source,omitempty"`
	ChunkID          string  `json:"chunk_id"`
	Content          string  `json:"content"`
	Score            float64 `json:"score"`
	PageStart        int     `json:"page_start,omitempty"`
	PageEnd          int     `json:"page_end,omitempty"`
	CitationDigest   string  `json:"citation_digest,omitempty"`
	SourceDigest     string  `json:"source_digest,omitempty"`
	SourceOffsetFrom int64   `json:"source_offset_start,omitempty"`
	SourceOffsetTo   int64   `json:"source_offset_end,omitempty"`
}

func encodeKnowledgeEvidence(hits []knowledge.SearchHit) string {
	if len(hits) == 0 {
		return ""
	}
	items := make([]knowledgeEvidenceItem, 0, len(hits))
	for _, hit := range hits {
		items = append(items, knowledgeEvidenceItem{
			DocumentID:       hit.DocID,
			DocumentTitle:    hit.DocTitle,
			Source:           hit.Source,
			ChunkID:          hit.ChunkID,
			Content:          hit.Content,
			Score:            hit.Score,
			PageStart:        hit.PageStart,
			PageEnd:          hit.PageEnd,
			CitationDigest:   hit.CitationDigest,
			SourceDigest:     hit.SourceDigest,
			SourceOffsetFrom: hit.SourceOffsetStart,
			SourceOffsetTo:   hit.SourceOffsetEnd,
		})
	}
	payload, err := json.Marshal(knowledgeEvidenceBlock{
		Schema: knowledgeEvidenceSchema,
		Trust:  knowledgeEvidenceTrust,
		Items:  items,
	})
	if err != nil {
		// The envelope contains only strings and numeric primitives, so this is
		// defensive fail-closed handling rather than a recoverable branch.
		return ""
	}
	return "<knowledge-evidence>\n" + string(payload) + "\n</knowledge-evidence>"
}

type untrustedKnowledgeEvidenceContextKey struct{}

// withUntrustedKnowledgeEvidence stamps a host-only authority taint. It is not
// derived from message metadata and cannot be set or cleared by a model/tool.
func withUntrustedKnowledgeEvidence(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, untrustedKnowledgeEvidenceContextKey{}, true)
}

func hasUntrustedKnowledgeEvidence(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(untrustedKnowledgeEvidenceContextKey{}).(bool)
	return marked
}

// UntrustedEvidenceTaskGrantChecker is intentionally stricter than the legacy
// task/tool allowlist. Existing broad grants do not authorize a tainted turn;
// a store must explicitly bind owner, task and the frozen argument scope.
type UntrustedEvidenceTaskGrantChecker interface {
	GrantAllowsUntrustedEvidence(ownerID, source, taskRef, canonicalToolName, securityScopeDigest string) bool
}

func untrustedEvidenceSecurityScopeDigest(args map[string]any) (string, error) {
	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func canonicalEvidenceToolName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
