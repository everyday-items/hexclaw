package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// buildTestPDF 构造一个最小可解析 PDF（单页单文本流），用于验证后端 PDF 抽取链路。
func buildTestPDF(contentStream string) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	o1 := buf.Len()
	buf.WriteString("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	o2 := buf.Len()
	buf.WriteString("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	o3 := buf.Len()
	buf.WriteString("3 0 obj\n<< /Type /Page /Parent 2 0 R /Contents 4 0 R >>\nendobj\n")
	o4 := buf.Len()
	buf.WriteString(fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(contentStream), contentStream))
	xref := buf.Len()
	buf.WriteString("xref\n0 5\n0000000000 65535 f \n")
	buf.WriteString(fmt.Sprintf("%010d 00000 n \n", o1))
	buf.WriteString(fmt.Sprintf("%010d 00000 n \n", o2))
	buf.WriteString(fmt.Sprintf("%010d 00000 n \n", o3))
	buf.WriteString(fmt.Sprintf("%010d 00000 n \n", o4))
	buf.WriteString("trailer\n<< /Size 5 /Root 1 0 R >>\n")
	buf.WriteString(fmt.Sprintf("startxref\n%d\n%%%%EOF\n", xref))
	return buf.Bytes()
}

func TestExtractPDFText(t *testing.T) {
	text, _, err := extractPDFText(context.Background(), buildTestPDF("BT\n(Hello World) Tj\nET"))
	if err != nil {
		t.Fatalf("extractPDFText error: %v", err)
	}
	if !strings.Contains(text, "Hello World") {
		t.Fatalf("expected text to contain 'Hello World', got %q", text)
	}
}

func multipartBody(t *testing.T, filename string, data []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &body, w.FormDataContentType()
}

func postExtract(t *testing.T, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ct := multipartBody(t, filename, data)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/documents/extract", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	(&Server{}).handleExtractDocument(rec, req)
	return rec
}

func TestHandleExtractDocument(t *testing.T) {
	t.Run("PDF 经后端解析返回文本", func(t *testing.T) {
		rec := postExtract(t, "doc.pdf", buildTestPDF("BT\n(Hello World) Tj\nET"))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp DocumentExtractResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(resp.Text, "Hello World") {
			t.Fatalf("expected text contains 'Hello World', got %q", resp.Text)
		}
		if resp.FileName != "doc.pdf" {
			t.Fatalf("expected file_name doc.pdf, got %q", resp.FileName)
		}
	})

	t.Run("纯文本直接回传", func(t *testing.T) {
		rec := postExtract(t, "a.txt", []byte("plain text body"))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var resp DocumentExtractResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Text != "plain text body" {
			t.Fatalf("got %q", resp.Text)
		}
	})

	t.Run("不支持的格式返回 400", func(t *testing.T) {
		rec := postExtract(t, "img.png", []byte("\x89PNG\r\n"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})

	t.Run("缺文件返回 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/documents/extract", strings.NewReader(""))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=none")
		rec := httptest.NewRecorder()
		(&Server{}).handleExtractDocument(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
	})
}
