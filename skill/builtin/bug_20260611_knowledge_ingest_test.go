package builtin

// ====================================================================
// BUG-20260611: knowledge-base ingestion loop broken — the Agent has no
// knowledge_ingest tool.
//
// Symptom: Agent / cron agent jobs claim "saved to knowledge base" but
//          the knowledge base stays empty.
// Cause:   no builtin skill can write to the knowledge base, so the
//          ReAct loop has no way to persist anything and the LLM can
//          only hallucinate success.
// Contract: knowledge_ingest exists, always goes through LLM
//          tool-calling (Match=false), and Execute persists
//          title/content/source via knowledge.Manager.
// ====================================================================

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

// fakeIngestor is a narrow-interface stub recording AddDocument calls.
type fakeIngestor struct {
	calls []struct{ title, content, source string }
	err   error
}

func (f *fakeIngestor) AddDocument(_ context.Context, title, content, source string) (*knowledge.Document, error) {
	f.calls = append(f.calls, struct{ title, content, source string }{title, content, source})
	if f.err != nil {
		return nil, f.err
	}
	return &knowledge.Document{ID: "doc-test-1", Title: title}, nil
}

func TestBug20260611_KnowledgeIngestSkill_Registration(t *testing.T) {
	s := NewKnowledgeIngestSkill(&fakeIngestor{})

	if s.Name() != "knowledge_ingest" {
		t.Errorf("skill name should be knowledge_ingest, got %q", s.Name())
	}
	// Must always go through LLM tool-calling, no keyword fast path
	// (same pattern as cron_task).
	if s.Match("把这段内容加入知识库") {
		t.Error("knowledge_ingest must not participate in keyword fast-path matching")
	}
	def := s.ToolDefinition()
	if def.Function.Name != "knowledge_ingest" {
		t.Errorf("ToolDefinition name should be knowledge_ingest, got %q", def.Function.Name)
	}
	// Upstream LLM identifier contract (BUG-20260523): OpenAI/Anthropic reject
	// tool names outside ^[a-zA-Z0-9_-]{1,128}$ with HTTP 400.
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`).MatchString(def.Function.Name) {
		t.Errorf("tool name %q violates the upstream LLM identifier pattern", def.Function.Name)
	}
	for _, required := range def.Function.Parameters.Required {
		if _, ok := def.Function.Parameters.Properties[required]; !ok {
			t.Errorf("required parameter %q missing from schema properties", required)
		}
	}
}

func TestBug20260611_KnowledgeIngestSkill_Execute(t *testing.T) {
	ing := &fakeIngestor{}
	s := NewKnowledgeIngestSkill(ing)

	result, err := s.Execute(context.Background(), map[string]any{
		"title":   "科技新闻摘要",
		"content": "今日要点：模型发布、芯片量产。",
		"source":  "cron:job-test",
	})
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(ing.calls) != 1 {
		t.Fatalf("AddDocument should be called once, got %d", len(ing.calls))
	}
	call := ing.calls[0]
	if call.title != "科技新闻摘要" || call.content != "今日要点：模型发布、芯片量产。" || call.source != "cron:job-test" {
		t.Errorf("AddDocument argument pass-through wrong: %+v", call)
	}
	if !strings.Contains(result.Content, "doc-test-1") {
		t.Errorf("result should echo the document ID for LLM confirmation, got %q", result.Content)
	}
}

func TestBug20260611_KnowledgeIngestSkill_EdgeCases(t *testing.T) {
	t.Run("empty content rejected", func(t *testing.T) {
		ing := &fakeIngestor{}
		s := NewKnowledgeIngestSkill(ing)
		if _, err := s.Execute(context.Background(), map[string]any{"title": "t"}); err == nil {
			t.Error("empty content should return an error")
		}
		if len(ing.calls) != 0 {
			t.Error("empty content must not reach the knowledge base")
		}
	})

	t.Run("missing title derived from content", func(t *testing.T) {
		ing := &fakeIngestor{}
		s := NewKnowledgeIngestSkill(ing)
		if _, err := s.Execute(context.Background(), map[string]any{
			"content": "这是一段没有显式标题的待入库内容，长度超过派生截断阈值用于验证截断逻辑是否生效。",
		}); err != nil {
			t.Fatalf("missing title should not error (a default title is derived): %v", err)
		}
		if len(ing.calls) != 1 || ing.calls[0].title == "" {
			t.Errorf("a non-empty title should be derived from content, got %+v", ing.calls)
		}
	})

	t.Run("knowledge base disabled", func(t *testing.T) {
		s := NewKnowledgeIngestSkill(nil)
		if _, err := s.Execute(context.Background(), map[string]any{"title": "t", "content": "c"}); err == nil {
			t.Error("nil ingestor should return an explicit error")
		}
	})

	t.Run("ingest failure propagated", func(t *testing.T) {
		ing := &fakeIngestor{err: errors.New("disk full")}
		s := NewKnowledgeIngestSkill(ing)
		if _, err := s.Execute(context.Background(), map[string]any{"title": "t", "content": "c"}); err == nil {
			t.Error("AddDocument failure must propagate so the ReAct loop sees it instead of hallucinating success")
		}
	})
}
