package knowledge

import (
	"strings"
	"time"
)

const qwen3Embedding8BModel = "qwen3-embedding:8b"

// EmbeddingExecutionProfile contains model-scoped execution facts that must
// stay aligned across immutable snapshots, document indexing and retrieval.
// Unknown models deliberately have no profile and retain the existing global
// defaults.
type EmbeddingExecutionProfile struct {
	Dimension               int
	QueryPrefix             string
	DocumentPrefix          string
	MaxInputRunes           int
	BatchMaxCount           int
	BatchMaxRunes           int
	BatchTimeout            time.Duration
	QueryTimeout            time.Duration
	AutoInjectionMinScore   float64
	AutoInjectionMaxResults int
}

// EmbeddingExecutionProfileForModel returns only evidence-calibrated exact
// model profiles. It must not infer policy from a family-name substring.
func EmbeddingExecutionProfileForModel(model string) (EmbeddingExecutionProfile, bool) {
	if strings.ToLower(strings.TrimSpace(model)) != qwen3Embedding8BModel {
		return EmbeddingExecutionProfile{}, false
	}
	return EmbeddingExecutionProfile{
		Dimension:               4096,
		QueryPrefix:             "Instruct: Given a search query, retrieve relevant passages that answer the query\nQuery:",
		DocumentPrefix:          "",
		MaxInputRunes:           400,
		BatchMaxCount:           2,
		BatchMaxRunes:           800,
		BatchTimeout:            120 * time.Second,
		QueryTimeout:            60 * time.Second,
		AutoInjectionMinScore:   0.65,
		AutoInjectionMaxResults: 1,
	}, true
}
