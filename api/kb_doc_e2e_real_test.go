package api

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/rag/reranker"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/knowledge"
	_ "modernc.org/sqlite"
)

// 真实文档解析 + 真实模型向量召回端到端测试。默认 skip。运行：
//
//	HEX_RAG_E2E=1 go test ./api/ -run TestKBDoc -v -timeout 40m
//	# 多模型矩阵另需：HEX_E2E_SF_BASE / HEX_E2E_SF_KEY（硅基流动，OpenAI 兼容）
//
// 覆盖：PDF（poppler pdftotext，英文）/ DOCX（OOXML，中英双语）/ DOC（textutil，中文）/
// CSV（原文表格）的真实解析；解析出的真实文本经真实 embedding 模型入库检索，跨
// 语义改写 / 跨语种 / 表格事实 / 元数据过滤 等全场景核验召回质量。
// Excel(.xlsx/.xls) 在桌面端 SheetJS 解析，单独由前端 file-parser 测试覆盖。

// ── 语料：四种格式，主题各异，便于多文档判别 ──

const (
	kbdocPDFText = "Vector Databases and Embeddings\n\n" +
		"A vector database stores high-dimensional embedding vectors and retrieves them by " +
		"approximate nearest neighbor search. Cosine similarity measures the angle between two " +
		"embedding vectors. Hybrid retrieval combines dense vector search with BM25 keyword search, " +
		"and a cross-encoder reranker reorders the candidates to improve precision.\n"

	kbdocDocxText = "光合作用 Photosynthesis\n\n" +
		"光合作用是绿色植物利用光能，把二氧化碳和水合成为富能有机物并释放氧气的过程。" +
		"它分为光反应与暗反应两个阶段，主要在叶绿体中进行。" +
		"Photosynthesis converts light energy into chemical energy stored in glucose and releases oxygen.\n"

	kbdocDocText = "京杭大运河\n\n" +
		"京杭大运河是世界上里程最长、工程最大的古代人工运河，北起北京，南到杭州，" +
		"沟通了海河、黄河、淮河、长江、钱塘江五大水系，全长约一千八百公里，在南北漕运中作用巨大。\n"

	kbdocCSVText = "城市,人口万,特色\n" +
		"杭州,1200,西湖与电子商务\n" +
		"成都,2100,大熊猫与火锅\n" +
		"哈尔滨,1000,冰雪旅游与中央大街\n"

	// TXT/MD/JSON/PPTX：补齐全格式覆盖（处理器 passthrough / 最小 pptx），主题各异、同语种可判别
	kbdocTXTText  = "万里长城是中国古代规模最大的军事防御工程，东起山海关，西到嘉峪关，绵延上万里，是世界文化遗产。"
	kbdocMDText   = "# 故宫\n\n故宫又称紫禁城，是北京明清两代的皇家宫殿，世界上现存规模最大、保存最完整的木质结构古建筑群。"
	kbdocJSONText = `{"service":"缓存服务","cache_ttl_seconds":600,"max_connections":100,"eviction":"LRU"}`
	kbdocPPTXText = "长江是中国第一长河、亚洲最长的河流，发源于青藏高原，自西向东注入东海，全长约六千三百公里。"
)

type kbdocFixture struct {
	title   string // 入库标题 / 召回断言目标
	source  string // upload:<file>（source_type 推导为 upload）
	format  string // pdf / docx / doc / csv
	content string // 真实解析出的文本
}

// kbdocTool 返回外部工具路径（找不到返回空）。
func kbdocTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, c := range []string{"/opt/homebrew/bin/" + name, "/usr/local/bin/" + name, "/usr/bin/" + name} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// kbdocRun 把 in 写临时文件，调 bin 把它转成 ext 格式，返回输出文件字节。
func kbdocRun(t *testing.T, bin, srcExt, dstExt, text string, args func(in, out string) []string) []byte {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in"+srcExt)
	out := filepath.Join(dir, "out"+dstExt)
	if err := os.WriteFile(in, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args(in, out)...)
	if outErr, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s 生成失败: %v: %s", filepath.Base(bin), err, outErr)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("读生成文件: %v", err)
	}
	return data
}

// kbdocBuildFixtures 用真实工具生成 PDF/DOCX/DOC，再用生产解析器抽取文本；CSV 走原文。
// 缺工具的格式自动跳过（返回的 map 仅含成功项）。
func kbdocBuildFixtures(t *testing.T) []kbdocFixture {
	t.Helper()
	ctx := context.Background()
	var fx []kbdocFixture

	// PDF：cupsfilter 文本→PDF（英文，ASCII 干净），extractPDFText（poppler）抽取
	if cups := kbdocTool("cupsfilter"); cups != "" && kbdocTool("pdftotext") != "" {
		pdf := kbdocCupsfilter(t, kbdocPDFText) // cupsfilter 输出到 stdout
		text, _, err := extractPDFText(ctx, pdf)
		if err != nil {
			t.Fatalf("extractPDFText: %v", err)
		}
		fx = append(fx, kbdocFixture{"vectordb", "upload:vectordb.pdf", "pdf", text})
	} else {
		t.Log("跳过 PDF：缺 cupsfilter/pdftotext")
	}

	// DOCX：textutil 文本→docx（中英双语），extractDocxText（纯 Go OOXML）抽取
	if tu := kbdocTool("textutil"); tu != "" {
		docx := kbdocRun(t, tu, ".txt", ".docx", kbdocDocxText, func(in, out string) []string {
			return []string{"-convert", "docx", "-output", out, in}
		})
		text, err := extractDocxText(docx)
		if err != nil {
			t.Fatalf("extractDocxText: %v", err)
		}
		fx = append(fx, kbdocFixture{"photosynthesis", "upload:photosynthesis.docx", "docx", text})

		// DOC：textutil 文本→doc（中文），extractDOCText（textutil）抽取
		doc := kbdocRun(t, tu, ".txt", ".doc", kbdocDocText, func(in, out string) []string {
			return []string{"-convert", "doc", "-output", out, in}
		})
		text2, err := extractDOCText(ctx, doc)
		if err != nil {
			t.Fatalf("extractDOCText: %v", err)
		}
		fx = append(fx, kbdocFixture{"canal", "upload:canal.doc", "doc", text2})
	} else {
		t.Log("跳过 DOCX/DOC：缺 textutil")
	}

	// CSV：原文即解析结果（后端 string(data)）
	fx = append(fx, kbdocFixture{"cities", "upload:cities.csv", "csv", kbdocCSVText})

	// TXT/MD/JSON：处理器 passthrough（content = 原文）
	fx = append(fx,
		kbdocFixture{"greatwall", "upload:greatwall.txt", "txt", kbdocTXTText},
		kbdocFixture{"forbiddencity", "upload:forbiddencity.md", "md", kbdocMDText},
		kbdocFixture{"cacheconf", "upload:cacheconf.json", "json", kbdocJSONText},
	)
	// PPTX：构造最小可解析 pptx，经生产解析器 extractPPTXText 抽取
	if txt, err := extractPPTXText(ctx, buildMinimalPPTX(t, kbdocPPTXText)); err == nil && strings.TrimSpace(txt) != "" {
		fx = append(fx, kbdocFixture{"yangtze", "upload:yangtze.pptx", "pptx", txt})
	} else {
		t.Logf("跳过 PPTX：解析为空/失败 %v", err)
	}
	return fx
}

// kbdocCupsfilter 调 cupsfilter 把文本转 PDF（输出在 stdout）。
func kbdocCupsfilter(t *testing.T, text string) []byte {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(in, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(kbdocTool("cupsfilter"), in).Output()
	if err != nil || len(out) == 0 {
		t.Fatalf("cupsfilter 生成 PDF 失败: %v", err)
	}
	return out
}

// ── TestKBDocExtraction_Real：解析正确性（无模型）──
func TestKBDocExtraction_Real(t *testing.T) {
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real-doc E2E：设 HEX_RAG_E2E=1 运行")
	}
	fx := kbdocBuildFixtures(t)
	byFormat := map[string]kbdocFixture{}
	for _, f := range fx {
		byFormat[f.format] = f
		t.Logf("  解析 %-5s (%s) → %d 字符  预览=%q", f.format, f.source, len([]rune(f.content)), kbdocClip(f.content, 40))
	}

	// PDF：英文术语完整抽取（poppler）
	if f, ok := byFormat["pdf"]; ok {
		for _, kw := range []string{"vector database", "nearest neighbor", "cosine", "reranker"} {
			if !strings.Contains(strings.ToLower(f.content), kw) {
				t.Errorf("PDF 抽取缺关键词 %q", kw)
			}
		}
	}
	// DOCX：中英双语 CJK 不乱码（OOXML 纯 Go 抽取）
	if f, ok := byFormat["docx"]; ok {
		for _, kw := range []string{"光合作用", "叶绿体", "氧气", "Photosynthesis", "oxygen"} {
			if !strings.Contains(f.content, kw) {
				t.Errorf("DOCX 抽取缺 %q（CJK 乱码或丢失？）", kw)
			}
		}
	}
	// DOC：中文抽取（textutil）
	if f, ok := byFormat["doc"]; ok {
		for _, kw := range []string{"京杭大运河", "五大水系", "杭州"} {
			if !strings.Contains(f.content, kw) {
				t.Errorf("DOC 抽取缺 %q", kw)
			}
		}
	}
	// CSV：表格行列完整
	if f, ok := byFormat["csv"]; ok {
		for _, kw := range []string{"成都", "大熊猫与火锅", "哈尔滨", "人口万"} {
			if !strings.Contains(f.content, kw) {
				t.Errorf("CSV 缺 %q", kw)
			}
		}
	}
	// TXT/MD/JSON：passthrough 原文完整
	if f, ok := byFormat["txt"]; ok && !strings.Contains(f.content, "万里长城") {
		t.Errorf("TXT 缺关键词")
	}
	if f, ok := byFormat["md"]; ok && !strings.Contains(f.content, "紫禁城") {
		t.Errorf("MD 缺关键词")
	}
	if f, ok := byFormat["json"]; ok && !strings.Contains(f.content, "cache_ttl_seconds") {
		t.Errorf("JSON 缺关键词")
	}
	// PPTX：最小幻灯片文本抽取（CJK 不乱码）
	if f, ok := byFormat["pptx"]; ok && !strings.Contains(f.content, "长江") {
		t.Errorf("PPTX 抽取缺 %q（解析失败？）", "长江")
	}
}

// ── 召回场景 ──
type kbdocScenario struct {
	q         string
	wantTitle string
	crossLing bool // 跨语种：仅要求 top-3 命中（同语种要求 top-1）
	desc      string
}

var kbdocScenarios = []kbdocScenario{
	{"approximate nearest neighbor search over high-dimensional vectors with reranking", "vectordb", false, "EN 语义→PDF"},
	{"植物如何利用阳光制造养分并释放氧气", "photosynthesis", false, "CN 语义→DOCX"},
	{"the longest ancient man-made canal in China linking five major river systems", "canal", true, "EN→CN 跨语种→DOC"},
	{"哪个城市以大熊猫和火锅闻名", "cities", false, "CN 表格事实→CSV"},
	{"which Chinese city is famous for ice and snow tourism", "cities", true, "EN→CN 跨语种表格→CSV"},
	{"中国古代东起山海关西到嘉峪关的万里军事防御城墙", "greatwall", false, "CN 语义→TXT"},
	{"北京明清两代的皇家宫殿、又称紫禁城的古建筑群", "forbiddencity", false, "CN 语义→MD"},
	{"缓存服务的过期时间 TTL 配置了多少秒", "cacheconf", false, "CN 配置事实→JSON"},
	{"中国第一长河、发源于青藏高原注入东海的河流", "yangtze", false, "CN 语义→PPTX"},
}

// kbdocRealEmbedder 与生产同构（OpenAI 兼容 + 缓存 + 截断）+ 真机抗瞬时抖动重试。
func kbdocRealEmbedder(base, key, model string, dim int) hexagon.VectorEmbedder {
	if key == "" {
		key = "ollama"
	}
	prov := hexagon.NewOpenAI(key, hexagon.OpenAIWithBaseURL(base))
	emb := hexagon.NewOpenAIEmbedder(prov, hexagon.WithEmbedderModel(model), hexagon.WithEmbedderDimension(dim))
	return &kbdocRetryEmbedder{inner: knowledge.NewTruncatingEmbedder(hexagon.NewCachedEmbedder(emb), 0), tries: 4}
}

type kbdocRetryEmbedder struct {
	inner hexagon.VectorEmbedder
	tries int
}

func (e *kbdocRetryEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	var err error
	for i := 0; i < e.tries; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(i*2) * time.Second):
			}
		}
		var out [][]float32
		if out, err = e.inner.Embed(ctx, texts); err == nil {
			return out, nil
		}
	}
	return nil, err
}

func (e *kbdocRetryEmbedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	var err error
	for i := 0; i < e.tries; i++ {
		var out []float32
		if out, err = e.inner.EmbedOne(ctx, text); err == nil {
			return out, nil
		}
	}
	return nil, err
}

func (e *kbdocRetryEmbedder) Dimension() int { return e.inner.Dimension() }

func kbdocManager(t *testing.T, emb hexagon.VectorEmbedder, rr reranker.Reranker) *knowledge.Manager {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	sp := splitter.NewMarkdownSplitter(
		splitter.WithMarkdownChunkSize(400), splitter.WithMarkdownChunkOverlap(80),
		splitter.WithHeadersToSplit([]string{"#", "##", "###"}))
	// 默认纯向量+RRF（HyDE/contextual 关），聚焦"真解析文本的向量召回质量"；
	// rr 非空时启用专用 cross-encoder 重排（生产默认形态），用于演示对跨语种难例的纠正。
	cfg := knowledge.DefaultHybridConfig()
	cfg.ExpandEnabled, cfg.ContextualEnabled = false, false
	cfg.RerankEnabled = rr != nil
	opts := []knowledge.ManagerOption{knowledge.WithSplitter(sp), knowledge.WithHybridConfig(cfg)}
	if rr != nil {
		opts = append(opts, knowledge.WithDocReranker(rr))
	}
	return knowledge.NewManager(store, store, emb, opts...)
}

// kbdocRunRecall 对一个 embedding 模型：入库四种真实解析文档 → 跑全场景 → 报告 recall@1/@3 + 元数据过滤。
// rr 非空=重排开（生产形态，要求所有场景含跨语种都进 top-3）；rr 空=纯向量层（横评，跨语种难例只测量）。
func kbdocRunRecall(t *testing.T, fx []kbdocFixture, emb hexagon.VectorEmbedder, rr reranker.Reranker) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if vv, err := emb.Embed(ctx, []string{"探针"}); err != nil || len(vv) == 0 || len(vv[0]) == 0 {
		t.Skipf("embedder 不可用，跳过：%v", err)
	}
	reranked := rr != nil
	mgr := kbdocManager(t, emb, rr)
	for _, f := range fx {
		doc, err := mgr.AddDocument(ctx, f.title, f.content, f.source)
		if err != nil {
			t.Fatalf("入库 %s: %v", f.title, err)
		}
		if doc.ChunkCount == 0 {
			t.Fatalf("入库 %s：0 chunk", f.title)
		}
	}

	at1, at3, total := 0, 0, 0
	var crossMiss []string
	for _, sc := range kbdocScenarios {
		// 跳过未生成的格式对应场景
		if !kbdocHasTitle(fx, sc.wantTitle) {
			continue
		}
		total++
		hits, err := mgr.Search(ctx, sc.q, 3)
		if err != nil {
			t.Errorf("[%s] search: %v", sc.desc, err)
			continue
		}
		rank := kbdocRankOf(hits, sc.wantTitle)
		if rank == 1 {
			at1++
		}
		if rank >= 1 && rank <= 3 {
			at3++
		}
		t.Logf("  %-22s q=%q → top=%q rank=%d %s", sc.desc, kbdocClip(sc.q, 30), kbdocFirstTitle(hits), rank, kbdocTick(rank == 1))

		switch {
		case reranked:
			// 重排开（生产形态）：所有场景含跨语种都应进 top-3
			if rank < 1 || rank > 3 {
				t.Errorf("[重排] %s 应在 top-3，实际 rank=%d (%v)", sc.desc, rank, kbdocTitles(hits))
			}
		case !sc.crossLing:
			// 纯向量层：同语种必须 top-1
			if rank != 1 {
				t.Errorf("[纯向量] %s 同语种应 top-1，实际 rank=%d top=%q", sc.desc, rank, kbdocFirstTitle(hits))
			}
		default:
			// 纯向量层跨语种：只测量（弱模型在最难的跨语种表格例会跌出 top-3，由生产重排纠正——见 rerank_recovers 子测试）
			if rank < 1 || rank > 3 {
				crossMiss = append(crossMiss, sc.desc)
			}
		}
	}
	if total > 0 {
		t.Logf("  ▶ recall@1=%.2f (%d/%d)  recall@3=%.2f (%d/%d)  跨语种纯向量跌出top3:%v",
			float64(at1)/float64(total), at1, total, float64(at3)/float64(total), at3, total, crossMiss)
	}
	// 灾难下限：纯向量层 recall@3≥0.6（最弱模型实测 0.8）；重排层应 ~1.0。
	floor := 0.6
	if reranked {
		floor = 0.8
	}
	if total > 0 && float64(at3)/float64(total) < floor {
		t.Errorf("recall@3=%.2f < %.2f（灾难下限）", float64(at3)/float64(total), floor)
	}

	// 元数据过滤：按具体来源（文件名）精确过滤——只应召回该文档
	if kbdocHasTitle(fx, "cities") {
		csvHits, err := mgr.SearchWithFilter(ctx, "城市 特色", 5, knowledge.Filter{Sources: []string{"upload:cities.csv"}})
		if err != nil {
			t.Fatalf("filtered search: %v", err)
		}
		if len(csvHits) == 0 {
			t.Error("source 过滤后应仍召回 cities.csv")
		}
		for _, h := range csvHits {
			if h.Source != "upload:cities.csv" {
				t.Errorf("source=upload:cities.csv 过滤泄漏：%s", h.Source)
			}
		}
		// source_type=upload 应覆盖全部上传文档（健全性）
		upHits, _ := mgr.SearchWithFilter(ctx, "vector database embeddings", 5, knowledge.Filter{SourceTypes: []string{"upload"}})
		if len(upHits) == 0 {
			t.Error("source_type=upload 过滤应召回上传文档")
		}
		t.Logf("  ✓ 元数据过滤：source 精确过滤命中 %d 条（全部来自 cities.csv）", len(csvHits))
	}
}

// ── TestKBDocRecall_Real：真实模型矩阵召回 ──
func TestKBDocRecall_Real(t *testing.T) {
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real-model E2E：设 HEX_RAG_E2E=1 运行")
	}
	fx := kbdocBuildFixtures(t)
	t.Logf("已生成并解析 %d 个真实文档：%s", len(fx), kbdocFormats(fx))

	// 本地 Ollama（免密钥，真实模型）— 纯向量层
	ollamaBase := kbdocEnvOr("HEX_E2E_OLLAMA_BASE", "http://localhost:11434/v1")
	t.Run("ollama_qwen3_embedding_8b", func(t *testing.T) {
		model := kbdocEnvOr("HEX_E2E_OLLAMA_EMBED", "qwen3-embedding:8b")
		kbdocRunRecall(t, fx, kbdocRealEmbedder(
			ollamaBase, "", model, kbdocRealOllamaEmbeddingDimension(t, model),
		), nil)
	})

	// 硅基流动多模型矩阵（需 HEX_E2E_SF_KEY）
	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	if key == "" {
		t.Log("HEX_E2E_SF_KEY 未设 → 跳过硅基流动多模型矩阵（仅跑本地 Ollama）")
		return
	}
	// 纯向量层横评：各 embedding 模型在真实解析文档上的召回质量
	for _, model := range kbdocSplitCSV(kbdocEnvOr("HEX_E2E_SF_EMBED_MODELS",
		"BAAI/bge-m3,Qwen/Qwen3-Embedding-8B,Qwen/Qwen3-Embedding-4B,BAAI/bge-large-zh-v1.5")) {
		t.Run(strings.ReplaceAll(model, "/", "_"), func(t *testing.T) {
			kbdocRunRecall(t, fx, kbdocRealEmbedder(base, key, model, 1024), nil)
		})
	}

	// 生产形态：bge-m3（纯向量层在最难的跨语种表格例跌出 top-3）+ 专用 cross-encoder 重排，
	// 演示生产默认配置把所有场景（含跨语种）纠回 top-3 —— 印证"弱例靠重排兜底"。
	t.Run("rerank_recovers_crosslingual", func(t *testing.T) {
		rerankBase := strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/v1")
		rr := reranker.NewCohereReranker(key,
			reranker.WithCohereBaseURL(rerankBase),
			reranker.WithCohereModel(kbdocEnvOr("HEX_E2E_SF_RERANK", "BAAI/bge-reranker-v2-m3")),
			reranker.WithCohereTopK(50))
		kbdocRunRecall(t, fx, kbdocRealEmbedder(base, key, "BAAI/bge-m3", 1024), rr)
	})
}

// ── helpers ──

func kbdocRealOllamaEmbeddingDimension(t *testing.T, model string) int {
	t.Helper()
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "qwen3-embedding:8b":
		return 4096
	case "nomic-embed-text", "nomic-embed-text:latest", "nomic-embed-text:v1.5":
		return 768
	case "mxbai-embed-large", "mxbai-embed-large:latest", "bge-m3", "bge-m3:latest":
		return 1024
	default:
		t.Fatalf("Ollama embedding model %q has no trusted exact test dimension", model)
		return 0
	}
}

func kbdocHasTitle(fx []kbdocFixture, title string) bool {
	for _, f := range fx {
		if f.title == title {
			return true
		}
	}
	return false
}

func kbdocRankOf(hits []knowledge.SearchHit, title string) int {
	for i, h := range hits {
		if h.DocTitle == title {
			return i + 1
		}
	}
	return -1
}

func kbdocFirstTitle(hits []knowledge.SearchHit) string {
	if len(hits) > 0 {
		return hits[0].DocTitle
	}
	return "(none)"
}

func kbdocTitles(hits []knowledge.SearchHit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.DocTitle
	}
	return out
}

func kbdocFormats(fx []kbdocFixture) string {
	var s []string
	for _, f := range fx {
		s = append(s, f.format)
	}
	return strings.Join(s, "/")
}

func kbdocTick(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func kbdocClip(s string, n int) string {
	r := []rune(strings.ReplaceAll(s, "\n", " "))
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return string(r)
}

func kbdocEnvOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func kbdocSplitCSV(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
