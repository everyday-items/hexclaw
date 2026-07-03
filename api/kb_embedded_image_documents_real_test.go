package api

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"image"
	"image/color"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

// TestKBEmbeddedImageDocuments_Real 覆盖“复杂图片嵌在 PDF/Word 里”的真实上传边界：
// PDF/DOCX 文本层可解析、可入库、可召回；内嵌图片/扫描页/图表会进入 VLM/OCR 增强段落。
//
//	HEX_RAG_E2E=1 HEX_E2E_SF_BASE/KEY go test ./api/ -run TestKBEmbeddedImageDocuments_Real -v -timeout 20m
func TestKBEmbeddedImageDocuments_Real(t *testing.T) {
	if os.Getenv("HEX_RAG_E2E") != "1" {
		t.Skip("real embedded-image document E2E：设 HEX_RAG_E2E=1 运行")
	}
	emb := kbRealEmbedderFromEnv(t)
	base, key := os.Getenv("HEX_E2E_SF_BASE"), os.Getenv("HEX_E2E_SF_KEY")
	vlModel := kbdocEnvOr("HEX_E2E_SF_VL_MODEL", "Qwen/Qwen3-VL-8B-Instruct")
	captioner := knowledge.CaptionerFunc(func(ctx context.Context, img []byte, mime string) (string, error) {
		return kbImageVLMCaption(ctx, base, key, vlModel, img, mime)
	})
	if _, err := captioner.Caption(context.Background(), kbImagePNG(t, kbEmbeddedDashboardScene()), "image/png"); err != nil {
		t.Skipf("视觉模型 %s 不可用：%v", vlModel, err)
	}

	chart := kbEmbeddedDashboardScene()
	cases := []struct {
		file      string
		data      []byte
		visible   string
		query     string
		wantTitle string
	}{
		{
			file:      "embedded-aster.pdf",
			data:      kbPDFWithEmbeddedImage(t, "Project Aster torque checkpoint equals 42 newton meters before launch.", chart),
			visible:   "Project Aster torque checkpoint equals 42 newton meters before launch.",
			query:     "Project Aster torque 42 newton meters",
			wantTitle: "embedded-aster",
		},
		{
			file:      "embedded-borealis.docx",
			data:      kbDocxWithEmbeddedImage(t, "Project Borealis coolant baseline equals 17 liters after inspection.", kbImagePNG(t, chart)),
			visible:   "Project Borealis coolant baseline equals 17 liters after inspection.",
			query:     "Project Borealis coolant 17 liters",
			wantTitle: "embedded-borealis",
		},
	}

	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			srv := kbHandlerServer(t, emb, captioner)
			rec := kbUploadMultipart(t, srv, c.file, c.data)
			if rec.Code != http.StatusOK {
				t.Fatalf("上传 %s 应 200，得 %d：%s", c.file, rec.Code, rec.Body.String())
			}
			var docResp knowledgeDocumentResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &docResp); err != nil {
				t.Fatalf("decode upload response: %v", err)
			}
			if docResp.SourceType != "upload" {
				t.Fatalf("%s source_type 应为 upload，得 %q", c.file, docResp.SourceType)
			}
			doc, err := srv.kb.GetDocument(context.Background(), docResp.ID)
			if err != nil {
				t.Fatalf("GetDocument %s: %v", c.file, err)
			}
			if !strings.Contains(doc.Content, c.visible) {
				t.Fatalf("%s 文本层未进入知识库：content=%q", c.file, kbdocClip(doc.Content, 160))
			}
			if !strings.Contains(doc.Content, "文档视觉解析") {
				t.Fatalf("%s 应包含 OCR/VLM 视觉增强段落：content=%q", c.file, kbdocClip(doc.Content, 220))
			}
			hits := kbSearch(t, srv, c.query)
			if kbTopTitle(hits) != c.wantTitle {
				t.Fatalf("%s 查询 %q 应召回 top=%q，实际 top=%q", c.file, c.query, c.wantTitle, kbTopTitle(hits))
			}
			visualHits := kbSearch(t, srv, "蓝色柱状图 红色折线 绿色圆形")
			if kbTopTitle(visualHits) != c.wantTitle {
				t.Fatalf("%s 图片语义查询应召回 top=%q，实际 top=%q", c.file, c.wantTitle, kbTopTitle(visualHits))
			}
			t.Logf("  ✓ %-24s text+visual=%q textTop=%q visualTop=%q", c.file, kbdocClip(doc.Content, 120), kbTopTitle(hits), kbTopTitle(visualHits))
		})
	}
}

func TestKBEmbeddedImageOCRExpected_Green(t *testing.T) {
	chart := kbEmbeddedDashboardScene()
	captioner := knowledge.CaptionerFunc(func(_ context.Context, _ []byte, mime string) (string, error) {
		return "blue bars, red line, green circle, ALPHA-47 dashboard OCR text. mime=" + mime, nil
	})
	srv := kbHandlerServer(t, kwEmbedder{}, captioner)

	t.Run("docx_embedded_image", func(t *testing.T) {
		docx := kbDocxWithEmbeddedImage(t, "Visible text only.", kbImagePNG(t, chart))
		got, err := extractDocumentForKnowledge(context.Background(), ".docx", docx, srv.kb)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.ToLower(got.Text)
		for _, want := range []string{"visible text only", "blue bars", "red line", "green circle", "alpha-47"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("期望 DOCX 增强文本包含 %q，实际：%q", want, kbdocClip(joined, 220))
			}
		}
		if len(got.Warnings) != 0 {
			t.Fatalf("fake captioner 不应产生 warning，得 %#v", got.Warnings)
		}
	})

	t.Run("pdf_page_visual", func(t *testing.T) {
		if findTool("pdftoppm", pdftoppmKnownPaths...) == "" {
			t.Skip("缺少 pdftoppm，跳过 PDF 页面视觉增强单测")
		}
		pdf := kbPDFWithEmbeddedImage(t, "Visible text only.", chart)
		got, err := extractDocumentForKnowledge(context.Background(), ".pdf", pdf, srv.kb)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.ToLower(got.Text)
		for _, want := range []string{"visible text only", "blue bars", "red line", "green circle", "alpha-47"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("期望 PDF 增强文本包含 %q，实际：%q", want, kbdocClip(joined, 220))
			}
		}
	})
}

func TestKBEmbeddedImageUploadWithFakeCaptioner(t *testing.T) {
	chart := kbEmbeddedDashboardScene()
	captioner := knowledge.CaptionerFunc(func(_ context.Context, _ []byte, mime string) (string, error) {
		return "blue bars, red line, green circle, ALPHA-47 dashboard OCR text. mime=" + mime, nil
	})

	t.Run("docx_upload_indexes_embedded_image", func(t *testing.T) {
		srv := kbHandlerServer(t, kwEmbedder{}, captioner)
		rec := kbUploadMultipart(t, srv, "fake-dashboard.docx", kbDocxWithEmbeddedImage(t, "Visible text only.", kbImagePNG(t, chart)))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload status=%d body=%s", rec.Code, rec.Body.String())
		}
		var docResp knowledgeDocumentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &docResp); err != nil {
			t.Fatal(err)
		}
		if len(docResp.Warnings) != 0 {
			t.Fatalf("fake captioner 不应产生 warning，得 %#v", docResp.Warnings)
		}
		doc, err := srv.kb.GetDocument(context.Background(), docResp.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Visible text only.", "文档视觉解析", "blue bars", "ALPHA-47"} {
			if !strings.Contains(doc.Content, want) {
				t.Fatalf("上传后的 DOCX 正文应包含 %q，实际：%q", want, kbdocClip(doc.Content, 220))
			}
		}
		hits := kbSearch(t, srv, "ALPHA-47 green circle")
		if kbTopTitle(hits) != "fake-dashboard" {
			t.Fatalf("图片语义应可检索到 DOCX，top=%q", kbTopTitle(hits))
		}
	})

	t.Run("pdf_upload_indexes_page_visual", func(t *testing.T) {
		if findTool("pdftoppm", pdftoppmKnownPaths...) == "" {
			t.Skip("缺少 pdftoppm，跳过 PDF 页面视觉增强上传单测")
		}
		srv := kbHandlerServer(t, kwEmbedder{}, captioner)
		rec := kbUploadMultipart(t, srv, "fake-dashboard.pdf", kbPDFWithEmbeddedImage(t, "Visible text only.", chart))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload status=%d body=%s", rec.Code, rec.Body.String())
		}
		var docResp knowledgeDocumentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &docResp); err != nil {
			t.Fatal(err)
		}
		doc, err := srv.kb.GetDocument(context.Background(), docResp.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Visible text only.", "文档视觉解析", "blue bars", "ALPHA-47"} {
			if !strings.Contains(doc.Content, want) {
				t.Fatalf("上传后的 PDF 正文应包含 %q，实际：%q", want, kbdocClip(doc.Content, 220))
			}
		}
		hits := kbSearch(t, srv, "ALPHA-47 green circle")
		if kbTopTitle(hits) != "fake-dashboard" {
			t.Fatalf("图片语义应可检索到 PDF，top=%q", kbTopTitle(hits))
		}
	})

	t.Run("scanned_pdf_without_text_layer_indexes_visual", func(t *testing.T) {
		if findTool("pdftoppm", pdftoppmKnownPaths...) == "" {
			t.Skip("缺少 pdftoppm，跳过扫描 PDF 视觉增强上传单测")
		}
		srv := kbHandlerServer(t, kwEmbedder{}, captioner)
		rec := kbUploadMultipart(t, srv, "scanned-dashboard.pdf", kbPDFWithEmbeddedImage(t, "", chart))
		if rec.Code != http.StatusOK {
			t.Fatalf("upload status=%d body=%s", rec.Code, rec.Body.String())
		}
		var docResp knowledgeDocumentResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &docResp); err != nil {
			t.Fatal(err)
		}
		doc, err := srv.kb.GetDocument(context.Background(), docResp.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"文档视觉解析", "blue bars", "ALPHA-47"} {
			if !strings.Contains(doc.Content, want) {
				t.Fatalf("扫描 PDF 正文应包含 %q，实际：%q", want, kbdocClip(doc.Content, 220))
			}
		}
		hits := kbSearch(t, srv, "ALPHA-47 green circle")
		if kbTopTitle(hits) != "scanned-dashboard" {
			t.Fatalf("扫描 PDF 图片语义应可检索，top=%q", kbTopTitle(hits))
		}
	})
}

func kbEmbeddedDashboardScene() *image.RGBA {
	img := kbNewCanvas(240, 160)
	kbFillRect(img, 30, 96, 62, 136, color.RGBA{0, 92, 220, 255})
	kbFillRect(img, 78, 68, 110, 136, color.RGBA{0, 92, 220, 255})
	kbFillRect(img, 126, 42, 158, 136, color.RGBA{0, 92, 220, 255})
	kbFillTriangle(img, 34, 68, 94, 42, 154, 76, color.RGBA{220, 0, 0, 255})
	kbFillCircle(img, 194, 78, 24, color.RGBA{0, 165, 80, 255})
	return img
}

func kbPDFWithEmbeddedImage(t *testing.T, visible string, img image.Image) []byte {
	t.Helper()
	b := img.Bounds()
	var rawRGB bytes.Buffer
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			rawRGB.WriteByte(byte(r >> 8))
			rawRGB.WriteByte(byte(g >> 8))
			rawRGB.WriteByte(byte(bl >> 8))
		}
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(rawRGB.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	content := []byte(fmt.Sprintf("BT /F1 16 Tf 72 740 Td (%s) Tj ET\nq 240 0 0 160 72 520 cm /Im1 Do Q\n", kbPDFLiteralString(visible)))
	imageObj := bytes.Buffer{}
	fmt.Fprintf(&imageObj, "<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceRGB /BitsPerComponent 8 /Filter /FlateDecode /Length %d >>\nstream\n", b.Dx(), b.Dy(), compressed.Len())
	imageObj.Write(compressed.Bytes())
	imageObj.WriteString("\nendstream")

	objs := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> /XObject << /Im1 6 0 R >> /ProcSet [/PDF /Text /ImageC] >> /Contents 4 0 R >>"),
		[]byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content)),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
		imageObj.Bytes(),
	}

	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs)+1)
	for i, obj := range objs {
		offsets[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		out.Write(obj)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for i := 1; i <= len(objs); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}

func kbPDFLiteralString(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '(', ')':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n', '\r', '\t':
			b.WriteByte(' ')
		default:
			if r < 32 || r > 126 {
				b.WriteByte('?')
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func kbDocxWithEmbeddedImage(t *testing.T, visible string, pngBytes []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	addString := func(name, body string) {
		t.Helper()
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	addBytes := func(name string, body []byte) {
		t.Helper()
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
	}

	addString("[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8"?>`+
		`<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">`+
		`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>`+
		`<Default Extension="xml" ContentType="application/xml"/>`+
		`<Default Extension="png" ContentType="image/png"/>`+
		`<Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/>`+
		`</Types>`)
	addString("_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?>`+
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+
		`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/>`+
		`</Relationships>`)
	addString("word/_rels/document.xml.rels", `<?xml version="1.0" encoding="UTF-8"?>`+
		`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`+
		`<Relationship Id="rIdImage1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/image" Target="media/image1.png"/>`+
		`</Relationships>`)
	addString("word/document.xml", `<?xml version="1.0" encoding="UTF-8"?>`+
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" `+
		`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" `+
		`xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" `+
		`xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" `+
		`xmlns:pic="http://schemas.openxmlformats.org/drawingml/2006/picture">`+
		`<w:body><w:p><w:r><w:t>`+kbXMLText(visible)+`</w:t></w:r></w:p>`+
		`<w:p><w:r><w:drawing><wp:inline><a:graphic><a:graphicData>`+
		`<pic:pic><pic:blipFill><a:blip r:embed="rIdImage1"/></pic:blipFill></pic:pic>`+
		`</a:graphicData></a:graphic></wp:inline></w:drawing></w:r></w:p>`+
		`</w:body></w:document>`)
	addBytes("word/media/image1.png", pngBytes)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func kbXMLText(s string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return s
	}
	return b.String()
}
