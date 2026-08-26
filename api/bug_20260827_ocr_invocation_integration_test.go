package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

type durableOCRProgressProbe struct {
	*memoryIngestPageProgress
	invocation *knowledge.OCRPageInvocation
	result     knowledge.OCRPageInvocationResult
	failCommit bool
}

func (p *durableOCRProgressProbe) ClaimOCRPageInvocationContext(
	_ context.Context, claim knowledge.OCRPageInvocationClaim,
) (knowledge.OCRPageInvocation, error) {
	if p.invocation != nil {
		stored := *p.invocation
		stored.Fresh = false
		return stored, nil
	}
	invocation := knowledge.OCRPageInvocation{
		InvocationID: "ocr-integration-1", JobID: "job-1", PageNumber: claim.PageNumber,
		PagesTotal: claim.PagesTotal, SourceDigest: claim.SourceDigest,
		RequestDigest: claim.RequestDigest, Provider: claim.Provider, Model: claim.Model,
		Operation: knowledge.OCRRouteOperationPDFPage,
		Status:    knowledge.OCRPageInvocationStatusRunning, Fresh: true,
	}
	p.invocation = &invocation
	return invocation, nil
}

func (p *durableOCRProgressProbe) SaveOCRPageInvocationContext(
	_ context.Context, invocation knowledge.OCRPageInvocation, result knowledge.OCRPageInvocationResult,
) error {
	if p.invocation == nil || p.invocation.InvocationID != invocation.InvocationID {
		return errors.New("missing OCR invocation")
	}
	p.invocation.Status = knowledge.OCRPageInvocationStatusSucceeded
	p.invocation.Content = result.Content
	p.invocation.RouteReceipt = result.RouteReceipt
	p.result = result
	return nil
}

func (p *durableOCRProgressProbe) CommitPage(
	ctx context.Context, checkpoint knowledge.IngestPageCheckpoint,
) error {
	if p.failCommit {
		p.failCommit = false
		return errors.New("injected checkpoint crash")
	}
	return p.memoryIngestPageProgress.CommitPage(ctx, checkpoint)
}

func TestOCRPageInvocationIsClaimedBeforeCheckpointAndReusedAfterCheckpointCrash(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", "1")
	source := writeAsyncProcessorPDF(t, buildImageOnlyTestPDF(t, 1))
	pdfBytes, err := os.ReadFile(source.StoragePath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(pdfBytes)
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerWithReceiptFunc(
		func(_ context.Context, _ []byte, _ string) (knowledge.CaptionResult, error) {
			return testOCRCaptionResult("第 1 页教材正文"), nil
		},
	))
	processor := NewKnowledgeDocumentIngestProcessor(manager).(knowledge.ResumableDocumentIngestProcessor)
	progress := &durableOCRProgressProbe{
		memoryIngestPageProgress: &memoryIngestPageProgress{}, failCommit: true,
	}
	source.JobID = "job-1"
	source.DocumentID = "doc-1"
	source.OwnerID = "owner"
	source.SizeBytes = int64(len(pdfBytes))
	source.SHA256 = hex.EncodeToString(digest[:])
	// 直接调用解析器时显式注入与 worker 相同的冻结视觉路由。
	ctx := knowledge.WithVisionRouteSnapshot(context.Background(), knowledge.VisionRouteSnapshot{
		ProviderInstanceID: "hexclaw-gpt", ProviderName: "hexclaw-gpt",
		ProviderDisplayName: "HexClaw-GPT", Model: "gpt-5.6-sol", Capabilities: []string{"vision"},
	})
	if _, err := processor.PrepareResumable(ctx, source, progress); err == nil {
		t.Fatal("checkpoint crash must interrupt first run")
	}
	if progress.invocation == nil || progress.invocation.Status != knowledge.OCRPageInvocationStatusSucceeded {
		t.Fatalf("OCR result was not durable before checkpoint failure: %+v", progress.invocation)
	}
	prepared, err := processor.PrepareResumable(ctx, source, progress)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.PageCount != 1 || progress.invocation.Content != "第 1 页教材正文" {
		t.Fatalf("recovered OCR result=%+v prepared=%+v", progress.invocation, prepared)
	}
	if len(progress.pages) != 1 {
		t.Fatalf("recovered checkpoint count=%d want 1", len(progress.pages))
	}
}
