package knowledge

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/hexagon-codes/hexagon"
)

type legacyProfileSplitter struct {
	count int
	text  string
}

func (s legacyProfileSplitter) Split(context.Context, []hexagon.Document) ([]hexagon.Document, error) {
	documents := make([]hexagon.Document, s.count)
	for i := range documents {
		documents[i] = hexagon.Document{ID: "legacy-profile-chunk", Content: s.text}
	}
	return documents, nil
}

func (legacyProfileSplitter) Name() string { return "legacy-profile-splitter" }

func TestLegacyQwenManagerUsesProfileQueryStageBudget(t *testing.T) {
	profile, ok := EmbeddingExecutionProfileForModel("qwen3-embedding:8b")
	if !ok {
		t.Fatal("qwen execution profile is missing")
	}
	raw := &executionProfileRecordingEmbedder{ready: true}
	embedder := NewExecutionProfileEmbedder(raw, profile)
	searcher := &rpFakeSearcher{vec: []*SearchResult{sr("qwen-hit", 1, 0)}}
	cfg := DefaultHybridConfig()
	cfg.ExpandEnabled = false
	cfg.RerankEnabled = false
	cfg.UseRRF = false
	cfg.MinScore = 0
	cfg.EmbedQueryPrefix = profile.QueryPrefix
	manager := NewManager(
		rpFakeRepo{}, searcher, embedder,
		WithHybridConfig(cfg),
		WithEmbeddingProviderLocation(ProviderLocationCloud),
		WithLegacyEmbeddingModel(" QWEN3-EMBEDDING:8B "),
	)

	query := strings.Repeat("跨语种检索", 100)
	if _, err := manager.Search(context.Background(), query, 1); err != nil {
		t.Fatal(err)
	}
	calls := raw.snapshot()
	if len(calls) != 1 {
		t.Fatalf("query physical calls=%d, want 1", len(calls))
	}
	if calls[0].remaining < 58*time.Second || calls[0].remaining > 60*time.Second {
		t.Fatalf("legacy qwen query deadline=%s, want shared profile budget ~60s", calls[0].remaining)
	}
	if got := utf8.RuneCountInString(calls[0].inputs[0]); got > profile.MaxInputRunes {
		t.Fatalf("legacy qwen query input runes=%d, want <=%d", got, profile.MaxInputRunes)
	}
}

func TestLegacyQwenManagerDoesNotCutProfileDocumentBatchesAtGeneric60Seconds(t *testing.T) {
	profile, ok := EmbeddingExecutionProfileForModel("qwen3-embedding:8b")
	if !ok {
		t.Fatal("qwen execution profile is missing")
	}
	raw := &executionProfileRecordingEmbedder{ready: true}
	embedder := NewExecutionProfileEmbedder(raw, profile)
	cfg := DefaultHybridConfig()
	cfg.ContextualEnabled = false
	manager := NewManager(
		rpFakeRepo{}, &rpFakeSearcher{}, embedder,
		WithHybridConfig(cfg),
		WithSplitter(legacyProfileSplitter{count: 5, text: strings.Repeat("教材", 250)}),
		WithEmbeddingProviderLocation(ProviderLocationCloud),
		WithLegacyEmbeddingModel("qwen3-embedding:8b"),
	)

	chunks, err := manager.buildChunks(context.Background(), &Document{
		ID: "legacy-qwen-document", Content: "教材正文",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 5 {
		t.Fatalf("chunks=%d, want 5", len(chunks))
	}
	calls := raw.snapshot()
	if len(calls) != 3 {
		t.Fatalf("document physical calls=%d, want three profile batches [2,2,1]", len(calls))
	}
	for i, call := range calls {
		wantCount := 2
		if i == len(calls)-1 {
			wantCount = 1
		}
		if len(call.inputs) != wantCount {
			t.Fatalf("batch[%d] count=%d, want %d", i, len(call.inputs), wantCount)
		}
		if call.remaining < 118*time.Second || call.remaining > 120*time.Second {
			t.Fatalf("batch[%d] deadline=%s, want physical profile budget ~120s", i, call.remaining)
		}
		totalRunes := 0
		for _, input := range call.inputs {
			runes := utf8.RuneCountInString(input)
			if runes > profile.MaxInputRunes {
				t.Fatalf("batch[%d] input runes=%d, want <=%d", i, runes, profile.MaxInputRunes)
			}
			totalRunes += runes
		}
		if totalRunes > profile.BatchMaxRunes {
			t.Fatalf("batch[%d] total runes=%d, want <=%d", i, totalRunes, profile.BatchMaxRunes)
		}
	}
}
