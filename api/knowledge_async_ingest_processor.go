package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/resourcegov"
)

const (
	maxAsyncTextSourceBytes         int64 = 32 << 20
	maxAsyncDOCXSourceBytes         int64 = 32 << 20
	maxAsyncImageSourceBytes        int64 = 20 << 20
	maxAsyncOtherSourceBytes        int64 = 32 << 20
	maxAsyncDOCXXMLBytes            int64 = 32 << 20
	maxAsyncDOCXEmbeddedImageBytes  int64 = 64 << 20
	asyncIngestStreamingBufferBytes       = 32 << 10
)

type knowledgeDocumentIngestProcessor struct {
	manager  *knowledge.Manager
	governor *resourcegov.Governor
}

type KnowledgeDocumentIngestOption func(*knowledgeDocumentIngestProcessor)

func WithKnowledgeResourceGovernor(governor *resourcegov.Governor) KnowledgeDocumentIngestOption {
	return func(processor *knowledgeDocumentIngestProcessor) { processor.governor = governor }
}

// NewKnowledgeDocumentIngestProcessor adapts the existing parser/OCR and
// Manager splitter to the durable ingest worker. It prepares data only: the
// repository owns the single publication transaction.
func NewKnowledgeDocumentIngestProcessor(
	manager *knowledge.Manager,
	options ...KnowledgeDocumentIngestOption,
) knowledge.DocumentIngestProcessor {
	processor := &knowledgeDocumentIngestProcessor{manager: manager}
	for _, option := range options {
		if option != nil {
			option(processor)
		}
	}
	return processor
}

func (p *knowledgeDocumentIngestProcessor) Prepare(
	ctx context.Context,
	source knowledge.PersistedIngestDocument,
) (knowledge.PreparedIngestDocument, error) {
	return p.prepare(ctx, source, nil)
}

func (p *knowledgeDocumentIngestProcessor) PrepareResumable(
	ctx context.Context,
	source knowledge.PersistedIngestDocument,
	progress knowledge.IngestPageProgress,
) (knowledge.PreparedIngestDocument, error) {
	return p.prepare(ctx, source, progress)
}

func (p *knowledgeDocumentIngestProcessor) prepare(
	ctx context.Context,
	source knowledge.PersistedIngestDocument,
	progress knowledge.IngestPageProgress,
) (knowledge.PreparedIngestDocument, error) {
	if p == nil || p.manager == nil {
		return knowledge.PreparedIngestDocument{}, knowledge.ErrDocumentIngestUnavailable
	}
	if err := validatePersistedIngestSource(source); err != nil {
		return knowledge.PreparedIngestDocument{}, err
	}
	if err := validateAsyncIngestMemoryBudget(source); err != nil {
		return knowledge.PreparedIngestDocument{}, err
	}
	ctx = resourcegov.WithPriority(ctx, resourcegov.PriorityBackground)
	pageCount := int64(1)
	warnings := []string{}
	var content string
	var extractedPages []documentPageExtraction
	if source.Extension == ".pdf" {
		if err := verifyIngestSource(ctx, source); err != nil {
			return knowledge.PreparedIngestDocument{}, err
		}
		extracted, extractErr := extractPDFForAsyncIngestWithProgress(
			ctx, source.StoragePath, p.manager, asyncPDFLimitsFromEnv(), source.SHA256, progress,
			p.governor,
		)
		if extractErr != nil {
			return knowledge.PreparedIngestDocument{}, extractErr
		}
		content = strings.TrimSpace(extracted.Text)
		warnings = append(warnings, extracted.Warnings...)
		extractedPages = extracted.Pages
		if extracted.PageCount > 0 {
			pageCount = int64(extracted.PageCount)
		}
	} else if strings.HasPrefix(strings.ToLower(source.MediaType), "image/") {
		data, err := readAndVerifyBufferedIngestSource(ctx, source, maxAsyncImageSourceBytes)
		if err != nil {
			return knowledge.PreparedIngestDocument{}, err
		}
		caption, captionErr := p.manager.CaptionImage(ctx, data, source.MediaType)
		if captionErr != nil {
			return knowledge.PreparedIngestDocument{}, captionErr
		}
		content = "【图像内容】\n" + strings.TrimSpace(caption)
	} else {
		extracted, extractErr := p.runCPU(ctx, func() (documentExtractionResult, error) {
			if isAsyncTextExtension(source.Extension) {
				text, err := readAndVerifyTextIngestSource(ctx, source)
				return documentExtractionResult{Text: text}, err
			}
			data, err := readAndVerifyBufferedIngestSource(
				ctx, source, asyncIngestSourceMemoryBudget(source),
			)
			if err != nil {
				return documentExtractionResult{}, err
			}
			if strings.EqualFold(source.Extension, ".docx") {
				if err := validateDOCXExpandedMemoryBudgets(
					ctx, data, maxAsyncDOCXXMLBytes, maxAsyncDOCXEmbeddedImageBytes,
				); err != nil {
					return documentExtractionResult{}, err
				}
			}
			return extractDocumentForKnowledge(ctx, source.Extension, data, p.manager)
		})
		if extractErr != nil {
			return knowledge.PreparedIngestDocument{}, extractErr
		}
		content = strings.TrimSpace(extracted.Text)
		warnings = append(warnings, extracted.Warnings...)
		if extracted.PageCount > 0 {
			pageCount = int64(extracted.PageCount)
		}
	}
	if content == "" {
		return knowledge.PreparedIngestDocument{}, fmt.Errorf("%w: extracted document is empty", knowledge.ErrInvalidDocumentUpload)
	}
	now := time.Now().UTC()
	document := &knowledge.Document{
		ID: source.DocumentID, Title: source.Filename, Content: content,
		Source: "upload:" + source.Filename, SourceType: "upload", Status: "indexed",
		CreatedAt: now, UpdatedAt: now,
	}
	if strings.HasPrefix(strings.ToLower(source.MediaType), "image/") {
		document.Source = "image:upload:" + source.Filename
		document.SourceType = "image"
	}
	var chunks []*knowledge.Chunk
	var err error
	if len(extractedPages) > 0 {
		pages := make([]knowledge.SourcePage, 0, len(extractedPages))
		for _, page := range extractedPages {
			if strings.TrimSpace(page.Text) == "" {
				continue
			}
			pages = append(pages, knowledge.SourcePage{
				PageStart: page.SourcePageFrom, PageEnd: page.SourcePageTo,
				Text: page.Text, SourceDigest: source.SHA256,
				SourceOffsetStart: page.SourceOffsetFrom, SourceOffsetEnd: page.SourceOffsetTo,
			})
		}
		chunks, err = p.manager.PrepareIngestPages(ctx, document, pages)
	} else {
		chunks, err = p.manager.PrepareIngestDocument(ctx, document)
		for _, chunk := range chunks {
			chunk.PageStart = 1
			chunk.PageEnd = 1
			chunk.SourceDigest = source.SHA256
			chunk.SourceOffsetStart = 0
			chunk.SourceOffsetEnd = int64(len(content))
		}
	}
	if err != nil {
		return knowledge.PreparedIngestDocument{}, err
	}
	return knowledge.PreparedIngestDocument{
		Document: document, Chunks: chunks, PageCount: pageCount, Warnings: warnings,
	}, nil
}

func validateAsyncIngestMemoryBudget(source knowledge.PersistedIngestDocument) error {
	limit := asyncIngestSourceMemoryBudget(source)
	if limit >= knowledge.MaxKnowledgeDocumentBytes {
		return nil
	}
	if source.SizeBytes > limit {
		return fmt.Errorf(
			"%w: %s source exceeds %d-byte processing memory budget",
			knowledge.ErrInvalidDocumentUpload, source.Extension, limit,
		)
	}
	return nil
}

func asyncIngestSourceMemoryBudget(source knowledge.PersistedIngestDocument) int64 {
	extension := strings.ToLower(strings.TrimSpace(source.Extension))
	mediaType := strings.ToLower(strings.TrimSpace(source.MediaType))
	switch {
	case extension == ".pdf":
		// The PDF path verifies the source with io.Copy and delegates extraction
		// to bounded file/process streams; it never materializes the source bytes.
		return knowledge.MaxKnowledgeDocumentBytes
	case strings.HasPrefix(mediaType, "image/"):
		return maxAsyncImageSourceBytes
	case isAsyncTextExtension(extension):
		return maxAsyncTextSourceBytes
	case extension == ".docx":
		return maxAsyncDOCXSourceBytes
	default:
		return maxAsyncOtherSourceBytes
	}
}

func isAsyncTextExtension(extension string) bool {
	switch strings.ToLower(strings.TrimSpace(extension)) {
	case ".txt", ".md", ".csv", ".json":
		return true
	default:
		return false
	}
}

func (p *knowledgeDocumentIngestProcessor) runCPU(
	ctx context.Context,
	operation func() (documentExtractionResult, error),
) (documentExtractionResult, error) {
	if p == nil || p.governor == nil {
		return operation()
	}
	permit, err := p.governor.Acquire(ctx, resourcegov.ResourceCPUHeavy, resourcegov.PriorityBackground)
	if err != nil {
		return documentExtractionResult{}, err
	}
	defer permit.Release()
	return operation()
}

func readAndVerifyTextIngestSource(
	ctx context.Context,
	source knowledge.PersistedIngestDocument,
) (string, error) {
	if err := validatePersistedIngestSource(source); err != nil {
		return "", err
	}
	file, err := os.Open(source.StoragePath)
	if err != nil {
		return "", fmt.Errorf("knowledge: open persisted source: %w", err)
	}
	defer file.Close()
	return readAndVerifyTextIngestReader(ctx, file, source.SizeBytes, source.SHA256)
}

func readAndVerifyTextIngestReader(
	ctx context.Context,
	reader io.Reader,
	sizeBytes int64,
	expectedSHA256 string,
) (string, error) {
	if sizeBytes <= 0 || sizeBytes > maxAsyncTextSourceBytes {
		return "", fmt.Errorf("%w: text source exceeds processing memory budget", knowledge.ErrInvalidDocumentUpload)
	}
	var content strings.Builder
	content.Grow(int(sizeBytes))
	hasher := sha256.New()
	limited := io.LimitReader(ingestContextReader{ctx: ctx, reader: reader}, sizeBytes+1)
	written, err := io.CopyBuffer(
		io.MultiWriter(&content, hasher), limited, make([]byte, asyncIngestStreamingBufferBytes),
	)
	if err != nil {
		return "", fmt.Errorf("knowledge: read persisted text source: %w", err)
	}
	if written != sizeBytes {
		return "", fmt.Errorf("%w: persisted source size mismatch", knowledge.ErrInvalidDocumentUpload)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expectedSHA256 {
		return "", fmt.Errorf("%w: persisted source digest mismatch", knowledge.ErrInvalidDocumentUpload)
	}
	return content.String(), nil
}

func readAndVerifyBufferedIngestSource(
	ctx context.Context,
	source knowledge.PersistedIngestDocument,
	maxBytes int64,
) ([]byte, error) {
	if err := validatePersistedIngestSource(source); err != nil {
		return nil, err
	}
	if maxBytes <= 0 || source.SizeBytes > maxBytes {
		return nil, fmt.Errorf("%w: %s source exceeds processing memory budget", knowledge.ErrInvalidDocumentUpload, source.Extension)
	}
	file, err := os.Open(source.StoragePath)
	if err != nil {
		return nil, fmt.Errorf("knowledge: open persisted source: %w", err)
	}
	defer file.Close()

	data := make([]byte, int(source.SizeBytes))
	hasher := sha256.New()
	reader := ingestContextReader{ctx: ctx, reader: file}
	read, err := io.ReadFull(io.TeeReader(reader, hasher), data)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("knowledge: read persisted source: %w", err)
		}
	}
	if read != len(data) {
		return nil, fmt.Errorf("%w: persisted source size mismatch", knowledge.ErrInvalidDocumentUpload)
	}
	var extra [1]byte
	extraBytes, extraErr := reader.Read(extra[:])
	if extraErr != nil && extraErr != io.EOF {
		return nil, fmt.Errorf("knowledge: verify persisted source length: %w", extraErr)
	}
	if extraBytes != 0 {
		return nil, fmt.Errorf("%w: persisted source size mismatch", knowledge.ErrInvalidDocumentUpload)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != source.SHA256 {
		return nil, fmt.Errorf("%w: persisted source digest mismatch", knowledge.ErrInvalidDocumentUpload)
	}
	return data, nil
}

func validateDOCXExpandedMemoryBudgets(
	ctx context.Context,
	data []byte,
	maxXMLBytes int64,
	maxEmbeddedImageBytes int64,
) error {
	if maxXMLBytes <= 0 || maxEmbeddedImageBytes <= 0 {
		return fmt.Errorf("%w: invalid DOCX expanded memory budget", knowledge.ErrInvalidDocumentUpload)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	var imageBytes int64
	imagesSeen := 0
	for _, entry := range archive.File {
		switch {
		case entry.Name == "word/document.xml":
			expanded, readErr := countExpandedZIPEntry(ctx, entry, maxXMLBytes)
			if readErr != nil {
				return readErr
			}
			if expanded > maxXMLBytes {
				return fmt.Errorf("%w: maximum is %d bytes", errDOCXXMLTooLarge, maxXMLBytes)
			}
		case strings.HasPrefix(entry.Name, "word/media/") && mimeFromImageName(entry.Name) != "":
			if imagesSeen >= docVisionMaxImages() {
				continue
			}
			imagesSeen++
			remaining := maxEmbeddedImageBytes - imageBytes
			if remaining < 0 {
				remaining = 0
			}
			expanded, readErr := countExpandedZIPEntry(ctx, entry, remaining)
			if readErr != nil {
				return readErr
			}
			imageBytes += expanded
			if imageBytes > maxEmbeddedImageBytes {
				return fmt.Errorf(
					"%w: DOCX embedded image memory budget exceeds %d bytes",
					knowledge.ErrInvalidDocumentUpload, maxEmbeddedImageBytes,
				)
			}
		}
	}
	return nil
}

func countExpandedZIPEntry(ctx context.Context, entry *zip.File, limit int64) (int64, error) {
	reader, err := entry.Open()
	if err != nil {
		return 0, err
	}
	defer reader.Close()
	return io.CopyBuffer(
		io.Discard,
		io.LimitReader(ingestContextReader{ctx: ctx, reader: reader}, limit+1),
		make([]byte, asyncIngestStreamingBufferBytes),
	)
}

func verifyIngestSource(ctx context.Context, source knowledge.PersistedIngestDocument) error {
	if err := validatePersistedIngestSource(source); err != nil {
		return err
	}
	file, err := os.Open(source.StoragePath)
	if err != nil {
		return fmt.Errorf("knowledge: open persisted source: %w", err)
	}
	defer file.Close()
	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(
		ingestContextReader{ctx: ctx, reader: file}, source.SizeBytes+1,
	))
	if err != nil {
		return fmt.Errorf("knowledge: verify persisted source: %w", err)
	}
	if written != source.SizeBytes {
		return fmt.Errorf("%w: persisted source size mismatch", knowledge.ErrInvalidDocumentUpload)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != source.SHA256 {
		return fmt.Errorf("%w: persisted source digest mismatch", knowledge.ErrInvalidDocumentUpload)
	}
	return nil
}

func validatePersistedIngestSource(source knowledge.PersistedIngestDocument) error {
	if source.SizeBytes <= 0 || source.SizeBytes > knowledge.MaxKnowledgeDocumentBytes ||
		len(source.SHA256) != 64 || strings.TrimSpace(source.StoragePath) == "" {
		return fmt.Errorf("%w: invalid persisted source metadata", knowledge.ErrInvalidDocumentUpload)
	}
	return nil
}

type ingestContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r ingestContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
