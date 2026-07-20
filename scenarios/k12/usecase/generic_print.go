package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

type PrepareGenericPrintRequest struct {
	AgentName         string
	IdempotencyKey    string
	SourceKind        string
	SourceRef         string
	Title             string
	CanonicalMarkdown string
}

type GenericPrintView struct {
	Job      k12.GenericPrintJob
	Artifact k12.PrintArtifact
}

type GenericPrintArtifactView struct {
	PrintJobID   string
	ArtifactID   string
	SourceKind   string
	SourceRef    string
	Title        string
	SourceDigest string
	Markdown     string
}

// PrepareGenericPrint persists canonical printable bytes before any native
// dialog opens. The source reference is descriptive; this use case never writes
// the source domain object.
func (d Deps) PrepareGenericPrint(ctx context.Context, req PrepareGenericPrintRequest) (GenericPrintView, bool, error) {
	req.AgentName = strings.TrimSpace(req.AgentName)
	req.IdempotencyKey = strings.TrimSpace(req.IdempotencyKey)
	req.SourceKind = strings.TrimSpace(req.SourceKind)
	req.SourceRef = strings.TrimSpace(req.SourceRef)
	req.Title = strings.TrimSpace(req.Title)
	if req.AgentName == "" || req.IdempotencyKey == "" || req.SourceRef == "" || req.Title == "" ||
		strings.TrimSpace(req.CanonicalMarkdown) == "" {
		return GenericPrintView{}, false, fmt.Errorf("%w: agent/idempotency_key/source_ref/title/canonical_markdown 必填", ErrInvalidInput)
	}
	if !k12.GenericPrintSourceKindAllowed(req.SourceKind) {
		return GenericPrintView{}, false, fmt.Errorf("%w: source_kind 不支持 %q", ErrInvalidInput, req.SourceKind)
	}
	if len(req.IdempotencyKey) > 512 || len(req.SourceRef) > 512 || len(req.Title) > 256 || len(req.CanonicalMarkdown) > 4<<20 {
		return GenericPrintView{}, false, fmt.Errorf("%w: 打印 Artifact 字段超出限制", ErrInvalidInput)
	}
	artifactBytes, _ := json.Marshal(struct {
		SourceKind        string `json:"source_kind"`
		SourceRef         string `json:"source_ref"`
		Title             string `json:"title"`
		CanonicalMarkdown string `json:"canonical_markdown"`
	}{req.SourceKind, req.SourceRef, req.Title, req.CanonicalMarkdown})
	sourceSum := sha256.Sum256(artifactBytes)
	sourceDigest := hex.EncodeToString(sourceSum[:])
	artifactIDSum := sha256.Sum256([]byte(req.AgentName + "\x00" + sourceDigest))
	artifactID := "part-" + hex.EncodeToString(artifactIDSum[:12])
	requestSum := sha256.Sum256([]byte(req.AgentName + "\x00" + artifactID))
	jobIDSum := sha256.Sum256([]byte(req.AgentName + "\x00" + req.IdempotencyKey))
	at := d.now()
	artifact := k12.PrintArtifact{
		ArtifactID: artifactID, AgentName: req.AgentName, SourceKind: req.SourceKind,
		SourceRef: req.SourceRef, Title: req.Title, CanonicalMarkdown: req.CanonicalMarkdown,
		SourceDigest: sourceDigest, CreatedAt: at,
	}
	job := k12.GenericPrintJob{
		PrintJobID: "gprint-" + hex.EncodeToString(jobIDSum[:12]), AgentName: req.AgentName,
		IdempotencyKey: req.IdempotencyKey, RequestDigest: hex.EncodeToString(requestSum[:]),
		ArtifactID: artifactID, PreparedAt: at,
	}
	stored, replay, err := d.Records.PrepareGenericPrintJob(ctx, artifact, job)
	if err != nil {
		return GenericPrintView{}, false, fmt.Errorf("usecase: prepare 通用 PrintJob: %w", err)
	}
	frozen, err := d.Records.GetPrintArtifact(ctx, req.AgentName, stored.ArtifactID)
	if err != nil {
		return GenericPrintView{}, false, fmt.Errorf("usecase: 取冻结打印 Artifact: %w", err)
	}
	return GenericPrintView{Job: stored, Artifact: frozen}, replay, nil
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
	return GenericPrintView{Job: job, Artifact: artifact}, nil
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
		Markdown: v.Artifact.CanonicalMarkdown,
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
