package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func validateOCRPageInvocationClaim(claim OCRPageInvocationClaim) error {
	if claim.PageNumber <= 0 || claim.PagesTotal <= 0 || int64(claim.PageNumber) > claim.PagesTotal ||
		!isRawSHA256(claim.SourceDigest) || !isRawSHA256(claim.RequestDigest) ||
		strings.TrimSpace(claim.Provider) == "" || strings.TrimSpace(claim.Model) == "" {
		return fmt.Errorf("%w: invalid OCR page invocation claim", ErrInvalidDocumentUpload)
	}
	return nil
}

func isRawSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func scanOCRPageInvocation(row interface{ Scan(...any) error }) (OCRPageInvocation, error) {
	var invocation OCRPageInvocation
	var status string
	var createdAt, updatedAt int64
	var routeJSON string
	if err := row.Scan(
		&invocation.InvocationID, &invocation.JobID, &invocation.PageNumber,
		&invocation.PagesTotal, &invocation.SourceDigest, &invocation.RequestDigest,
		&invocation.Provider, &invocation.Model, &invocation.Operation, &status,
		&invocation.Content, &invocation.ContentDigest, &routeJSON, &invocation.LeaseEpoch,
		&createdAt, &updatedAt,
	); err != nil {
		return OCRPageInvocation{}, err
	}
	invocation.Status = OCRPageInvocationStatus(status)
	invocation.CreatedAt = time.UnixMilli(createdAt).UTC()
	invocation.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	if strings.TrimSpace(routeJSON) != "" && routeJSON != "{}" {
		if err := json.Unmarshal([]byte(routeJSON), &invocation.RouteReceipt); err != nil {
			return OCRPageInvocation{}, fmt.Errorf("%w: invalid OCR route receipt", ErrInvalidDocumentUpload)
		}
	}
	return invocation, nil
}

const ocrPageInvocationSelect = `SELECT invocation_id,job_id,page_number,pages_total,
source_digest,request_digest,provider,model,operation,status,content,content_digest,
route_receipt_json,lease_epoch,created_at,updated_at
FROM kb_ingest_page_invocations`

// ClaimOCRPageInvocation 在逐页 VLM 调用前声明不可变身份。首次声明返回 Fresh=true；
// 明确 failed 可在同一身份内重新声明；succeeded 只复用，未知调用只对账。
func (r *SQLiteSemanticIndexRepository) ClaimOCRPageInvocation(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	claim OCRPageInvocationClaim,
) (OCRPageInvocation, error) {
	if err := validateOCRPageInvocationClaim(claim); err != nil {
		return OCRPageInvocation{}, err
	}
	claim.Provider = strings.TrimSpace(claim.Provider)
	claim.Model = strings.TrimSpace(claim.Model)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return OCRPageInvocation{}, err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return OCRPageInvocation{}, err
	}
	if job.Kind != KnowledgeJobIngest || job.DocumentID == "" ||
		(job.PagesTotal != nil && *job.PagesTotal != claim.PagesTotal) {
		return OCRPageInvocation{}, ErrJobFenced
	}
	var invocation OCRPageInvocation
	invocation, err = scanOCRPageInvocation(tx.QueryRowContext(ctx, ocrPageInvocationSelect+`
	WHERE job_id=? AND page_number=? AND source_digest=? AND request_digest=?`,
		job.JobID, claim.PageNumber, claim.SourceDigest, claim.RequestDigest))
	if err == nil {
		if invocation.Provider != claim.Provider || invocation.Model != claim.Model ||
			invocation.Operation != OCRRouteOperationPDFPage {
			return OCRPageInvocation{}, fmt.Errorf("%w: OCR invocation route identity changed", ErrInvalidDocumentUpload)
		}
		if invocation.Status == OCRPageInvocationStatusFailed {
			res, err := tx.ExecContext(ctx, `UPDATE kb_ingest_page_invocations
				SET status='running',lease_epoch=?,updated_at=?
				WHERE invocation_id=? AND job_id=? AND status='failed'`,
				lease.Epoch, now.UTC().UnixMilli(), invocation.InvocationID, job.JobID)
			if err != nil {
				return OCRPageInvocation{}, err
			}
			if changed, _ := res.RowsAffected(); changed != 1 {
				return OCRPageInvocation{}, ErrJobFenced
			}
			invocation.Status, invocation.LeaseEpoch = OCRPageInvocationStatusRunning, lease.Epoch
			invocation.UpdatedAt, invocation.Fresh = now.UTC(), true
		}
		if err := tx.Commit(); err != nil {
			return OCRPageInvocation{}, err
		}
		return invocation, nil
	}
	if err != sql.ErrNoRows {
		return OCRPageInvocation{}, err
	}
	invocationID, err := semanticID("ocr")
	if err != nil {
		return OCRPageInvocation{}, err
	}
	nowMillis := now.UTC().UnixMilli()
	if _, err := tx.ExecContext(ctx, `INSERT INTO kb_ingest_page_invocations
		(invocation_id,job_id,page_number,pages_total,source_digest,request_digest,
		 provider,model,operation,status,lease_epoch,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,'running',?,?,?)`,
		invocationID, job.JobID, claim.PageNumber, claim.PagesTotal, claim.SourceDigest,
		claim.RequestDigest, claim.Provider, claim.Model, OCRRouteOperationPDFPage,
		lease.Epoch, nowMillis, nowMillis); err != nil {
		return OCRPageInvocation{}, fmt.Errorf("knowledge: claim OCR page invocation: %w", err)
	}
	invocation = OCRPageInvocation{
		InvocationID: invocationID, JobID: job.JobID, PageNumber: claim.PageNumber,
		PagesTotal: claim.PagesTotal, SourceDigest: claim.SourceDigest,
		RequestDigest: claim.RequestDigest, Provider: claim.Provider, Model: claim.Model,
		Operation: OCRRouteOperationPDFPage, Status: OCRPageInvocationStatusRunning,
		LeaseEpoch: lease.Epoch, CreatedAt: now.UTC(), UpdatedAt: now.UTC(), Fresh: true,
	}
	if err := tx.Commit(); err != nil {
		return OCRPageInvocation{}, err
	}
	return invocation, nil
}

// SaveOCRPageInvocation 将 VLM 成功结果持久化为内部恢复事实。该操作本身不发布页检查点；
// 调用方随后仍通过 CommitPage 写入既有公开 OCR receipt，重复保存同一结果是幂等的。
func (r *SQLiteSemanticIndexRepository) SaveOCRPageInvocation(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	invocation OCRPageInvocation,
	result OCRPageInvocationResult,
) error {
	if strings.TrimSpace(invocation.InvocationID) == "" || strings.TrimSpace(invocation.JobID) == "" ||
		invocation.PageNumber <= 0 || invocation.PagesTotal <= 0 || !isRawSHA256(invocation.SourceDigest) ||
		!isRawSHA256(invocation.RequestDigest) || strings.TrimSpace(result.Content) == "" {
		return fmt.Errorf("%w: invalid OCR page invocation result", ErrInvalidDocumentUpload)
	}
	receipt := canonicalOCRRouteReceipt(result.RouteReceipt)
	if err := validateOCRRouteReceipt(receipt); err != nil || receipt.Provider != strings.TrimSpace(invocation.Provider) ||
		receipt.Model != strings.TrimSpace(invocation.Model) {
		return fmt.Errorf("%w: OCR invocation route receipt mismatch", ErrInvalidDocumentUpload)
	}
	contentDigest := ingestPageContentDigest(result.Content)
	routeJSON, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if job.JobID != invocation.JobID {
		return ErrJobFenced
	}
	stored, err := scanOCRPageInvocation(tx.QueryRowContext(ctx, ocrPageInvocationSelect+`
	WHERE invocation_id=? AND job_id=?`, invocation.InvocationID, job.JobID))
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: OCR invocation is not claimed", ErrInvalidDocumentUpload)
		}
		return err
	}
	if stored.PageNumber != invocation.PageNumber || stored.PagesTotal != invocation.PagesTotal ||
		stored.SourceDigest != invocation.SourceDigest || stored.RequestDigest != invocation.RequestDigest ||
		stored.Provider != invocation.Provider || stored.Model != invocation.Model {
		return fmt.Errorf("%w: OCR invocation identity drifted", ErrInvalidDocumentUpload)
	}
	if stored.Status == OCRPageInvocationStatusSucceeded {
		if stored.Content != result.Content || stored.ContentDigest != contentDigest || stored.RouteReceipt != receipt {
			return fmt.Errorf("%w: conflicting OCR invocation result", ErrInvalidDocumentUpload)
		}
		return tx.Commit()
	}
	nowMillis := now.UTC().UnixMilli()
	res, err := tx.ExecContext(ctx, `UPDATE kb_ingest_page_invocations SET status='succeeded',
		content=?,content_digest=?,route_receipt_json=?,updated_at=?
		WHERE invocation_id=? AND job_id=? AND lease_epoch=? AND status IN ('prepared','running')`,
		result.Content, contentDigest, string(routeJSON), nowMillis,
		invocation.InvocationID, job.JobID, lease.Epoch)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return ErrJobFenced
	}
	return tx.Commit()
}

// MarkOCRPageInvocationOutcomeUnknown 停止无法确认结果的 Provider 调用；同一身份后续
// 只能由恢复/对账路径处理，不能借重试重新触发模型。
func (r *SQLiteSemanticIndexRepository) MarkOCRPageInvocationOutcomeUnknown(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	invocation OCRPageInvocation,
	lastError string,
) error {
	if strings.TrimSpace(invocation.InvocationID) == "" || strings.TrimSpace(invocation.JobID) == "" {
		return fmt.Errorf("%w: invalid OCR page invocation identity", ErrInvalidDocumentUpload)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if job.JobID != invocation.JobID {
		return ErrJobFenced
	}
	stored, err := scanOCRPageInvocation(tx.QueryRowContext(ctx, ocrPageInvocationSelect+
		` WHERE invocation_id=? AND job_id=?`, invocation.InvocationID, job.JobID))
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: OCR invocation is not claimed", ErrInvalidDocumentUpload)
		}
		return err
	}
	if stored.Status == OCRPageInvocationStatusSucceeded ||
		stored.Status == OCRPageInvocationStatusOutcomeUnknown {
		return tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE kb_ingest_page_invocations SET
		status='outcome_unknown',updated_at=?
		WHERE invocation_id=? AND job_id=? AND lease_epoch=? AND status IN ('prepared','running')`,
		now.UTC().UnixMilli(), invocation.InvocationID, job.JobID, lease.Epoch)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return ErrJobFenced
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET last_error=?,updated_at=?
		WHERE job_id=? AND lease_epoch=?`,
		fmt.Sprintf("%s: page %d invocation %s: %s", ErrOCRPageInvocationOutcomeUnknown,
			stored.PageNumber, stored.InvocationID, lastError), now.UTC().UnixMilli(), job.JobID, lease.Epoch); err != nil {
		return err
	}
	return tx.Commit()
}

// MarkOCRPageInvocationFailed 只接纳当前租约已确认的失败；不改变成功或未知结果。
func (r *SQLiteSemanticIndexRepository) MarkOCRPageInvocationFailed(
	ctx context.Context,
	lease JobLease,
	now time.Time,
	invocation OCRPageInvocation,
	lastError string,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := loadLiveJob(ctx, tx, lease, now)
	if err != nil {
		return err
	}
	if job.JobID != invocation.JobID || strings.TrimSpace(invocation.InvocationID) == "" {
		return ErrJobFenced
	}
	stored, err := scanOCRPageInvocation(tx.QueryRowContext(ctx, ocrPageInvocationSelect+
		` WHERE invocation_id=? AND job_id=?`, invocation.InvocationID, job.JobID))
	if err != nil {
		return err
	}
	if stored.Status == OCRPageInvocationStatusFailed {
		return tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE kb_ingest_page_invocations
		SET status='failed',updated_at=?
		WHERE invocation_id=? AND job_id=? AND lease_epoch=? AND status='running'`,
		now.UTC().UnixMilli(), stored.InvocationID, job.JobID, lease.Epoch)
	if err != nil {
		return err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return ErrJobFenced
	}
	if _, err := tx.ExecContext(ctx, `UPDATE kb_knowledge_jobs SET last_error=?,updated_at=?
		WHERE job_id=? AND lease_epoch=?`,
		fmt.Sprintf("knowledge: OCR page %d invocation %s failed: %s", stored.PageNumber,
			stored.InvocationID, lastError), now.UTC().UnixMilli(), job.JobID, lease.Epoch); err != nil {
		return err
	}
	return tx.Commit()
}
