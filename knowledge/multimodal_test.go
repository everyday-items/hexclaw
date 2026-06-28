package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 多模态图像摄取（AddImageDocument）的确定性测试（无网络、无真实模型）。
//
// 核心不变量：VLM caption → 文本 RAG 全链路，文档 SourceType="image"，可被既有文本
// 检索召回、可被 Filter{SourceTypes:["image"]} 精确筛选；缺 captioner / 空图像 /
// 空 caption 一律优雅报错，绝不摄取空或垃圾文档。

// fakeCaptioner 返回固定 caption，并记录最后一次收到的 image/mime，供断言透传。
type fakeCaptioner struct {
	caption  string
	err      error
	lastMime string
	lastLen  int
}

func (f *fakeCaptioner) Caption(_ context.Context, image []byte, mime string) (string, error) {
	f.lastMime = mime
	f.lastLen = len(image)
	if f.err != nil {
		return "", f.err
	}
	return f.caption, nil
}

// imageTestConfig 关掉所有需要 LLM 的特性（查询扩展/重排/contextual）并用朴素加权和，
// 让摄取与检索完全确定、可复现。
func imageTestConfig() HybridConfig {
	return HybridConfig{
		VectorWeight:  0.7,
		TextWeight:    0.3,
		MMRLambda:     0.7,
		TimeDecayDays: 0,
		MinScore:      0,
	}
}

func newImageMgr(t *testing.T, cap Captioner) (*Manager, *SQLiteStore) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("init store: %v", err)
	}
	opts := []ManagerOption{WithSplitter(testSplitter()), WithHybridConfig(imageTestConfig())}
	if cap != nil {
		opts = append(opts, WithCaptioner(cap))
	}
	return NewManager(store, store, &mockEmbedder{dim: 8}, opts...), store
}

// ① 摄取 + 检索 + source_type：caption 入库后可被匹配 caption 的查询召回，且标为 image。
func TestAddImageDocument_IngestAndRetrieve(t *testing.T) {
	ctx := context.Background()
	cap := &fakeCaptioner{caption: "这是一张 Acme 公司的发票扫描件，金额合计 1280 元。"}
	mgr, _ := newImageMgr(t, cap)

	img := []byte{0x89, 0x50, 0x4E, 0x47} // 任意非空字节（PNG 魔数）
	doc, err := mgr.AddImageDocument(ctx, "", img, "image/png", "invoice.png")
	if err != nil {
		t.Fatalf("AddImageDocument: %v", err)
	}
	if doc.SourceType != "image" {
		t.Fatalf("文档 source_type 应为 image，得 %q", doc.SourceType)
	}
	if cap.lastMime != "image/png" || cap.lastLen != len(img) {
		t.Fatalf("captioner 应收到原始 image/mime，得 mime=%q len=%d", cap.lastMime, cap.lastLen)
	}

	// 正文应为「【图像内容】」前缀块，使检索/回答端知道这是图像描述。
	full, err := mgr.GetDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(full.Content, imageCaptionPrefix) {
		t.Fatalf("正文应以图像内容前缀打头，得 %q", full.Content)
	}
	if !strings.Contains(full.Content, "Acme") {
		t.Fatalf("正文应含 caption，得 %q", full.Content)
	}

	// 可被匹配 caption 的查询召回，且 Metadata.source_type 回填为 image。
	hits, err := mgr.Search(ctx, "Acme", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("应能通过匹配 caption 的查询检索到图像文档")
	}
	if hits[0].Metadata["source_type"] != "image" {
		t.Fatalf("SearchHit.Metadata.source_type 应为 image，得 %v", hits[0].Metadata)
	}
}

// ② 默认标题：title 为空时从 caption 首行派生（裁剪到 24 rune）。
func TestAddImageDocument_DerivesTitle(t *testing.T) {
	ctx := context.Background()
	long := strings.Repeat("猫", 50)
	mgr, _ := newImageMgr(t, &fakeCaptioner{caption: long})
	doc, err := mgr.AddImageDocument(ctx, "   ", []byte{1, 2, 3}, "image/jpeg", "")
	if err != nil {
		t.Fatalf("AddImageDocument: %v", err)
	}
	if r := []rune(doc.Title); len(r) != 24 {
		t.Fatalf("空标题应从 caption 派生并裁剪到 24 rune，得 %d (%q)", len(r), doc.Title)
	}
}

// ③ 元数据过滤：Filter{SourceTypes:["image"]} 只召回图像，排除普通文本文档。
func TestAddImageDocument_FilterScopesToImages(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newImageMgr(t, &fakeCaptioner{caption: "一份共享关键字 ledger 的图片描述"})

	if _, err := mgr.AddImageDocument(ctx, "图片", []byte{9, 9, 9}, "image/png", "pic.png"); err != nil {
		t.Fatalf("AddImageDocument: %v", err)
	}
	if _, err := mgr.AddDocument(ctx, "普通笔记", "一份共享关键字 ledger 的普通文本笔记", "test"); err != nil {
		t.Fatalf("AddDocument: %v", err)
	}

	// 无过滤：两篇都命中。
	all, err := mgr.Search(ctx, "ledger", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 2 {
		t.Fatalf("无过滤应同时召回图像与文本文档，得 %d 条", len(all))
	}

	// 仅 image：只剩图像文档，排除文本文档。
	imgOnly, err := mgr.SearchWithFilter(ctx, "ledger", 10, Filter{SourceTypes: []string{"image"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(imgOnly) == 0 {
		t.Fatal("Filter{SourceTypes:[image]} 应召回图像文档")
	}
	for _, h := range imgOnly {
		if h.Metadata["source_type"] != "image" {
			t.Fatalf("image 过滤下不应出现非 image 文档，得 %v", h.Metadata)
		}
	}
}

// ④ 缺 captioner → 优雅报错（不静默写入垃圾），且确实未落库。
func TestAddImageDocument_NilCaptioner(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newImageMgr(t, nil) // 不注入 captioner
	_, err := mgr.AddImageDocument(ctx, "x", []byte{1, 2, 3}, "image/png", "x.png")
	if err == nil {
		t.Fatal("缺 captioner 应返回明确错误")
	}
	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 0 {
		t.Fatalf("缺 captioner 时不应写入任何文档，得 %d 个", len(docs))
	}
}

// ⑤ 空图像 → 报错。
func TestAddImageDocument_EmptyImage(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newImageMgr(t, &fakeCaptioner{caption: "不应被调用"})
	if _, err := mgr.AddImageDocument(ctx, "x", nil, "image/png", "x.png"); err == nil {
		t.Fatal("空图像应返回错误")
	}
	if _, err := mgr.AddImageDocument(ctx, "x", []byte{}, "image/png", "x.png"); err == nil {
		t.Fatal("零长图像应返回错误")
	}
}

// ⑥ 空 caption（VLM 返回空白）→ 报错，不摄取空文档。
func TestAddImageDocument_EmptyCaption(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newImageMgr(t, &fakeCaptioner{caption: "   \n  "})
	_, err := mgr.AddImageDocument(ctx, "x", []byte{1, 2, 3}, "image/png", "x.png")
	if err == nil {
		t.Fatal("空 caption 应返回错误")
	}
	docs, _ := mgr.ListDocuments(ctx)
	if len(docs) != 0 {
		t.Fatalf("空 caption 时不应写入文档，得 %d 个", len(docs))
	}
}

// ⑦ captioner 报错 → 透传为明确错误。
func TestAddImageDocument_CaptionerError(t *testing.T) {
	ctx := context.Background()
	mgr, _ := newImageMgr(t, &fakeCaptioner{err: errors.New("vision model down")})
	_, err := mgr.AddImageDocument(ctx, "x", []byte{1, 2, 3}, "image/png", "x.png")
	if err == nil || !strings.Contains(err.Error(), "vision model down") {
		t.Fatalf("captioner 错误应被透传，得 %v", err)
	}
}

// ⑧ CaptionerFunc 适配器与 imageSource/sourceTypeFromSource 约定。
func TestCaptionerFuncAndImageSource(t *testing.T) {
	called := false
	var f Captioner = CaptionerFunc(func(_ context.Context, _ []byte, _ string) (string, error) {
		called = true
		return "ok", nil
	})
	if _, err := f.Caption(context.Background(), []byte{1}, "image/png"); err != nil || !called {
		t.Fatalf("CaptionerFunc 应转调底层函数，called=%v err=%v", called, err)
	}

	// imageSource 幂等加前缀；sourceTypeFromSource 据前缀归类为 image。
	if got := imageSource("a.png"); got != "image:a.png" {
		t.Fatalf("imageSource 应加 image: 前缀，得 %q", got)
	}
	if got := imageSource("image:a.png"); got != "image:a.png" {
		t.Fatalf("imageSource 应幂等，得 %q", got)
	}
	if got := imageSource("  "); got != imageSourcePrefix {
		t.Fatalf("空 source 应退化为裸前缀，得 %q", got)
	}
	if got := sourceTypeFromSource("image:a.png"); got != "image" {
		t.Fatalf("sourceTypeFromSource(image:...) 应为 image，得 %q", got)
	}
	// 既有约定不回归。
	if got := sourceTypeFromSource("upload:a.pdf"); got != "upload" {
		t.Fatalf("upload 约定回归，得 %q", got)
	}
	if got := sourceTypeFromSource("agent"); got != "agent" {
		t.Fatalf("agent 约定回归，得 %q", got)
	}
}
