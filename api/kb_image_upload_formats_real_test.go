package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

// TestKBImageUploadFormats_Real 覆盖用户真实上传入口：
// .png/.jpg/.jpeg/.webp/.gif → /knowledge/upload → 真实 VLM caption →
// AddImageDocument → embedding 入库 → search 召回。默认 skip。
//
//	HEX_RAG_E2E=1 HEX_E2E_SF_BASE/KEY go test ./api/ -run TestKBImageUploadFormats_Real -v -timeout 20m
func TestKBImageUploadFormats_Real(t *testing.T) {
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real image E2E：设 HEX_RAG_E2E=1 运行")
	}
	emb := kbRealEmbedderFromEnv(t)

	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	vlModel := kbdocEnvOr("HEX_E2E_SF_VL_MODEL", "Qwen/Qwen3-VL-8B-Instruct")
	captioner := knowledge.CaptionerFunc(func(ctx context.Context, img []byte, mime string) (string, error) {
		return kbImageVLMCaption(ctx, base, key, vlModel, img, mime)
	})

	// Probe 一次，避免每个子用例都用 400/429 噪声失败。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := captioner.Caption(ctx, kbImagePNG(t, kbSceneRedCircle()), "image/png"); err != nil {
		t.Skipf("视觉模型 %s 不可用：%v", vlModel, err)
	}

	srv := kbHandlerServer(t, emb, captioner)
	cases := []struct {
		file      string
		mime      string
		data      []byte
		query     string
		wantTitle string
		hints     []string
	}{
		{
			file: "red-circle.png", mime: "image/png", data: kbImagePNG(t, kbSceneRedCircle()),
			query: "红色圆形图片", wantTitle: "red-circle", hints: []string{"红"},
		},
		{
			file: "green-square.jpg", mime: "image/jpeg", data: kbImageJPEG(t, kbSceneGreenSquare()),
			query: "绿色方形图片", wantTitle: "green-square", hints: []string{"绿"},
		},
		{
			file: "blue-triangle.jpeg", mime: "image/jpeg", data: kbImageJPEG(t, kbSceneBlueTriangle()),
			query: "蓝色三角形图片", wantTitle: "blue-triangle", hints: []string{"蓝"},
		},
		{
			file: "yellow-star.gif", mime: "image/gif", data: kbImageGIF(t, kbSceneYellowStar()),
			query: "黄色星形图片", wantTitle: "yellow-star", hints: []string{"黄"},
		},
		{
			file: "dashboard.webp", mime: "image/webp", data: kbImageWebP(t),
			query: "蓝色柱状图 红色折线 绿色圆形 图表", wantTitle: "dashboard", hints: []string{"图"},
		},
	}

	for _, c := range cases {
		t.Run(strings.TrimPrefix(filepath.Ext(c.file), "."), func(t *testing.T) {
			rec := kbUploadMultipart(t, srv, c.file, c.data)
			if rec.Code != http.StatusOK {
				t.Fatalf("上传 %s 应 200，得 %d：%s", c.file, rec.Code, rec.Body.String())
			}
			var docResp knowledgeDocumentResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &docResp); err != nil {
				t.Fatalf("decode upload response: %v", err)
			}
			if docResp.SourceType != "image" {
				t.Fatalf("%s source_type 应为 image，得 %q", c.file, docResp.SourceType)
			}
			doc, err := srv.kb.GetDocument(context.Background(), docResp.ID)
			if err != nil {
				t.Fatalf("GetDocument %s: %v", c.file, err)
			}
			caption := strings.TrimPrefix(doc.Content, "【图像内容】\n")
			if strings.TrimSpace(caption) == "" {
				t.Fatalf("%s VLM caption 为空", c.file)
			}
			for _, h := range c.hints {
				if !strings.Contains(caption, h) {
					t.Fatalf("%s caption 未包含关键提示 %q：%q", c.file, h, caption)
				}
			}
			hits := kbSearch(t, srv, c.query)
			if kbTopTitle(hits) != c.wantTitle {
				t.Fatalf("%s 查询 %q 应召回 top=%q，实际 top=%q", c.file, c.query, c.wantTitle, kbTopTitle(hits))
			}
			t.Logf("  ✓ %-19s caption=%q top=%q", c.file, kbdocClip(caption, 70), kbTopTitle(hits))
		})
	}
}

func kbImageVLMCaption(ctx context.Context, base, key, model string, img []byte, mime string) (string, error) {
	dataURL := "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)
	payload, _ := json.Marshal(map[string]any{
		"model":       model,
		"temperature": 0,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "请用中文客观、简洁地描述这张图片的主色、形状、图表元素和可见内容。只输出描述本身。"},
				{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			},
		}},
	})
	return kbPostVisionCaption(ctx, base, key, payload)
}

func kbPostVisionCaption(ctx context.Context, base, key string, payload []byte) (string, error) {
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

func kbSceneRedCircle() *image.RGBA {
	img := kbNewCanvas(220, 180)
	kbFillCircle(img, 110, 90, 58, color.RGBA{220, 0, 0, 255})
	return img
}

func kbSceneGreenSquare() *image.RGBA {
	img := kbNewCanvas(220, 180)
	kbFillRect(img, 55, 35, 165, 145, color.RGBA{0, 170, 0, 255})
	return img
}

func kbSceneBlueTriangle() *image.RGBA {
	img := kbNewCanvas(220, 180)
	kbFillTriangle(img, 110, 30, 35, 150, 185, 150, color.RGBA{0, 60, 220, 255})
	return img
}

func kbSceneYellowStar() *image.RGBA {
	img := kbNewCanvas(220, 180)
	// 用交叉菱形近似高对比星形，GIF 调色板下更稳定。
	kbFillTriangle(img, 110, 20, 82, 155, 138, 155, color.RGBA{240, 190, 0, 255})
	kbFillTriangle(img, 40, 88, 180, 62, 180, 114, color.RGBA{240, 190, 0, 255})
	kbFillTriangle(img, 180, 88, 40, 62, 40, 114, color.RGBA{240, 190, 0, 255})
	return img
}

func kbNewCanvas(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{255, 255, 255, 255}), image.Point{}, draw.Src)
	return img
}

func kbFillRect(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.Set(x, y, c)
		}
	}
}

func kbFillCircle(img *image.RGBA, cx, cy, r int, c color.RGBA) {
	rr := r * r
	for y := cy - r; y <= cy+r; y++ {
		for x := cx - r; x <= cx+r; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= rr {
				img.Set(x, y, c)
			}
		}
	}
}

func kbFillTriangle(img *image.RGBA, x1, y1, x2, y2, x3, y3 int, c color.RGBA) {
	minX, maxX := min(x1, min(x2, x3)), max(x1, max(x2, x3))
	minY, maxY := min(y1, min(y2, y3)), max(y1, max(y2, y3))
	area := kbTriArea(x1, y1, x2, y2, x3, y3)
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			a1 := kbTriArea(x, y, x2, y2, x3, y3)
			a2 := kbTriArea(x1, y1, x, y, x3, y3)
			a3 := kbTriArea(x1, y1, x2, y2, x, y)
			if a1+a2+a3 == area {
				img.Set(x, y, c)
			}
		}
	}
}

func kbTriArea(x1, y1, x2, y2, x3, y3 int) int {
	v := (x1*(y2-y3) + x2*(y3-y1) + x3*(y1-y2))
	if v < 0 {
		return -v
	}
	return v
}

func kbImagePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func kbImageJPEG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func kbImageGIF(t *testing.T, img image.Image) []byte {
	t.Helper()
	paletted := image.NewPaletted(img.Bounds(), []color.Color{
		color.RGBA{255, 255, 255, 255},
		color.RGBA{240, 190, 0, 255},
		color.RGBA{0, 0, 0, 255},
	})
	draw.Draw(paletted, paletted.Bounds(), img, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := gif.Encode(&buf, paletted, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func kbImageWebP(t *testing.T) []byte {
	t.Helper()
	// 240x160 WebP: white dashboard-like image with blue bars, red line,
	// green circle and "ALPHA-47" text. Generated once from an HTML canvas.
	const b64 = "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"
	out, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
