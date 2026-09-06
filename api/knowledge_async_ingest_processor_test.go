package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/ai-core/transport"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/toolkit/util/logger"
	_ "modernc.org/sqlite"
)

func TestKnowledgeAsyncProcessorScannedPDFDoesNotPublishPartialOCR(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")
	t.Setenv("HEXCLAW_DOC_VLM_PAGE_TIMEOUT_SECONDS", "180")

	_ = logger.Default()
	var output bytes.Buffer
	previousDefault := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previousDefault) })
	var pageDeadlines []time.Time
	calls := 0
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerWithReceiptFunc(func(ctx context.Context, _ []byte, _ string) (knowledge.CaptionResult, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("OCR page context has no deadline")
		}
		pageDeadlines = append(pageDeadlines, deadline)
		if calls == 2 {
			return knowledge.CaptionResult{}, &llm.ProviderError{
				StatusCode: 408, Status: "408 Request Timeout", Cause: io.ErrUnexpectedEOF,
			}
		}
		return testOCRCaptionResult(fmt.Sprintf("第 %d 页扫描正文", calls)), nil
	}))
	source := writeAsyncProcessorPDF(t, buildImageOnlyTestPDF(t, 5))
	pdfBytes, err := os.ReadFile(source.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(pdfBytes)
	source.JobID, source.DocumentID, source.OwnerID = "job-1", "doc-1", "owner"
	source.SizeBytes, source.SHA256 = int64(len(pdfBytes)), hex.EncodeToString(digest[:])
	parentCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx := knowledge.WithVisionRouteSnapshot(parentCtx, knowledge.VisionRouteSnapshot{
		ProviderInstanceID: "hexclaw-gpt", ProviderName: "hexclaw-gpt",
		ProviderDisplayName: "HexClaw-GPT", Model: "gpt-5.6-sol", Capabilities: []string{"vision"},
	})
	progress := &durableOCRProgressProbe{memoryIngestPageProgress: &memoryIngestPageProgress{}}
	processor := NewKnowledgeDocumentIngestProcessor(manager).(knowledge.ResumableDocumentIngestProcessor)
	_, err = processor.PrepareResumable(ctx, source, progress)
	if !errors.Is(err, knowledge.ErrOCRPageInvocationOutcomeUnknown) ||
		!errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "ocr-integration-2") ||
		!strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("OCR failure must preserve invocation and original cause: err=%v", err)
	}
	if len(progress.pages) != 1 || calls != 2 {
		t.Fatalf("partial OCR must stop without publishing: checkpoints=%d calls=%d", len(progress.pages), calls)
	}
	started, finished := 0, 0
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["msg"] != "[knowledge] OCR page provider started" &&
			entry["msg"] != "[knowledge] OCR page provider finished" {
			continue
		}
		if entry["document_id"] != source.DocumentID || entry["job_id"] != source.JobID ||
			entry["invocation_id"] == nil || entry["invocation_id"] == "" ||
			entry["provider"] != "hexclaw-gpt" || entry["model"] != "gpt-5.6-sol" {
			t.Fatalf("OCR collector correlation missing: %v", entry)
		}
		page := int(entry["page"].(float64))
		if entry["msg"] == "[knowledge] OCR page provider started" {
			started++
			remaining, ok := entry["remaining_ms"].(float64)
			if entry["deadline_unix_ms"] != float64(pageDeadlines[page-1].UnixMilli()) ||
				!ok || remaining <= 0 || remaining > 60_000 || entry["timeout_ms"] != float64(180_000) {
				t.Fatalf("OCR deadline must reflect shorter parent budget: %v", entry)
			}
		} else {
			finished++
			if _, ok := entry["elapsed_ms"].(float64); !ok {
				t.Fatalf("OCR completion duration missing: %v", entry)
			}
			if page == 2 && (!strings.Contains(fmt.Sprint(entry["error"]), "unexpected EOF") ||
				entry["accepted_evidence"] != "request_outcome_unknown") {
				t.Fatalf("OCR failure cause missing: %v", entry)
			}
		}
	}
	if started != 2 || finished != 2 {
		t.Fatalf("OCR nodes did not reach current slog handler: started=%d finished=%d", started, finished)
	}
}

func TestKnowledgeAsyncProcessorScannedPDFKeepsStablePageSpans(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")

	calls := 0
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerFunc(func(_ context.Context, _ []byte, _ string) (string, error) {
		calls++
		return fmt.Sprintf("第 %d 页扫描正文", calls), nil
	}))
	source := writeAsyncProcessorPDF(t, buildImageOnlyTestPDF(t, 5))

	prepared, err := NewKnowledgeDocumentIngestProcessor(manager).Prepare(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 5 || prepared.PageCount != 5 {
		t.Fatalf("逐页 OCR calls=%d page_count=%d，want 5/5", calls, prepared.PageCount)
	}
	seenStructuredPages := map[int]bool{}
	for _, chunk := range prepared.Chunks {
		if chunk.PageStart <= 0 || chunk.PageEnd < chunk.PageStart ||
			chunk.SourceDigest != source.SHA256 ||
			chunk.SourceOffsetEnd <= chunk.SourceOffsetStart {
			t.Fatalf("chunk 缺少结构化来源跨度：%+v", chunk)
		}
		for page := chunk.PageStart; page <= chunk.PageEnd; page++ {
			seenStructuredPages[page] = true
		}
	}
	for _, page := range []int{1, 3, 5} {
		if !seenStructuredPages[page] {
			t.Fatalf("结构化来源跨度未覆盖 PDF 第 %d 页，chunks=%+v", page, prepared.Chunks)
		}
		marker := "<!-- source_page_span=" + strconv.Itoa(page) + "-" + strconv.Itoa(page) + " -->"
		if !strings.Contains(prepared.Document.Content, marker) {
			t.Fatalf("缺少稳定页来源锚点 %q，content=%q", marker, prepared.Document.Content)
		}
	}
	for _, warning := range prepared.Warnings {
		if strings.Contains(warning, "抽样") || strings.Contains(warning, "前 3 页") {
			t.Fatalf("完整逐页 OCR 不应留下抽样告警：%q", warning)
		}
	}
}

func TestKnowledgeAsyncProcessorTextLayerPDFDoesNotCallVLM(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	if findTool("pdfinfo", pdfinfoKnownPaths...) == "" {
		t.Skip("requires pdfinfo")
	}
	visible := "algebra lesson content one"
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerFunc(func(_ context.Context, _ []byte, _ string) (string, error) {
		return "", errors.New("VLM must not be called for a usable text-layer page")
	}))
	source := writeAsyncProcessorPDF(t, buildTextLayerTestPDF(t, []string{
		visible,
		"geometry lesson content two",
		"fraction lesson content three",
		"measurement lesson content four",
	}))
	pages, warning, textErr := extractPDFTextPagesFromPath(context.Background(), source.StoragePath, 1, 10<<20)
	if textErr != nil || warning != "" || len(pages) != 1 || !pdfPageHasUsableTextLayer(pages[0]) {
		t.Fatalf("text fixture precondition pages=%q warning=%q err=%v", pages, warning, textErr)
	}

	prepared, err := NewKnowledgeDocumentIngestProcessor(manager).Prepare(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.PageCount != 1 || !strings.Contains(prepared.Document.Content, visible) {
		t.Fatalf("text-layer PDF extraction=%q page_count=%d", prepared.Document.Content, prepared.PageCount)
	}
	if !strings.Contains(prepared.Document.Content, "<!-- source_page_span=1-1 -->") {
		t.Fatalf("text-layer page lost source span: %q", prepared.Document.Content)
	}
	if len(prepared.Chunks) == 0 || prepared.Chunks[0].PageStart != 1 ||
		prepared.Chunks[0].PageEnd != 1 || prepared.Chunks[0].SourceDigest != source.SHA256 {
		t.Fatalf("text-layer PDF missing structured source span: %+v", prepared.Chunks)
	}
}

func TestKnowledgeAsyncProcessorRendersPDFInSmallBatches(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", "2")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")

	tmp := t.TempDir()
	renderLog := filepath.Join(tmp, "render.log")
	t.Setenv("HEXCLAW_TEST_RENDER_LOG", renderLog)
	fakeRenderer := filepath.Join(tmp, "pdftoppm")
	const fakeRendererScript = `#!/bin/sh
first=1
last=1
while [ "$#" -gt 0 ]; do
  case "$1" in
    -f) first="$2"; shift 2 ;;
    -l) last="$2"; shift 2 ;;
    *) prefix="$1"; shift ;;
  esac
done
printf '%s-%s\n' "$first" "$last" >> "$HEXCLAW_TEST_RENDER_LOG"
page="$first"
while [ "$page" -le "$last" ]; do
  printf 'fake-page-%s' "$page" > "${prefix}-${page}.png"
  page=$((page + 1))
done
`
	if err := os.WriteFile(fakeRenderer, []byte(fakeRendererScript), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEXCLAW_PDFTOPPM", fakeRenderer)

	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerFunc(func(_ context.Context, _ []byte, _ string) (string, error) {
		return "逐页本地 fake OCR", nil
	}))
	source := writeAsyncProcessorPDF(t, buildImageOnlyTestPDF(t, 6))
	if _, err := NewKnowledgeDocumentIngestProcessor(manager).Prepare(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(renderLog)
	if err != nil {
		t.Fatal(err)
	}
	var renderedPages []int
	for _, line := range strings.Fields(string(raw)) {
		var first, last int
		if _, err := fmt.Sscanf(line, "%d-%d", &first, &last); err != nil {
			t.Fatalf("invalid render log %q: %v", line, err)
		}
		if last-first+1 > 2 {
			t.Fatalf("PDF 渲染必须按小批次释放页面，observed batch=%s", line)
		}
		for page := first; page <= last; page++ {
			renderedPages = append(renderedPages, page)
		}
	}
	if fmt.Sprint(renderedPages) != "[1 2 3 4 5 6]" {
		t.Fatalf("小批渲染必须无重无漏覆盖所有页，pages=%v", renderedPages)
	}
}

func TestKnowledgeAsyncProcessorHonorsCancellationBetweenPDFPages(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", "1")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerFunc(func(ctx context.Context, _ []byte, _ string) (string, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return fmt.Sprintf("page %d", calls), ctx.Err()
	}))
	source := writeAsyncProcessorPDF(t, buildImageOnlyTestPDF(t, 8))

	_, err := NewKnowledgeDocumentIngestProcessor(manager).Prepare(ctx, source)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel must stop PDF pipeline, err=%v", err)
	}
	if calls > 2 {
		t.Fatalf("cancel 后仍继续调用 VLM，calls=%d", calls)
	}
}

type memoryIngestPageProgress struct {
	digest string
	total  int64
	pages  map[int]knowledge.IngestPageCheckpoint
}

func (p *memoryIngestPageProgress) SetPageTotal(_ context.Context, digest string, total int64) error {
	if p.digest != "" && (p.digest != digest || p.total != total) {
		return errors.New("page manifest changed")
	}
	p.digest, p.total = digest, total
	if p.pages == nil {
		p.pages = map[int]knowledge.IngestPageCheckpoint{}
	}
	return nil
}

func (p *memoryIngestPageProgress) LoadCompletedPages(
	_ context.Context,
	digest string,
	total int64,
) ([]knowledge.IngestPageCheckpoint, error) {
	if p.digest != digest || p.total != total {
		return nil, errors.New("unexpected page manifest")
	}
	result := make([]knowledge.IngestPageCheckpoint, 0, len(p.pages))
	for page := 1; page <= int(total); page++ {
		if checkpoint, ok := p.pages[page]; ok {
			result = append(result, checkpoint)
		}
	}
	return result, nil
}

func (p *memoryIngestPageProgress) CommitPage(
	_ context.Context,
	checkpoint knowledge.IngestPageCheckpoint,
) error {
	p.pages[checkpoint.PageNumber] = checkpoint
	return nil
}

func TestKnowledgeAsyncProcessorResumeSkipsCompletedVLMPageCheckpoints(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", "2")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")

	calls := 0
	failThirdOnce := true
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerWithReceiptFunc(func(
		ctx context.Context,
		_ []byte,
		_ string,
	) (knowledge.CaptionResult, error) {
		calls++
		if calls == 1 && (transport.OperationSafetyFromContext(ctx) != transport.OperationSafetyNonIdempotent ||
			!transport.HasBeforeSendHook(ctx)) {
			t.Error("durable OCR request lacks its before-send boundary or permits hidden transport replay")
		}
		if failThirdOnce && calls == 3 {
			failThirdOnce = false
			return knowledge.CaptionResult{}, &llm.ProviderError{StatusCode: 429, Status: "429 Too Many Requests"}
		}
		return testOCRCaptionResult(fmt.Sprintf("第 %d 次 VLM 调用的正文", calls)), nil
	}))
	source := writeAsyncProcessorPDF(t, buildImageOnlyTestPDF(t, 5))
	processor, ok := NewKnowledgeDocumentIngestProcessor(manager).(knowledge.ResumableDocumentIngestProcessor)
	if !ok {
		t.Fatal("knowledge processor does not implement resumable protocol")
	}
	progress := &durableOCRProgressProbe{memoryIngestPageProgress: &memoryIngestPageProgress{}}
	pdfBytes, err := os.ReadFile(source.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(pdfBytes)
	source.JobID, source.DocumentID, source.OwnerID = "job-1", "doc-1", "owner"
	source.SizeBytes, source.SHA256 = int64(len(pdfBytes)), hex.EncodeToString(digest[:])
	ctx := knowledge.WithVisionRouteSnapshot(context.Background(), knowledge.VisionRouteSnapshot{
		ProviderInstanceID: "hexclaw-gpt", ProviderName: "hexclaw-gpt",
		ProviderDisplayName: "HexClaw-GPT", Model: "gpt-5.6-sol", Capabilities: []string{"vision"},
	})

	if _, err := processor.PrepareResumable(ctx, source, progress); err == nil ||
		!strings.Contains(err.Error(), "failed_pages=[3]") {
		t.Fatalf("first interrupted OCR err=%v", err)
	}
	if len(progress.pages) != 4 {
		t.Fatalf("durable completed pages=%v", progress.pages)
	}
	if progress.invocations[3].Status != knowledge.OCRPageInvocationStatusFailed {
		t.Fatalf("explicit rejection became unknown: %+v", progress.invocations[3])
	}
	failedID := progress.invocations[3].InvocationID
	prepared, err := processor.PrepareResumable(ctx, source, progress)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 6 {
		t.Fatalf("VLM calls=%d, want 5 initial attempts + only failed page retry", calls)
	}
	if progress.invocations[3].InvocationID != failedID || progress.invocations[3].Status != knowledge.OCRPageInvocationStatusSucceeded {
		t.Fatalf("retry replaced the failed invocation: %+v", progress.invocations[3])
	}
	if prepared.PageCount != 5 || len(progress.pages) != 5 {
		t.Fatalf("resumed page_count=%d checkpoints=%d", prepared.PageCount, len(progress.pages))
	}
	for page, checkpoint := range progress.pages {
		if checkpoint.OCRRouteReceipt == nil || checkpoint.OCRRouteReceipt.Provider != "hexclaw-gpt" ||
			checkpoint.OCRRouteReceipt.Model != "gpt-5.6-sol" ||
			checkpoint.OCRRouteReceipt.Operation != knowledge.OCRRouteOperationPDFPage ||
			checkpoint.OCRRouteReceipt.Status != knowledge.OCRRouteStatusSucceeded ||
			!checkpoint.OCRRouteReceipt.Fake {
			t.Fatalf("page %d checkpoint receipt=%+v", page, checkpoint.OCRRouteReceipt)
		}
	}
}

func testOCRCaptionResult(content string) knowledge.CaptionResult {
	return knowledge.CaptionResult{
		Content: content,
		RouteReceipt: knowledge.OCRRouteReceipt{
			Provider: "hexclaw-gpt", Model: "gpt-5.6-sol",
			Operation: knowledge.OCRRouteOperationPDFPage,
			Status:    knowledge.OCRRouteStatusSucceeded, Fake: true,
		},
	}
}

func TestKnowledgeAsyncProcessorFailsClosedWhenScannedPDFExceedsPageBudget(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "4")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")

	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerFunc(func(_ context.Context, _ []byte, _ string) (string, error) {
		return "本页正文", nil
	}))
	source := writeAsyncProcessorPDF(t, buildImageOnlyTestPDF(t, 6))
	_, err := NewKnowledgeDocumentIngestProcessor(manager).Prepare(context.Background(), source)
	if err == nil || !strings.Contains(err.Error(), "failed_pages=[5 6]") {
		t.Fatalf("超出页面预算必须 fail closed 并记录所有未处理页，err=%v", err)
	}
}

func TestKnowledgeAsyncProcessorFailsClosedOnRenderedByteBudgets(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	if findTool("pdfinfo", pdfinfoKnownPaths...) == "" {
		t.Skip("requires pdfinfo")
	}
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerFunc(func(_ context.Context, _ []byte, _ string) (string, error) {
		return "本页正文", nil
	}))
	path := filepath.Join(t.TempDir(), "budget.pdf")
	if err := os.WriteFile(path, buildImageOnlyTestPDF(t, 3), 0o600); err != nil {
		t.Fatal(err)
	}
	base := asyncPDFExtractionLimits{
		MaxPages: 10, RenderBatchPages: 2, DPI: 72,
		MaxPageBytes: 10 << 20, MaxRenderedBytes: 100 << 20,
		MaxTextBytes: 10 << 20,
		PageTimeout:  time.Minute, TotalTimeout: time.Minute,
	}

	t.Run("per-page bytes", func(t *testing.T) {
		limits := base
		limits.MaxPageBytes = 10
		_, err := extractPDFForAsyncIngest(context.Background(), path, manager, limits)
		if err == nil || !strings.Contains(err.Error(), "failed_pages=[1 2 3]") ||
			!strings.Contains(err.Error(), "per-page budget") {
			t.Fatalf("单页超预算必须逐页 fail closed，err=%v", err)
		}
	})

	t.Run("total rendered bytes", func(t *testing.T) {
		limits := base
		limits.MaxRenderedBytes = 10
		_, err := extractPDFForAsyncIngest(context.Background(), path, manager, limits)
		if err == nil || !strings.Contains(err.Error(), "failed_pages=[1 2 3]") ||
			!strings.Contains(err.Error(), "total rendered") {
			t.Fatalf("累计渲染超预算必须停止并记录剩余页，err=%v", err)
		}
	})
}

func TestPDFTextExtractionFailsClosedWhenDecompressedStdoutExceedsBudget(t *testing.T) {
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "pdftotext")
	const script = `#!/bin/sh
dd if=/dev/zero bs=1024 count=8 2>/dev/null | tr '\000' x
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HEXCLAW_PDFTOTEXT", fake)
	path := filepath.Join(tmp, "compressed.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.7\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := extractPDFTextPagesFromPath(context.Background(), path, 1, 1024)
	if !errors.Is(err, knowledge.ErrInvalidDocumentUpload) ||
		!strings.Contains(err.Error(), "text output") {
		t.Fatalf("decompressed text overflow must fail closed, err=%v", err)
	}
}

func TestAsyncPDFExtractionLimitsComeFromBoundedConfiguration(t *testing.T) {
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "122")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", "3")
	t.Setenv("HEXCLAW_DOC_VLM_MAX_IMAGE_MB", "7")
	t.Setenv("HEXCLAW_DOC_VLM_MAX_RENDER_MB", "321")
	t.Setenv("HEXCLAW_DOC_PDF_TEXT_MAX_MB", "19")
	t.Setenv("HEXCLAW_DOC_VLM_PAGE_TIMEOUT_SECONDS", "45")
	t.Setenv("HEXCLAW_DOC_VLM_TOTAL_TIMEOUT_SECONDS", "900")
	limits := asyncPDFLimitsFromEnv()
	if limits.MaxPages != 122 || limits.RenderBatchPages != 3 || limits.MaxPageBytes != 7<<20 ||
		limits.MaxRenderedBytes != 321<<20 || limits.PageTimeout != 45*time.Second ||
		limits.MaxTextBytes != 19<<20 || limits.TotalTimeout != 15*time.Minute {
		t.Fatalf("unexpected async PDF limits: %+v", limits)
	}
}

func TestKnowledgeAsyncProcessorReal122PageFixtureUsesLocalFakeVLMForEveryPage(t *testing.T) {
	if testing.Short() {
		t.Skip("real 57.3MB/122-page fixture")
	}
	fixture := strings.TrimSpace(os.Getenv("HEXCLAW_KNOWLEDGE_REAL_PDF"))
	if fixture == "" {
		t.Skip("set HEXCLAW_KNOWLEDGE_REAL_PDF to the frozen 122-page scanned fixture")
	}
	requirePopplerForAsyncPDFTest(t)
	if findTool("pdfinfo", pdfinfoKnownPaths...) == "" {
		t.Skip("requires pdfinfo")
	}
	info, err := os.Stat(fixture)
	if err != nil {
		t.Fatal(err)
	}
	const wantBytes int64 = 57_313_616
	const wantSHA = "65bd80bd35be524bf68f66f9b67820a97176e1487db81810cb268e04e44dd8b2"
	if info.Size() != wantBytes {
		t.Fatalf("fixture bytes=%d want=%d", info.Size(), wantBytes)
	}

	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", "2")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")
	calls := 0
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerFunc(func(_ context.Context, page []byte, mime string) (string, error) {
		calls++
		if len(page) == 0 || mime != "image/png" {
			return "", fmt.Errorf("invalid local rendered page bytes=%d mime=%q", len(page), mime)
		}
		return fmt.Sprintf("本地 fake VLM 第 %d 页转写", calls), nil
	}))
	source := knowledge.PersistedIngestDocument{
		DocumentID: "doc-real-122", OwnerID: "desktop-user", CorpusUID: "corpus-1", CorpusAlias: "default",
		ContentGeneration: 1, Filename: filepath.Base(fixture), Extension: ".pdf", MediaType: "application/pdf",
		SizeBytes: wantBytes, SHA256: wantSHA, StoragePath: fixture,
	}

	processor, ok := NewKnowledgeDocumentIngestProcessor(manager).(knowledge.ResumableDocumentIngestProcessor)
	if !ok {
		t.Fatal("knowledge processor does not implement resumable protocol")
	}
	progress := &memoryIngestPageProgress{}
	prepared, err := processor.PrepareResumable(context.Background(), source, progress)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.PageCount != 122 || calls != 122 || len(progress.pages) != 122 {
		t.Fatalf("real fixture page_count=%d fake_vlm_calls=%d checkpoints=%d want=122/122/122",
			prepared.PageCount, calls, len(progress.pages))
	}
	structuredPages := map[int]bool{}
	for _, chunk := range prepared.Chunks {
		if chunk.SourceDigest != wantSHA || chunk.PageStart <= 0 || chunk.PageEnd < chunk.PageStart ||
			chunk.SourceOffsetEnd <= chunk.SourceOffsetStart {
			t.Fatalf("real fixture chunk missing structured source span: %+v", chunk)
		}
		for page := chunk.PageStart; page <= chunk.PageEnd; page++ {
			structuredPages[page] = true
		}
	}
	for page := 1; page <= 122; page++ {
		marker := fmt.Sprintf("<!-- source_page_span=%d-%d -->", page, page)
		if !strings.Contains(prepared.Document.Content, marker) {
			t.Fatalf("real fixture missing page span %s", marker)
		}
		if !structuredPages[page] {
			t.Fatalf("real fixture structured source spans missing page %d", page)
		}
	}
	for _, warning := range prepared.Warnings {
		if strings.Contains(warning, "抽样") || strings.Contains(warning, "未处理") {
			t.Fatalf("122/122 完成后不应报告抽样/缺页：%q", warning)
		}
	}
}

func TestKnowledgeAsyncProcessorPreparesTextWithoutWritingOrEmbedding(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "processor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	manager := knowledge.NewManager(store, store, nil, knowledge.WithSplitter(
		splitter.NewMarkdownSplitter(splitter.WithMarkdownChunkSize(80), splitter.WithMarkdownChunkOverlap(10)),
	))
	content := []byte("# 分数\n异分母分数相加时，先通分，再相加。")
	path := filepath.Join(t.TempDir(), "lesson.md")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	processor := NewKnowledgeDocumentIngestProcessor(manager)
	prepared, err := processor.Prepare(ctx, knowledge.PersistedIngestDocument{
		DocumentID: "doc-async", OwnerID: "desktop-user", CorpusUID: "corpus-1", CorpusAlias: "default",
		ContentGeneration: 1, Filename: "lesson.md", Extension: ".md", MediaType: "text/markdown",
		SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), StoragePath: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Document == nil || prepared.Document.ID != "doc-async" || len(prepared.Chunks) == 0 {
		t.Fatalf("prepared=%+v", prepared)
	}
	for _, chunk := range prepared.Chunks {
		if len(chunk.Embedding) != 0 {
			t.Fatalf("async text preparation wrote legacy embedding")
		}
		if strings.Contains(chunk.Content, "lesson.md") || strings.Contains(chunk.Content, path) {
			t.Fatalf("filename/local path leaked into embedding chunk: %q", chunk.Content)
		}
	}
	var documents int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM kb_documents`).Scan(&documents); err != nil {
		t.Fatal(err)
	}
	if documents != 0 {
		t.Fatalf("processor wrote %d documents before durable worker completion", documents)
	}
}

func TestKnowledgeAsyncProcessorRejectsBufferedSourcesAboveTypeMemoryBudgetsBeforeOpen(t *testing.T) {
	tests := []struct {
		name      string
		extension string
		mediaType string
		size      int64
	}{
		{name: "text", extension: ".txt", mediaType: "text/plain", size: (32 << 20) + 1},
		{name: "docx", extension: ".docx", mediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", size: (32 << 20) + 1},
		{name: "image", extension: ".png", mediaType: "image/png", size: (20 << 20) + 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewKnowledgeDocumentIngestProcessor(&knowledge.Manager{}).Prepare(
				context.Background(),
				knowledge.PersistedIngestDocument{
					DocumentID:  "doc-budget-" + test.name,
					Filename:    "oversized" + test.extension,
					Extension:   test.extension,
					MediaType:   test.mediaType,
					StoragePath: filepath.Join(t.TempDir(), "must-not-be-opened"+test.extension),
					SizeBytes:   test.size,
					SHA256:      strings.Repeat("a", 64),
				},
			)
			if !errors.Is(err, knowledge.ErrInvalidDocumentUpload) ||
				!strings.Contains(err.Error(), "memory budget") {
				t.Fatalf("%s source above its in-memory budget err=%v", test.name, err)
			}
		})
	}
}

type boundedReadProbe struct {
	remaining  int
	maxRequest int
}

func (r *boundedReadProbe) Read(buffer []byte) (int, error) {
	if len(buffer) > r.maxRequest {
		r.maxRequest = len(buffer)
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(buffer), r.remaining)
	for i := 0; i < n; i++ {
		buffer[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}

func TestReadAndVerifyTextIngestReaderUsesBoundedChunks(t *testing.T) {
	const size = 2 << 20
	wantBytes := bytes.Repeat([]byte{'x'}, size)
	digest := sha256.Sum256(wantBytes)
	probe := &boundedReadProbe{remaining: size}

	got, err := readAndVerifyTextIngestReader(
		context.Background(), probe, size, hex.EncodeToString(digest[:]),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != size || got[0] != 'x' || got[len(got)-1] != 'x' {
		t.Fatalf("streamed text length/content mismatch: len=%d", len(got))
	}
	if probe.maxRequest > 32<<10 {
		t.Fatalf("text reader requested %d bytes at once, want <= 32768", probe.maxRequest)
	}
}

func TestReadAndVerifyBufferedIngestSourceUsesOneExactBuffer(t *testing.T) {
	content := bytes.Repeat([]byte("image-bytes-"), 64)
	path := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	source := knowledge.PersistedIngestDocument{
		Filename: "photo.png", Extension: ".png", MediaType: "image/png",
		StoragePath: path, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}

	got, err := readAndVerifyBufferedIngestSource(context.Background(), source, maxAsyncImageSourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("buffered source bytes changed")
	}
	if cap(got) != len(got) {
		t.Fatalf("buffer capacity=%d length=%d; exact allocation required", cap(got), len(got))
	}
}

func TestReadAndVerifyBufferedIngestSourcePreservesCancellation(t *testing.T) {
	content := []byte("bounded image")
	path := filepath.Join(t.TempDir(), "cancelled.png")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	source := knowledge.PersistedIngestDocument{
		Filename: "cancelled.png", Extension: ".png", MediaType: "image/png",
		StoragePath: path, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := readAndVerifyBufferedIngestSource(ctx, source, maxAsyncImageSourceBytes)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("buffered source cancellation err=%v", err)
	}
}

func TestValidateDOCXExpandedMemoryBudgetsRejectsCompressedExpansion(t *testing.T) {
	build := func(t *testing.T, xmlBytes int, imageBytes ...int) []byte {
		t.Helper()
		var document bytes.Buffer
		writer := zip.NewWriter(&document)
		xmlEntry, err := writer.Create("word/document.xml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.CopyN(xmlEntry, &boundedReadProbe{remaining: xmlBytes}, int64(xmlBytes)); err != nil {
			t.Fatal(err)
		}
		for index, size := range imageBytes {
			entry, createErr := writer.Create(fmt.Sprintf("word/media/image-%d.png", index))
			if createErr != nil {
				t.Fatal(createErr)
			}
			if _, copyErr := io.CopyN(entry, &boundedReadProbe{remaining: size}, int64(size)); copyErr != nil {
				t.Fatal(copyErr)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		return document.Bytes()
	}

	t.Run("document xml", func(t *testing.T) {
		err := validateDOCXExpandedMemoryBudgets(
			context.Background(), build(t, 2048), 1024, 4096,
		)
		if !errors.Is(err, errDOCXXMLTooLarge) {
			t.Fatalf("expanded XML over budget err=%v", err)
		}
	})

	t.Run("embedded images aggregate", func(t *testing.T) {
		err := validateDOCXExpandedMemoryBudgets(
			context.Background(), build(t, 128, 700, 700), 1024, 1024,
		)
		if !errors.Is(err, knowledge.ErrInvalidDocumentUpload) ||
			!strings.Contains(err.Error(), "embedded image memory budget") {
			t.Fatalf("expanded image aggregate over budget err=%v", err)
		}
	})
}

func requirePopplerForAsyncPDFTest(t *testing.T) {
	t.Helper()
	if findTool("pdftotext", pdftotextKnownPaths...) == "" {
		t.Skip("requires pdftotext")
	}
	if findTool("pdftoppm", pdftoppmKnownPaths...) == "" {
		t.Skip("requires pdftoppm")
	}
	if findTool("pdfinfo", pdfinfoKnownPaths...) == "" {
		t.Skip("requires pdfinfo")
	}
}

func newAsyncProcessorTestManager(t *testing.T, captioner knowledge.Captioner) *knowledge.Manager {
	t.Helper()
	if _, ok := captioner.(knowledge.CaptionerWithReceipt); !ok {
		legacy := captioner
		captioner = knowledge.CaptionerWithReceiptFunc(func(
			ctx context.Context,
			image []byte,
			mime string,
		) (knowledge.CaptionResult, error) {
			content, err := legacy.Caption(ctx, image, mime)
			if err != nil {
				return knowledge.CaptionResult{}, err
			}
			return testOCRCaptionResult(content), nil
		})
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "processor-pdf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := knowledge.NewSQLiteStore(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return knowledge.NewManager(store, store, nil,
		knowledge.WithSplitter(splitter.NewMarkdownSplitter(
			splitter.WithMarkdownChunkSize(120), splitter.WithMarkdownChunkOverlap(10),
		)),
		knowledge.WithCaptioner(captioner),
	)
}

func writeAsyncProcessorPDF(t *testing.T, data []byte) knowledge.PersistedIngestDocument {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scanned.pdf")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return knowledge.PersistedIngestDocument{
		DocumentID: "doc-scanned", OwnerID: "desktop-user", CorpusUID: "corpus-1", CorpusAlias: "default",
		ContentGeneration: 1, Filename: "scanned.pdf", Extension: ".pdf", MediaType: "application/pdf",
		SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), StoragePath: path,
	}
}

// buildImageOnlyTestPDF creates a valid vector-only PDF with no text layer.
// Poppler therefore reports the exact page count while pdftotext emits only
// form-feed separators, which exercises the production scanned-page path.
func buildImageOnlyTestPDF(t *testing.T, pages int) []byte {
	t.Helper()
	if pages <= 0 {
		t.Fatal("pages must be positive")
	}
	kids := make([]string, pages)
	objects := make([][]byte, 2+pages*2)
	objects[0] = []byte("<< /Type /Catalog /Pages 2 0 R >>")
	for i := 0; i < pages; i++ {
		pageObject := 3 + i*2
		contentObject := pageObject + 1
		kids[i] = strconv.Itoa(pageObject) + " 0 R"
		objects[pageObject-1] = []byte(fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Contents %d 0 R >>",
			contentObject,
		))
		stream := []byte(fmt.Sprintf("q 0 0 0 RG %d %d 80 80 re S Q", 5+i%3, 5+i%5))
		objects[contentObject-1] = []byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	objects[1] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pages))

	var out bytes.Buffer
	out.WriteString("%PDF-1.6\n")
	offsets := make([]int, len(objects)+1)
	for i, object := range objects {
		offsets[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}

func buildTextLayerTestPDF(t *testing.T, lines []string) []byte {
	t.Helper()
	var content strings.Builder
	content.WriteString("BT /F1 12 Tf 40 740 Td ")
	for i, line := range lines {
		if i > 0 {
			content.WriteString(" 0 -20 Td ")
		}
		fmt.Fprintf(&content, "(%s) Tj", line)
	}
	content.WriteString(" ET")
	objects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		[]byte("<< /Type /Pages /Kids [3 0 R] /Count 1 >>"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>"),
		[]byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", content.Len(), content.String())),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	}
	var out bytes.Buffer
	out.WriteString("%PDF-1.6\n")
	offsets := make([]int, len(objects)+1)
	for i, object := range objects {
		offsets[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.Bytes()
}
