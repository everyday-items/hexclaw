package knowledge

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

// 元数据过滤（source / source_type / 创建日期）的确定性测试。
//
// 核心不变量：过滤在「打分 / topK / LIMIT 截断之前」生效——否则匹配文档可能因排在
// 候选池之外被静默漏召回（filter-after-topK 经典 bug）。源/类型下推 SQL（IN），
// 日期在 Go 层按真实 time.Time 比较（modernc 的 RFC3339 文本存储跨时区不可 SQL 比较）。

var (
	tJun01 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tJun10 = time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	tJun15 = time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)
	tJun20 = time.Date(2026, 6, 20, 0, 0, 0, 0, time.UTC)
	tJun22 = time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	tJun25 = time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
)

// addFilterDoc 向库写入一个「单 chunk」文档，可控源 / 源类型 / 创建时间 / 向量 / 正文。
func addFilterDoc(t *testing.T, s *SQLiteStore, id, source, sourceType, content string, vecSeed, dim int, created time.Time) {
	t.Helper()
	doc := &Document{
		ID: id, Title: id, Source: source, SourceType: sourceType,
		ChunkCount: 1, CreatedAt: created, UpdatedAt: created, Status: "indexed",
	}
	var emb []float32
	if vecSeed > 0 {
		emb = unitVec(vecSeed, dim)
	}
	ch := &Chunk{
		ID: id + "-c0", DocID: id, DocTitle: id, Source: source,
		ChunkCount: 1, Content: content, Index: 0, Embedding: emb, CreatedAt: created,
	}
	if err := s.Add(context.Background(), doc, []*Chunk{ch}); err != nil {
		t.Fatalf("add doc %s: %v", id, err)
	}
}

func resultDocIDs(rs []*SearchResult) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Chunk.DocID
	}
	sort.Strings(out)
	return out
}

func eqSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ─── 单元：Filter 语义 ──────────────────────────────────

func TestFilter_IsZeroAndNormalize(t *testing.T) {
	if !(Filter{}).IsZero() {
		t.Error("空 Filter 应 IsZero")
	}
	// 仅空白多值 → 规整后无约束 → IsZero
	if !(Filter{Sources: []string{"", "  "}, SourceTypes: []string{" "}}).IsZero() {
		t.Error("仅空白多值的 Filter 规整后应等价于无约束（IsZero）")
	}
	if (Filter{Sources: []string{"agent"}}).IsZero() {
		t.Error("有 source 的 Filter 不应 IsZero")
	}
	if (Filter{CreatedAfter: tJun01}).IsZero() {
		t.Error("有日期边界的 Filter 不应 IsZero")
	}
	// normalize 去空白并 trim
	n := (Filter{Sources: []string{" a ", "", "b"}}).normalize()
	if !eqSet(n.Sources, []string{"a", "b"}) {
		t.Errorf("normalize 应 trim 并去空，得 %v", n.Sources)
	}
}

func TestParseFilterDate(t *testing.T) {
	if z, err := ParseFilterDate(""); err != nil || !z.IsZero() {
		t.Errorf("空串应返回零值无错，得 (%v,%v)", z, err)
	}
	rfc, err := ParseFilterDate("2026-06-20T04:00:00Z")
	if err != nil || !rfc.Equal(tJun20.Add(4*time.Hour)) {
		t.Errorf("RFC3339 解析错: (%v,%v)", rfc, err)
	}
	dateOnly, err := ParseFilterDate("2026-06-20")
	if err != nil || !dateOnly.Equal(tJun20) {
		t.Errorf("YYYY-MM-DD 应按 UTC 零点: (%v,%v)", dateOnly, err)
	}
	if _, err := ParseFilterDate("garbage"); err == nil {
		t.Error("非法日期应报错")
	}
}

// ─── 单元：SQL 子句构造（仅源/类型，日期不下推）──────────

func TestBuildFilterClause(t *testing.T) {
	if c, a := buildFilterClause(Filter{}, "d"); c != "" || a != nil {
		t.Errorf("空 Filter 应返回空子句，得 (%q,%v)", c, a)
	}
	// 日期单独存在时不进 SQL 子句（由 Go 层处理）
	if c, a := buildFilterClause(Filter{CreatedAfter: tJun01, CreatedBefore: tJun25}, "d"); c != "" || a != nil {
		t.Errorf("日期不应下推 SQL，得 (%q,%v)", c, a)
	}
	c, a := buildFilterClause(Filter{Sources: []string{"x", "y"}}, "d")
	if c != "d.source IN (?,?)" || len(a) != 2 {
		t.Errorf("多值 source 子句不符: (%q,%v)", c, a)
	}
	c, a = buildFilterClause(Filter{Sources: []string{"x"}, SourceTypes: []string{"agent"}}, "d")
	if !strings.Contains(c, "d.source IN (?)") || !strings.Contains(c, "d.source_type IN (?)") || !strings.Contains(c, " AND ") || len(a) != 2 {
		t.Errorf("源+类型应 AND 串联两子句: (%q,%v)", c, a)
	}
}

// ─── 集成：VectorSearch 过滤 ────────────────────────────

func TestVectorSearch_FilterBySource(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	const dim = 16
	addFilterDoc(t, s, "A", "upload:a.pdf", "upload", "光合作用", 1, dim, tJun20)
	addFilterDoc(t, s, "B", "agent", "agent", "光合作用", 1, dim, tJun20)
	addFilterDoc(t, s, "C", "https://x", "url", "光合作用", 1, dim, tJun20)
	q := unitVec(1, dim)

	all, _ := s.VectorSearch(ctx, q, 10, Filter{})
	if !eqSet(resultDocIDs(all), []string{"A", "B", "C"}) {
		t.Fatalf("无过滤应返回全部 3 文档，得 %v", resultDocIDs(all))
	}
	got, _ := s.VectorSearch(ctx, q, 10, Filter{Sources: []string{"agent"}})
	if !eqSet(resultDocIDs(got), []string{"B"}) {
		t.Fatalf("按 source=agent 过滤应只剩 B，得 %v", resultDocIDs(got))
	}
}

func TestVectorSearch_FilterBySourceType_OR(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	const dim = 16
	addFilterDoc(t, s, "A", "upload:a.pdf", "upload", "光合作用", 1, dim, tJun20)
	addFilterDoc(t, s, "B", "agent", "agent", "光合作用", 1, dim, tJun20)
	addFilterDoc(t, s, "C", "https://x", "url", "光合作用", 1, dim, tJun20)

	got, _ := s.VectorSearch(ctx, unitVec(1, dim), 10, Filter{SourceTypes: []string{"upload", "url"}})
	if !eqSet(resultDocIDs(got), []string{"A", "C"}) {
		t.Fatalf("source_type∈{upload,url} 应返回 A,C（维度内 OR），得 %v", resultDocIDs(got))
	}
}

func TestVectorSearch_FilterByDateRange(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	const dim = 16
	addFilterDoc(t, s, "A", "agent", "agent", "光合作用", 1, dim, tJun01)
	addFilterDoc(t, s, "B", "agent", "agent", "光合作用", 1, dim, tJun20)
	addFilterDoc(t, s, "C", "agent", "agent", "光合作用", 1, dim, tJun25)
	q := unitVec(1, dim)

	after, _ := s.VectorSearch(ctx, q, 10, Filter{CreatedAfter: tJun15})
	if !eqSet(resultDocIDs(after), []string{"B", "C"}) {
		t.Fatalf("createdAfter=06-15 应返回 B,C，得 %v", resultDocIDs(after))
	}
	before, _ := s.VectorSearch(ctx, q, 10, Filter{CreatedBefore: tJun15})
	if !eqSet(resultDocIDs(before), []string{"A"}) {
		t.Fatalf("createdBefore=06-15 应返回 A，得 %v", resultDocIDs(before))
	}
	window, _ := s.VectorSearch(ctx, q, 10, Filter{CreatedAfter: tJun10, CreatedBefore: tJun22})
	if !eqSet(resultDocIDs(window), []string{"B"}) {
		t.Fatalf("[06-10,06-22] 窗口应只返回 B，得 %v", resultDocIDs(window))
	}
}

func TestVectorSearch_FilterAND_TypeAndDate(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	const dim = 16
	addFilterDoc(t, s, "A", "upload:a", "upload", "光合作用", 1, dim, tJun20) // 类型不符
	addFilterDoc(t, s, "B", "agent", "agent", "光合作用", 1, dim, tJun20)     // 命中
	addFilterDoc(t, s, "C", "https://x", "url", "光合作用", 1, dim, tJun25)   // 日期超界

	got, _ := s.VectorSearch(ctx, unitVec(1, dim), 10,
		Filter{SourceTypes: []string{"agent", "url"}, CreatedBefore: tJun22})
	if !eqSet(resultDocIDs(got), []string{"B"}) {
		t.Fatalf("type∈{agent,url} AND before 06-22 应只剩 B，得 %v", resultDocIDs(got))
	}
}

// ★ 关键：过滤必须在 topK 截断之前生效。
// 5 个 noise 文档与查询向量完全一致（cos=1，排在最前），target 向量较远（排第 6）。
// topK=3 + 仅 agent 过滤：若过滤发生在 topK 之后，top-3 全是 noise→被滤空（漏召回 bug）；
// 下推到扫描阶段则 noise 根本不进候选，target 被正确召回。
func TestVectorSearch_FilterPushdownBeforeTopK(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	const dim = 16
	for i := 0; i < 5; i++ {
		addFilterDoc(t, s, "N"+string(rune('0'+i)), "upload:n", "upload", "噪声", 1, dim, tJun20)
	}
	addFilterDoc(t, s, "T", "agent", "agent", "目标", 999, dim, tJun20)

	got, _ := s.VectorSearch(ctx, unitVec(1, dim), 3, Filter{SourceTypes: []string{"agent"}})
	if len(got) != 1 || got[0].Chunk.DocID != "T" {
		t.Fatalf("过滤须在 topK 截断前生效：应仅召回 target T，得 %v（=0 即 filter-after-topK 漏召回 bug）", resultDocIDs(got))
	}
}

func TestVectorSearch_EmptyFilterStringsNoAccidentalExclude(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	const dim = 16
	addFilterDoc(t, s, "A", "agent", "agent", "光合作用", 1, dim, tJun20)
	addFilterDoc(t, s, "B", "upload:x", "upload", "光合作用", 1, dim, tJun20)

	// 空串多值规整后应等价无过滤，绝不能生成 IN ('') 把全部排除。
	got, _ := s.VectorSearch(ctx, unitVec(1, dim), 10,
		Filter{Sources: []string{"", "  "}, SourceTypes: []string{" "}})
	if !eqSet(resultDocIDs(got), []string{"A", "B"}) {
		t.Fatalf("空白过滤值应等价无过滤返回全部，得 %v", resultDocIDs(got))
	}
}

// ─── 集成：TextSearch 过滤 ──────────────────────────────

func TestTextSearch_FilterBySource(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	addFilterDoc(t, s, "A", "upload:a", "upload", "the widget specification", 0, 0, tJun20)
	addFilterDoc(t, s, "B", "agent", "agent", "the widget manual", 0, 0, tJun20)

	all, _ := s.TextSearch(ctx, "widget", 10, Filter{})
	if !eqSet(resultDocIDs(all), []string{"A", "B"}) {
		t.Fatalf("无过滤 TextSearch 应返回 A,B，得 %v", resultDocIDs(all))
	}
	got, _ := s.TextSearch(ctx, "widget", 10, Filter{Sources: []string{"agent"}})
	if !eqSet(resultDocIDs(got), []string{"B"}) {
		t.Fatalf("TextSearch source=agent 应只剩 B，得 %v", resultDocIDs(got))
	}
}

// ★ TextSearch 过滤须在 SQL LIMIT 之前。noise 词频更高（bm25 更优、排在前），
// target 词频低（排第 6）。topK=3 + agent 过滤：下推后 noise 被 SQL 裁掉，target 召回。
func TestTextSearch_FilterPushdownBeforeLimit(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		addFilterDoc(t, s, "N"+string(rune('0'+i)), "upload:n", "upload", "widget widget widget", 0, 0, tJun20)
	}
	addFilterDoc(t, s, "T", "agent", "agent", "widget", 0, 0, tJun20)

	got, _ := s.TextSearch(ctx, "widget", 3, Filter{Sources: []string{"agent"}})
	if !eqSet(resultDocIDs(got), []string{"T"}) {
		t.Fatalf("过滤须在 LIMIT 前：应仅召回 target T，得 %v", resultDocIDs(got))
	}
}

func TestTextSearch_FilterByDate(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	addFilterDoc(t, s, "A", "agent", "agent", "delta keyword", 0, 0, tJun01)
	addFilterDoc(t, s, "B", "agent", "agent", "delta keyword", 0, 0, tJun20)

	got, _ := s.TextSearch(ctx, "delta", 10, Filter{CreatedAfter: tJun15})
	if !eqSet(resultDocIDs(got), []string{"B"}) {
		t.Fatalf("TextSearch createdAfter=06-15 应只剩 B，得 %v", resultDocIDs(got))
	}
}

// ─── 集成：fallbackTextSearch（LIKE 降级）也尊重过滤 ────

func TestFallbackTextSearch_RespectsFilter(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	addFilterDoc(t, s, "A", "upload:a", "upload", "zeta payload", 0, 0, tJun01)
	addFilterDoc(t, s, "B", "agent", "agent", "zeta payload", 0, 0, tJun20)

	bySource, err := s.fallbackTextSearch(ctx, []string{"zeta"}, 10, Filter{Sources: []string{"agent"}})
	if err != nil {
		t.Fatal(err)
	}
	if !eqSet(resultDocIDs(bySource), []string{"B"}) {
		t.Fatalf("fallback source=agent 应只剩 B，得 %v", resultDocIDs(bySource))
	}
	byDate, err := s.fallbackTextSearch(ctx, []string{"zeta"}, 10, Filter{CreatedAfter: tJun15})
	if err != nil {
		t.Fatal(err)
	}
	if !eqSet(resultDocIDs(byDate), []string{"B"}) {
		t.Fatalf("fallback createdAfter=06-15 应只剩 B，得 %v", resultDocIDs(byDate))
	}
}

// ─── Manager：过滤透传 + Metadata 回填 ──────────────────

func TestManager_SearchWithFilter_ThreadsFilter(t *testing.T) {
	fake := &rpFakeSearcher{
		vec:  []*SearchResult{sr("A", 0.9, 0)},
		text: []*SearchResult{sr("A", 0, 0.5)},
	}
	mgr := newRetrievalMgr(t, fake, baseCfg(), nil)
	f := Filter{Sources: []string{"upload:x"}, SourceTypes: []string{"agent"}, CreatedAfter: tJun15}
	if _, err := mgr.SearchWithFilter(context.Background(), "q", 3, f); err != nil {
		t.Fatal(err)
	}
	if !eqSet(fake.lastFilter.Sources, []string{"upload:x"}) ||
		!eqSet(fake.lastFilter.SourceTypes, []string{"agent"}) ||
		!fake.lastFilter.CreatedAfter.Equal(tJun15) {
		t.Fatalf("Manager 应把 Filter 透传到搜索层，得 %+v", fake.lastFilter)
	}
}

func TestManager_SearchWithFilter_PopulatesMetadata(t *testing.T) {
	s := deepStore(t)
	ctx := context.Background()
	addFilterDoc(t, s, "M", "upload:f.pdf", "upload", "metakeyword content", 0, 0, tJun20)

	// nil embedder → 纯关键词路径；通过 getChunksByIDs 回填 source_type + created_at。
	mgr := NewManager(s, s, nil, WithHybridConfig(baseCfg()))
	hits, err := mgr.Search(ctx, "metakeyword", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("应命中 metakeyword 文档")
	}
	md := hits[0].Metadata
	if md == nil || md["source_type"] != "upload" {
		t.Fatalf("Metadata.source_type 应为 upload，得 %v", md)
	}
	if _, ok := md["created_at"]; !ok {
		t.Fatalf("Metadata 应含 created_at，得 %v", md)
	}
}
