package knowledge

import (
	"context"

	"github.com/hexagon-codes/hexclaw/egress"
)

func withEmbeddingBatchClientRequestKey(ctx context.Context, key string) context.Context {
	return egress.WithProviderClientRequestKey(ctx, key)
}

// EmbeddingBatchClientRequestKeyFromContext returns the durable identity of a
// document-embedding batch. The provider transport maps it to Idempotency-Key,
// but that does not make provider execution exactly-once: providers may ignore
// the header and remain at-least-once. Query embedding contexts have no key.
func EmbeddingBatchClientRequestKeyFromContext(ctx context.Context) (string, bool) {
	return egress.ProviderClientRequestKeyFromContext(ctx)
}
