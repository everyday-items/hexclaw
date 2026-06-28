package knowledge

import (
	"context"
	"strings"
	"testing"
)

// fakeEmbedder 记录最后一次收到的文本，便于断言截断是否生效。
type fakeEmbedder struct {
	lastTexts []string
	dim       int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.lastTexts = append([]string(nil), texts...)
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func (f *fakeEmbedder) EmbedOne(_ context.Context, text string) ([]float32, error) {
	f.lastTexts = []string{text}
	return []float32{1, 0, 0}, nil
}

func (f *fakeEmbedder) Dimension() int { return f.dim }

func TestTruncatingEmbedder_TruncatesByRunes(t *testing.T) {
	fake := &fakeEmbedder{dim: 3}
	emb := NewTruncatingEmbedder(fake, 5)

	// 100 个 CJK rune（多字节）：截断必须按 rune，不能切坏字符或按字节切。
	long := strings.Repeat("中", 100)
	if _, err := emb.Embed(context.Background(), []string{long}); err != nil {
		t.Fatalf("Embed err: %v", err)
	}
	got := []rune(fake.lastTexts[0])
	if len(got) != 5 {
		t.Fatalf("want 5 runes after truncation, got %d", len(got))
	}
	if fake.lastTexts[0] != "中中中中中" {
		t.Fatalf("truncation must keep valid runes, got %q", fake.lastTexts[0])
	}
}

func TestTruncatingEmbedder_ShortPassThroughAndDimension(t *testing.T) {
	fake := &fakeEmbedder{dim: 1536}
	emb := NewTruncatingEmbedder(fake, 0) // 0 → DefaultEmbedMaxRunes

	const short = "hello world"
	if _, err := emb.Embed(context.Background(), []string{short}); err != nil {
		t.Fatalf("Embed err: %v", err)
	}
	if fake.lastTexts[0] != short {
		t.Fatalf("short text should pass through unchanged, got %q", fake.lastTexts[0])
	}
	if emb.Dimension() != 1536 {
		t.Fatalf("Dimension must delegate to inner, got %d", emb.Dimension())
	}
}

func TestTruncatingEmbedder_EmbedOneTruncates(t *testing.T) {
	fake := &fakeEmbedder{dim: 3}
	emb := NewTruncatingEmbedder(fake, 4)
	if _, err := emb.EmbedOne(context.Background(), strings.Repeat("a", 50)); err != nil {
		t.Fatalf("EmbedOne err: %v", err)
	}
	if len([]rune(fake.lastTexts[0])) != 4 {
		t.Fatalf("EmbedOne should truncate to 4 runes, got %d", len([]rune(fake.lastTexts[0])))
	}
}
