package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/transport"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/resourcegov"
	"github.com/hexagon-codes/toolkit/util/logger"
)

type documentExtractionResult struct {
	Text      string
	PageCount int
	Warnings  []string
	Pages     []documentPageExtraction
}

type documentPageExtraction struct {
	PageNumber       int
	SourcePageFrom   int
	SourcePageTo     int
	Mode             string
	Text             string
	SourceOffsetFrom int64
	SourceOffsetTo   int64
}

type visualSection struct {
	Title string
	Text  string
}

type renderedPDFPage struct {
	Page int
	Data []byte
	Err  error
}

var pdftoppmKnownPaths = []string{"/opt/homebrew/bin/pdftoppm", "/usr/local/bin/pdftoppm", "/usr/bin/pdftoppm"}
var pdfinfoKnownPaths = []string{"/opt/homebrew/bin/pdfinfo", "/usr/local/bin/pdfinfo", "/usr/bin/pdfinfo"}
var errPDFPageByteBudget = errors.New("rendered PDF page exceeds byte budget")
var errPDFTextByteBudget = errors.New("decompressed PDF text output exceeds byte budget")

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
		// 文本层完整的大 PDF 不重复逐页调用 VLM；文本层不足的扫描页则按
		// 明确页数预算处理，绝不再抽样前几页后冒充完整索引。
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
	dir, err := os.MkdirTemp("", "hexpdf-vision-source-*")
	if err != nil {
		return nil, []string{"PDF 视觉解析失败，仅索引文本层：" + err.Error()}
	}
	defer func() { _ = os.RemoveAll(dir) }()
	pdfPath := filepath.Join(dir, "input.pdf")
	if err := os.WriteFile(pdfPath, data, 0o600); err != nil {
		return nil, []string{"PDF 视觉解析失败，仅索引文本层：" + err.Error()}
	}
	info, err := inspectPDFDocument(ctx, pdfPath)
	if err != nil {
		return nil, []string{"PDF 视觉解析失败，仅索引文本层：" + err.Error()}
	}
	warnings := []string{}
	pageLimit := info.PageCount
	if pageLimit > maxPages {
		pageLimit = maxPages
		warnings = append(warnings, fmt.Sprintf("PDF 视觉解析最多处理前 %d 页；更长文档后续页面仅索引文本层", maxPages))
	}
	sections := make([]visualSection, 0, pageLimit)
	batchPages := intEnvBounded("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", 2, 1, 8)
	for first := 1; first <= pageLimit; first += batchPages {
		if err := ctx.Err(); err != nil {
			warnings = append(warnings, "PDF 视觉解析已取消："+err.Error())
			break
		}
		last := first + batchPages - 1
		if last > pageLimit {
			last = pageLimit
		}
		pages, renderErr := renderPDFPageBatch(
			ctx, pdfPath, first, last, docVisionDPI(), int64(docVisionMaxImageBytes()),
		)
		if renderErr != nil {
			warnings = append(warnings, fmt.Sprintf("PDF 第 %d-%d 页视觉渲染失败：%v", first, last, renderErr))
			continue
		}
		seen := make(map[int]struct{}, len(pages))
		for _, page := range pages {
			seen[page.Page] = struct{}{}
			if page.Err != nil {
				warnings = append(warnings, fmt.Sprintf("PDF 第 %d 页视觉渲染失败：%v", page.Page, page.Err))
				continue
			}
			caption, captionErr := kb.CaptionImage(ctx, page.Data, "image/png")
			if captionErr != nil {
				warnings = append(warnings, fmt.Sprintf("PDF 第 %d 页视觉转写失败：%v", page.Page, captionErr))
				continue
			}
			sections = append(sections, visualSection{
				Title: fmt.Sprintf("PDF 第 %d 页视觉解析", page.Page),
				Text:  caption,
			})
		}
		for page := first; page <= last; page++ {
			if _, ok := seen[page]; !ok {
				warnings = append(warnings, fmt.Sprintf("PDF 第 %d 页视觉渲染缺失", page))
			}
		}
	}
	if len(sections) == 0 {
		warnings = append(warnings, "PDF 未渲染出可供视觉解析的页面，仅索引文本层")
	}
	return sections, warnings
}

type asyncPDFExtractionLimits struct {
	MaxPages         int
	RenderBatchPages int
	DPI              int
	MaxPageBytes     int64
	MaxRenderedBytes int64
	MaxTextBytes     int64
	PageTimeout      time.Duration
	TotalTimeout     time.Duration
}

type pdfDocumentInfo struct {
	PageCount int
	Encrypted bool
}

type pdfPageExtractionError struct {
	PagesTotal  int
	FailedPages []int
	Reasons     []string
	Cause       error
}

func (e *pdfPageExtractionError) Error() string {
	if e == nil {
		return "knowledge: incomplete PDF page extraction"
	}
	message := fmt.Sprintf(
		"knowledge: incomplete PDF page extraction: pages_total=%d failed_pages=%v",
		e.PagesTotal, e.FailedPages,
	)
	if len(e.Reasons) > 0 {
		message += " reasons=" + strings.Join(e.Reasons, "; ")
	}
	return message
}

func (e *pdfPageExtractionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func asyncPDFLimitsFromEnv() asyncPDFExtractionLimits {
	return asyncPDFExtractionLimits{
		MaxPages:         docVisionMaxPages(),
		RenderBatchPages: intEnvBounded("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", 2, 1, 8),
		DPI:              docVisionDPI(),
		MaxPageBytes:     int64(docVisionMaxImageBytes()),
		MaxRenderedBytes: int64(intEnvBounded("HEXCLAW_DOC_VLM_MAX_RENDER_MB", 1024, 1, 4096)) << 20,
		MaxTextBytes:     int64(intEnvBounded("HEXCLAW_DOC_PDF_TEXT_MAX_MB", 64, 1, 512)) << 20,
		PageTimeout:      time.Duration(intEnvBounded("HEXCLAW_DOC_VLM_PAGE_TIMEOUT_SECONDS", 180, 1, 1800)) * time.Second,
		TotalTimeout:     time.Duration(intEnvBounded("HEXCLAW_DOC_VLM_TOTAL_TIMEOUT_SECONDS", 14400, 1, 86400)) * time.Second,
	}
}

// extractPDFForAsyncIngest parses a durable PDF source by page. Text-layer
// pages stay local and only low-text/scanned pages enter the configured
// captioner. Rendered pages are materialized in small batches and released
// before the next batch; a partial page set is never returned as a successful
// document.
func extractPDFForAsyncIngest(
	ctx context.Context,
	path string,
	kb *knowledge.Manager,
	limits asyncPDFExtractionLimits,
) (documentExtractionResult, error) {
	return extractPDFForAsyncIngestWithProgress(ctx, path, kb, limits, "", nil)
}

func extractPDFForAsyncIngestWithProgress(
	ctx context.Context,
	path string,
	kb *knowledge.Manager,
	limits asyncPDFExtractionLimits,
	sourceDigest string,
	progress knowledge.IngestPageProgress,
	governors ...*resourcegov.Governor,
) (result documentExtractionResult, extractionErr error) {
	startedAt := time.Now()
	extractionLog := logger.FromContext(ctx)
	extractionLog.Info("[knowledge] PDF extraction started", "source_digest", sourceDigest,
		"max_pages", limits.MaxPages, "page_timeout_ms", limits.PageTimeout.Milliseconds(),
		"total_timeout_ms", limits.TotalTimeout.Milliseconds())
	defer func() {
		extractionLog.Info("[knowledge] PDF extraction finished", "source_digest", sourceDigest,
			"elapsed_ms", time.Since(startedAt).Milliseconds(), "pages_total", result.PageCount,
			"text_bytes", len(result.Text), "warnings", result.Warnings, "error", extractionErr)
	}()
	if err := validateAsyncPDFLimits(limits); err != nil {
		return result, err
	}
	governor := firstResourceGovernor(governors)
	info, err := runGovernedCPU(ctx, governor, func() (pdfDocumentInfo, error) {
		return inspectPDFDocument(ctx, path)
	})
	if err != nil {
		return result, err
	}
	extractionLog.Info("[knowledge] PDF inspection completed", "source_digest", sourceDigest,
		"elapsed_ms", time.Since(startedAt).Milliseconds(), "pages_total", info.PageCount,
		"encrypted", info.Encrypted)
	result.PageCount = info.PageCount
	if info.Encrypted {
		return result, fmt.Errorf("%w: encrypted PDF is not supported", knowledge.ErrInvalidDocumentUpload)
	}
	if info.PageCount > limits.MaxPages {
		failedPages := boundedPDFPageRange(limits.MaxPages+1, info.PageCount, 1024)
		return result, &pdfPageExtractionError{
			PagesTotal:  info.PageCount,
			FailedPages: failedPages,
			Reasons: []string{fmt.Sprintf(
				"pages %d-%d exceed configured maximum %d; import rejected before OCR/VLM",
				limits.MaxPages+1, info.PageCount, limits.MaxPages,
			)},
			Cause: knowledge.ErrInvalidDocumentUpload,
		}
	}
	completed := map[int]knowledge.IngestPageCheckpoint{}
	if progress != nil {
		if len(sourceDigest) != 64 {
			return result, fmt.Errorf("%w: invalid PDF source digest", knowledge.ErrInvalidDocumentUpload)
		}
		if err := progress.SetPageTotal(ctx, sourceDigest, int64(info.PageCount)); err != nil {
			return result, err
		}
		checkpoints, err := progress.LoadCompletedPages(ctx, sourceDigest, int64(info.PageCount))
		if err != nil {
			return result, err
		}
		for _, checkpoint := range checkpoints {
			if checkpoint.PageNumber <= 0 || checkpoint.PageNumber > info.PageCount ||
				checkpoint.PagesTotal != int64(info.PageCount) || checkpoint.SourceDigest != sourceDigest ||
				strings.TrimSpace(checkpoint.Content) == "" {
				return result, fmt.Errorf("%w: invalid durable PDF page checkpoint", knowledge.ErrInvalidDocumentUpload)
			}
			if _, duplicate := completed[checkpoint.PageNumber]; duplicate {
				return result, fmt.Errorf("%w: duplicate durable PDF page checkpoint", knowledge.ErrInvalidDocumentUpload)
			}
			completed[checkpoint.PageNumber] = checkpoint
		}
	}

	type textLayerResult struct {
		pages   []string
		warning string
	}
	textStartedAt := time.Now()
	extractionLog.Info("[knowledge] PDF text extraction started", "source_digest", sourceDigest,
		"pages_total", info.PageCount, "completed_pages", len(completed), "max_text_bytes", limits.MaxTextBytes)
	textLayer, err := runGovernedCPU(ctx, governor, func() (textLayerResult, error) {
		pages, warning, extractErr := extractPDFTextPagesFromPath(
			ctx, path, info.PageCount, limits.MaxTextBytes,
		)
		return textLayerResult{pages: pages, warning: warning}, extractErr
	})
	extractionLog.Info("[knowledge] PDF text extraction finished", "source_digest", sourceDigest,
		"elapsed_ms", time.Since(textStartedAt).Milliseconds(), "pages", len(textLayer.pages),
		"warning", textLayer.warning, "error", err)
	if err != nil {
		return result, err
	}
	textPages, textWarning := textLayer.pages, textLayer.warning
	if textWarning != "" {
		result.Warnings = append(result.Warnings, textWarning)
	}
	result.Pages = make([]documentPageExtraction, info.PageCount)
	visualPages := make([]int, 0, info.PageCount)
	for page := 1; page <= info.PageCount; page++ {
		if checkpoint, ok := completed[page]; ok {
			result.Pages[page-1] = documentPageExtraction{
				PageNumber: page, SourcePageFrom: page, SourcePageTo: page,
				Mode: checkpoint.ExtractionMode, Text: strings.TrimSpace(checkpoint.Content),
			}
			continue
		}
		text := strings.TrimSpace(textPages[page-1])
		result.Pages[page-1] = documentPageExtraction{
			PageNumber: page, SourcePageFrom: page, SourcePageTo: page,
			Mode: "text", Text: text,
		}
		if !pdfPageHasUsableTextLayer(text) {
			result.Pages[page-1].Mode = "ocr_vlm"
			result.Pages[page-1].Text = ""
			visualPages = append(visualPages, page)
		}
	}
	// 所有页都先进入同一 canonical 文本坐标系，再提交 checkpoint；这样文本页、
	// 已完成 OCR 页和后续 OCR 页不会各自维护一套偏移算法。
	assignPDFPageSourceOffsets(result.Pages)
	pagePlans := make([]knowledge.IngestPagePlan, 0, len(result.Pages))
	for _, page := range result.Pages {
		mode := knowledge.IngestSegmentText
		if page.Mode == "ocr_vlm" {
			mode = knowledge.IngestSegmentVisual
		}
		pagePlans = append(pagePlans, knowledge.IngestPagePlan{
			PageNumber: page.PageNumber,
			Mode:       mode,
		})
	}
	segments, err := knowledge.PlanAdaptiveIngestSegments(pagePlans, limits.RenderBatchPages)
	if err != nil {
		return result, err
	}
	if segmentProgress, ok := progress.(knowledge.IngestSegmentProgress); ok {
		if err := segmentProgress.SaveSegmentPlan(ctx, sourceDigest, segments); err != nil {
			return result, err
		}
	}
	routeSnapshot, hasRouteSnapshot := knowledge.VisionRouteSnapshotFromContext(ctx)
	extractionLog.Info("[knowledge] PDF extraction plan ready", "source_digest", sourceDigest,
		"pages_total", info.PageCount, "completed_pages", len(completed), "visual_pages", visualPages,
		"segments", len(segments), "has_route_snapshot", hasRouteSnapshot,
		"provider", routeSnapshot.ProviderName, "model", routeSnapshot.Model,
		"capabilities", routeSnapshot.Capabilities, "has_captioner", kb != nil && kb.HasCaptioner())
	if snapshot, ok := knowledge.VisionRouteSnapshotFromContext(ctx); ok &&
		len(visualPages) > 0 && !snapshot.HasCapability("vision") {
		return result, knowledge.NewVisionModelRequiredError(snapshot, visualPages)
	}
	if len(visualPages) > 0 && (kb == nil || !kb.HasCaptioner()) {
		failed := make(map[int]string, len(visualPages))
		for _, page := range visualPages {
			failed[page] = "OCR/VLM is not configured"
		}
		failedPages, reasons := orderedPDFFailures(failed)
		return result, &pdfPageExtractionError{
			PagesTotal: info.PageCount, FailedPages: failedPages, Reasons: reasons,
		}
	}
	invocationProgress, invocationSupported := progress.(knowledge.OCRPageInvocationContextProgress)

	failed := make(map[int]string)
	permanentFailure := false

	totalCtx, cancelTotal := context.WithTimeout(ctx, limits.TotalTimeout)
	defer cancelTotal()
	var renderedBytes int64
	for start := 0; start < len(visualPages); {
		if err := totalCtx.Err(); err != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			markRemainingPDFPagesFailed(failed, visualPages[start:], "total OCR/VLM budget exceeded")
			break
		}
		end := start + 1
		for end < len(visualPages) && end-start < limits.RenderBatchPages &&
			visualPages[end] == visualPages[end-1]+1 {
			end++
		}
		firstPage, lastPage := visualPages[start], visualPages[end-1]
		renderStartedAt := time.Now()
		extractionLog.Info("[knowledge] PDF render started", "source_digest", sourceDigest,
			"page_from", firstPage, "page_to", lastPage, "dpi", limits.DPI)
		renderBudget := time.Duration(lastPage-firstPage+1) * limits.PageTimeout
		renderCtx, cancelRender := context.WithTimeout(totalCtx, renderBudget)
		rendered, renderErr := runGovernedCPU(renderCtx, governor, func() ([]renderedPDFPage, error) {
			return renderPDFPageBatch(
				renderCtx, path, firstPage, lastPage, limits.DPI, limits.MaxPageBytes,
			)
		})
		cancelRender()
		extractionLog.Info("[knowledge] PDF render finished", "source_digest", sourceDigest,
			"page_from", firstPage, "page_to", lastPage,
			"elapsed_ms", time.Since(renderStartedAt).Milliseconds(), "error", renderErr)
		if renderErr != nil {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			for page := firstPage; page <= lastPage; page++ {
				failed[page] = "render failed: " + renderErr.Error()
			}
			start = end
			continue
		}
		byPage := make(map[int]renderedPDFPage, len(rendered))
		for _, renderedPage := range rendered {
			byPage[renderedPage.Page] = renderedPage
		}
		for page := firstPage; page <= lastPage; page++ {
			if err := totalCtx.Err(); err != nil {
				if ctx.Err() != nil {
					return result, ctx.Err()
				}
				failed[page] = "total OCR/VLM budget exceeded"
				continue
			}
			renderedPage, ok := byPage[page]
			if !ok {
				failed[page] = "renderer did not produce the requested page"
				continue
			}
			if renderedPage.Err != nil {
				failed[page] = renderedPage.Err.Error()
				if errors.Is(renderedPage.Err, errPDFPageByteBudget) {
					permanentFailure = true
				}
				continue
			}
			renderedBytes += int64(len(renderedPage.Data))
			if renderedBytes > limits.MaxRenderedBytes {
				permanentFailure = true
				failed[page] = fmt.Sprintf("total rendered bytes exceed budget %d", limits.MaxRenderedBytes)
				for unprocessed := page + 1; unprocessed <= lastPage; unprocessed++ {
					failed[unprocessed] = "total rendered byte budget exceeded"
				}
				markRemainingPDFPagesFailed(failed, visualPages[end:], "total rendered byte budget exceeded")
				start = len(visualPages)
				break
			}
			var (
				caption    knowledge.CaptionResult
				captionErr error
				invocation knowledge.OCRPageInvocation
			)
			if invocationSupported {
				route, routeOK := knowledge.VisionRouteSnapshotFromContext(ctx)
				if !routeOK {
					return result, knowledge.ErrInvalidVisionRouteSnapshot
				}
				route = route.Canonical()
				requestDigest := pdfOCRPageRequestDigest(
					sourceDigest, page, info.PageCount, renderedPage.Data, route,
				)
				invocation, captionErr = invocationProgress.ClaimOCRPageInvocationContext(
					ctx, knowledge.OCRPageInvocationClaim{
						PageNumber: page, PagesTotal: int64(info.PageCount), SourceDigest: sourceDigest,
						RequestDigest: requestDigest, Provider: route.ProviderName, Model: route.Model,
					},
				)
				extractionLog.Info("[knowledge] OCR page invocation claimed", "source_digest", sourceDigest,
					"job_id", invocation.JobID, "invocation_id", invocation.InvocationID,
					"page", page, "pages_total", info.PageCount, "fresh", invocation.Fresh,
					"status", invocation.Status, "provider", route.ProviderName, "model", route.Model,
					"error", captionErr)
				if errors.Is(captionErr, knowledge.ErrOCRPageInvocationLedgerUnavailable) {
					invocationSupported = false
					captionErr = nil
				} else if captionErr != nil {
					return result, captionErr
				} else if !invocation.Fresh {
					if invocation.Status != knowledge.OCRPageInvocationStatusSucceeded ||
						strings.TrimSpace(invocation.Content) == "" {
						return result, knowledge.ErrOCRPageInvocationOutcomeUnknown
					}
					caption.Content = invocation.Content
					caption.RouteReceipt = invocation.RouteReceipt
				}
			}
			if !invocationSupported || invocation.Fresh {
				pageStartedAt := time.Now()
				pageCtx, cancelPage := context.WithTimeout(totalCtx, limits.PageTimeout)
				pageDeadline, _ := pageCtx.Deadline()
				extractionLog.Info("[knowledge] OCR page provider started", "source_digest", sourceDigest,
					"job_id", invocation.JobID, "invocation_id", invocation.InvocationID,
					"page", page, "pages_total", info.PageCount, "image_bytes", len(renderedPage.Data),
					"provider", routeSnapshot.ProviderName, "model", routeSnapshot.Model,
					"timeout_ms", limits.PageTimeout.Milliseconds(),
					"deadline_unix_ms", pageDeadline.UnixMilli(), "remaining_ms", time.Until(pageDeadline).Milliseconds())
				providerBegan := false
				pageCtx = transport.WithOperationSafety(pageCtx, transport.OperationSafetyNonIdempotent)
				pageCtx = transport.WithBeforeSendHookForAction(pageCtx, "complete", func(context.Context) error {
					providerBegan = true
					extractionLog.Info("[knowledge] OCR page HTTP attempt started", "phase", "http_begin",
						"job_id", invocation.JobID, "invocation_id", invocation.InvocationID,
						"page", page, "provider", routeSnapshot.ProviderName, "model", routeSnapshot.Model,
						"accepted_evidence", "http_attempt_started",
						"deadline_unix_ms", pageDeadline.UnixMilli(), "remaining_ms", time.Until(pageDeadline).Milliseconds())
					return nil
				})
				caption, captionErr = kb.CaptionImageWithRouteReceipt(
					pageCtx, renderedPage.Data, "image/png",
				)
				outcomeUnknown := providerBegan
				httpStatus, requestID, safeCause := 0, "", ""
				acceptedEvidence := "completed"
				var providerErr *llm.ProviderError
				if captionErr != nil {
					safeCause, _ = redactProviderErrorBody(captionErr.Error())
					if errors.As(captionErr, &providerErr) && providerErr != nil {
						httpStatus, requestID = providerErr.StatusCode, providerErr.RequestID
						if providerErr.Cause != nil {
							cause, _ := redactProviderErrorBody(providerErr.Cause.Error())
							if !strings.Contains(safeCause, cause) {
								safeCause += ": " + cause
							}
						}
						// 错误链保留结构化状态和底层原因，不携带 Provider 正文或请求预览。
						sanitized := *providerErr
						sanitized.Body, sanitized.RequestPreview = "", ""
						captionErr = fmt.Errorf("knowledge: OCR transcription failed: %w", &sanitized)
					}
					if httpStatus > 0 {
						outcomeUnknown = true
						switch httpStatus {
						case 400, 401, 403, 404, 405, 413, 415, 422, 429:
							outcomeUnknown = false
						}
					} else {
						var dnsErr *net.DNSError
						var opErr *net.OpError
						if errors.As(captionErr, &dnsErr) || (errors.As(captionErr, &opErr) && opErr.Op == "dial") {
							outcomeUnknown = false
						} else if errors.Is(captionErr, io.EOF) || errors.Is(captionErr, io.ErrUnexpectedEOF) {
							outcomeUnknown = true
						}
					}
					acceptedEvidence = "request_not_sent"
					if outcomeUnknown {
						acceptedEvidence = "request_outcome_unknown"
					} else if httpStatus > 0 {
						acceptedEvidence = "http_rejected"
					}
				}
				extractionLog.Info("[knowledge] OCR page provider finished", "source_digest", sourceDigest,
					"job_id", invocation.JobID, "invocation_id", invocation.InvocationID,
					"page", page, "pages_total", info.PageCount,
					"elapsed_ms", time.Since(pageStartedAt).Milliseconds(), "text_bytes", len(caption.Content),
					"provider", routeSnapshot.ProviderName, "model", routeSnapshot.Model,
					"route_receipt", caption.RouteReceipt, "context_error", pageCtx.Err(), "error", safeCause,
					"phase", "caption_result", "http_attempt_started", providerBegan,
					"http_status", httpStatus, "provider_request_id", requestID, "accepted_evidence", acceptedEvidence,
					"deadline_unix_ms", pageDeadline.UnixMilli(), "remaining_ms", time.Until(pageDeadline).Milliseconds())
				cancelPage()
				if captionErr != nil {
					if invocationSupported && invocation.Fresh {
						marker, ok := progress.(knowledge.OCRPageInvocationContextOutcomeMarker)
						if !ok {
							return result, knowledge.ErrOCRPageInvocationLedgerUnavailable
						}
						// 请求取消后仍需提交短暂的本地结果状态，不再发出模型请求。
						markCtx, cancelMark := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
						var markErr error
						if outcomeUnknown {
							markErr = marker.MarkOCRPageInvocationOutcomeUnknownContext(markCtx, invocation, safeCause)
						} else {
							markErr = marker.MarkOCRPageInvocationFailedContext(markCtx, invocation, safeCause)
						}
						cancelMark()
						if markErr != nil {
							return result, markErr
						}
						if outcomeUnknown {
							return result, fmt.Errorf("%w: page %d invocation %s: %s (%w)",
								knowledge.ErrOCRPageInvocationOutcomeUnknown, page, invocation.InvocationID, safeCause, captionErr)
						}
					}
					if ctx.Err() != nil {
						return result, ctx.Err()
					}
					failed[page] = "OCR/VLM failed: " + safeCause
					continue
				}
			}
			result.Pages[page-1].Text = strings.TrimSpace(caption.Content)
			assignPDFPageSourceOffsets(result.Pages)
			if progress != nil {
				receipt := caption.RouteReceipt
				if invocationSupported && invocation.Fresh {
					if err := invocationProgress.SaveOCRPageInvocationContext(
						ctx, invocation,
						knowledge.OCRPageInvocationResult{Content: result.Pages[page-1].Text, RouteReceipt: receipt},
					); err != nil {
						if !errors.Is(err, knowledge.ErrOCRPageInvocationLedgerUnavailable) {
							return result, err
						}
					}
				}
				if err := progress.CommitPage(ctx, knowledge.IngestPageCheckpoint{
					PageNumber: page, PagesTotal: int64(info.PageCount), SourceDigest: sourceDigest,
					ExtractionMode: "ocr_vlm", Content: result.Pages[page-1].Text,
					SourceOffsetStart: result.Pages[page-1].SourceOffsetFrom,
					SourceOffsetEnd:   result.Pages[page-1].SourceOffsetTo,
					OCRRouteReceipt:   &receipt,
				}); err != nil {
					return result, err
				}
			}
		}
		if start < len(visualPages) {
			start = end
		}
	}

	if len(failed) > 0 {
		failedPages, reasons := orderedPDFFailures(failed)
		cause := totalCtx.Err()
		if cause == nil && permanentFailure {
			cause = knowledge.ErrInvalidDocumentUpload
		}
		return result, &pdfPageExtractionError{
			PagesTotal: info.PageCount, FailedPages: failedPages, Reasons: reasons,
			Cause: cause,
		}
	}
	// OCR 页可能位于文本页之前；在 OCR 全部完成后重新计算一次完整页序，
	// 再统一提交文本页和已完成页，避免先提交的后续文本页占用错误坐标。
	assignPDFPageSourceOffsets(result.Pages)
	if progress != nil {
		for page := 1; page <= info.PageCount; page++ {
			pageResult := result.Pages[page-1]
			if strings.TrimSpace(pageResult.Text) == "" {
				continue
			}
			if pageResult.SourceOffsetTo <= pageResult.SourceOffsetFrom {
				return result, fmt.Errorf("%w: missing canonical PDF page offset %d", knowledge.ErrInvalidDocumentUpload, page)
			}
			checkpoint, completedPage := completed[page]
			if completedPage {
				checkpoint.SourceOffsetStart = pageResult.SourceOffsetFrom
				checkpoint.SourceOffsetEnd = pageResult.SourceOffsetTo
			} else {
				checkpoint = knowledge.IngestPageCheckpoint{
					PageNumber: page, PagesTotal: int64(info.PageCount), SourceDigest: sourceDigest,
					ExtractionMode: pageResult.Mode, Content: pageResult.Text,
					SourceOffsetStart: pageResult.SourceOffsetFrom, SourceOffsetEnd: pageResult.SourceOffsetTo,
				}
			}
			if pageResult.Mode == "ocr_vlm" {
				// OCR 新页已在 Provider 成功后提交过一次；再次提交只用于
				// 让重试/历史页与最终 canonical 坐标保持同一事实。
				if checkpoint.OCRRouteReceipt == nil {
					if invocation, ok := completed[page]; ok {
						checkpoint.OCRRouteReceipt = invocation.OCRRouteReceipt
					}
				}
				if checkpoint.OCRRouteReceipt == nil {
					// 新 OCR 页的 receipt 已由当前循环构造并提交；此处跳过，
					// 避免在没有 route receipt 的情况下伪造 checkpoint。
					continue
				}
			}
			if err := progress.CommitPage(ctx, checkpoint); err != nil {
				return result, err
			}
		}
	}
	result.Text = joinPDFPageExtractions(result.Pages)
	return result, nil
}

func pdfOCRPageRequestDigest(
	sourceDigest string,
	page, pagesTotal int,
	image []byte,
	route knowledge.VisionRouteSnapshot,
) string {
	imageSum := sha256.Sum256(image)
	route = route.Canonical()
	h := sha256.New()
	for _, part := range [][]byte{
		[]byte("knowledge-pdf-page-ocr-v1"),
		[]byte(sourceDigest), []byte(fmt.Sprintf("%d/%d", page, pagesTotal)),
		imageSum[:], []byte(route.Fingerprint), []byte(route.ProviderName), []byte(route.Model),
	} {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func firstResourceGovernor(governors []*resourcegov.Governor) *resourcegov.Governor {
	if len(governors) == 0 {
		return nil
	}
	return governors[0]
}

func runGovernedCPU[T any](
	ctx context.Context,
	governor *resourcegov.Governor,
	operation func() (T, error),
) (T, error) {
	if governor == nil {
		return operation()
	}
	permit, err := governor.Acquire(
		ctx,
		resourcegov.ResourceCPUHeavy,
		resourcegov.PriorityFromContext(ctx, resourcegov.PriorityInteractive),
	)
	if err != nil {
		var zero T
		return zero, err
	}
	defer permit.Release()
	return operation()
}

func validateAsyncPDFLimits(limits asyncPDFExtractionLimits) error {
	if limits.MaxPages <= 0 || limits.RenderBatchPages <= 0 || limits.DPI <= 0 ||
		limits.MaxPageBytes <= 0 || limits.MaxRenderedBytes <= 0 || limits.MaxTextBytes <= 0 ||
		limits.PageTimeout <= 0 || limits.TotalTimeout <= 0 {
		return fmt.Errorf("knowledge: invalid asynchronous PDF resource limits")
	}
	return nil
}

func pdfPageHasUsableTextLayer(text string) bool {
	visible := 0
	for _, r := range text {
		switch r {
		case ' ', '\t', '\r', '\n':
		default:
			visible++
		}
	}
	return visible >= minPDFTextRunesPerPage
}

func markRemainingPDFPagesFailed(failed map[int]string, pages []int, reason string) {
	for _, page := range pages {
		if _, exists := failed[page]; !exists {
			failed[page] = reason
		}
	}
}

func orderedPDFFailures(failed map[int]string) ([]int, []string) {
	pages := make([]int, 0, len(failed))
	for page := range failed {
		pages = append(pages, page)
	}
	sort.Ints(pages)
	reasons := make([]string, 0, len(pages))
	for _, page := range pages {
		reasons = append(reasons, fmt.Sprintf("page %d: %s", page, failed[page]))
	}
	return pages, reasons
}

func boundedPDFPageRange(first, last, maximum int) []int {
	if first <= 0 || last < first || maximum <= 0 {
		return nil
	}
	count := last - first + 1
	if count > maximum {
		count = maximum
	}
	pages := make([]int, count)
	for i := range pages {
		pages[i] = first + i
	}
	return pages
}

func joinPDFPageExtractions(pages []documentPageExtraction) string {
	assignPDFPageSourceOffsets(pages)
	var b strings.Builder
	for _, page := range pages {
		if strings.TrimSpace(page.Text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## PDF 第 %d 页\n\n<!-- source_page_span=%d-%d -->\n\n",
			page.PageNumber, page.SourcePageFrom, page.SourcePageTo)
		b.WriteString(strings.TrimSpace(page.Text))
	}
	return strings.TrimSpace(b.String())
}

// assignPDFPageSourceOffsets 为每个非空页计算 canonical 文本中的内容范围。
// 页标题和来源注释属于 canonical 文本，但不计入该页正文范围；范围以 UTF-8 字节计。
func assignPDFPageSourceOffsets(pages []documentPageExtraction) {
	var cursor int64
	hasPreviousPage := false
	for index := range pages {
		pages[index].SourceOffsetFrom = 0
		pages[index].SourceOffsetTo = 0
		text := strings.TrimSpace(pages[index].Text)
		if text == "" {
			continue
		}
		if hasPreviousPage {
			cursor += int64(len("\n\n"))
		}
		prefix := fmt.Sprintf("## PDF 第 %d 页\n\n<!-- source_page_span=%d-%d -->\n\n",
			pages[index].PageNumber, pages[index].SourcePageFrom, pages[index].SourcePageTo)
		cursor += int64(len(prefix))
		pages[index].SourceOffsetFrom = cursor
		cursor += int64(len(text))
		pages[index].SourceOffsetTo = cursor
		hasPreviousPage = true
	}
}

func inspectPDFDocument(ctx context.Context, path string) (pdfDocumentInfo, error) {
	bin := findTool("pdfinfo", pdfinfoKnownPaths...)
	if bin == "" {
		return pdfDocumentInfo{}, fmt.Errorf("缺少 pdfinfo，无法可靠确认 PDF 页数/加密状态；请安装 poppler 或设置 HEXCLAW_PDFINFO")
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, path)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return pdfDocumentInfo{}, fmt.Errorf("pdfinfo 执行失败: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	info := pdfDocumentInfo{}
	for _, line := range strings.Split(stdout.String(), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Pages":
			info.PageCount, _ = strconv.Atoi(strings.TrimSpace(value))
		case "Encrypted":
			info.Encrypted = strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "yes")
		}
	}
	if info.PageCount <= 0 {
		return pdfDocumentInfo{}, fmt.Errorf("pdfinfo 未返回有效页数")
	}
	return info, nil
}

func extractPDFTextPagesFromPath(
	ctx context.Context,
	path string,
	pageCount int,
	maxTextBytes int64,
) ([]string, string, error) {
	pages := make([]string, pageCount)
	if maxTextBytes <= 0 {
		return nil, "", fmt.Errorf("%w: invalid PDF text output budget", knowledge.ErrInvalidDocumentUpload)
	}
	bin := findTool("pdftotext", pdftotextKnownPaths...)
	if bin == "" {
		return pages, "缺少 pdftotext，已按扫描文档逐页进入 OCR/VLM", nil
	}
	spool, err := os.CreateTemp("", "hexpdf-text-*.txt")
	if err != nil {
		return nil, "", fmt.Errorf("knowledge: create PDF text spool: %w", err)
	}
	spoolPath := spool.Name()
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spoolPath)
	}()
	stdout := &boundedFileWriter{file: spool, limit: maxTextBytes}
	var stderr bytes.Buffer
	// 保留 PDF 页面布局，确保页脚数字仍位于本页正文末尾，供教材目录的确定性页码证据使用。
	cmd := exec.CommandContext(ctx, bin, "-layout", "-enc", "UTF-8", "-q", path, "-")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if stdout.overflow {
		return nil, "", fmt.Errorf("%w: %v (limit=%d)",
			knowledge.ErrInvalidDocumentUpload, errPDFTextByteBudget, maxTextBytes)
	}
	if runErr != nil {
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, "", ctx.Err()
		}
		return pages, "PDF 文本层解析失败，已按扫描文档逐页进入 OCR/VLM：" + strings.TrimSpace(stderr.String()), nil
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return nil, "", fmt.Errorf("knowledge: rewind PDF text spool: %w", err)
	}
	reader := bufio.NewReaderSize(spool, 64<<10)
	page := 0
	for {
		text, readErr := reader.ReadString('\f')
		if len(text) > 0 {
			text = strings.TrimSuffix(text, "\f")
			text = strings.ReplaceAll(text, "\r\n", "\n")
			text = strings.ReplaceAll(text, "\r", "\n")
			if page < pageCount {
				pages[page] = strings.TrimSpace(text)
			}
			page++
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, "", fmt.Errorf("knowledge: read PDF text spool: %w", readErr)
		}
	}
	return pages, "", nil
}

// boundedFileWriter makes os/exec copy stdout through a bounded writer rather
// than attaching the file directly. Returning a write error closes the pipe;
// overflow is checked independently because the child may surface SIGPIPE
// instead of preserving the writer sentinel in cmd.Run's error chain.
type boundedFileWriter struct {
	file     *os.File
	limit    int64
	written  int64
	overflow bool
}

func (w *boundedFileWriter) Write(p []byte) (int, error) {
	remaining := w.limit - w.written
	if remaining <= 0 {
		w.overflow = true
		return 0, errPDFTextByteBudget
	}
	if int64(len(p)) > remaining {
		n, err := w.file.Write(p[:remaining])
		w.written += int64(n)
		w.overflow = true
		if err != nil {
			return n, err
		}
		return n, errPDFTextByteBudget
	}
	n, err := w.file.Write(p)
	w.written += int64(n)
	return n, err
}

func renderPDFPageBatch(
	ctx context.Context,
	pdfPath string,
	firstPage, lastPage, dpi int,
	maxPageBytes int64,
) ([]renderedPDFPage, error) {
	if firstPage <= 0 || lastPage < firstPage {
		return nil, fmt.Errorf("invalid PDF page range %d-%d", firstPage, lastPage)
	}
	bin := findTool("pdftoppm", pdftoppmKnownPaths...)
	if bin == "" {
		return nil, fmt.Errorf("缺少 pdftoppm，无法渲染 PDF 扫描页/图表；请安装 poppler 或设置 HEXCLAW_PDFTOPPM")
	}
	dir, err := os.MkdirTemp("", "hexpdf-vision-batch-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	prefix := filepath.Join(dir, "page")
	args := []string{
		"-png", "-r", strconv.Itoa(dpi), "-f", strconv.Itoa(firstPage),
		"-l", strconv.Itoa(lastPage), pdfPath, prefix,
	}
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
	sort.Slice(files, func(i, j int) bool { return pdfPageNumber(files[i]) < pdfPageNumber(files[j]) })
	pages := make([]renderedPDFPage, 0, len(files))
	for _, file := range files {
		page := pdfPageNumber(file)
		info, statErr := os.Stat(file)
		if statErr != nil {
			pages = append(pages, renderedPDFPage{Page: page, Err: statErr})
			continue
		}
		if info.Size() > maxPageBytes {
			pages = append(pages, renderedPDFPage{Page: page, Err: fmt.Errorf(
				"%w: bytes=%d per-page budget=%d", errPDFPageByteBudget, info.Size(), maxPageBytes,
			)})
			continue
		}
		raw, readErr := os.ReadFile(file)
		pages = append(pages, renderedPDFPage{Page: page, Data: raw, Err: readErr})
		_ = os.Remove(file)
	}
	return pages, nil
}

const (
	largePDFPageThreshold  = 40
	minPDFTextRunesPerPage = 80
)

// pdfVisualPageLimit 为 PDF 选择视觉页预算。百页 PDF 有可用文本层时直接索引
// 全部文本；扫描版则逐页进入 OCR/VLM，绝不再抽样前几页后冒充完整索引。调用方
// 必须把 configured 视为硬上限：页数超过它时应 fail closed 或显式 degraded。
func pdfVisualPageLimit(text string, pageCount, configured int) (int, string) {
	if configured <= 0 || pageCount <= largePDFPageThreshold {
		return configured, ""
	}
	if pdfHasUsableTextLayer(text, pageCount) {
		return 0, fmt.Sprintf("PDF 共 %d 页且文本层可用，已优先完成全文文本索引；为避免大文档超时，本次跳过同步逐页视觉增强", pageCount)
	}
	if pageCount <= configured {
		return pageCount, ""
	}
	return configured, fmt.Sprintf("PDF 共 %d 页且文本层不足，超过视觉解析硬上限 %d 页；禁止把未处理页标成已索引", pageCount, configured)
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
	return intEnvBounded("HEXCLAW_DOC_VLM_MAX_PAGES", 250, 0, 500)
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
