package usecase

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const printPDFRenderContractVersion = "k12-pdf-v1"

type PrepareGenericPrintRequest struct {
	AgentName         string
	IdempotencyKey    string
	SourceKind        string
	SourceRef         string
	Title             string
	CanonicalMarkdown string
}

type PreparePrintableArtifactRequest struct {
	AgentName         string
	SourceKind        string
	SourceRef         string
	Title             string
	CanonicalMarkdown string
	// RenderMarkdown 只在首次冻结 PDF 时承载同源媒体，不进入公开 Artifact 或 Desktop。
	RenderMarkdown string
}

type PrintableArtifactView struct {
	Artifact k12.PrintArtifact
	Render   k12.PrintArtifactRender
}

type GenericPrintView struct {
	Job      k12.GenericPrintJob
	Artifact k12.PrintArtifact
	Render   k12.PrintArtifactRender
}

type GenericPrintArtifactView struct {
	PrintJobID            string
	ArtifactID            string
	SourceKind            string
	SourceRef             string
	Title                 string
	SourceDigest          string
	Markdown              string
	RenderFormat          string
	RenderContractVersion string
	ContentType           string
	ByteDigest            string
	ByteSize              int64
	PDF                   []byte
}

func normalizePrintableArtifactRequest(req PreparePrintableArtifactRequest) (PreparePrintableArtifactRequest, error) {
	req.AgentName = strings.TrimSpace(req.AgentName)
	req.SourceKind = strings.TrimSpace(req.SourceKind)
	req.SourceRef = strings.TrimSpace(req.SourceRef)
	req.Title = strings.TrimSpace(req.Title)
	if req.AgentName == "" || req.SourceRef == "" || req.Title == "" ||
		strings.TrimSpace(req.CanonicalMarkdown) == "" {
		return PreparePrintableArtifactRequest{},
			fmt.Errorf("%w: agent/source_ref/title/canonical_markdown 必填", ErrInvalidInput)
	}
	if !k12.GenericPrintSourceKindAllowed(req.SourceKind) {
		return PreparePrintableArtifactRequest{},
			fmt.Errorf("%w: source_kind 不支持 %q", ErrInvalidInput, req.SourceKind)
	}
	if len(req.SourceRef) > 512 || len(req.Title) > 256 || len(req.CanonicalMarkdown) > 4<<20 {
		return PreparePrintableArtifactRequest{},
			fmt.Errorf("%w: 打印 Artifact 字段超出限制", ErrInvalidInput)
	}
	return req, nil
}

func buildPrintArtifact(req PreparePrintableArtifactRequest, at int64) k12.PrintArtifact {
	artifactBytes, _ := json.Marshal(struct {
		SourceKind        string `json:"source_kind"`
		SourceRef         string `json:"source_ref"`
		Title             string `json:"title"`
		CanonicalMarkdown string `json:"canonical_markdown"`
	}{req.SourceKind, req.SourceRef, req.Title, req.CanonicalMarkdown})
	sourceSum := sha256.Sum256(artifactBytes)
	sourceDigest := hex.EncodeToString(sourceSum[:])
	artifactIDSum := sha256.Sum256([]byte(req.AgentName + "\x00" + sourceDigest))
	return k12.PrintArtifact{
		ArtifactID: "part-" + hex.EncodeToString(artifactIDSum[:12]),
		AgentName:  req.AgentName, SourceKind: req.SourceKind,
		SourceRef: req.SourceRef, Title: req.Title, CanonicalMarkdown: req.CanonicalMarkdown,
		SourceDigest: sourceDigest, CreatedAt: at,
	}
}

func samePrintArtifact(left, right k12.PrintArtifact) bool {
	return left.ArtifactID == right.ArtifactID && left.AgentName == right.AgentName &&
		left.SourceKind == right.SourceKind && left.SourceRef == right.SourceRef &&
		left.Title == right.Title && left.CanonicalMarkdown == right.CanonicalMarkdown &&
		left.SourceDigest == right.SourceDigest
}

func (d Deps) renderPrintableArtifact(ctx context.Context,
	artifact k12.PrintArtifact, renderMarkdown string) (k12.PrintArtifactRender, error) {
	if d.Renderer == nil {
		return k12.PrintArtifactRender{},
			fmt.Errorf("%w: PDF renderer 未配置", ErrRenderUnavailable)
	}
	if strings.TrimSpace(renderMarkdown) == "" {
		renderMarkdown = artifact.CanonicalMarkdown
	}
	pdf, contentType, err := d.Renderer.Render(ctx, renderMarkdown, "pdf")
	if err != nil {
		return k12.PrintArtifactRender{},
			fmt.Errorf("%w: %v", ErrRenderUnavailable, err)
	}
	mediaType, _, mediaErr := mime.ParseMediaType(contentType)
	headerLimit := min(len(pdf), 1024)
	if mediaErr != nil || mediaType != "application/pdf" || len(pdf) == 0 ||
		len(pdf) > 30<<20 || !bytes.Contains(pdf[:headerLimit], []byte("%PDF-")) {
		return k12.PrintArtifactRender{},
			fmt.Errorf("%w: renderer 未返回有效 PDF", ErrRenderUnavailable)
	}
	sum := sha256.Sum256(pdf)
	return k12.PrintArtifactRender{
		ArtifactID: artifact.ArtifactID, Format: "pdf",
		RenderContractVersion: printPDFRenderContractVersion,
		ContentType:           "application/pdf",
		ByteDigest:            hex.EncodeToString(sum[:]),
		ByteSize:              int64(len(pdf)),
		Payload:               append([]byte(nil), pdf...),
		CreatedAt:             d.now(),
	}, nil
}

// PreparePrintableArtifact freezes one PDF projection without creating a
// PrintJob. Printing and export both call this single source-of-truth boundary.
func (d Deps) PreparePrintableArtifact(ctx context.Context,
	req PreparePrintableArtifactRequest) (PrintableArtifactView, bool, error) {
	req, err := normalizePrintableArtifactRequest(req)
	if err != nil {
		return PrintableArtifactView{}, false, err
	}
	artifact := buildPrintArtifact(req, d.now())
	if frozen, getErr := d.Records.GetPrintArtifact(ctx, req.AgentName, artifact.ArtifactID); getErr == nil {
		if !samePrintArtifact(frozen, artifact) {
			return PrintableArtifactView{}, false,
				fmt.Errorf("usecase: 打印 Artifact ID 已绑定其他内容")
		}
		render, renderErr := d.Records.GetPrintArtifactRender(ctx, req.AgentName, artifact.ArtifactID)
		if renderErr == nil {
			return PrintableArtifactView{Artifact: frozen, Render: render}, true, nil
		}
		if !errors.Is(renderErr, records.ErrNotFound) {
			return PrintableArtifactView{}, false,
				fmt.Errorf("usecase: 取冻结打印 Artifact PDF: %w", renderErr)
		}
	} else if !errors.Is(getErr, records.ErrNotFound) {
		return PrintableArtifactView{}, false,
			fmt.Errorf("usecase: 取打印 Artifact: %w", getErr)
	}
	render, err := d.renderPrintableArtifact(ctx, artifact, req.RenderMarkdown)
	if err != nil {
		return PrintableArtifactView{}, false, err
	}
	frozenArtifact, frozenRender, replay, err := d.Records.FreezePrintArtifact(ctx, artifact, render)
	if err != nil {
		return PrintableArtifactView{}, false,
			fmt.Errorf("usecase: freeze 打印 Artifact/PDF: %w", err)
	}
	return PrintableArtifactView{Artifact: frozenArtifact, Render: frozenRender}, replay, nil
}

func (d Deps) GetPrintableArtifactPDF(ctx context.Context, agentName,
	artifactID string) (PrintableArtifactView, error) {
	artifact, err := d.Records.GetPrintArtifact(ctx, strings.TrimSpace(agentName),
		strings.TrimSpace(artifactID))
	if err != nil {
		return PrintableArtifactView{}, fmt.Errorf("usecase: 取打印 Artifact: %w", err)
	}
	render, err := d.Records.GetPrintArtifactRender(ctx, artifact.AgentName, artifact.ArtifactID)
	if err != nil {
		return PrintableArtifactView{}, fmt.Errorf("usecase: 取冻结打印 Artifact PDF: %w", err)
	}
	return PrintableArtifactView{Artifact: artifact, Render: render}, nil
}

// PrepareGenericPrint freezes PDF bytes before any native dialog or receipt
// state is created. The source domain object is never written.
func (d Deps) PrepareGenericPrint(ctx context.Context, req PrepareGenericPrintRequest) (GenericPrintView, bool, error) {
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	if req.IdempotencyKey == "" {
		return GenericPrintView{}, false,
			fmt.Errorf("%w: idempotency_key 必填", ErrInvalidInput)
	}
	if len(req.IdempotencyKey) > 512 {
		return GenericPrintView{}, false,
			fmt.Errorf("%w: idempotency_key 超出限制", ErrInvalidInput)
	}
	printable, _, err := d.PreparePrintableArtifact(ctx, PreparePrintableArtifactRequest{
		AgentName: req.AgentName, SourceKind: req.SourceKind, SourceRef: req.SourceRef,
		Title: req.Title, CanonicalMarkdown: req.CanonicalMarkdown,
	})
	if err != nil {
		return GenericPrintView{}, false, err
	}
	requestSum := sha256.Sum256([]byte(printable.Artifact.AgentName + "\x00" + printable.Artifact.ArtifactID))
	jobIDSum := sha256.Sum256([]byte(printable.Artifact.AgentName + "\x00" + req.IdempotencyKey))
	at := d.now()
	job := k12.GenericPrintJob{
		PrintJobID: "gprint-" + hex.EncodeToString(jobIDSum[:12]), AgentName: printable.Artifact.AgentName,
		IdempotencyKey: req.IdempotencyKey, RequestDigest: hex.EncodeToString(requestSum[:]),
		ArtifactID: printable.Artifact.ArtifactID, PreparedAt: at,
	}
	stored, replay, err := d.Records.PrepareGenericPrintJob(ctx, printable.Artifact, job)
	if err != nil {
		return GenericPrintView{}, false, fmt.Errorf("usecase: prepare 通用 PrintJob: %w", err)
	}
	frozen, err := d.Records.GetPrintArtifact(ctx, req.AgentName, stored.ArtifactID)
	if err != nil {
		return GenericPrintView{}, false, fmt.Errorf("usecase: 取冻结打印 Artifact: %w", err)
	}
	return GenericPrintView{Job: stored, Artifact: frozen, Render: printable.Render}, replay, nil
}

// PrepareGenericPrintFromArtifact creates the shared native PrintJob from an
// already-frozen artifact. It never rerenders and therefore preserves the PDF
// byte digest returned by prepare-output.
func (d Deps) PrepareGenericPrintFromArtifact(ctx context.Context, agentName,
	idempotencyKey, artifactID string) (GenericPrintView, bool, error) {
	agentName = strings.TrimSpace(agentName)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	artifactID = strings.TrimSpace(artifactID)
	if agentName == "" || idempotencyKey == "" || artifactID == "" {
		return GenericPrintView{}, false,
			fmt.Errorf("%w: agent/idempotency_key/artifact_id 必填", ErrInvalidInput)
	}
	if len(idempotencyKey) > 512 {
		return GenericPrintView{}, false,
			fmt.Errorf("%w: idempotency_key 超出限制", ErrInvalidInput)
	}
	artifact, err := d.Records.GetPrintArtifact(ctx, agentName, artifactID)
	if err != nil {
		return GenericPrintView{}, false, fmt.Errorf("usecase: 取冻结打印 Artifact: %w", err)
	}
	render, err := d.Records.GetPrintArtifactRender(ctx, agentName, artifactID)
	if err != nil {
		return GenericPrintView{}, false, fmt.Errorf("usecase: 取冻结打印 Artifact PDF: %w", err)
	}
	requestSum := sha256.Sum256([]byte(agentName + "\x00" + artifactID))
	jobIDSum := sha256.Sum256([]byte(agentName + "\x00" + idempotencyKey))
	at := d.now()
	job := k12.GenericPrintJob{
		PrintJobID: "gprint-" + hex.EncodeToString(jobIDSum[:12]), AgentName: agentName,
		IdempotencyKey: idempotencyKey, RequestDigest: hex.EncodeToString(requestSum[:]),
		ArtifactID: artifactID, PreparedAt: at,
	}
	stored, replay, err := d.Records.PrepareGenericPrintJob(ctx, artifact, job)
	if err != nil {
		return GenericPrintView{}, false, fmt.Errorf("usecase: prepare 通用 PrintJob: %w", err)
	}
	return GenericPrintView{Job: stored, Artifact: artifact, Render: render}, replay, nil
}

func (d Deps) GetGenericPrint(ctx context.Context, agentName, jobID string) (GenericPrintView, error) {
	job, err := d.Records.GetGenericPrintJob(ctx, strings.TrimSpace(agentName), strings.TrimSpace(jobID))
	if err != nil {
		return GenericPrintView{}, fmt.Errorf("usecase: 取通用 PrintJob: %w", err)
	}
	artifact, err := d.Records.GetPrintArtifact(ctx, job.AgentName, job.ArtifactID)
	if err != nil {
		return GenericPrintView{}, fmt.Errorf("usecase: 取通用 PrintJob Artifact: %w", err)
	}
	render, renderErr := d.Records.GetPrintArtifactRender(ctx, job.AgentName, job.ArtifactID)
	if renderErr != nil && !errors.Is(renderErr, records.ErrNotFound) {
		return GenericPrintView{}, fmt.Errorf("usecase: 取通用 PrintJob PDF: %w", renderErr)
	}
	return GenericPrintView{Job: job, Artifact: artifact, Render: render}, nil
}

func (d Deps) RenderGenericPrintArtifact(ctx context.Context, agentName, jobID string) (GenericPrintArtifactView, error) {
	v, err := d.GetGenericPrint(ctx, agentName, jobID)
	if err != nil {
		return GenericPrintArtifactView{}, err
	}
	return GenericPrintArtifactView{
		PrintJobID: v.Job.PrintJobID, ArtifactID: v.Artifact.ArtifactID,
		SourceKind: v.Artifact.SourceKind, SourceRef: v.Artifact.SourceRef,
		Title: v.Artifact.Title, SourceDigest: v.Artifact.SourceDigest,
		Markdown: v.Artifact.CanonicalMarkdown, RenderFormat: v.Render.Format,
		RenderContractVersion: v.Render.RenderContractVersion,
		ContentType:           v.Render.ContentType, ByteDigest: v.Render.ByteDigest,
		ByteSize: v.Render.ByteSize, PDF: append([]byte(nil), v.Render.Payload...),
	}, nil
}

func (d Deps) RecordGenericPrintEvent(ctx context.Context, agentName, jobID string,
	event PracticePrintEvent) (GenericPrintView, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(jobID) == "" {
		return GenericPrintView{}, fmt.Errorf("%w: agent / print_job_id 必填", ErrInvalidInput)
	}
	var job k12.GenericPrintJob
	var err error
	switch event.Status {
	case k12.PrintJobPrinted:
		if strings.TrimSpace(event.NativeJobID) == "" || strings.TrimSpace(event.NativeReceiptID) == "" ||
			!validPrinterSnapshot(event.PrinterSnapshot) {
			return GenericPrintView{}, fmt.Errorf("%w: printed 必须携带有效 native_job_id/native_receipt_id/printer_snapshot", ErrInvalidInput)
		}
		job, err = d.Records.CommitGenericPrintJob(ctx, agentName, jobID, event.NativeJobID,
			event.NativeReceiptID, event.PrinterSnapshot, d.now())
	case k12.PrintJobDialogOpen, k12.PrintJobSubmitted, k12.PrintJobCancelled,
		k12.PrintJobFailed, k12.PrintJobOutcomeUnknown:
		if (event.Status == k12.PrintJobFailed || event.Status == k12.PrintJobOutcomeUnknown) &&
			strings.TrimSpace(event.FailureKind) == "" {
			return GenericPrintView{}, fmt.Errorf("%w: failed/outcome_unknown 必须携带 failure_kind", ErrInvalidInput)
		}
		job, err = d.Records.AdvanceGenericPrintJob(ctx, agentName, jobID, event.Status,
			event.NativeJobID, event.FailureKind, event.FailureDetail, d.now())
	default:
		return GenericPrintView{}, fmt.Errorf("%w: PrintJob 事件状态非法 %q", ErrInvalidInput, event.Status)
	}
	if err != nil {
		return GenericPrintView{}, fmt.Errorf("usecase: 记录通用 PrintJob 事件: %w", err)
	}
	artifact, err := d.Records.GetPrintArtifact(ctx, agentName, job.ArtifactID)
	if err != nil {
		return GenericPrintView{}, err
	}
	return GenericPrintView{Job: job, Artifact: artifact}, nil
}

func (d Deps) RetryGenericPrint(ctx context.Context, agentName, jobID string) (GenericPrintView, error) {
	job, err := d.Records.RetryGenericPrintJob(ctx, strings.TrimSpace(agentName), strings.TrimSpace(jobID), d.now())
	if err != nil {
		return GenericPrintView{}, fmt.Errorf("usecase: 重试通用 PrintJob: %w", err)
	}
	artifact, err := d.Records.GetPrintArtifact(ctx, job.AgentName, job.ArtifactID)
	if err != nil {
		return GenericPrintView{}, err
	}
	return GenericPrintView{Job: job, Artifact: artifact}, nil
}

func (d Deps) gradingFinalArtifactPrintRequest(
	ctx context.Context,
	artifact k12.GradingFinalArtifact,
	title string,
) (PreparePrintableArtifactRequest, error) {
	req := PreparePrintableArtifactRequest{
		AgentName:         artifact.AgentName,
		SourceKind:        k12.PrintSourceGradingFinalArtifact,
		SourceRef:         "final_artifact:" + artifact.ArtifactID + ":" + artifact.ArtifactDigest,
		Title:             title,
		CanonicalMarkdown: artifact.CanonicalMarkdown,
	}
	if !artifact.HasAnnotatedAsset() {
		return req, nil
	}
	annotated, err := d.Records.OpenGradingFinalAnnotatedAsset(
		ctx, artifact.AgentName, artifact.ArtifactID,
	)
	if err != nil {
		return PreparePrintableArtifactRequest{}, err
	}
	imageMarkdown := "![](data:" + annotated.MIME + ";base64," +
		base64.StdEncoding.EncodeToString(annotated.Data) + "){width=92%}"
	const heading = "# 作业批改结果"
	canonical := strings.TrimSpace(artifact.CanonicalMarkdown)
	if strings.HasPrefix(canonical, heading) {
		body := strings.TrimSpace(strings.TrimPrefix(canonical, heading))
		req.RenderMarkdown = heading + "\n\n" + imageMarkdown
		if body != "" {
			req.RenderMarkdown += "\n\n" + body
		}
	} else {
		req.RenderMarkdown = imageMarkdown + "\n\n" + canonical
	}
	return req, nil
}

func (d Deps) getExactGradingFinalArtifact(
	ctx context.Context,
	agentName, finalArtifactID, expectedDigest string,
) (k12.GradingFinalArtifact, error) {
	finalArtifact, err := d.Records.GetGradingFinalArtifact(
		ctx, strings.TrimSpace(agentName), strings.TrimSpace(finalArtifactID),
	)
	if err != nil {
		return k12.GradingFinalArtifact{}, err
	}
	expectedDigest = strings.TrimSpace(expectedDigest)
	if expectedDigest != "" && finalArtifact.ArtifactDigest != expectedDigest {
		return k12.GradingFinalArtifact{},
			fmt.Errorf("%w: final_artifact identity mismatch", ErrInvalidInput)
	}
	return finalArtifact, nil
}

// PrepareGradingFinalArtifactPDF reuses the canonical PDF renderer and reads
// only a frozen final_artifact identity.
func (d Deps) PrepareGradingFinalArtifactPDF(
	ctx context.Context,
	agentName, finalArtifactID, title string,
) (PrintableArtifactView, bool, error) {
	return d.PrepareGradingFinalArtifactPDFExact(
		ctx, agentName, finalArtifactID, "", title,
	)
}

// PrepareGradingFinalArtifactPDFExact rejects a stale or cross-artifact
// identity before deriving the shared immutable PDF.
func (d Deps) PrepareGradingFinalArtifactPDFExact(
	ctx context.Context,
	agentName, finalArtifactID, expectedDigest, title string,
) (PrintableArtifactView, bool, error) {
	finalArtifact, err := d.getExactGradingFinalArtifact(
		ctx, agentName, finalArtifactID, expectedDigest,
	)
	if err != nil {
		return PrintableArtifactView{}, false, err
	}
	req, err := d.gradingFinalArtifactPrintRequest(ctx, finalArtifact, title)
	if err != nil {
		return PrintableArtifactView{}, false, err
	}
	return d.PreparePrintableArtifact(
		ctx, req,
	)
}

// PrepareGradingFinalArtifactPrint reuses the shared frozen PDF and
// GenericPrintJob lifecycle.
func (d Deps) PrepareGradingFinalArtifactPrint(
	ctx context.Context,
	agentName, finalArtifactID, title, idempotencyKey string,
) (GenericPrintView, bool, error) {
	finalArtifact, err := d.getExactGradingFinalArtifact(
		ctx, agentName, finalArtifactID, "",
	)
	if err != nil {
		return GenericPrintView{}, false, err
	}
	req, err := d.gradingFinalArtifactPrintRequest(ctx, finalArtifact, title)
	if err != nil {
		return GenericPrintView{}, false, err
	}
	printable, _, err := d.PreparePrintableArtifact(ctx, req)
	if err != nil {
		return GenericPrintView{}, false, err
	}
	return d.PrepareGenericPrintFromArtifact(
		ctx, finalArtifact.AgentName, idempotencyKey, printable.Artifact.ArtifactID,
	)
}
