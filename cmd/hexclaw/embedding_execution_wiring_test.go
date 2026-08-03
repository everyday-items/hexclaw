package main

import (
	"context"
	"errors"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/localinfer"
)

type sharedEmbeddingCacheOrderDouble struct {
	calls int
}

func (e *sharedEmbeddingCacheOrderDouble) Embed(
	_ context.Context,
	texts []string,
) ([][]float32, error) {
	e.calls++
	if len(texts) == 1 && texts[0] == "force-provider-failure" {
		return nil, errors.New("provider failure canary")
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{1, 0, 0}
	}
	return vectors, nil
}

func (e *sharedEmbeddingCacheOrderDouble) EmbedOne(
	ctx context.Context,
	text string,
) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (*sharedEmbeddingCacheOrderDouble) Dimension() int { return 3 }

type commandEmbeddingExecutionRecorder struct {
	calls []commandEmbeddingExecutionCall
}

type commandEmbeddingExecutionCall struct {
	inputs            []string
	deadlineRemaining time.Duration
}

func (e *commandEmbeddingExecutionRecorder) Embed(
	ctx context.Context,
	texts []string,
) ([][]float32, error) {
	call := commandEmbeddingExecutionCall{inputs: append([]string(nil), texts...)}
	if deadline, ok := ctx.Deadline(); ok {
		call.deadlineRemaining = time.Until(deadline)
	}
	e.calls = append(e.calls, call)
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{1, 0, 0}
	}
	return vectors, nil
}

func (e *commandEmbeddingExecutionRecorder) EmbedOne(
	ctx context.Context,
	text string,
) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (*commandEmbeddingExecutionRecorder) Dimension() int { return 3 }

func TestWrapKnowledgeEmbeddingExecutionProfileUsesExactModelCapacity(t *testing.T) {
	profile, ok := knowledge.EmbeddingExecutionProfileForModel("qwen3-embedding:8b")
	if !ok {
		t.Fatal("qwen3-embedding:8b execution profile missing")
	}
	recorder := &commandEmbeddingExecutionRecorder{}
	embedder := wrapKnowledgeEmbeddingExecutionProfile(recorder, " QWEN3-EMBEDDING:8B ")
	ctx := localinfer.WithOperation(context.Background(), localinfer.OperationDocumentEmbedding)
	inputs := []string{
		repeatRune('甲', profile.MaxInputRunes+17),
		repeatRune('乙', profile.MaxInputRunes),
		repeatRune('丙', profile.MaxInputRunes),
	}

	vectors, err := embedder.Embed(ctx, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(inputs) {
		t.Fatalf("vectors=%d, want %d", len(vectors), len(inputs))
	}
	if len(recorder.calls) != 2 || len(recorder.calls[0].inputs) != 2 || len(recorder.calls[1].inputs) != 1 {
		t.Fatalf("provider call shape=%v, want [2,1]", commandEmbeddingBatchSizes(recorder.calls))
	}
	for callIndex, call := range recorder.calls {
		totalRunes := 0
		for inputIndex, input := range call.inputs {
			runes := utf8.RuneCountInString(input)
			if runes > profile.MaxInputRunes {
				t.Fatalf("call[%d].input[%d] runes=%d, want <=%d",
					callIndex, inputIndex, runes, profile.MaxInputRunes)
			}
			totalRunes += runes
		}
		if totalRunes > profile.BatchMaxRunes {
			t.Fatalf("call[%d] total runes=%d, want <=%d", callIndex, totalRunes, profile.BatchMaxRunes)
		}
		if call.deadlineRemaining <= 0 || call.deadlineRemaining > profile.BatchTimeout {
			t.Fatalf("call[%d] deadline remaining=%v, want (0,%v]", callIndex, call.deadlineRemaining, profile.BatchTimeout)
		}
	}
}

func TestWrapKnowledgeEmbeddingExecutionProfileKeepsGenericFallback(t *testing.T) {
	recorder := &commandEmbeddingExecutionRecorder{}
	embedder := wrapKnowledgeEmbeddingExecutionProfile(recorder, "vendor/unknown-embedding")
	if _, err := embedder.Embed(context.Background(), []string{"one", "two", "three"}); err != nil {
		t.Fatal(err)
	}
	if got := commandEmbeddingBatchSizes(recorder.calls); len(got) != 1 || got[0] != 3 {
		t.Fatalf("generic provider call shape=%v, want one batch of three", got)
	}
}

func TestAssembleKnowledgeSharedEmbedderKeepsCacheOutsideReadiness(t *testing.T) {
	raw := &sharedEmbeddingCacheOrderDouble{}
	embedder := assembleKnowledgeSharedEmbedder(
		raw,
		"qwen3-embedding:8b",
		true,
		true,
		nil,
		true,
		func(context.Context) bool { return false },
	)
	if _, err := embedder.Embed(context.Background(), []string{"cached-input"}); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"force-provider-failure"}); err == nil {
		t.Fatal("provider failure did not close readiness gate")
	}
	if knowledge.EmbeddingReady(context.Background(), embedder) {
		t.Fatal("outer shared embedder erased unavailable provider readiness")
	}
	if _, err := embedder.Embed(context.Background(), []string{"cached-input"}); err != nil {
		t.Fatalf("cache hit was blocked by unavailable readiness: %v", err)
	}
	if raw.calls != 2 {
		t.Fatalf("raw provider calls=%d, want prime+failed miss only", raw.calls)
	}
}

func TestKnowledgeEmbeddingHotReadinessProbeHasIndependentCeiling(t *testing.T) {
	var fullBudget time.Duration
	ready := knowledgeEmbeddingHotReadinessAvailable(
		context.Background(),
		func(probeCtx context.Context) knowledge.ProfileAvailability {
			deadline, ok := probeCtx.Deadline()
			if !ok {
				t.Fatal("hot readiness probe has no deadline")
			}
			fullBudget = time.Until(deadline)
			return knowledge.ProfileAvailabilityConnected
		},
	)
	if !ready {
		t.Fatal("connected probe reported unavailable")
	}
	if fullBudget < knowledgeEmbeddingProbeTimeout-time.Second || fullBudget > knowledgeEmbeddingProbeTimeout {
		t.Fatalf("independent hot-probe budget=%v, want approximately %v", fullBudget, knowledgeEmbeddingProbeTimeout)
	}

	parent, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var parentBudget time.Duration
	ready = knowledgeEmbeddingHotReadinessAvailable(
		parent,
		func(probeCtx context.Context) knowledge.ProfileAvailability {
			deadline, ok := probeCtx.Deadline()
			if !ok {
				t.Fatal("parent-bounded hot readiness probe has no deadline")
			}
			parentBudget = time.Until(deadline)
			return knowledge.ProfileAvailabilityInstalled
		},
	)
	if !ready {
		t.Fatal("installed probe reported unavailable")
	}
	if parentBudget <= 0 || parentBudget > 100*time.Millisecond {
		t.Fatalf("parent-bounded hot-probe budget=%v, want (0,100ms]", parentBudget)
	}

	canceled, cancelCanceled := context.WithCancel(context.Background())
	cancelCanceled()
	if knowledgeEmbeddingHotReadinessAvailable(
		canceled,
		func(context.Context) knowledge.ProfileAvailability {
			return knowledge.ProfileAvailabilityConnected
		},
	) {
		t.Fatal("canceled hot readiness probe must fail closed")
	}
}

func repeatRune(r rune, count int) string {
	result := make([]rune, count)
	for i := range result {
		result[i] = r
	}
	return string(result)
}

func commandEmbeddingBatchSizes(calls []commandEmbeddingExecutionCall) []int {
	result := make([]int, len(calls))
	for i := range calls {
		result[i] = len(calls[i].inputs)
	}
	return result
}
