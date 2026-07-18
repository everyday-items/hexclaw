package engineadapter

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

// subjectKB 是支持多 source OR 过滤的内存 KB fake（对齐 knowledge.Filter.Sources
// “任一命中即可”语义），用于分科教材契约测试。
type subjectKB struct {
	docs []struct{ source, content string }
}

func (s *subjectKB) AddDocument(_ context.Context, title, content, source string) (*knowledge.Document, error) {
	s.docs = append(s.docs, struct{ source, content string }{source, content})
	return &knowledge.Document{Title: title, Content: content, Source: source}, nil
}

func (s *subjectKB) QueryWithFilter(_ context.Context, _ string, _ int, f knowledge.Filter) (string, error) {
	allowed := map[string]bool{}
	for _, src := range f.Sources {
		allowed[src] = true
	}
	var hits []string
	for _, d := range s.docs {
		if allowed[d.source] {
			hits = append(hits, d.content)
		}
	}
	return strings.Join(hits, "\n"), nil
}

// TestGroundingAdapter_SubjectRoundtrip 分科写入 roundtrip：数学教材以数学 scope 入库，
// 数学检索命中。
func TestGroundingAdapter_SubjectRoundtrip(t *testing.T) {
	a := NewGroundingAdapter(&subjectKB{})
	ctx := context.Background()
	if err := a.AddGroundingSubject(ctx, "mingming", "数学", "数学五上", "小数乘法竖式讲法"); err != nil {
		t.Fatalf("分科写入: %v", err)
	}
	text, found, err := a.GroundSubject(ctx, "mingming", "数学", "小数乘法", "五年级上")
	if err != nil || !found || !strings.Contains(text, "小数乘法竖式讲法") {
		t.Fatalf("分科检索 text=%q found=%v err=%v", text, found, err)
	}
}

// TestGroundingAdapter_CrossSubjectDoesNotLeak 跨学科不串：数学题不取语文教材；
// 无本学科教材时只回退通用（不分科）教材。
func TestGroundingAdapter_CrossSubjectDoesNotLeak(t *testing.T) {
	a := NewGroundingAdapter(&subjectKB{})
	ctx := context.Background()
	if err := a.AddGroundingSubject(ctx, "mingming", "语文", "语文五上", "比喻句教材讲法"); err != nil {
		t.Fatal(err)
	}

	// 只有语文教材：数学检索必须不命中（宁缺毋串）。
	if text, found, _ := a.GroundSubject(ctx, "mingming", "数学", "小数乘法", "五年级上"); found || text != "" {
		t.Fatalf("数学题不得取语文教材: text=%q found=%v", text, found)
	}

	// 加一份通用（不分科）教材：数学检索回退到通用。
	if err := a.AddGrounding(ctx, "mingming", "通用手册", "通用学习方法讲法"); err != nil {
		t.Fatal(err)
	}
	text, found, err := a.GroundSubject(ctx, "mingming", "数学", "小数乘法", "五年级上")
	if err != nil || !found || !strings.Contains(text, "通用学习方法讲法") || strings.Contains(text, "比喻句") {
		t.Fatalf("应只回退通用教材: text=%q found=%v err=%v", text, found, err)
	}

	// 有本学科教材后优先本学科，不再混入回退。
	if err := a.AddGroundingSubject(ctx, "mingming", "数学", "数学五上", "小数乘法竖式讲法"); err != nil {
		t.Fatal(err)
	}
	text, found, _ = a.GroundSubject(ctx, "mingming", "数学", "小数乘法", "五年级上")
	if !found || !strings.Contains(text, "小数乘法竖式讲法") || strings.Contains(text, "比喻句") {
		t.Fatalf("本学科优先: text=%q found=%v", text, found)
	}
}

// TestGroundingAdapter_LegacyDataCompatible 老数据兼容：分科上线前入库的（不分科）教材，
// 分科检索回退可见，旧 Ground 读侧行为不变。
func TestGroundingAdapter_LegacyDataCompatible(t *testing.T) {
	a := NewGroundingAdapter(&subjectKB{})
	ctx := context.Background()
	if err := a.AddGrounding(ctx, "mingming", "人教版五上", "老数据教材讲法"); err != nil {
		t.Fatal(err)
	}
	if text, found, err := a.GroundSubject(ctx, "mingming", "数学", "小数乘法", "五年级上"); err != nil || !found || !strings.Contains(text, "老数据教材讲法") {
		t.Fatalf("老数据应经通用回退可见: text=%q found=%v err=%v", text, found, err)
	}
	if text, found, err := a.Ground(ctx, "mingming", "小数乘法", "五年级上"); err != nil || !found || !strings.Contains(text, "老数据教材讲法") {
		t.Fatalf("旧 Ground 行为应不变: text=%q found=%v err=%v", text, found, err)
	}
}

// TestGroundingAdapter_EmptySubjectSeesAll subject 空 = 不分科旧语义：检索该实例全部教材
// （通用 + 各学科），不因分科上线而丢失可见性。
func TestGroundingAdapter_EmptySubjectSeesAll(t *testing.T) {
	a := NewGroundingAdapter(&subjectKB{})
	ctx := context.Background()
	if err := a.AddGrounding(ctx, "mingming", "通用", "通用讲法"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddGroundingSubject(ctx, "mingming", "数学", "数学五上", "小数乘法讲法"); err != nil {
		t.Fatal(err)
	}
	text, found, err := a.GroundSubject(ctx, "mingming", "", "小数乘法", "五年级上")
	if err != nil || !found || !strings.Contains(text, "通用讲法") || !strings.Contains(text, "小数乘法讲法") {
		t.Fatalf("不分科检索应见全部教材: text=%q found=%v err=%v", text, found, err)
	}
}

// TestGroundingAdapter_SubjectAgentIsolated 分科教材仍按 agent 隔离。
func TestGroundingAdapter_SubjectAgentIsolated(t *testing.T) {
	a := NewGroundingAdapter(&subjectKB{})
	ctx := context.Background()
	if err := a.AddGroundingSubject(ctx, "child-a", "数学", "数学五上", "child-a 数学教材"); err != nil {
		t.Fatal(err)
	}
	if text, found, _ := a.GroundSubject(ctx, "child-b", "数学", "小数乘法", "五年级上"); found || text != "" {
		t.Fatalf("跨实例泄漏: text=%q found=%v", text, found)
	}
	if _, found, err := a.GroundSubject(ctx, "", "数学", "小数乘法", "五年级上"); err == nil || found {
		t.Fatalf("空 agent 必须 fail closed: found=%v err=%v", found, err)
	}
}
