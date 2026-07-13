package api

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

type documentExtractionResult struct {
	Text      string
	PageCount int
	Warnings  []string
}

type visualSection struct {
	Title string
	Text  string
}

type renderedPDFPage struct {
	Page int
	Data []byte
}

var pdftoppmKnownPaths = []string{"/opt/homebrew/bin/pdftoppm", "/usr/local/bin/pdftoppm", "/usr/bin/pdftoppm"}

// extractDocumentForKnowledge 是知识库上传的统一摄取入口：
// 文本层解析 + 可选 OCR/VLM 视觉增强。视觉增强复用 knowledge.Manager 的 captioner，
// 不在 API 层另起模型选择路径，避免知识库与图片上传的视觉能力分叉。
func extractDocumentForKnowledge(ctx context.Context, ext string, data []byte, kb *knowledge.Manager) (documentExtractionResult, error) {
	var (
		res documentExtractionResult
		err error
	)
	switch ext {
	case ".txt", ".md", ".csv", ".json":
		res.Text = string(data)
	case ".docx":
		res.Text, err = extractDocxText(data)
		if err != nil {
			return res, err
		}
		sections, warnings := extractDocxVisualSections(ctx, data, kb)
		res.Warnings = append(res.Warnings, warnings...)
		res.Text = mergeVisualSections(res.Text, sections)
	case ".pdf":
		res.Text, res.PageCount, err = extractPDFText(ctx, data)
		if err != nil {
			res.Warnings = append(res.Warnings, "PDF 文本层解析失败，已尝试视觉 OCR/VLM："+err.Error())
			res.Text = ""
		}
		visualLimit, adaptiveWarning := pdfVisualPageLimit(res.Text, res.PageCount, docVisionMaxPages())
		if adaptiveWarning != "" {
			res.Warnings = append(res.Warnings, adaptiveWarning)
		}
		// 文本层完整的大 PDF 不在同步上传请求里再逐页调用 VLM；扫描版大 PDF 则
		// 有界抽样，避免 20 次慢本地视觉请求把整个 5 分钟预算耗尽。
		if visualLimit > 0 || adaptiveWarning == "" {
			sections, warnings := extractPDFVisualSectionsWithLimit(ctx, data, kb, visualLimit)
			res.Warnings = append(res.Warnings, warnings...)
			res.Text = mergeVisualSections(res.Text, sections)
		}
		if strings.TrimSpace(res.Text) == "" && err != nil {
			return res, err
		}
		if strings.TrimSpace(res.Text) != "" {
			err = nil
		}
	case ".doc":
		res.Text, err = extractDOCText(ctx, data)
		if err == nil {
			if docx, convErr := convertDOCToDOCX(ctx, data); convErr == nil {
				sections, warnings := extractDocxVisualSections(ctx, docx, kb)
				res.Warnings = append(res.Warnings, warnings...)
				res.Text = mergeVisualSections(res.Text, sections)
			} else {
				res.Warnings = append(res.Warnings, "DOC 内嵌图片增强失败，仅索引文本层："+convErr.Error())
			}
		}
	case ".pptx":
		res.Text, err = extractPPTXText(ctx, data)
	default:
		res.Text = string(data)
	}
	return res, err
}

func convertDOCToDOCX(ctx context.Context, data []byte) ([]byte, error) {
	bin := findTool("textutil", "/usr/bin/textutil")
	if bin == "" {
		return nil, fmt.Errorf("缺少 textutil")
	}
	dir, err := os.MkdirTemp("", "hexdoc-docx-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	in := filepath.Join(dir, "input.doc")
	out := filepath.Join(dir, "output.docx")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, err
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, "-convert", "docx", "-output", out, in)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("textutil 转 DOCX 失败: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return os.ReadFile(out)
}

func mergeVisualSections(text string, sections []visualSection) string {
	text = strings.TrimSpace(text)
	if len(sections) == 0 {
		return text
	}
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	b.WriteString("---\n\n# 文档视觉解析（OCR/VLM）\n")
	for _, s := range sections {
		if strings.TrimSpace(s.Text) == "" {
			continue
		}
		b.WriteString("\n## ")
		b.WriteString(s.Title)
		b.WriteString("\n")
		b.WriteString(strings.TrimSpace(s.Text))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func extractDocxVisualSections(ctx context.Context, data []byte, kb *knowledge.Manager) ([]visualSection, []string) {
	images, warnings := extractDocxImages(data, docVisionMaxImages())
	if len(images) == 0 {
		return nil, warnings
	}
	if kb == nil || !kb.HasCaptioner() {
		warnings = append(warnings, fmt.Sprintf("DOCX 含 %d 张内嵌图片；当前未配置视觉模型，仅索引文本层，图片语义未入库", len(images)))
		return nil, warnings
	}
	sections := make([]visualSection, 0, len(images))
	for i, img := range images {
		caption, err := kb.CaptionImage(ctx, img.data, img.mime)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("DOCX 内嵌图片 %s 转写失败：%v", img.name, err))
			continue
		}
		sections = append(sections, visualSection{
			Title: fmt.Sprintf("DOCX 内嵌图片 %d（%s）", i+1, img.name),
			Text:  caption,
		})
	}
	return sections, warnings
}

func extractPDFVisualSections(ctx context.Context, data []byte, kb *knowledge.Manager) ([]visualSection, []string) {
	return extractPDFVisualSectionsWithLimit(ctx, data, kb, docVisionMaxPages())
}

func extractPDFVisualSectionsWithLimit(ctx context.Context, data []byte, kb *knowledge.Manager, maxPages int) ([]visualSection, []string) {
	if maxPages <= 0 {
		return nil, []string{"PDF 视觉解析已关闭，仅索引文本层"}
	}
	if kb == nil || !kb.HasCaptioner() {
		return nil, []string{"PDF 未配置视觉模型，仅索引文本层；扫描页、内嵌图片、图表像素未入库"}
	}
	pages, err := renderPDFPages(ctx, data, maxPages, docVisionDPI())
	if err != nil {
		return nil, []string{"PDF 视觉解析失败，仅索引文本层：" + err.Error()}
	}
	if len(pages) == 0 {
		return nil, []string{"PDF 未渲染出可供视觉解析的页面，仅索引文本层"}
	}
	warnings := []string{}
	if len(pages) == maxPages {
		warnings = append(warnings, fmt.Sprintf("PDF 视觉解析最多处理前 %d 页；更长文档后续页面仅索引文本层", maxPages))
	}
	sections := make([]visualSection, 0, len(pages))
	for _, p := range pages {
		caption, err := kb.CaptionImage(ctx, p.Data, "image/png")
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("PDF 第 %d 页视觉转写失败：%v", p.Page, err))
			continue
		}
		sections = append(sections, visualSection{
			Title: fmt.Sprintf("PDF 第 %d 页视觉解析", p.Page),
			Text:  caption,
		})
	}
	return sections, warnings
}

const (
	largePDFPageThreshold     = 40
	largePDFVisualSamplePages = 3
	minPDFTextRunesPerPage    = 80
)

// pdfVisualPageLimit 为同步上传阶段选择视觉页预算。页数较少时保持原配置；百页 PDF
// 有可用文本层时直接索引全部文本，扫描版则只抽样少数页面，避免逐页 VLM 串行阻塞。
func pdfVisualPageLimit(text string, pageCount, configured int) (int, string) {
	if configured <= 0 || pageCount <= largePDFPageThreshold {
		return configured, ""
	}
	if pdfHasUsableTextLayer(text, pageCount) {
		return 0, fmt.Sprintf("PDF 共 %d 页且文本层可用，已优先完成全文文本索引；为避免大文档超时，本次跳过同步逐页视觉增强", pageCount)
	}
	limit := configured
	if limit > largePDFVisualSamplePages {
		limit = largePDFVisualSamplePages
	}
	return limit, fmt.Sprintf("PDF 共 %d 页且文本层不足，本次仅抽样前 %d 页做视觉解析以避免上传超时；建议按章节拆分扫描版 PDF 以完整入库", pageCount, limit)
}

func pdfHasUsableTextLayer(text string, pageCount int) bool {
	if pageCount <= 0 {
		return false
	}
	visibleRunes := 0
	for _, r := range text {
		switch r {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			visibleRunes++
		}
	}
	return visibleRunes >= pageCount*minPDFTextRunesPerPage
}

type docxImage struct {
	name string
	mime string
	data []byte
}

func extractDocxImages(data []byte, maxImages int) ([]docxImage, []string) {
	if maxImages <= 0 {
		return nil, []string{"DOCX 内嵌图片解析已关闭，仅索引文本层"}
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, []string{"DOCX 图片扫描失败：" + err.Error()}
	}
	var images []docxImage
	var warnings []string
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "word/media/") {
			continue
		}
		mime := mimeFromImageName(f.Name)
		if mime == "" {
			warnings = append(warnings, "跳过暂不支持的 DOCX 内嵌媒体："+f.Name)
			continue
		}
		if len(images) >= maxImages {
			warnings = append(warnings, fmt.Sprintf("DOCX 内嵌图片超过 %d 张，后续图片未做视觉入库", maxImages))
			break
		}
		rc, err := f.Open()
		if err != nil {
			warnings = append(warnings, "读取 DOCX 内嵌图片失败："+f.Name+": "+err.Error())
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(rc, int64(docVisionMaxImageBytes())+1))
		_ = rc.Close()
		if err != nil {
			warnings = append(warnings, "读取 DOCX 内嵌图片失败："+f.Name+": "+err.Error())
			continue
		}
		if len(raw) > docVisionMaxImageBytes() {
			warnings = append(warnings, fmt.Sprintf("跳过过大的 DOCX 内嵌图片 %s（超过 %dMB）", f.Name, docVisionMaxImageBytes()>>20))
			continue
		}
		if detected := http.DetectContentType(raw); strings.HasPrefix(detected, "image/") && mime == "application/octet-stream" {
			mime = detected
		}
		images = append(images, docxImage{name: f.Name, mime: mime, data: raw})
	}
	return images, warnings
}

func renderPDFPages(ctx context.Context, data []byte, maxPages, dpi int) ([]renderedPDFPage, error) {
	bin := findTool("pdftoppm", pdftoppmKnownPaths...)
	if bin == "" {
		return nil, fmt.Errorf("缺少 pdftoppm，无法渲染 PDF 扫描页/图表；请安装 poppler 或设置 HEXCLAW_PDFTOPPM")
	}
	dir, err := os.MkdirTemp("", "hexpdf-vision-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	pdfPath := filepath.Join(dir, "input.pdf")
	if err := os.WriteFile(pdfPath, data, 0o600); err != nil {
		return nil, err
	}
	prefix := filepath.Join(dir, "page")
	args := []string{"-png", "-r", strconv.Itoa(dpi), "-f", "1", "-l", strconv.Itoa(maxPages), pdfPath, prefix}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftoppm 执行失败: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	files, err := filepath.Glob(prefix + "-*.png")
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return pdfPageNumber(files[i]) < pdfPageNumber(files[j])
	})
	pages := make([]renderedPDFPage, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		if len(raw) > docVisionMaxImageBytes() {
			continue
		}
		pages = append(pages, renderedPDFPage{Page: pdfPageNumber(f), Data: raw})
	}
	return pages, nil
}

func pdfPageNumber(path string) int {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	idx := strings.LastIndex(base, "-")
	if idx < 0 {
		return 0
	}
	n, _ := strconv.Atoi(base[idx+1:])
	return n
}

func mimeFromImageName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg", ".jpe":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".tif", ".tiff":
		return "image/tiff"
	default:
		return ""
	}
}

func docVisionMaxPages() int {
	return intEnvBounded("HEXCLAW_DOC_VLM_MAX_PAGES", 20, 0, 200)
}

func docVisionMaxImages() int {
	return intEnvBounded("HEXCLAW_DOC_VLM_MAX_IMAGES", 50, 0, 500)
}

func docVisionDPI() int {
	return intEnvBounded("HEXCLAW_DOC_VLM_RENDER_DPI", 144, 72, 300)
}

func docVisionMaxImageBytes() int {
	return intEnvBounded("HEXCLAW_DOC_VLM_MAX_IMAGE_MB", 20, 1, 100) << 20
}

func intEnvBounded(name string, def, minVal, maxVal int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if v < minVal {
		return minVal
	}
	if v > maxVal {
		return maxVal
	}
	return v
}
