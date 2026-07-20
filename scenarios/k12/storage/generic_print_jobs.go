package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const genericPrintJobColumns = `print_job_id, agent_name, idempotency_key, request_digest,
    artifact_id, status, attempt_count, native_job_id, native_receipt_id,
    printer_snapshot_json, failure_kind, failure_detail, prepared_at, printed_at,
    created_at, updated_at, version`

func scanGenericPrintJob(row rowScanner) (k12.GenericPrintJob, error) {
	var job k12.GenericPrintJob
	err := row.Scan(&job.PrintJobID, &job.AgentName, &job.IdempotencyKey, &job.RequestDigest,
		&job.ArtifactID, &job.Status, &job.AttemptCount, &job.NativeJobID, &job.NativeReceiptID,
		&job.PrinterSnapshot, &job.FailureKind, &job.FailureDetail, &job.PreparedAt, &job.PrintedAt,
		&job.CreatedAt, &job.UpdatedAt, &job.Version)
	return job, err
}

func scanPrintArtifact(row rowScanner) (k12.PrintArtifact, error) {
	var artifact k12.PrintArtifact
	err := row.Scan(&artifact.ArtifactID, &artifact.AgentName, &artifact.SourceKind,
		&artifact.SourceRef, &artifact.Title, &artifact.CanonicalMarkdown,
		&artifact.SourceDigest, &artifact.CreatedAt)
	return artifact, err
}

func validPrinterSnapshotJSON(raw string) bool {
	var snapshot map[string]any
	return json.Unmarshal([]byte(raw), &snapshot) == nil && len(snapshot) > 0
}

func getGenericPrintJobVia(ctx context.Context, q dbQueryer, agentName, jobID string) (k12.GenericPrintJob, error) {
	job, err := scanGenericPrintJob(q.QueryRowContext(ctx, `SELECT `+genericPrintJobColumns+`
        FROM k12_generic_print_jobs WHERE print_job_id=? AND agent_name=?`, jobID, agentName))
	if err == sql.ErrNoRows {
		return k12.GenericPrintJob{}, records.ErrNotFound
	}
	if err != nil {
		return k12.GenericPrintJob{}, fmt.Errorf("k12storage: 读通用 PrintJob: %w", err)
	}
	return job, nil
}

func getPrintArtifactVia(ctx context.Context, q dbQueryer, agentName, artifactID string) (k12.PrintArtifact, error) {
	artifact, err := scanPrintArtifact(q.QueryRowContext(ctx, `SELECT artifact_id, agent_name,
        source_kind, source_ref, title, canonical_markdown, source_digest, created_at
        FROM k12_print_artifacts WHERE artifact_id=? AND agent_name=?`, artifactID, agentName))
	if err == sql.ErrNoRows {
		return k12.PrintArtifact{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PrintArtifact{}, fmt.Errorf("k12storage: 读打印 Artifact: %w", err)
	}
	return artifact, nil
}

func (s *Store) GetGenericPrintJob(ctx context.Context, agentName, jobID string) (k12.GenericPrintJob, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(jobID) == "" {
		return k12.GenericPrintJob{}, records.ErrNotFound
	}
	return getGenericPrintJobVia(ctx, s.db, agentName, jobID)
}

func (s *Store) GetPrintArtifact(ctx context.Context, agentName, artifactID string) (k12.PrintArtifact, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(artifactID) == "" {
		return k12.PrintArtifact{}, records.ErrNotFound
	}
	return getPrintArtifactVia(ctx, s.db, agentName, artifactID)
}

// PrepareGenericPrintJob freezes the Artifact and establishes the idempotency
// point in one transaction. No source-domain table is read or mutated.
func (s *Store) PrepareGenericPrintJob(ctx context.Context, artifact k12.PrintArtifact,
	job k12.GenericPrintJob) (stored k12.GenericPrintJob, replay bool, err error) {
	if strings.TrimSpace(artifact.ArtifactID) == "" || strings.TrimSpace(artifact.AgentName) == "" ||
		!k12.GenericPrintSourceKindAllowed(artifact.SourceKind) || strings.TrimSpace(artifact.SourceRef) == "" ||
		strings.TrimSpace(artifact.Title) == "" || strings.TrimSpace(artifact.CanonicalMarkdown) == "" ||
		len(artifact.SourceDigest) != 64 || artifact.CreatedAt <= 0 ||
		strings.TrimSpace(job.PrintJobID) == "" || job.AgentName != artifact.AgentName ||
		strings.TrimSpace(job.IdempotencyKey) == "" || len(job.RequestDigest) != 64 ||
		job.ArtifactID != artifact.ArtifactID || job.PreparedAt <= 0 {
		return k12.GenericPrintJob{}, false, fmt.Errorf("k12storage: 通用 PrintJob prepare 字段不完整")
	}
	if err := ensureAgentRegistered(ctx, s.db, job.AgentName); err != nil {
		return k12.GenericPrintJob{}, false, err
	}
	job.Status, job.AttemptCount = k12.PrintJobPreparing, 1
	job.CreatedAt, job.UpdatedAt, job.Version = job.PreparedAt, job.PreparedAt, 0
	job.PrinterSnapshot = "{}"

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.GenericPrintJob{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_print_artifacts
        (artifact_id,agent_name,source_kind,source_ref,title,canonical_markdown,source_digest,created_at)
        VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, artifact.ArtifactID, artifact.AgentName,
		artifact.SourceKind, artifact.SourceRef, artifact.Title, artifact.CanonicalMarkdown,
		artifact.SourceDigest, artifact.CreatedAt); err != nil {
		return k12.GenericPrintJob{}, false, fmt.Errorf("k12storage: freeze 打印 Artifact: %w", err)
	}
	frozen, err := getPrintArtifactVia(ctx, tx, artifact.AgentName, artifact.ArtifactID)
	if err != nil {
		return k12.GenericPrintJob{}, false, err
	}
	if frozen.SourceKind != artifact.SourceKind || frozen.SourceRef != artifact.SourceRef ||
		frozen.Title != artifact.Title || frozen.CanonicalMarkdown != artifact.CanonicalMarkdown ||
		frozen.SourceDigest != artifact.SourceDigest {
		return k12.GenericPrintJob{}, false, fmt.Errorf("k12storage: 打印 Artifact ID 已绑定其他内容")
	}
	// A renderer reload may generate a fresh UI idempotency key. Recover the
	// unresolved job for the exact immutable Artifact instead of opening a
	// second native dialog. The partial UNIQUE index closes the concurrent race.
	unresolved, unresolvedErr := scanGenericPrintJob(tx.QueryRowContext(ctx, `SELECT `+genericPrintJobColumns+`
        FROM k12_generic_print_jobs WHERE agent_name=? AND artifact_id=?
        AND status IN ('preparing','dialog_open','submitted','outcome_unknown')
        ORDER BY created_at DESC LIMIT 1`, job.AgentName, job.ArtifactID))
	if unresolvedErr == nil {
		if unresolved.RequestDigest != job.RequestDigest {
			return k12.GenericPrintJob{}, false, fmt.Errorf("k12storage: 未决 PrintJob Artifact 请求摘要冲突")
		}
		return unresolved, true, nil
	}
	if unresolvedErr != sql.ErrNoRows {
		return k12.GenericPrintJob{}, false, fmt.Errorf("k12storage: 查找未决 PrintJob: %w", unresolvedErr)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO k12_generic_print_jobs (`+genericPrintJobColumns+`)
        VALUES(?,?,?,?,?,?,?,'','','{}','','',?,0,?,?,0) ON CONFLICT DO NOTHING`,
		job.PrintJobID, job.AgentName, job.IdempotencyKey, job.RequestDigest, job.ArtifactID,
		job.Status, job.AttemptCount, job.PreparedAt, job.CreatedAt, job.UpdatedAt)
	if err != nil {
		return k12.GenericPrintJob{}, false, fmt.Errorf("k12storage: 建立通用 PrintJob: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, qErr := scanGenericPrintJob(tx.QueryRowContext(ctx, `SELECT `+genericPrintJobColumns+`
            FROM k12_generic_print_jobs WHERE agent_name=? AND idempotency_key=?`,
			job.AgentName, job.IdempotencyKey))
		if qErr == sql.ErrNoRows {
			existing, qErr = scanGenericPrintJob(tx.QueryRowContext(ctx, `SELECT `+genericPrintJobColumns+`
                FROM k12_generic_print_jobs WHERE agent_name=? AND artifact_id=?
                AND status IN ('preparing','dialog_open','submitted','outcome_unknown')
                ORDER BY created_at DESC LIMIT 1`, job.AgentName, job.ArtifactID))
		}
		if qErr != nil {
			return k12.GenericPrintJob{}, false, fmt.Errorf("k12storage: 回查通用 PrintJob: %w", qErr)
		}
		if existing.RequestDigest != job.RequestDigest || existing.ArtifactID != job.ArtifactID {
			return k12.GenericPrintJob{}, false, fmt.Errorf("k12storage: PrintJob 幂等键已绑定其他 Artifact")
		}
		return existing, true, nil
	}
	if err := tx.Commit(); err != nil {
		return k12.GenericPrintJob{}, false, err
	}
	return job, false, nil
}

func (s *Store) AdvanceGenericPrintJob(ctx context.Context, agentName, jobID, status,
	nativeJobID, failureKind, failureDetail string, at int64) (k12.GenericPrintJob, error) {
	if status == k12.PrintJobPrinted {
		return k12.GenericPrintJob{}, fmt.Errorf("k12storage: printed 必须走 receipt commit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.GenericPrintJob{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE k12_generic_print_jobs SET version=version
        WHERE print_job_id=? AND agent_name=?`, jobID, agentName)
	if err != nil {
		return k12.GenericPrintJob{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return k12.GenericPrintJob{}, records.ErrNotFound
	}
	job, err := getGenericPrintJobVia(ctx, tx, agentName, jobID)
	if err != nil {
		return k12.GenericPrintJob{}, err
	}
	if job.Status == status {
		if nativeJobID != "" && job.NativeJobID != "" && nativeJobID != job.NativeJobID {
			return k12.GenericPrintJob{}, fmt.Errorf("k12storage: 同一 PrintJob 收到冲突 native_job_id")
		}
		return job, nil
	}
	if !canAdvancePracticePrintJob(job.Status, status) {
		return k12.GenericPrintJob{}, fmt.Errorf("k12storage: PrintJob 不允许 %s→%s", job.Status, status)
	}
	if status == k12.PrintJobSubmitted && strings.TrimSpace(nativeJobID) == "" {
		return k12.GenericPrintJob{}, fmt.Errorf("k12storage: submitted 必须携带 native_job_id")
	}
	if nativeJobID == "" {
		nativeJobID = job.NativeJobID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_generic_print_jobs SET status=?,native_job_id=?,
        failure_kind=?,failure_detail=?,updated_at=?,version=version+1
        WHERE print_job_id=? AND agent_name=? AND version=?`, status, nativeJobID,
		failureKind, failureDetail, at, jobID, agentName, job.Version); err != nil {
		return k12.GenericPrintJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return k12.GenericPrintJob{}, err
	}
	return s.GetGenericPrintJob(ctx, agentName, jobID)
}

func (s *Store) RetryGenericPrintJob(ctx context.Context, agentName, jobID string, at int64) (k12.GenericPrintJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.GenericPrintJob{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE k12_generic_print_jobs SET version=version
        WHERE print_job_id=? AND agent_name=?`, jobID, agentName)
	if err != nil {
		return k12.GenericPrintJob{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return k12.GenericPrintJob{}, records.ErrNotFound
	}
	job, err := getGenericPrintJobVia(ctx, tx, agentName, jobID)
	if err != nil {
		return k12.GenericPrintJob{}, err
	}
	if job.Status != k12.PrintJobCancelled && job.Status != k12.PrintJobFailed {
		return k12.GenericPrintJob{}, fmt.Errorf("k12storage: 只有 cancelled/failed PrintJob 可普通重试，当前 %s", job.Status)
	}
	if job.AttemptCount >= k12.MaxPrintAttempts {
		return k12.GenericPrintJob{}, fmt.Errorf("k12storage: PrintJob 已达到最大尝试次数 %d", k12.MaxPrintAttempts)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_generic_print_jobs SET status=?,attempt_count=attempt_count+1,
        native_job_id='',native_receipt_id='',printer_snapshot_json='{}',failure_kind='',failure_detail='',
        updated_at=?,version=version+1 WHERE print_job_id=? AND agent_name=? AND version=?`,
		k12.PrintJobPreparing, at, jobID, agentName, job.Version); err != nil {
		return k12.GenericPrintJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return k12.GenericPrintJob{}, err
	}
	return s.GetGenericPrintJob(ctx, agentName, jobID)
}

func (s *Store) CommitGenericPrintJob(ctx context.Context, agentName, jobID, nativeJobID,
	nativeReceiptID, printerSnapshot string, at int64) (k12.GenericPrintJob, error) {
	if strings.TrimSpace(nativeJobID) == "" || strings.TrimSpace(nativeReceiptID) == "" ||
		!validPrinterSnapshotJSON(printerSnapshot) {
		return k12.GenericPrintJob{}, fmt.Errorf("k12storage: printed 必须携带 native job/receipt/printer snapshot")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.GenericPrintJob{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE k12_generic_print_jobs SET version=version
        WHERE print_job_id=? AND agent_name=?`, jobID, agentName)
	if err != nil {
		return k12.GenericPrintJob{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return k12.GenericPrintJob{}, records.ErrNotFound
	}
	job, err := getGenericPrintJobVia(ctx, tx, agentName, jobID)
	if err != nil {
		return k12.GenericPrintJob{}, err
	}
	if job.Status == k12.PrintJobPrinted {
		if job.NativeJobID != nativeJobID || job.NativeReceiptID != nativeReceiptID {
			return k12.GenericPrintJob{}, fmt.Errorf("k12storage: PrintJob 已由其他原生 receipt 提交")
		}
		return job, nil
	}
	if job.Status == k12.PrintJobCancelled || job.Status == k12.PrintJobFailed {
		return k12.GenericPrintJob{}, fmt.Errorf("k12storage: %s PrintJob 必须先 retry", job.Status)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_generic_print_jobs SET status=?,native_job_id=?,
        native_receipt_id=?,printer_snapshot_json=?,failure_kind='',failure_detail='',printed_at=?,
        updated_at=?,version=version+1 WHERE print_job_id=? AND agent_name=? AND version=?`,
		k12.PrintJobPrinted, nativeJobID, nativeReceiptID, printerSnapshot, at, at,
		jobID, agentName, job.Version); err != nil {
		return k12.GenericPrintJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return k12.GenericPrintJob{}, err
	}
	return s.GetGenericPrintJob(ctx, agentName, jobID)
}
