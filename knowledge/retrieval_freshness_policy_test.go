package knowledge

import (
	"context"
	"testing"
	"time"
)

func TestEvergreenFreshnessPolicySkipsTimeDecay(t *testing.T) {
	mgr := mgrWithConfig(HybridConfig{TimeDecayDays: 30, UseRRF: false})
	old := time.Now().Add(-365 * 24 * time.Hour)
	result := &SearchResult{
		Chunk:     &Chunk{ID: "old-textbook", CreatedAt: old},
		TextScore: 1,
	}

	candidates := mgr.fuse(
		map[string]*SearchResult{result.Chunk.ID: result},
		nil,
		false,
		RetrievalFreshnessPolicyFromContext(
			WithRetrievalFreshnessPolicy(context.Background(), RetrievalFreshnessEvergreen),
		),
	)
	if got := candidates[0].Chunk.Score; got != 1 {
		t.Fatalf("常青教材不应因文档年龄衰减，得 %.6f", got)
	}
}
