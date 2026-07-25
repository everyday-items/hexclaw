package main

import (
	"context"

	"github.com/hexagon-codes/ai-core/llm"
)

// k12NonIdempotentLLMContext marks one K12 model invocation as non-replayable.
// K12 owns explicit invocation receipts and recovery; an ambiguous provider
// result must return to that state machine instead of becoming a second hidden
// HTTP POST inside ai-core.
func k12NonIdempotentLLMContext(ctx context.Context) context.Context {
	return llm.WithOperationSafety(ctx, llm.OperationSafetyNonIdempotent)
}
