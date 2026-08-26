package api

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

// TestPDFPageCheckpointsPersistCanonicalOffsets 钉住页 checkpoint 与 canonical 文本坐标的一致性。
// 在修复前，PrepareResumable 返回的 chunk 有偏移，但 durable page checkpoint 仍是零值。
func TestPDFPageCheckpointsPersistCanonicalOffsets(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	source := writeAsyncProcessorPDF(t, buildTextLayerPagesForOffsetTest(t, []string{
		"page one alpha contains enough canonical lesson text for the text layer threshold and offset assertion",
		"page two beta contains enough canonical lesson text for the text layer threshold and offset assertion",
	}))
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerFunc(func(_ context.Context, _ []byte, _ string) (string, error) {
		return "", fmt.Errorf("text-layer fixture must not call VLM")
	}))
	processor := NewKnowledgeDocumentIngestProcessor(manager).(knowledge.ResumableDocumentIngestProcessor)
	progress := &memoryIngestPageProgress{}
	prepared, err := processor.PrepareResumable(context.Background(), source, progress)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.PageCount != 2 || len(progress.pages) != 2 {
		t.Fatalf("page_count=%d checkpoints=%d, want 2/2", prepared.PageCount, len(progress.pages))
	}
	previousEnd := int64(-1)
	for pageNumber, want := range map[int]string{
		1: "page one alpha contains enough canonical lesson text for the text layer threshold and offset assertion",
		2: "page two beta contains enough canonical lesson text for the text layer threshold and offset assertion",
	} {
		checkpoint, ok := progress.pages[pageNumber]
		if !ok {
			t.Fatalf("missing checkpoint page=%d", pageNumber)
		}
		if checkpoint.SourceOffsetStart < 0 || checkpoint.SourceOffsetEnd <= checkpoint.SourceOffsetStart {
			t.Fatalf("page %d has empty source range: %+v", pageNumber, checkpoint)
		}
		if previousEnd >= 0 && checkpoint.SourceOffsetStart < previousEnd {
			t.Fatalf("page %d source range regressed: previous_end=%d checkpoint=%+v", pageNumber, previousEnd, checkpoint)
		}
		if checkpoint.SourceOffsetEnd > int64(len(prepared.Document.Content)) {
			t.Fatalf("page %d source range exceeds canonical text: %+v", pageNumber, checkpoint)
		}
		if got := prepared.Document.Content[checkpoint.SourceOffsetStart:checkpoint.SourceOffsetEnd]; got != want {
			t.Fatalf("page %d range=%q want exact page text %q", pageNumber, got, want)
		}
		previousEnd = checkpoint.SourceOffsetEnd
	}
}

// TestPDFPageCheckpointsCommitFinalOffsetsAfterEarlierOCR 验证 OCR 页位于文本页之前时，
// 后续文本页 checkpoint 仍使用完整 canonical 文本中的最终范围。
func TestPDFPageCheckpointsCommitFinalOffsetsAfterEarlierOCR(t *testing.T) {
	requirePopplerForAsyncPDFTest(t)
	t.Setenv("HEXCLAW_DOC_VLM_MAX_PAGES", "250")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_DPI", "72")
	t.Setenv("HEXCLAW_DOC_VLM_RENDER_BATCH_PAGES", "1")
	const textPage = "page two text layer remains after the scanned first page and must keep its final canonical offset"
	manager := newAsyncProcessorTestManager(t, knowledge.CaptionerFunc(func(_ context.Context, _ []byte, _ string) (string, error) {
		return "page one OCR caption", nil
	}))
	source := writeAsyncProcessorPDF(t, buildMixedPDFForOffsetTest(t, textPage))
	processor := NewKnowledgeDocumentIngestProcessor(manager).(knowledge.ResumableDocumentIngestProcessor)
	progress := &memoryIngestPageProgress{}
	prepared, err := processor.PrepareResumable(context.Background(), source, progress)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.PageCount != 2 || len(progress.pages) != 2 {
		t.Fatalf("page_count=%d checkpoints=%d, want 2/2", prepared.PageCount, len(progress.pages))
	}
	checkpoint := progress.pages[2]
	if checkpoint.SourceOffsetEnd <= checkpoint.SourceOffsetStart ||
		checkpoint.SourceOffsetEnd > int64(len(prepared.Document.Content)) {
		t.Fatalf("text page has invalid final range: %+v content_len=%d", checkpoint, len(prepared.Document.Content))
	}
	if got := prepared.Document.Content[checkpoint.SourceOffsetStart:checkpoint.SourceOffsetEnd]; got != textPage {
		t.Fatalf("text page range=%q want exact page text %q; content=%q", got, textPage, prepared.Document.Content)
	}
	if first := progress.pages[1]; first.SourceOffsetEnd >= checkpoint.SourceOffsetStart {
		t.Fatalf("page ranges overlap or regress: page1=%+v page2=%+v", first, checkpoint)
	}
}

func buildTextLayerPagesForOffsetTest(t *testing.T, pages []string) []byte {
	t.Helper()
	if len(pages) == 0 {
		t.Fatal("pages must be positive")
	}
	pageObjects := make([][]byte, 0, len(pages)*2+2)
	pageObjects = append(pageObjects,
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		nil,
	)
	kids := make([]string, 0, len(pages))
	for i, pageText := range pages {
		pageObject := 3 + i*2
		contentObject := pageObject + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", pageObject))
		stream := fmt.Sprintf("BT /F1 12 Tf 40 740 Td (%s) Tj ET", pageText)
		pageObjects = append(pageObjects,
			[]byte(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>", 3+len(pages)*2, contentObject)),
			[]byte(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)),
		)
	}
	pageObjects[1] = []byte(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	pageObjects = append(pageObjects, []byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"))

	var out bytes.Buffer
	out.WriteString("%PDF-1.6\n")
	offsets := make([]int, len(pageObjects)+1)
	for i, object := range pageObjects {
		offsets[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(pageObjects)+1)
	for i := 1; i <= len(pageObjects); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(pageObjects)+1, xref)
	return out.Bytes()
}

func buildMixedPDFForOffsetTest(t *testing.T, textPage string) []byte {
	t.Helper()
	pageObjects := [][]byte{
		[]byte("<< /Type /Catalog /Pages 2 0 R >>"),
		nil,
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 100 100] /Contents 4 0 R >>"),
		[]byte("<< /Length 29 >>\nstream\nq 0 0 0 RG 5 5 80 80 re S Q\nendstream"),
		[]byte("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 7 0 R >> >> /Contents 6 0 R >>"),
		[]byte(fmt.Sprintf("<< /Length %d >>\nstream\nBT /F1 12 Tf 40 740 Td (%s) Tj ET\nendstream", len(fmt.Sprintf("BT /F1 12 Tf 40 740 Td (%s) Tj ET", textPage)), fmt.Sprintf("%s", textPage))),
		[]byte("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"),
	}
	pageObjects[1] = []byte("<< /Type /Pages /Kids [3 0 R 5 0 R] /Count 2 >>")
	var out bytes.Buffer
	out.WriteString("%PDF-1.6\n")
	offsets := make([]int, len(pageObjects)+1)
	for i, object := range pageObjects {
		offsets[i+1] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n", i+1)
		out.Write(object)
		out.WriteString("\nendobj\n")
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(pageObjects)+1)
	for i := 1; i <= len(pageObjects); i++ {
		fmt.Fprintf(&out, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(pageObjects)+1, xref)
	return out.Bytes()
}
