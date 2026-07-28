package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
)

const bug20260728QwenQueryPrefix = "Instruct: Given a search query, retrieve relevant passages that answer the query\nQuery:"

type bug20260728QwenExecutor struct {
	dimension int
	failAt    int
	calls     int
	batches   [][]string
	deadlines []time.Duration
}

func (e *bug20260728QwenExecutor) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embed(ctx, texts)
}

func (e *bug20260728QwenExecutor) EmbedForPurpose(
	ctx context.Context,
	_ knowledge.EmbeddingPurpose,
	texts []string,
) ([][]float32, error) {
	return e.embed(ctx, texts)
}

func (e *bug20260728QwenExecutor) embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls++
	e.batches = append(e.batches, append([]string(nil), texts...))
	if deadline, ok := ctx.Deadline(); ok {
		e.deadlines = append(e.deadlines, time.Until(deadline))
	} else {
		e.deadlines = append(e.deadlines, 0)
	}
	if e.failAt > 0 && e.calls == e.failAt {
		return nil, errors.New("BUG012 injected provider interruption")
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = make([]float32, e.dimension)
		vectors[i][i%e.dimension] = 1
	}
	return vectors, nil
}

type bug20260728QwenRegistry struct {
	executor knowledge.ProfileEmbeddingExecutor
}

func (r *bug20260728QwenRegistry) ExecutorForProfile(
	context.Context,
	knowledge.EmbeddingProfileSnapshot,
) (knowledge.ProfileEmbeddingExecutor, error) {
	if r.executor == nil {
		return nil, knowledge.ErrProfileUnavailable
	}
	return r.executor, nil
}

func bug20260728QwenRuntime(
	t *testing.T,
	chunks []string,
	executor knowledge.ProfileEmbeddingExecutor,
) (*knowledgeSemanticIndexRuntime, *bug20260728QwenRegistry, *sql.DB) {
	t.Helper()
	db, ctx := newKnowledgeSemanticRuntimeTestDB(t)
	if _, err := db.ExecContext(ctx, `INSERT INTO kb_documents(id,title,content,source,deleted)
		VALUES('bug012-doc','BUG012 held-out','source body','test',0)`); err != nil {
		t.Fatal(err)
	}
	for i, content := range chunks {
		if _, err := db.ExecContext(ctx, `INSERT INTO kb_chunks(id,doc_id,content,chunk_index,embedding)
			VALUES(?,?,?,?,NULL)`, fmt.Sprintf("bug012-chunk-%02d", i), "bug012-doc", content, i); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.DefaultConfig()
	cfg.Knowledge.ChunkSize = 400
	cfg.Knowledge.ChunkOverlap = 80
	cfg.Knowledge.Embedding.Provider = "ollama"
	cfg.Knowledge.Embedding.Model = "qwen3-embedding:8b"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"ollama": {BaseURL: "http://127.0.0.1:11434/v1"},
	}
	resolver := newKnowledgeEmbeddingProfileResolver(cfg, knowledgeEmbeddingPlan{
		Provider: "ollama", Model: "qwen3-embedding:8b", Configured: true,
		Ready: true, Ollama: true, ServiceAvailable: true,
	}, 4096)
	registry := &bug20260728QwenRegistry{executor: executor}
	runtime, err := setupKnowledgeSemanticIndex(ctx, db, resolver, registry, "bug012-worker")
	if err != nil {
		t.Fatal(err)
	}
	return runtime, registry, db
}

func TestBug20260728QwenProfileUsesOfficialQueryOnlyTransform(t *testing.T) {
	cfg := config.DefaultConfig()
	gotQuery, gotDocument := knowledgeEmbeddingPrefixes(cfg, "qwen3-embedding:8b")
	if gotQuery != bug20260728QwenQueryPrefix || gotDocument != "" {
		t.Fatalf("Qwen transforms=(%q,%q), want official query-only Instruct and empty document prefix",
			gotQuery, gotDocument)
	}

	gotNomicQuery, gotNomicDocument := knowledgeEmbeddingPrefixes(cfg, "nomic-embed-text")
	if gotNomicQuery != "search_query: " || gotNomicDocument != "search_document: " {
		t.Fatalf("Qwen calibration mutated Nomic transforms=(%q,%q)",
			gotNomicQuery, gotNomicDocument)
	}
}

func TestBug20260728QwenTransformChangesProfileHashAnd400RuneRevision(t *testing.T) {
	base := knowledgeEmbeddingProfileEntry{
		ProviderID: "pvd_v1_0123456789abcdef0123456789abcdef", ProviderName: "Ollama",
		ModelName: "qwen3-embedding:8b", Protocol: config.LLMEmbeddingProtocolOllama,
		BaseURL: "http://127.0.0.1:11434/v1", Location: knowledge.ProviderLocationLocal,
		Capability: "embedding", Dimension: 4096, Availability: knowledge.ProfileAvailabilityInstalled,
		Normalization: "l2",
	}
	legacy, err := knowledgeEmbeddingProfileSnapshotForEntry(base)
	if err != nil {
		t.Fatal(err)
	}
	instructedEntry := base
	instructedEntry.QueryPrefix = bug20260728QwenQueryPrefix
	instructed, err := knowledgeEmbeddingProfileSnapshotForEntry(instructedEntry)
	if err != nil {
		t.Fatal(err)
	}
	if instructed.Profile.ProfileID != legacy.Profile.ProfileID {
		t.Fatalf("query transform changed logical profile identity: legacy=%q instructed=%q",
			legacy.Profile.ProfileID, instructed.Profile.ProfileID)
	}
	if instructed.ProfileConfigHash == legacy.ProfileConfigHash {
		t.Fatal("query transform did not change immutable profile_config_hash")
	}
	wantChunkHash := knowledgeEmbeddingDocumentTransformHash(
		knowledgeEmbeddingDocumentTransformEpoch, "", 400,
	)
	if instructed.ChunkConfigHash != wantChunkHash {
		t.Fatalf("Qwen chunk transform hash=%q, want max_input_runes=400 hash %q",
			instructed.ChunkConfigHash, wantChunkHash)
	}
}

func TestBug20260728QwenRuntimeShapesBatchesAndUses120SecondBudget(t *testing.T) {
	executor := &bug20260728QwenExecutor{dimension: 4096}
	runtime, _, _ := bug20260728QwenRuntime(t, []string{
		strings.Repeat("甲", 400),
		strings.Repeat("乙", 400),
		strings.Repeat("丙", 400),
	}, executor)

	processed, err := runtime.Worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("RunOnce processed=%v err=%v", processed, err)
	}
	if len(executor.batches) != 2 {
		t.Errorf("Qwen provider batches=%d (%v), want two adaptive batches", len(executor.batches), batchRuneShape(executor.batches))
	}
	for i, batch := range executor.batches {
		count, totalRunes, maxRunes := len(batch), 0, 0
		for _, input := range batch {
			n := len([]rune(input))
			totalRunes += n
			if n > maxRunes {
				maxRunes = n
			}
		}
		if count > 2 || totalRunes > 800 || maxRunes > 400 {
			t.Errorf("batch %d shape=(count=%d,total_runes=%d,max_runes=%d), want <=2/<=800/<=400",
				i, count, totalRunes, maxRunes)
		}
	}
	for i, remaining := range executor.deadlines {
		if remaining < 119*time.Second || remaining > 120*time.Second {
			t.Errorf("batch %d deadline=%v, want profile-scoped 120s budget", i, remaining)
		}
	}
}

func TestBug20260728QwenAdaptiveBatchResumeSkipsCommittedPrefix(t *testing.T) {
	chunks := []string{
		strings.Repeat("甲", 400),
		strings.Repeat("乙", 400),
		strings.Repeat("丙", 400),
		strings.Repeat("丁", 400),
		strings.Repeat("戊", 400),
	}
	interrupted := &bug20260728QwenExecutor{dimension: 4096, failAt: 2}
	runtime, registry, db := bug20260728QwenRuntime(t, chunks, interrupted)

	processed, err := runtime.Worker.RunOnce(context.Background())
	if !processed || err == nil {
		t.Fatalf("interrupted RunOnce processed=%v err=%v, want second adaptive batch failure", processed, err)
	}
	var committed int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM kb_revision_vectors`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 2 {
		t.Fatalf("committed vectors=%d, want first two-input batch only", committed)
	}

	if _, err := db.ExecContext(context.Background(),
		`UPDATE kb_knowledge_jobs SET next_attempt_at=? WHERE state='retry_wait'`,
		time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	restarted := &bug20260728QwenExecutor{dimension: 4096}
	registry.executor = restarted
	processed, err = runtime.Worker.RunOnce(context.Background())
	if err != nil || !processed {
		t.Fatalf("restart RunOnce processed=%v err=%v", processed, err)
	}
	want := [][]string{{chunks[2], chunks[3]}, {chunks[4]}}
	if fmt.Sprint(restarted.batches) != fmt.Sprint(want) {
		t.Fatalf("restart batches=%s, want only uncommitted %s",
			batchRuneShape(restarted.batches), batchRuneShape(want))
	}
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM kb_revision_vectors`).Scan(&committed); err != nil {
		t.Fatal(err)
	}
	if committed != 5 {
		t.Fatalf("final committed vectors=%d, want 5", committed)
	}
}

func batchRuneShape(batches [][]string) string {
	shapes := make([]string, 0, len(batches))
	for _, batch := range batches {
		total, max := 0, 0
		for _, input := range batch {
			n := len([]rune(input))
			total += n
			if n > max {
				max = n
			}
		}
		shapes = append(shapes, fmt.Sprintf("%d/%d/%d", len(batch), total, max))
	}
	return strings.Join(shapes, ",")
}
