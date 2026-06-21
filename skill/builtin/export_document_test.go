package builtin

// 文档导出 Skill 测试，验证不变量：
//   - 8 种格式各产出非空 Path 且 Size>0。
//   - 非法格式 / 空 content → 明确报错（不静默）。
//   - raw_html / raw_tex 默认关（沙箱攻击面收敛）。
//   - ToolDefinition Required = ["content","format"]。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/render"
)

// fakeRenderer 记录最后一次 opts，并返回与格式相关的 fake 产物。
type fakeRenderer struct {
	lastOpts render.RenderOptions
	calls    int
}

func (f *fakeRenderer) Render(_ context.Context, _ string, format render.Format, opts render.RenderOptions) (*render.RenderResult, error) {
	f.calls++
	f.lastOpts = opts
	return &render.RenderResult{
		Path:        "/tmp/out." + string(format),
		Size:        42,
		ContentType: "application/octet-stream",
	}, nil
}

func TestExport_AllFormats_ProduceFile(t *testing.T) {
	r := &fakeRenderer{}
	s := NewExportDocumentSkill(r)
	for _, f := range render.AllFormats {
		res, err := s.Execute(context.Background(), map[string]any{
			"content": "# Hello\n\nbody",
			"format":  string(f),
		})
		if err != nil {
			t.Fatalf("format %s: Execute: %v", f, err)
		}
		rr, ok := res.Data.(*render.RenderResult)
		if !ok {
			t.Fatalf("format %s: Data should be *render.RenderResult, got %T", f, res.Data)
		}
		if rr.Path == "" {
			t.Fatalf("format %s: Path must be non-empty", f)
		}
		if rr.Size <= 0 {
			t.Fatalf("format %s: Size must be > 0, got %d", f, rr.Size)
		}
		if res.Metadata["path"] != rr.Path {
			t.Fatalf("format %s: metadata path mismatch", f)
		}
	}
}

func TestExport_InvalidFormat_Errors(t *testing.T) {
	s := NewExportDocumentSkill(&fakeRenderer{})
	if _, err := s.Execute(context.Background(), map[string]any{
		"content": "x", "format": "exe",
	}); err == nil {
		t.Fatalf("unsupported format must error loudly")
	}
}

func TestExport_EmptyContent_Errors(t *testing.T) {
	s := NewExportDocumentSkill(&fakeRenderer{})
	if _, err := s.Execute(context.Background(), map[string]any{
		"content": "   ", "format": "pdf",
	}); err == nil {
		t.Fatalf("empty content must error")
	}
}

func TestExport_NotEnabled_Errors(t *testing.T) {
	s := NewExportDocumentSkill(nil)
	if _, err := s.Execute(context.Background(), map[string]any{
		"content": "x", "format": "pdf",
	}); err == nil {
		t.Fatalf("nil renderer must error clearly")
	}
}

func TestExport_RawHTMLDefaultsOff(t *testing.T) {
	r := &fakeRenderer{}
	s := NewExportDocumentSkill(r)
	if _, err := s.Execute(context.Background(), map[string]any{
		"content": "<script>alert(1)</script>", "format": "html",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.lastOpts.AllowRawHTML {
		t.Fatalf("AllowRawHTML must default OFF (attack surface)")
	}
	if r.lastOpts.AllowRawTeX {
		t.Fatalf("AllowRawTeX must default OFF (attack surface)")
	}
}

func TestExport_LocaleDefault(t *testing.T) {
	r := &fakeRenderer{}
	s := NewExportDocumentSkill(r)
	if _, err := s.Execute(context.Background(), map[string]any{
		"content": "x", "format": "pdf",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if r.lastOpts.Locale != "zh-CN" {
		t.Fatalf("locale should default to zh-CN, got %q", r.lastOpts.Locale)
	}
}

func TestExport_ToolDef_Required(t *testing.T) {
	s := NewExportDocumentSkill(nil)
	req := s.ToolDefinition().Function.Parameters.Required
	if len(req) != 2 {
		t.Fatalf("Required must be [content format], got %v", req)
	}
	joined := strings.Join(req, ",")
	if !strings.Contains(joined, "content") || !strings.Contains(joined, "format") {
		t.Fatalf("Required must contain content+format, got %v", req)
	}
}
