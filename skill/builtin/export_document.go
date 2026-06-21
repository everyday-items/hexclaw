package builtin

// 文档导出 Skill —— Markdown → md/html/docx/pdf/epub/odt/rtf/txt。
//
// 行为：
//   - 包一层 render.Service（pandoc/typst sidecar），不经过 LLM。
//   - 返回落盘路径（RenderResult.Path）非 bytes —— 下游可作送达附件 / 入库 / 用户下载。
//   - raw_html / raw_tex 默认禁（沙箱攻击面收敛，不暴露 UI 开关）。

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/render"
	"github.com/hexagon-codes/hexclaw/skill"
)

// DocumentExporter 是 *render.Service 的窄注入面（便于测试 + 复用 Server 已持有的 renderSvc）。
type DocumentExporter interface {
	Render(ctx context.Context, content string, format render.Format, opts render.RenderOptions) (*render.RenderResult, error)
}

// ExportDocumentSkill 把 Markdown 渲染成可下载文档并返回文件路径。
type ExportDocumentSkill struct {
	render DocumentExporter
}

func NewExportDocumentSkill(r DocumentExporter) *ExportDocumentSkill {
	return &ExportDocumentSkill{render: r}
}

func (s *ExportDocumentSkill) Name() string { return "export_document" }
func (s *ExportDocumentSkill) Description() string {
	return "Render Markdown into a downloadable document and return its file path"
}

// Match 只走 LLM tool-call 主路径。
func (s *ExportDocumentSkill) Match(string) bool { return false }

func (s *ExportDocumentSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("export_document",
		"Render Markdown into a downloadable document (md/html/docx/pdf/epub/odt/rtf/txt) and return "+
			"its file path. Use when the user asks to export/save-as a document; the path can be sent or ingested.",
		&llm.Schema{Type: "object", Properties: map[string]*llm.Schema{
			"content":  {Type: "string", Description: "Markdown content to render (required)"},
			"format":   {Type: "string", Description: "md|html|docx|pdf|epub|odt|rtf|txt (required)"},
			"filename": {Type: "string", Description: "suggested file name (optional)"},
			"locale":   {Type: "string", Description: `output language, default "zh-CN"`},
		}, Required: []string{"content", "format"}})
}

func (s *ExportDocumentSkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
	if s.render == nil {
		return nil, fmt.Errorf("document export is not enabled")
	}
	content := firstStringArg(args, "content")
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("content must not be empty")
	}

	format := render.Format(strings.ToLower(firstStringArg(args, "format")))
	if !format.IsValid() {
		return nil, fmt.Errorf("unsupported format %q (want one of %v)", format, render.AllFormats)
	}

	locale := firstStringArg(args, "locale")
	if locale == "" {
		locale = "zh-CN"
	}
	// AllowRawHTML / AllowRawTeX 保持默认 false —— 不暴露 UI，沙箱攻击面收敛。
	opts := render.RenderOptions{Locale: locale}

	res, err := s.render.Render(ctx, content, format, opts)
	if err != nil {
		return nil, fmt.Errorf("render to %s failed: %w", format, err)
	}

	return &skill.Result{
		Content: fmt.Sprintf("Exported %s (%d bytes): %s", format, res.Size, res.Path),
		Data:    res, // Path/Size/ContentType/CacheHit → 下游（送达附件 / 入库 / 下载）
		Metadata: map[string]string{
			"format":       string(format),
			"path":         res.Path,
			"content_type": res.ContentType,
		},
	}, nil
}
