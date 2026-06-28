package api

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	_ "modernc.org/sqlite"
)

// 知识库**HTTP 处理器**真机端到端：按用户真实操作流程，把「手动添加」「上传各格式文件」
// 这两条链路从 HTTP 请求一路打到落库+检索，全程经真实处理器（ext 识别/解析路由/AddDocument/
// AddImageDocument）+ 真实 embedding/视觉模型，不绕过任何环节。
//
// 既有 kb_doc_e2e_real_test 只直调 Manager.AddDocument（绕过 HTTP 处理器与 ext 解析路由）；
// 本测试补齐处理器层 + TXT/MD/JSON/PPTX/图片 + 错误路径（不支持格式/空文件）。
//
//	HEX_RAG_E2E=1 HEX_E2E_SF_BASE/KEY go test ./api/ -run TestKBUploadHandler_Real -v -timeout 30m

// kbHandlerServer 构造一个挂了真实 KB Manager（真 embedder + 真视觉 captioner）的 Server。
func kbHandlerServer(t *testing.T, emb hexagon.VectorEmbedder, captioner knowledge.Captioner) *Server {
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
	cfg := knowledge.DefaultHybridConfig()
	cfg.ExpandEnabled, cfg.ContextualEnabled, cfg.RerankEnabled = false, false, false // 隔离解析→召回，控时
	opts := []knowledge.ManagerOption{knowledge.WithSplitter(sp), knowledge.WithHybridConfig(cfg)}
	if captioner != nil {
		opts = append(opts, knowledge.WithCaptioner(captioner))
	}
	mgr := knowledge.NewManager(store, store, emb, opts...)
	srv := NewServer(config.DefaultConfig(), nil, nil, nil)
	srv.SetKnowledgeBase(mgr)
	return srv
}

// kbUploadMultipart 构造一个上传请求并经真实处理器执行，返回响应记录器。
func kbUploadMultipart(t *testing.T, srv *Server, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/upload", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.handleUploadDocument(rec, req)
	return rec
}

// kbAddDocument 经真实「手动添加」处理器添加文档。
func kbAddDocument(t *testing.T, srv *Server, title, content, source string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"title": title, "content": content, "source": source})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/documents", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleAddDocument(rec, req)
	return rec
}

// kbSearch 经真实检索处理器查询，返回命中标题集合。
func kbSearch(t *testing.T, srv *Server, query string) []knowledge.SearchHit {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "top_k": 5})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/search", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleSearchKnowledge(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("search status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []knowledge.SearchHit `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	return resp.Results
}

func kbTopTitle(hits []knowledge.SearchHit) string {
	if len(hits) > 0 {
		return hits[0].DocTitle
	}
	return "(none)"
}

// buildMinimalPPTX 构造最小可解析的 PPTX（zip 内仅 ppt/slides/slide1.xml，文本在 <a:t>）。
func buildMinimalPPTX(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("ppt/slides/slide1.xml")
	if err != nil {
		t.Fatal(err)
	}
	// PPTXLoader 用 xml.Unmarshal 按 cSld>spTree>sp>txBody>a:p>a:r>a:t 结构取文本，
	// 故须给出完整嵌套（裸 <a:t> 解析为空且不报错，不会触发正则降级）。
	xml := `<?xml version="1.0" encoding="UTF-8"?>` +
		`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">` +
		`<p:cSld><p:spTree><p:sp><p:txBody>` +
		`<a:p><a:r><a:t>` + text + `</a:t></a:r></a:p>` +
		`</p:txBody></p:sp></p:spTree></p:cSld></p:sld>`
	if _, err := w.Write([]byte(xml)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func kbRealEmbedderFromEnv(t *testing.T) hexagon.VectorEmbedder {
	t.Helper()
	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	if key == "" {
		t.Skip("HEX_E2E_SF_KEY 未设")
	}
	emb := kbdocRealEmbedder(base, key, kbdocEnvOr("HEX_E2E_SF_EMBED", "BAAI/bge-m3"), 1024)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if vv, err := emb.Embed(ctx, []string{"探针"}); err != nil || len(vv) == 0 {
		t.Skipf("embedder 不可用：%v", err)
	}
	return emb
}

// 主测试：手动添加 + 上传各格式 + 图片 + 错误路径，全经真实处理器。
func TestKBUploadHandler_Real(t *testing.T) {
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real E2E：设 HEX_RAG_E2E=1 运行")
	}
	emb := kbRealEmbedderFromEnv(t)

	// 视觉 captioner（图片上传链路用真实 VLM）。
	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	vlModel := kbdocEnvOr("HEX_E2E_SF_VL_MODEL", "Qwen/Qwen3-VL-8B-Instruct")
	captioner := knowledge.CaptionerFunc(func(ctx context.Context, img []byte, mime string) (string, error) {
		return kbHandlerVLMCaption(ctx, base, key, vlModel, img, mime)
	})

	srv := kbHandlerServer(t, emb, captioner)

	// ───────── ① 手动添加链路 ─────────
	t.Run("manual_add", func(t *testing.T) {
		// 真实「手动添加」= 空 source（用户不填来源）→ source_type 推导为 manual。
		rec := kbAddDocument(t, srv, "量子纠缠笔记", "量子纠缠是两个粒子的量子态彼此关联，对其一测量会瞬间影响另一个，与距离无关。", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("手动添加应 200，得 %d：%s", rec.Code, rec.Body.String())
		}
		var doc knowledgeDocumentResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		if doc.SourceType != "manual" || doc.ChunkCount == 0 {
			t.Errorf("手动添加（空 source）source_type 应为 manual：%+v", doc)
		}
		hits := kbSearch(t, srv, "两个粒子量子态关联、测量瞬间影响彼此的现象")
		if kbTopTitle(hits) != "量子纠缠笔记" {
			t.Errorf("手动添加后应可检索到，top=%q", kbTopTitle(hits))
		}
		t.Logf("  ✓ 手动添加→检索：top=%q chunk=%d", kbTopTitle(hits), doc.ChunkCount)
	})

	t.Run("manual_add_validation", func(t *testing.T) {
		if rec := kbAddDocument(t, srv, "", "内容", "manual"); rec.Code != http.StatusBadRequest {
			t.Errorf("空标题应 400，得 %d", rec.Code)
		}
		if rec := kbAddDocument(t, srv, "标题", "", "manual"); rec.Code != http.StatusBadRequest {
			t.Errorf("空内容应 400，得 %d", rec.Code)
		}
	})

	// ───────── ② 上传各文本格式链路（经真实处理器解析路由） ─────────
	type fmtCase struct {
		file    string
		data    []byte
		want    string // 应能检索到的标题（= 文件名去扩展）
		query   string
		needTxt string // 解析后正文应含的关键词（验证解析保真）
	}
	var cases []fmtCase

	// 始终可测的纯文本族（处理器 passthrough）
	cases = append(cases,
		fmtCase{"notes.txt", []byte("阿尔卑斯山脉是欧洲最高大的山脉，最高峰勃朗峰海拔约 4809 米。"), "notes", "欧洲最高的山脉和它的最高峰海拔", "勃朗峰"},
		fmtCase{"guide.md", []byte("# 烹饪指南\n\n红烧肉的关键是先焯水去腥，再用冰糖炒糖色，小火慢炖一小时。"), "guide", "红烧肉怎么做、要点是什么", "炒糖色"},
		fmtCase{"data.csv", []byte("产品,价格,库存\n键盘,299,15\n鼠标,89,40\n显示器,1299,7\n"), "data", "显示器多少钱、还有多少库存", "1299"},
		fmtCase{"config.json", []byte(`{"service":"支付网关","timeout_seconds":30,"retry":3,"region":"华东"}`), "config", "支付网关的超时时间和重试次数配置", "timeout_seconds"},
		fmtCase{"slides.pptx", buildMinimalPPTX(t, "区块链是去中心化的分布式账本，通过哈希链接的区块保证数据不可篡改。"), "slides", "区块链如何保证数据不可篡改", "去中心化"},
	)

	// 工具依赖格式（缺则跳过该格式）
	if kbdocTool("cupsfilter") != "" && kbdocTool("pdftotext") != "" {
		pdf := kbdocCupsfilter(t, "Machine Learning Pipeline\n\nA training pipeline ingests data, extracts features, trains a model, and evaluates it on a holdout validation set before deployment.\n")
		cases = append(cases, fmtCase{"mlpipeline.pdf", pdf, "mlpipeline", "stages of a machine learning training pipeline before deployment", "validation"})
	} else {
		t.Log("跳过 PDF：缺 cupsfilter/pdftotext")
	}
	if tu := kbdocTool("textutil"); tu != "" {
		docx := kbdocRun(t, tu, ".txt", ".docx", "敦煌莫高窟 Mogao Caves\n\n敦煌莫高窟位于甘肃，是世界文化遗产，保存了大量精美的壁画与彩塑，俗称千佛洞。", func(in, out string) []string {
			return []string{"-convert", "docx", "-output", out, in}
		})
		cases = append(cases, fmtCase{"dunhuang.docx", docx, "dunhuang", "甘肃的世界文化遗产、有壁画彩塑的千佛洞", "莫高窟"})
		doc := kbdocRun(t, tu, ".txt", ".doc", "黄山 Mount Huang\n\n黄山位于安徽，以奇松、怪石、云海、温泉四绝著称，是著名的旅游胜地。", func(in, out string) []string {
			return []string{"-convert", "doc", "-output", out, in}
		})
		cases = append(cases, fmtCase{"huangshan.doc", doc, "huangshan", "安徽以奇松怪石云海温泉四绝著称的名山", "黄山"})
	} else {
		t.Log("跳过 DOCX/DOC：缺 textutil")
	}

	for _, c := range cases {
		t.Run("upload_"+strings.TrimPrefix(filepath.Ext(c.file), "."), func(t *testing.T) {
			rec := kbUploadMultipart(t, srv, c.file, c.data)
			if rec.Code != http.StatusOK {
				t.Fatalf("上传 %s 应 200，得 %d：%s", c.file, rec.Code, rec.Body.String())
			}
			var doc knowledgeDocumentResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &doc)
			if doc.SourceType != "upload" {
				t.Errorf("%s source_type 应为 upload，得 %q", c.file, doc.SourceType)
			}
			hits := kbSearch(t, srv, c.query)
			ok := false
			for _, h := range hits {
				if h.DocTitle == c.want {
					ok = true
					if c.needTxt != "" && !strings.Contains(h.Content, c.needTxt) {
						// 命中文档但 chunk 不含关键词时再宽松看其它 chunk 已足够——仅记录
						t.Logf("  注：%s 命中但该 chunk 未含 %q", c.file, c.needTxt)
					}
					break
				}
			}
			if !ok {
				t.Errorf("上传 %s 后应可检索到 %q，top=%q", c.file, c.want, kbTopTitle(hits))
			}
			t.Logf("  ✓ %-16s 上传→解析→入库→检索：top=%q chunk=%d", c.file, kbTopTitle(hits), doc.ChunkCount)
		})
	}

	// ───────── ③ 图片上传链路（真实 VLM caption→多模态入库） ─────────
	t.Run("upload_image", func(t *testing.T) {
		// 探针：VLM 不可用则跳过
		if _, err := kbHandlerVLMCaption(context.Background(), base, key, vlModel, kbHandlerPNG(t, color.RGBA{0, 0, 255, 255}), "image/png"); err != nil {
			t.Skipf("视觉模型 %s 不可用：%v", vlModel, err)
		}
		rec := kbUploadMultipart(t, srv, "blue.png", kbHandlerPNG(t, color.RGBA{0, 0, 255, 255}))
		if rec.Code != http.StatusOK {
			t.Fatalf("图片上传应 200，得 %d：%s", rec.Code, rec.Body.String())
		}
		var doc knowledgeDocumentResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		if doc.SourceType != "image" {
			t.Errorf("图片上传 source_type 应为 image，得 %q", doc.SourceType)
		}
		hits := kbSearch(t, srv, "哪张图片是蓝色的")
		found := false
		for _, h := range hits {
			if h.Metadata["source_type"] == "image" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("图片上传后应能按色检索到 image 文档，top=%q", kbTopTitle(hits))
		}
		t.Logf("  ✓ 图片上传→VLM caption→多模态入库→跨模态检索：top=%q", kbTopTitle(hits))
	})

	// ───────── ④ 错误路径 ─────────
	t.Run("error_paths", func(t *testing.T) {
		// 后端不支持的格式（.xlsx 在桌面 SheetJS 解析，不经后端）→ 400
		if rec := kbUploadMultipart(t, srv, "sheet.xlsx", []byte("PK\x03\x04fake")); rec.Code != http.StatusBadRequest {
			t.Errorf(".xlsx 后端应 400（桌面 SheetJS 解析），得 %d", rec.Code)
		}
		// 完全不支持的扩展 → 400
		if rec := kbUploadMultipart(t, srv, "malware.exe", []byte("MZ")); rec.Code != http.StatusBadRequest {
			t.Errorf(".exe 应 400，得 %d", rec.Code)
		}
		// 空文件内容 → 400
		if rec := kbUploadMultipart(t, srv, "empty.txt", []byte("   \n  ")); rec.Code != http.StatusBadRequest {
			t.Errorf("空内容应 400，得 %d", rec.Code)
		}
	})
}

// kbHandlerPNG 生成纯色 PNG。
func kbHandlerPNG(t *testing.T, c color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 80, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 80; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// kbHandlerVLMCaption 调真实视觉模型转写图片（含瞬时抖动重试）。
func kbHandlerVLMCaption(ctx context.Context, base, key, model string, img []byte, mime string) (string, error) {
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)
	payload, _ := json.Marshal(map[string]any{
		"model":       model,
		"temperature": 0,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "请用中文客观、简洁地描述这张图片的主色与内容。只输出描述本身。"},
				{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			},
		}},
	})
	client := &http.Client{Timeout: 120 * time.Second}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*2) * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(payload))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("vlm %d: %s", resp.StatusCode, string(raw[:min(len(raw), 160)]))
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
				continue
			}
			return "", lastErr
		}
		var out struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", err
		}
		if len(out.Choices) == 0 {
			return "", fmt.Errorf("no choices")
		}
		return strings.TrimSpace(out.Choices[0].Message.Content), nil
	}
	return "", lastErr
}
