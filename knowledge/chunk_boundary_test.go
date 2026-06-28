package knowledge

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/rag/splitter"
)

// 分块边界质量（KB 深度质量门 #3）的确定性测试。
//
// 核心质量风险：事实被分块器在边界切断 → 没有任何一个 chunk 含完整事实 → 召回也拼不回
// 完整答案。本文件锁两条不变量（模型无关、确定性）：
//   ① 重叠生效：相邻 chunk 共享 overlap 文本（chunk 长度之和 > 原文长度），且分块器优先在
//      句子/段落边界切分，使每条事实句完整落在某个 chunk 内（不被腰斩到两个 chunk 都不含）。
//   ② 入库不丢词：跨整篇分布（含边界处）的关键词，经真实入库管线后都仍可被检索到。
// 跨 chunk 语义答案完整性（需要语义召回多个 chunk 拼回答案）见 rag_chunk_boundary_real_test.go。

// boundaryDoc 构造一篇含 8 条独立事实句的长文档，句子足够长以跨越多个 chunk 边界。
func boundaryDoc() (content string, facts []string) {
	facts = []string{
		"事实甲：泽塔公司成立于二零零一年春天总部位于云端城。",
		"事实乙：旗下旗舰产品代号猎户座主打超低延迟音频传输。",
		"事实丙：猎户座耳机的连续播放续航时间长达三十六小时整。",
		"事实丁：充电盒采用磁吸接口并支持十五分钟快充两小时使用。",
		"事实戊：降噪深度可达四十二分贝在地铁环境下表现尤为突出。",
		"事实己：通透模式能在保留环境声的同时清晰拾取人声对话。",
		"事实庚：固件可通过手机应用无线升级且向后兼容三代设备。",
		"事实辛：整机提供长达三年的有限保修并支持七天无理由退换。",
	}
	var sb strings.Builder
	for i, f := range facts {
		sb.WriteString(f)
		// 句间补足够填充，逼分块器在多处边界切分（填充本身不含事实关键字）。
		sb.WriteString("此处为补充说明文字仅用于撑开段落长度并不携带任何关键事实信息。")
		if i%2 == 1 {
			sb.WriteString("\n\n")
		}
	}
	return sb.String(), facts
}

// ① 重叠生效 + 句子完整性：相邻 chunk 重叠（总长 > 原文），每条事实句完整落在某个 chunk 内。
func TestChunkBoundary_SplitterOverlapAndIntegrity(t *testing.T) {
	content, facts := boundaryDoc()
	sp := splitter.NewRecursiveSplitter(
		splitter.WithRecursiveChunkSize(120),
		splitter.WithRecursiveChunkOverlap(30),
	)
	chunks, err := sp.Split(context.Background(), []hexagon.Document{{ID: "D", Content: content}})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(chunks) < 3 {
		t.Fatalf("文档应被切成 ≥3 个 chunk，得 %d", len(chunks))
	}

	// 重叠：所有 chunk 内容长度之和 > 原文长度（重叠重复了边界内容）。
	total := 0
	for _, c := range chunks {
		total += len([]rune(c.Content))
	}
	if total <= len([]rune(content)) {
		t.Errorf("重叠应使 chunk 总长(%d) > 原文长(%d)；疑似 overlap 未生效", total, len([]rune(content)))
	}

	// 句子完整性：每条事实句应完整落在某个 chunk 内（边界未腰斩事实）。
	for _, f := range facts {
		found := false
		for _, c := range chunks {
			if strings.Contains(c.Content, f) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("事实句被边界腰斩、无任一 chunk 完整包含：%q", f)
		}
	}
	t.Logf("✓ %d chunk，总长 %d > 原文 %d（重叠生效），8 条事实句均完整保留", len(chunks), total, len([]rune(content)))
}

// ② 入库不丢词：跨整篇分布的关键词经真实入库管线后都仍可被检索（FTS 路径，与 embedder 无关）。
func TestChunkBoundary_NoKeywordLostThroughIngest(t *testing.T) {
	ctx := context.Background()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init: %v", err)
	}
	sp := splitter.NewRecursiveSplitter(
		splitter.WithRecursiveChunkSize(120), splitter.WithRecursiveChunkOverlap(30))
	mgr := NewManager(store, store, &mockEmbedder{dim: 8},
		WithSplitter(sp), WithHybridConfig(imageTestConfig()))

	content, _ := boundaryDoc()
	doc, err := mgr.AddDocument(ctx, "泽塔产品手册", content, "test")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if doc.ChunkCount < 3 {
		t.Fatalf("入库 chunk 数应 ≥3，得 %d", doc.ChunkCount)
	}

	// 每个 chunk 边界附近的独特词都应能命中本文档（FTS 关键词检索）。
	keywords := []string{"云端城", "猎户座", "三十六小时", "十五分钟快充", "四十二分贝", "通透模式", "无线升级", "三年的有限保修"}
	for _, kw := range keywords {
		hits, err := mgr.Search(ctx, kw, 5)
		if err != nil {
			t.Fatalf("search %q: %v", kw, err)
		}
		if !hitsContainDoc(hits, doc.ID) {
			t.Errorf("关键词 %q 入库后丢失、检索不到（边界丢词）", kw)
		}
	}
	t.Logf("✓ %d chunk，8 个跨边界关键词入库后全部可检索（零丢词）", doc.ChunkCount)
}

func hitsContainDoc(hits []SearchHit, docID string) bool {
	for _, h := range hits {
		if h.DocID == docID {
			return true
		}
	}
	return false
}

// 防御性：确保填充文本本身不含事实关键字（否则上面的 contains 断言会假阳性）。
func TestChunkBoundary_FillerHasNoFactKeywords(t *testing.T) {
	const filler = "此处为补充说明文字仅用于撑开段落长度并不携带任何关键事实信息。"
	for _, kw := range []string{"云端城", "猎户座", "三十六", "四十二分贝", "保修"} {
		if strings.Contains(filler, kw) {
			t.Fatalf("填充文本不应含事实关键字 %q（测试设计自检）", kw)
		}
	}
}
