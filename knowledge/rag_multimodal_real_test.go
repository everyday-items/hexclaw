package knowledge

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	_ "modernc.org/sqlite"
)

// 多模态入库的真模型 E2E（默认 skip，HEX_RAG_E2E=1 运行）。
//
// 用真实视觉模型（SiliconFlow Qwen-VL，可经 HEX_E2E_SF_VL_MODEL 覆盖）给程序生成的、
// 颜色/形状各异的图片转写中文描述，入库后验证：① caption 质量（正确识别主色）②可检索性
// （按颜色查询能召回对应图片，跨模态：图→文 RAG）③ source_type=image 标注与过滤。
//
//	HEX_RAG_E2E=1 HEX_E2E_SF_* go test ./knowledge/ -run TestRAGReal_Multimodal -v

// ── 测试图片生成（纯色形状 + 白底，确定性、无外部素材）──

func newCanvas(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{255, 255, 255, 255}) // 白底
		}
	}
	return img
}

func fillCircle(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, c)
			}
		}
	}
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.Set(x, y, c)
		}
	}
}

// fillTriangle 填充以 (ax,ay)(bx,by)(cx,cy) 为顶点的三角形（同号叉积点内测试）。
func fillTriangle(img *image.RGBA, ax, ay, bx, by, cx, cy int, c color.RGBA) {
	sign := func(px, py, qx, qy, rx, ry int) int {
		return (px-rx)*(qy-ry) - (qx-rx)*(py-ry)
	}
	minX, maxX := minOf(ax, bx, cx), maxOf(ax, bx, cx)
	minY, maxY := minOf(ay, by, cy), maxOf(ay, by, cy)
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			d1 := sign(x, y, ax, ay, bx, by)
			d2 := sign(x, y, bx, by, cx, cy)
			d3 := sign(x, y, cx, cy, ax, ay)
			hasNeg := d1 < 0 || d2 < 0 || d3 < 0
			hasPos := d1 > 0 || d2 > 0 || d3 > 0
			if !(hasNeg && hasPos) {
				img.Set(x, y, c)
			}
		}
	}
}

func minOf(xs ...int) int {
	m := xs[0]
	for _, x := range xs {
		if x < m {
			m = x
		}
	}
	return m
}

func maxOf(xs ...int) int {
	m := xs[0]
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

func pngBytes(t *testing.T, img *image.RGBA) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

// vlmCaption 调真实视觉模型转写图片（OpenAI 兼容 chat/completions + image_url，含瞬时抖动重试）。
func vlmCaption(ctx context.Context, base, key, model string, image []byte, mime string) (string, error) {
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
	payload, _ := json.Marshal(map[string]any{
		"model":       model,
		"temperature": 0,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "请用中文客观、简洁地描述这张图片的主要内容（包含主色和形状）。只输出描述本身。"},
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
		req, err := http.NewRequestWithContext(ctx, "POST",
			strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(payload))
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
			lastErr = &httpErr{resp.StatusCode, string(raw)}
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
			return "", &httpErr{200, "no choices"}
		}
		return strings.TrimSpace(out.Choices[0].Message.Content), nil
	}
	return "", lastErr
}

type httpErr struct {
	code int
	body string
}

func (e *httpErr) Error() string {
	return "vlm " + strconv.Itoa(e.code) + ": " + snippet([]byte(e.body))
}

func newImageRealMgr(t *testing.T, emb interface {
	Embed(context.Context, []string) ([][]float32, error)
	EmbedOne(context.Context, string) ([]float32, error)
	Dimension() int
}, cap Captioner) *Manager {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "kb.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	sp := splitter.NewMarkdownSplitter(splitter.WithMarkdownChunkSize(400), splitter.WithMarkdownChunkOverlap(80))
	cfg := coreCfgNoLLM()
	return NewManager(store, store, emb, WithSplitter(sp), WithHybridConfig(cfg), WithCaptioner(cap))
}

func TestRAGReal_Multimodal(t *testing.T) {
	emb := requireE2E(t)
	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	model := envOr("HEX_E2E_SF_VL_MODEL", "Qwen/Qwen3-VL-8B-Instruct")

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	cap := CaptionerFunc(func(ctx context.Context, img []byte, mime string) (string, error) {
		return vlmCaption(ctx, base, key, model, img, mime)
	})

	// 探针：视觉模型不可用（余额/模型名）→ 跳过而非误判为 bug。
	probe := newCanvas(32, 32)
	if _, err := cap.Caption(ctx, pngBytes(t, probe), "image/png"); err != nil {
		t.Skipf("视觉模型 %s 不可用：%v", model, err)
	}

	mgr := newImageRealMgr(t, emb, cap)

	// 三张颜色/形状各异的图片。
	red := newCanvas(160, 160)
	fillCircle(red, 80, 80, 64, color.RGBA{255, 0, 0, 255})
	green := newCanvas(160, 160)
	fillRect(green, 24, 24, 136, 136, color.RGBA{0, 170, 0, 255})
	blue := newCanvas(160, 160)
	fillTriangle(blue, 80, 20, 20, 140, 140, 140, color.RGBA{0, 0, 255, 255})

	cases := []struct {
		title, mime string
		img         []byte
		colorChar   string
		query       string
	}{
		{"图A", "image/png", pngBytes(t, red), "红", "哪张图片是红色的？"},
		{"图B", "image/png", pngBytes(t, green), "绿", "哪张图片是绿色的？"},
		{"图C", "image/png", pngBytes(t, blue), "蓝", "哪张图片是蓝色的？"},
	}

	for _, c := range cases {
		doc, err := mgr.AddImageDocument(ctx, c.title, c.img, c.mime, c.title+".png")
		if err != nil {
			t.Fatalf("AddImageDocument %s: %v", c.title, err)
		}
		if doc.SourceType != "image" {
			t.Errorf("%s source_type 应为 image，得 %q", c.title, doc.SourceType)
		}
		caption := strings.TrimPrefix(doc.Content, imageCaptionPrefix)
		t.Logf("  %s caption=%q", c.title, clip(caption, 70))
		// caption 质量：应正确识别主色。
		if !strings.Contains(caption, c.colorChar) {
			t.Errorf("%s caption 未识别主色 %q：%q", c.title, c.colorChar, caption)
		}
	}

	// 跨模态可检索性：按颜色查询召回对应图片，且标为 image。
	for _, c := range cases {
		hits, err := mgr.Search(ctx, c.query, 3)
		if err != nil {
			t.Fatalf("search %q: %v", c.query, err)
		}
		if len(hits) == 0 {
			t.Fatalf("查询 %q 零召回", c.query)
		}
		if hits[0].DocTitle != c.title {
			t.Errorf("查询 %q top-1 应为 %s，得 %q（图→文 RAG 召回错位）", c.query, c.title, hits[0].DocTitle)
		}
		if hits[0].Metadata["source_type"] != "image" {
			t.Errorf("%s 召回项 source_type 应为 image，得 %v", c.title, hits[0].Metadata)
		}
	}

	// 元数据过滤：仅 image 全召回，普通文本文档不混入。
	if _, err := mgr.AddDocument(ctx, "纯文本笔记", "这是一段不含颜色信息的普通文本笔记。", "test"); err != nil {
		t.Fatal(err)
	}
	imgOnly, err := mgr.SearchWithFilter(ctx, "图片", 10, Filter{SourceTypes: []string{"image"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range imgOnly {
		if h.Metadata["source_type"] != "image" {
			t.Errorf("image 过滤下混入非 image 文档：%v", h.Metadata)
		}
	}
	t.Logf("  ✓ 多模态：3 图 caption 主色正确 + 按色跨模态召回 + image 过滤纯净")
}
