package usecase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// PracticePrintView is the public use-case view of a durable native print attempt.
type PracticePrintView struct {
	Job k12.PracticePrintJob
}

// PracticePrintEvent is one fact reported by the Desktop native PrintAdapter.
// printed is accepted only with a definitive native job + receipt pair.
type PracticePrintEvent struct {
	Status          string
	NativeJobID     string
	NativeReceiptID string
	PrinterSnapshot string
	FailureKind     string
	FailureDetail   string
}

// PracticePrintPaperView renders the immutable snapshot held by PrintJob, not the
// still-mutable draft PracticeSet. Question and answer carry the same SourceDigest.
type PracticePrintPaperView struct {
	PrintJobID   string
	Kind         string
	Title        string
	PaperNo      string
	SourceDigest string
	ArtifactID   string
	Markdown     string
}

func validPrinterSnapshot(raw string) bool {
	var snapshot map[string]any
	return json.Unmarshal([]byte(raw), &snapshot) == nil && len(snapshot) > 0
}

// PreparePracticePrint performs phase one of DD-023. It freezes the verified paper
// source and reserves a formal paper_no, while the PracticeSet remains draft.
func (d Deps) PreparePracticePrint(ctx context.Context, agentName, recordID, idempotencyKey,
	artifactKind string) (PracticePrintView, bool, error) {
	agentName = strings.TrimSpace(agentName)
	recordID = strings.TrimSpace(recordID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if agentName == "" || recordID == "" || idempotencyKey == "" {
		return PracticePrintView{}, false, fmt.Errorf("%w: agent / practice_set_id / idempotency_key 必填", ErrInvalidInput)
	}
	if artifactKind == "" {
		artifactKind = k12.PaperKindQuestion
	}
	if !k12.PracticePrintArtifactKindAllowed(artifactKind) {
		return PracticePrintView{}, false, fmt.Errorf("%w: artifact_kind 仅支持 question/answer", ErrInvalidInput)
	}
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return PracticePrintView{}, false, err
	}
	if v.Record.Status != k12.PracticeStatusDraft {
		return PracticePrintView{}, false, fmt.Errorf("usecase: 只有待打印（draft）练习集可 prepare PrintJob，当前 %s", v.Record.Status)
	}
	publishable, skipped := k12.PublishableItems(v.Fields)
	if len(publishable) == 0 {
		return PracticePrintView{}, false, fmt.Errorf("%w: 还没有已验证的题，暂时出不了卷", ErrInvalidInput)
	}

	// The idempotency digest excludes wall-clock derived presentation fields. A
	// process crash/retry seconds later must still resolve to the existing job.
	digestInput, _ := json.Marshal(struct {
		Agent        string `json:"agent"`
		SetID        string `json:"set_id"`
		SetVersion   int    `json:"set_version"`
		ArtifactKind string `json:"artifact_kind"`
		Fields       string `json:"fields"`
	}{Agent: agentName, SetID: recordID, SetVersion: v.Record.Version,
		ArtifactKind: artifactKind, Fields: v.Record.Fields})
	requestSum := sha256.Sum256(digestInput)
	requestDigest := hex.EncodeToString(requestSum[:])

	prepared := v.Fields
	prepared.Items = append([]k12.PracticeItem(nil), v.Fields.Items...)
	prepared.ReturnAssets = append([]k12.PracticeReturnAsset(nil), v.Fields.ReturnAssets...)
	for i := range prepared.Items {
		if !k12.PracticeItemPublishable(prepared.Items[i]) && prepared.Items[i].BlockedReason == "" {
			prepared.Items[i].BlockedReason = blockedReasonFor(prepared.Items[i].VerificationStatus)
		}
	}
	preparedAt := d.now()
	preparedTime := time.Unix(preparedAt, 0)
	if prepared.Title == basketTitle {
		prepared.Title = k12.GeneratePaperTitle(prepared, preparedTime)
	}
	prepared.SourceKind = k12.AggregateSourceKind(prepared, prepared.SourceKind)
	prepared.SkippedBlockedCount = skipped
	prepared.PaperNo = ""
	prepared.FinalizedAt = 0
	prepared.FinalizedVia = ""
	k12.AssignPaperSeqs(prepared.Items)
	for i := range prepared.Items {
		if k12.PracticeItemPublishable(prepared.Items[i]) && prepared.Items[i].PracticeProblemID == "" {
			prepared.Items[i].PracticeProblemID = "pprob-" + recordID[:min(8, len(recordID))] + "-" + prepared.Items[i].ItemID
		}
	}
	shortDigest := requestDigest[:12]
	prepared.QuestionArtifact = "qsheet-" + recordID + "-" + shortDigest
	prepared.AnswerArtifact = "asheet-" + recordID + "-" + shortDigest
	artifactID := prepared.QuestionArtifact
	if artifactKind == k12.PaperKindAnswer {
		artifactID = prepared.AnswerArtifact
	}
	jobIDSum := sha256.Sum256([]byte(agentName + "\x00" + idempotencyKey))
	job := k12.PracticePrintJob{
		PrintJobID:         "print-" + hex.EncodeToString(jobIDSum[:12]),
		AgentName:          agentName,
		IdempotencyKey:     idempotencyKey,
		RequestDigest:      requestDigest,
		PracticeSetID:      recordID,
		BaseSetVersion:     v.Record.Version,
		ArtifactKind:       artifactKind,
		ArtifactID:         artifactID,
		QuestionArtifactID: prepared.QuestionArtifact,
		AnswerArtifactID:   prepared.AnswerArtifact,
		PreparedAt:         preparedAt,
	}
	stored, replay, err := d.Records.PreparePracticePrintJob(ctx, job, prepared)
	if err != nil {
		return PracticePrintView{}, false, fmt.Errorf("usecase: prepare PrintJob: %w", err)
	}
	return PracticePrintView{Job: stored}, replay, nil
}

func (d Deps) GetPracticePrint(ctx context.Context, agentName, jobID string) (PracticePrintView, error) {
	job, err := d.Records.GetPracticePrintJob(ctx, strings.TrimSpace(agentName), strings.TrimSpace(jobID))
	if err != nil {
		return PracticePrintView{}, fmt.Errorf("usecase: 取 PrintJob: %w", err)
	}
	return PracticePrintView{Job: job}, nil
}

// freezePracticePrintArtifact 在 PrintJob 预占卷面号后冻结精确 Markdown 快照。
// 保留既有 qsheet/asheet 身份，使原生协调器继续复用现有 PDF 内容路由。
func (d Deps) freezePracticePrintArtifact(ctx context.Context, job k12.PracticePrintJob,
	fields k12.PracticeSetFields, kind string) (k12.PrintArtifact, k12.PrintArtifactRender, error) {
	artifactID := job.QuestionArtifactID
	sourceKind := k12.PrintSourcePracticeQuestion
	if kind == k12.PaperKindAnswer {
		artifactID = job.AnswerArtifactID
		sourceKind = k12.PrintSourcePracticeAnswer
	}
	markdown := k12.RenderPaperMarkdown(fields, kind, k12.PaperMeta{
		Term: fields.GradeTerm, Date: time.Unix(job.PreparedAt, 0), Preview: false,
	})
	artifact := buildPrintArtifact(PreparePrintableArtifactRequest{
		AgentName:         job.AgentName,
		SourceKind:        sourceKind,
		SourceRef:         "practice-print-job:" + job.PrintJobID + ":" + kind,
		Title:             fields.Title,
		CanonicalMarkdown: markdown,
	}, job.PreparedAt)
	artifact.ArtifactID = artifactID
	if strings.TrimSpace(artifact.ArtifactID) == "" {
		return k12.PrintArtifact{}, k12.PrintArtifactRender{}, fmt.Errorf("usecase: practice print artifact id is empty")
	}

	if frozen, getErr := d.Records.GetPrintArtifact(ctx, job.AgentName, artifact.ArtifactID); getErr == nil {
		if !samePrintArtifact(frozen, artifact) {
			return k12.PrintArtifact{}, k12.PrintArtifactRender{}, fmt.Errorf("usecase: practice print artifact id is bound to different content")
		}
		frozenRender, renderErr := d.Records.GetPrintArtifactRender(ctx, job.AgentName, artifact.ArtifactID)
		if renderErr == nil {
			return frozen, frozenRender, nil
		}
		if !errors.Is(renderErr, records.ErrNotFound) {
			return k12.PrintArtifact{}, k12.PrintArtifactRender{}, fmt.Errorf("usecase: read frozen practice print PDF: %w", renderErr)
		}
	} else if !errors.Is(getErr, records.ErrNotFound) {
		return k12.PrintArtifact{}, k12.PrintArtifactRender{}, fmt.Errorf("usecase: read practice print artifact: %w", getErr)
	}

	render, err := d.renderPrintableArtifact(ctx, artifact, "")
	if err != nil {
		return k12.PrintArtifact{}, k12.PrintArtifactRender{}, err
	}
	frozen, frozenRender, _, err := d.Records.FreezePrintArtifact(ctx, artifact, render)
	if err != nil {
		return k12.PrintArtifact{}, k12.PrintArtifactRender{}, fmt.Errorf("usecase: freeze practice print artifact/PDF: %w", err)
	}
	return frozen, frozenRender, nil
}

func (d Deps) RenderPracticePrintJobPaper(ctx context.Context, agentName, jobID, kind string) (PracticePrintPaperView, error) {
	if kind == "" {
		kind = k12.PaperKindQuestion
	}
	if !k12.PracticePrintArtifactKindAllowed(kind) {
		return PracticePrintPaperView{}, fmt.Errorf("%w: 卷面种类非法 %q（question/answer）", ErrInvalidInput, kind)
	}
	job, err := d.Records.GetPracticePrintJob(ctx, agentName, jobID)
	if err != nil {
		return PracticePrintPaperView{}, fmt.Errorf("usecase: 取 PrintJob 卷源: %w", err)
	}
	var fields k12.PracticeSetFields
	if err := json.Unmarshal([]byte(job.PreparedFieldsJSON), &fields); err != nil {
		return PracticePrintPaperView{}, fmt.Errorf("usecase: 解析 PrintJob 卷源: %w", err)
	}
	artifact, _, err := d.freezePracticePrintArtifact(ctx, job, fields, kind)
	if err != nil {
		return PracticePrintPaperView{}, fmt.Errorf("usecase: freeze practice print paper: %w", err)
	}
	return PracticePrintPaperView{
		PrintJobID: job.PrintJobID, Kind: kind, Title: fields.Title, PaperNo: job.PaperNo,
		SourceDigest: job.SourceDigest, ArtifactID: artifact.ArtifactID,
		Markdown: artifact.CanonicalMarkdown,
	}, nil
}

func (d Deps) RecordPracticePrintEvent(ctx context.Context, agentName, jobID string,
	event PracticePrintEvent) (PracticePrintView, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(jobID) == "" {
		return PracticePrintView{}, fmt.Errorf("%w: agent / print_job_id 必填", ErrInvalidInput)
	}
	switch event.Status {
	case k12.PrintJobPrinted:
		if strings.TrimSpace(event.NativeJobID) == "" || strings.TrimSpace(event.NativeReceiptID) == "" ||
			!validPrinterSnapshot(event.PrinterSnapshot) {
			return PracticePrintView{}, fmt.Errorf("%w: printed 必须携带有效 native_job_id / native_receipt_id / printer_snapshot", ErrInvalidInput)
		}
		job, err := d.Records.CommitPracticePrintJob(ctx, agentName, jobID, event.NativeJobID,
			event.NativeReceiptID, event.PrinterSnapshot, d.now())
		if err != nil {
			return PracticePrintView{}, fmt.Errorf("usecase: 提交原生打印 receipt: %w", err)
		}
		return PracticePrintView{Job: job}, nil
	case k12.PrintJobDialogOpen, k12.PrintJobSubmitted, k12.PrintJobCancelled,
		k12.PrintJobFailed, k12.PrintJobOutcomeUnknown:
	default:
		return PracticePrintView{}, fmt.Errorf("%w: PrintJob 事件状态非法 %q", ErrInvalidInput, event.Status)
	}
	if (event.Status == k12.PrintJobFailed || event.Status == k12.PrintJobOutcomeUnknown) &&
		strings.TrimSpace(event.FailureKind) == "" {
		return PracticePrintView{}, fmt.Errorf("%w: failed/outcome_unknown 必须携带 failure_kind", ErrInvalidInput)
	}
	job, err := d.Records.AdvancePracticePrintJob(ctx, agentName, jobID, event.Status,
		event.NativeJobID, event.FailureKind, event.FailureDetail, d.now())
	if err != nil {
		return PracticePrintView{}, fmt.Errorf("usecase: 记录 PrintJob 事件: %w", err)
	}
	return PracticePrintView{Job: job}, nil
}

func (d Deps) RetryPracticePrint(ctx context.Context, agentName, jobID string) (PracticePrintView, error) {
	job, err := d.Records.RetryPracticePrintJob(ctx, strings.TrimSpace(agentName), strings.TrimSpace(jobID), d.now())
	if err != nil {
		return PracticePrintView{}, fmt.Errorf("usecase: 重试 PrintJob: %w", err)
	}
	return PracticePrintView{Job: job}, nil
}
