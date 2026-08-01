package knowledge

import (
	"context"
	"errors"
	"unicode/utf8"
)

const (
	// DefaultSearchTopK is the compatibility default for direct Knowledge
	// searches. Public callers may request fewer results, never an unbounded
	// result set.
	DefaultSearchTopK = 3
	// MaxSearchTopK preserves the Desktop PDF lifecycle's legitimate top_k=50
	// deletion probe while preventing arbitrary result expansion.
	MaxSearchTopK = 50
	// MaxCandidateK matches the highest approved Desktop retrieval preset. It
	// bounds every vector/text lane and reranker input even for hand-edited YAML.
	MaxCandidateK = 100
	// MaxSearchQueryRunes keeps one request from amplifying FTS LIKE fallback,
	// query embedding, and auxiliary LLM prompts.
	MaxSearchQueryRunes = 4096
)

var ErrSearchQueryBudgetExceeded = errors.New("knowledge: search query exceeds retrieval budget")

// SearchQueryWithinBudget reports whether a query is safe to fan out into the
// retrieval pipeline. Empty-query validation remains a caller concern because
// some internal paths intentionally treat it as a no-op.
func SearchQueryWithinBudget(query string) bool {
	return utf8.RuneCountInString(query) <= MaxSearchQueryRunes
}

func normalizeSearchTopK(topK int) int {
	if topK <= 0 {
		return DefaultSearchTopK
	}
	if topK > MaxSearchTopK {
		return MaxSearchTopK
	}
	return topK
}

func effectiveCandidateK(configured, topK int) int {
	if configured <= 0 {
		configured = 50
	}
	if configured > MaxCandidateK {
		configured = MaxCandidateK
	}
	minimum := topK * 3
	if minimum > MaxCandidateK {
		minimum = MaxCandidateK
	}
	if configured < minimum {
		configured = minimum
	}
	return configured
}

func normalizeHybridConfigBudget(config HybridConfig) HybridConfig {
	if config.CandidateK > MaxCandidateK {
		config.CandidateK = MaxCandidateK
	}
	return config
}

// RetrievalFreshnessPolicy is request-scoped ranking policy. It deliberately
// changes ranking only; source scope, revision pinning, and evidence checks are
// untouched.
type RetrievalFreshnessPolicy string

const (
	RetrievalFreshnessDefault   RetrievalFreshnessPolicy = "default"
	RetrievalFreshnessEvergreen RetrievalFreshnessPolicy = "evergreen"
)

type retrievalFreshnessPolicyContextKey struct{}

// WithRetrievalFreshnessPolicy marks a retrieval request's ranking semantics.
// K12 textbook grounding uses Evergreen because publication age is not a
// relevance signal for an immutable curriculum edition.
func WithRetrievalFreshnessPolicy(ctx context.Context, policy RetrievalFreshnessPolicy) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if policy != RetrievalFreshnessEvergreen {
		policy = RetrievalFreshnessDefault
	}
	return context.WithValue(ctx, retrievalFreshnessPolicyContextKey{}, policy)
}

// RetrievalFreshnessPolicyFromContext returns Default for normal retrieval and
// for unknown/untrusted context values.
func RetrievalFreshnessPolicyFromContext(ctx context.Context) RetrievalFreshnessPolicy {
	if ctx == nil {
		return RetrievalFreshnessDefault
	}
	if policy, ok := ctx.Value(retrievalFreshnessPolicyContextKey{}).(RetrievalFreshnessPolicy); ok &&
		policy == RetrievalFreshnessEvergreen {
		return policy
	}
	return RetrievalFreshnessDefault
}
