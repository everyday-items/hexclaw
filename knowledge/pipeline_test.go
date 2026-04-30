package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func withRAGFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagRAGPipelineV1: on,
	})
	return featureflag.WithContext(ctx, flags)
}

// fakeRetriever 控制返回 hits / 错误。
type fakeRetriever struct {
	hits []SearchHit
	err  error
}

func (f *fakeRetriever) Retrieve(_ context.Context, _ []string, _ int) ([]SearchHit, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]SearchHit, len(f.hits))
	copy(out, f.hits)
	return out, nil
}

func TestPipeline_FlagOffReturnsErr(t *testing.T) {
	p := &Pipeline{Retriever: &fakeRetriever{}}
	ctx := withRAGFlag(context.Background(), false)
	_, err := p.RunRAG(ctx, "q", 5)
	if !errors.Is(err, ErrPipelineDisabled) {
		t.Errorf("flag OFF 应返回 ErrPipelineDisabled；got %v", err)
	}
}

func TestPipeline_RetrieverRequired(t *testing.T) {
	p := &Pipeline{}
	ctx := withRAGFlag(context.Background(), true)
	_, err := p.RunRAG(ctx, "q", 5)
	if err == nil {
		t.Error("缺 Retriever 应报错")
	}
}

func TestPipeline_HappyPath_DefaultStages(t *testing.T) {
	hits := []SearchHit{
		{ChunkID: "c1", DocID: "d1", ChunkIndex: 0, Content: "first chunk", Score: 0.5},
		{ChunkID: "c2", DocID: "d2", ChunkIndex: 1, Content: "second chunk", Score: 0.9},
	}
	p := &Pipeline{Retriever: &fakeRetriever{hits: hits}}
	ctx := withRAGFlag(context.Background(), true)
	res, err := p.RunRAG(ctx, "搜索词", 2)
	if err != nil {
		t.Fatal(err)
	}
	if res.Original != "搜索词" {
		t.Errorf("Original 错；got %q", res.Original)
	}
	if len(res.Queries) != 1 || res.Queries[0] != "搜索词" {
		t.Errorf("默认 Identity rewriter 应原样；got %v", res.Queries)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("hits 数量错；got %d", len(res.Hits))
	}
	// 默认 ScoreReranker 按分数降序：c2 应排第一
	if res.Hits[0].ChunkID != "c2" {
		t.Errorf("Reranker 应按 score 降序；got %v", res.Hits)
	}
	// SimpleContextBuilder 包含 chunk 内容
	if !strings.Contains(res.Context, "second chunk") || !strings.Contains(res.Context, "first chunk") {
		t.Errorf("Context 应包含两段；got %q", res.Context)
	}
	// 没注入 Answerer 时 Answer 应为空
	if res.Answer != "" {
		t.Errorf("无 Answerer 时 Answer 应为空；got %q", res.Answer)
	}
}

func TestPipeline_RewriterMultiQueries(t *testing.T) {
	multiRewriter := rewriterFn(func(_ context.Context, q string) ([]string, error) {
		return []string{q, q + "?", q + "!"}, nil
	})
	captured := []string{}
	retriever := retrieverFn(func(_ context.Context, queries []string, _ int) ([]SearchHit, error) {
		captured = queries
		return nil, nil
	})
	p := &Pipeline{Rewriter: multiRewriter, Retriever: retriever}
	ctx := withRAGFlag(context.Background(), true)
	if _, err := p.RunRAG(ctx, "原 query", 1); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 3 {
		t.Errorf("Retriever 应收到 3 条 queries；got %v", captured)
	}
}

func TestPipeline_StageErrorShortCircuits(t *testing.T) {
	p := &Pipeline{Retriever: &fakeRetriever{err: errors.New("retrieve boom")}}
	ctx := withRAGFlag(context.Background(), true)
	_, err := p.RunRAG(ctx, "q", 5)
	if err == nil {
		t.Fatal("Retriever 失败应被传播")
	}
	if !strings.Contains(err.Error(), "retrieve") {
		t.Errorf("错误应携带阶段名；got %v", err)
	}
}

func TestPipeline_AnswererInvoked(t *testing.T) {
	hits := []SearchHit{{ChunkID: "c1", Content: "x", Score: 1}}
	p := &Pipeline{
		Retriever: &fakeRetriever{hits: hits},
		Answerer: AnswerFunc(func(_ context.Context, q, ctx string) (string, error) {
			if !strings.Contains(ctx, "x") {
				return "", fmt.Errorf("Answerer 应收到 context；got %q", ctx)
			}
			return "answer-for-" + q, nil
		}),
	}
	ctx := withRAGFlag(context.Background(), true)
	res, err := p.RunRAG(ctx, "Q", 5)
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "answer-for-Q" {
		t.Errorf("Answer 不对；got %q", res.Answer)
	}
}

func TestIdentityQueryRewriter_RejectsEmpty(t *testing.T) {
	if _, err := (IdentityQueryRewriter{}).Rewrite(context.Background(), "  "); err == nil {
		t.Error("空 query 应报错")
	}
}

func TestScoreReranker_OrderAndCap(t *testing.T) {
	hits := []SearchHit{{ChunkID: "a", Score: 0.1}, {ChunkID: "b", Score: 0.9}, {ChunkID: "c", Score: 0.5}}
	out, err := (ScoreReranker{}).Rerank(context.Background(), "q", hits, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].ChunkID != "b" || out[1].ChunkID != "c" {
		t.Errorf("排序错；got %v", out)
	}
}

func TestSimpleContextBuilder_Truncates(t *testing.T) {
	hits := []SearchHit{{ChunkID: "c1", Content: strings.Repeat("a", 100)}}
	out, _ := (SimpleContextBuilder{MaxChars: 10}).Build(hits)
	if !strings.Contains(out, "...") {
		t.Errorf("应截断；got %q", out)
	}
	if len(out) > 200 {
		t.Errorf("整段过长；len=%d", len(out))
	}
}

func TestSimpleContextBuilder_FormatsCitations(t *testing.T) {
	hits := []SearchHit{
		{DocID: "d1", ChunkIndex: 0, Content: "alpha"},
		{DocID: "d2", ChunkIndex: 1, Content: "beta"},
	}
	out, _ := (SimpleContextBuilder{}).Build(hits)
	if !strings.Contains(out, "[1]") || !strings.Contains(out, "[2]") {
		t.Errorf("应有引用编号；got %q", out)
	}
	if !strings.Contains(out, "doc=d1") || !strings.Contains(out, "doc=d2") {
		t.Errorf("应有 doc id；got %q", out)
	}
}

// helpers --------------------------------------------------------

type rewriterFn func(context.Context, string) ([]string, error)

func (f rewriterFn) Rewrite(ctx context.Context, q string) ([]string, error) { return f(ctx, q) }

type retrieverFn func(context.Context, []string, int) ([]SearchHit, error)

func (f retrieverFn) Retrieve(ctx context.Context, qs []string, k int) ([]SearchHit, error) {
	return f(ctx, qs, k)
}
