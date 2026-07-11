package engineadapter

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

type groundingQuerySpy struct{ filter knowledge.Filter }

func (s *groundingQuerySpy) QueryWithFilter(_ context.Context, _ string, _ int, f knowledge.Filter) (string, error) {
	s.filter = f
	return "scoped textbook", nil
}

type scopedGroundingKB struct{ bySource map[string]string }

func (s *scopedGroundingKB) AddDocument(_ context.Context, title, content, source string) (*knowledge.Document, error) {
	if s.bySource == nil {
		s.bySource = map[string]string{}
	}
	s.bySource[source] = content
	return &knowledge.Document{Title: title, Content: content, Source: source}, nil
}
func (s *scopedGroundingKB) QueryWithFilter(_ context.Context, _ string, _ int, f knowledge.Filter) (string, error) {
	if len(f.Sources) != 1 {
		return "", nil
	}
	return s.bySource[f.Sources[0]], nil
}

func (s *groundingQuerySpy) AddDocument(_ context.Context, title, content, source string) (*knowledge.Document, error) {
	s.filter.Sources = []string{source}
	return &knowledge.Document{Title: title, Content: content, Source: source}, nil
}

func TestGroundingAdapter_UsesAgentScopedSource(t *testing.T) {
	spy := &groundingQuerySpy{}
	a := NewGroundingAdapter(spy)
	text, found, err := a.Ground(context.Background(), "mingming", "小数乘法", "五年级上")
	if err != nil || !found || text == "" {
		t.Fatalf("ground=%q found=%v err=%v", text, found, err)
	}
	want := GroundingSource("mingming")
	if len(spy.filter.Sources) != 1 || spy.filter.Sources[0] != want {
		t.Fatalf("filter sources=%v want [%q]", spy.filter.Sources, want)
	}
}

func TestGroundingAdapter_WriteUsesSameAgentScopeAsRead(t *testing.T) {
	spy := &groundingQuerySpy{}
	a := NewGroundingAdapter(spy)
	if err := a.AddGrounding(context.Background(), "mingming", "数学五上", "小数乘法教材讲法"); err != nil {
		t.Fatal(err)
	}
	want := GroundingSource("mingming")
	if len(spy.filter.Sources) != 1 || spy.filter.Sources[0] != want {
		t.Fatalf("write source=%v want [%q]", spy.filter.Sources, want)
	}
}

func TestGroundingAdapter_WriteReadClosureIsAgentIsolated(t *testing.T) {
	a := NewGroundingAdapter(&scopedGroundingKB{})
	ctx := context.Background()
	if err := a.AddGrounding(ctx, "child-a", "人教版五上", "child-a 教材"); err != nil {
		t.Fatal(err)
	}
	if text, found, err := a.Ground(ctx, "child-a", "小数乘法", "五年级上"); err != nil || !found || text != "child-a 教材" {
		t.Fatalf("owner read text=%q found=%v err=%v", text, found, err)
	}
	if text, found, err := a.Ground(ctx, "child-b", "小数乘法", "五年级上"); err != nil || found || text != "" {
		t.Fatalf("cross-agent leak text=%q found=%v err=%v", text, found, err)
	}
}

func TestGroundingAdapter_EmptyAgentFailsClosed(t *testing.T) {
	spy := &groundingQuerySpy{}
	if _, found, err := NewGroundingAdapter(spy).Ground(context.Background(), "", "小数乘法", "五年级上"); err == nil || found {
		t.Fatalf("empty agent must fail closed: found=%v err=%v", found, err)
	}
}

func TestGroundingSource_PreservesExactAgentIdentity(t *testing.T) {
	if GroundingSource("child") == GroundingSource("child ") {
		t.Fatal("distinct router agent names collapsed to one grounding source")
	}
}
