package knowledge

import (
	"context"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hexagon-codes/hexclaw/localinfer"
)

type executionProfileEmbeddingCall struct {
	inputs    []string
	remaining time.Duration
}

type executionProfileRecordingEmbedder struct {
	mu    sync.Mutex
	calls []executionProfileEmbeddingCall
	ready bool
}

func (e *executionProfileRecordingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	call := executionProfileEmbeddingCall{inputs: append([]string(nil), texts...)}
	if deadline, ok := ctx.Deadline(); ok {
		call.remaining = time.Until(deadline)
	}
	e.mu.Lock()
	e.calls = append(e.calls, call)
	e.mu.Unlock()
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{float32(i + 1)}
	}
	return vectors, nil
}

func (e *executionProfileRecordingEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	vectors, err := e.Embed(ctx, []string{text})
	if err != nil || len(vectors) == 0 {
		return nil, err
	}
	return vectors[0], nil
}

func (*executionProfileRecordingEmbedder) Dimension() int { return 1 }
func (e *executionProfileRecordingEmbedder) Ready(context.Context) bool {
	return e.ready
}

func (e *executionProfileRecordingEmbedder) snapshot() []executionProfileEmbeddingCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]executionProfileEmbeddingCall(nil), e.calls...)
}

func TestExecutionProfileEmbedderEnforcesCountRuneAndPhysicalBatchBudget(t *testing.T) {
	raw := &executionProfileRecordingEmbedder{ready: true}
	profile := EmbeddingExecutionProfile{
		MaxInputRunes: 7,
		BatchMaxCount: 3,
		BatchMaxRunes: 10,
		BatchTimeout:  100 * time.Millisecond,
		QueryTimeout:  40 * time.Millisecond,
	}
	embedder := NewExecutionProfileEmbedder(raw, profile)
	inputs := []string{"123456789", "abcdefg", "xy"}
	vectors, err := embedder.Embed(
		localinfer.WithOperation(context.Background(), localinfer.OperationDocumentEmbedding),
		inputs,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(inputs) {
		t.Fatalf("vectors=%d, want %d", len(vectors), len(inputs))
	}
	calls := raw.snapshot()
	if len(calls) != 2 || len(calls[0].inputs) != 1 || len(calls[1].inputs) != 2 {
		t.Fatalf("physical batches=%v, want rune-shaped [1,2]", calls)
	}
	for callIndex, call := range calls {
		if call.remaining < 80*time.Millisecond || call.remaining > 110*time.Millisecond {
			t.Fatalf("batch[%d] remaining=%s, want ~100ms", callIndex, call.remaining)
		}
		totalRunes := 0
		for inputIndex, input := range call.inputs {
			runes := utf8.RuneCountInString(input)
			if runes > profile.MaxInputRunes {
				t.Fatalf("batch[%d] input[%d] runes=%d, want <=%d", callIndex, inputIndex, runes, profile.MaxInputRunes)
			}
			totalRunes += runes
		}
		if len(call.inputs) > profile.BatchMaxCount || totalRunes > profile.BatchMaxRunes {
			t.Fatalf("batch[%d] count=%d runes=%d exceeds profile", callIndex, len(call.inputs), totalRunes)
		}
	}
	if calls[0].inputs[0] != "1234567" {
		t.Fatalf("first truncated input=%q, want %q", calls[0].inputs[0], "1234567")
	}
}

func TestExecutionProfileEmbedderUsesQueryBudgetAndHonorsShorterParent(t *testing.T) {
	raw := &executionProfileRecordingEmbedder{ready: true}
	embedder := NewExecutionProfileEmbedder(raw, EmbeddingExecutionProfile{
		MaxInputRunes: 400,
		BatchMaxCount: 2,
		BatchMaxRunes: 800,
		BatchTimeout:  120 * time.Second,
		QueryTimeout:  60 * time.Second,
	})
	parent, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := embedder.Embed(
		localinfer.WithOperation(parent, localinfer.OperationQueryEmbedding),
		[]string{"query"},
	); err != nil {
		t.Fatal(err)
	}
	calls := raw.snapshot()
	if len(calls) != 1 || calls[0].remaining <= 0 || calls[0].remaining > 50*time.Millisecond {
		t.Fatalf("query calls=%+v, want shorter parent deadline", calls)
	}
	if !embedder.Ready(context.Background()) {
		t.Fatal("profile wrapper erased wrapped readiness=true")
	}
	raw.ready = false
	if embedder.Ready(context.Background()) {
		t.Fatal("profile wrapper erased wrapped readiness=false")
	}
}
