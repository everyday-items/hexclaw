package recall

import (
	"context"
	"testing"
	"time"
)

// rawStubSource 原样返回预置候选（不按 userID 过滤），用于检验 CuratedRetriever 自身的
// 防御性租户/有效性过滤（defense-in-depth）。
type rawStubSource struct{ items []Candidate }

func (s rawStubSource) Candidates(_ context.Context, _, _, _ string, _ int) ([]Candidate, error) {
	return s.items, nil
}

func newRetriever(items []Candidate, minScore float64, topK int) *CuratedRetriever {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	return &CuratedRetriever{
		Source:   rawStubSource{items},
		MinScore: minScore,
		TopK:     topK,
		Now:      func() time.Time { return now },
	}
}

func ids(rs []Result) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// 关键机制：minScore（相关度地板）剔除「不相关但新鲜」的噪声 —— 这是召回准确性的护栏，
// 而非依赖打分权重。朴素的「最近优先」召回会带上 E_noise，本检索器砍掉它、留下相关的旧条。
func TestRetrieve_MinScoreCutsFreshNoise(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	items := []Candidate{
		{ // 相关但旧：100 天前
			Entry:     Entry{ID: "answer", UserID: "u1", Type: TypeFact, Content: "用户喜欢深色主题", AccessedAt: now.AddDate(0, 0, -100)},
			BM25Score: 0.9,
		},
		{ // 不相关但全新
			Entry:     Entry{ID: "noise", UserID: "u1", Type: TypeFact, Content: "今天天气不错", AccessedAt: now},
			BM25Score: 0.05,
		},
	}
	r := newRetriever(items, 0.1, 5)
	got, err := r.Retrieve(context.Background(), "u1", "", "深色主题")
	if err != nil {
		t.Fatal(err)
	}
	got2 := ids(got)
	if !contains(got2, "answer") {
		t.Fatalf("相关旧条应被召回，得 %v", got2)
	}
	if contains(got2, "noise") {
		t.Fatalf("不相关新鲜噪声应被 minScore 砍掉，得 %v", got2)
	}
}

// 在「同样相关、同龄」的集合里，被高频召回（importance 高）的条目应排前 —— 行为反馈环生效。
func TestRetrieve_ImportanceRanksAmongEqual(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	items := []Candidate{
		{Entry: Entry{ID: "hot", UserID: "u1", Type: TypeFact, Content: "事实A", AccessedAt: now, RecallCount: 8}, BM25Score: 0.5},
		{Entry: Entry{ID: "cold", UserID: "u1", Type: TypeFact, Content: "事实B", AccessedAt: now, RecallCount: 0}, BM25Score: 0.5},
	}
	r := newRetriever(items, 0, 5)
	got, _ := r.Retrieve(context.Background(), "u1", "", "事实")
	if len(got) != 2 || got[0].ID != "hot" {
		t.Fatalf("高频召回条目应排第一，得 %v", ids(got))
	}
}

func TestRetrieve_TopKBound(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	var items []Candidate
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		items = append(items, Candidate{Entry: Entry{ID: id, UserID: "u1", Type: TypeFact, Content: "事实 " + id, AccessedAt: now}, BM25Score: 0.5})
	}
	r := newRetriever(items, 0, 3)
	got, _ := r.Retrieve(context.Background(), "u1", "", "事实")
	if len(got) != 3 {
		t.Fatalf("top-K 应限为 3，得 %d", len(got))
	}
}

// 多租户不串场（修 P4）：检索器自身按 userID 过滤，u2 的记忆永不出现在 u1 的召回里。
func TestRetrieve_TenantIsolation(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	items := []Candidate{
		{Entry: Entry{ID: "mine", UserID: "u1", Type: TypeFact, Content: "秘密项目X", AccessedAt: now}, BM25Score: 0.9},
		{Entry: Entry{ID: "theirs", UserID: "u2", Type: TypeFact, Content: "秘密项目X", AccessedAt: now}, BM25Score: 0.9},
	}
	r := newRetriever(items, 0, 5)
	got, _ := r.Retrieve(context.Background(), "u1", "", "项目X")
	g := ids(got)
	if !contains(g, "mine") || contains(g, "theirs") {
		t.Fatalf("应只召回本租户记忆，得 %v", g)
	}
}

// G3 时序取代：被 supersede 的旧条（ValidTo 已过）不进召回，只返当前有效的新条。
func TestRetrieve_SupersededExcluded(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	yesterday := now.AddDate(0, 0, -1)
	items := []Candidate{
		{Entry: Entry{ID: "old", UserID: "u1", Type: TypeFact, Subject: "居住地", Content: "用户住北京", AccessedAt: yesterday, ValidTo: &yesterday}, BM25Score: 0.9},
		{Entry: Entry{ID: "new", UserID: "u1", Type: TypeFact, Subject: "居住地", Content: "用户住上海", AccessedAt: now, ValidFrom: yesterday, Supersedes: "old"}, BM25Score: 0.9},
	}
	r := newRetriever(items, 0, 5)
	got, _ := r.Retrieve(context.Background(), "u1", "", "住哪")
	g := ids(got)
	if contains(g, "old") {
		t.Fatalf("被取代的旧事实不应召回，得 %v", g)
	}
	if !contains(g, "new") {
		t.Fatalf("当前有效的新事实应召回，得 %v", g)
	}
}

func TestRetrieve_EmptyQuery(t *testing.T) {
	r := newRetriever(nil, 0, 5)
	got, err := r.Retrieve(context.Background(), "u1", "", "   ")
	if err != nil || got != nil {
		t.Fatalf("空 query 应返回 nil，得 %v err=%v", got, err)
	}
}

func TestRelevance_HybridBlendAndDegrade(t *testing.T) {
	// 有向量层：0.7·cosine + 0.3·bm25。
	c := Candidate{VectorScore: 0.8, BM25Score: 0.4, HasVector: true}
	if got := relevance(c, true); !approx(got, 0.7*0.8+0.3*0.4) {
		t.Fatalf("hybrid 混合应为 0.68，得 %.4f", got)
	}
	// 全程无向量：软降级纯 bm25。
	d := Candidate{VectorScore: 0, BM25Score: 0.5, HasVector: false}
	if got := relevance(d, false); !approx(got, 0.5) {
		t.Fatalf("降级应为纯 bm25=0.5，得 %.4f", got)
	}
	// 混合集中某条无向量：vec 计 0，仅得 bm25 权重。
	e := Candidate{VectorScore: 0, BM25Score: 1.0, HasVector: false}
	if got := relevance(e, true); !approx(got, 0.3) {
		t.Fatalf("无向量条在混合集中应为 0.3·bm25=0.3，得 %.4f", got)
	}
}

// 降级路径端到端：整集无 embedding 时仍能按 BM25 正确排序召回（不硬 unavailable）。
func TestRetrieve_DegradeNoVector(t *testing.T) {
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	items := []Candidate{
		{Entry: Entry{ID: "hi", UserID: "u1", Type: TypeFact, Content: "高相关", AccessedAt: now}, BM25Score: 0.9, HasVector: false},
		{Entry: Entry{ID: "lo", UserID: "u1", Type: TypeFact, Content: "低相关", AccessedAt: now}, BM25Score: 0.3, HasVector: false},
	}
	r := newRetriever(items, 0, 5)
	got, _ := r.Retrieve(context.Background(), "u1", "", "相关")
	if len(got) != 2 || got[0].ID != "hi" {
		t.Fatalf("降级模式应按 BM25 排序，得 %v", ids(got))
	}
}
