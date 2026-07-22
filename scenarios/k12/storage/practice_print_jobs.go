package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const practicePrintJobColumns = `print_job_id, agent_name, idempotency_key, request_digest,
    practice_set_id, base_set_version, artifact_kind, artifact_id, question_artifact_id,
    answer_artifact_id, paper_no, source_digest, prepared_fields_json, status, attempt_count,
    native_job_id, native_receipt_id, printer_snapshot_json, failure_kind, failure_detail,
    prepared_at, printed_at, created_at, updated_at, version`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPracticePrintJob(row rowScanner) (k12.PracticePrintJob, error) {
	var job k12.PracticePrintJob
	err := row.Scan(&job.PrintJobID, &job.AgentName, &job.IdempotencyKey, &job.RequestDigest,
		&job.PracticeSetID, &job.BaseSetVersion, &job.ArtifactKind, &job.ArtifactID,
		&job.QuestionArtifactID, &job.AnswerArtifactID, &job.PaperNo, &job.SourceDigest,
		&job.PreparedFieldsJSON, &job.Status, &job.AttemptCount, &job.NativeJobID,
		&job.NativeReceiptID, &job.PrinterSnapshot, &job.FailureKind, &job.FailureDetail,
		&job.PreparedAt, &job.PrintedAt, &job.CreatedAt, &job.UpdatedAt, &job.Version)
	return job, err
}

func getPracticePrintJobVia(ctx context.Context, q dbQueryer, agentName, jobID string) (k12.PracticePrintJob, error) {
	job, err := scanPracticePrintJob(q.QueryRowContext(ctx, `SELECT `+practicePrintJobColumns+`
        FROM k12_print_jobs WHERE print_job_id=? AND agent_name=?`, jobID, agentName))
	if err == sql.ErrNoRows {
		return k12.PracticePrintJob{}, records.ErrNotFound
	}
	if err != nil {
		return k12.PracticePrintJob{}, fmt.Errorf("k12storage: 读 PrintJob: %w", err)
	}
	return job, nil
}

// GetPracticePrintJob returns a PrintJob only inside its immutable Tutor scope.
func (s *Store) GetPracticePrintJob(ctx context.Context, agentName, jobID string) (k12.PracticePrintJob, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(jobID) == "" {
		return k12.PracticePrintJob{}, records.ErrNotFound
	}
	return getPracticePrintJobVia(ctx, s.db, agentName, jobID)
}

// PreparePracticePrintJob atomically creates the durable job, validates the exact
// draft version, reserves a learner-local paper number, and freezes the source
// fields used by both question and answer artifacts. It does not mutate PracticeSet.
func (s *Store) PreparePracticePrintJob(ctx context.Context, job k12.PracticePrintJob,
	prepared k12.PracticeSetFields) (stored k12.PracticePrintJob, replay bool, err error) {
	if strings.TrimSpace(job.PrintJobID) == "" || strings.TrimSpace(job.AgentName) == "" ||
		strings.TrimSpace(job.IdempotencyKey) == "" || strings.TrimSpace(job.RequestDigest) == "" ||
		strings.TrimSpace(job.PracticeSetID) == "" || !k12.PracticePrintArtifactKindAllowed(job.ArtifactKind) ||
		job.PreparedAt <= 0 {
		return k12.PracticePrintJob{}, false, fmt.Errorf("k12storage: PrintJob prepare 缺少 job/owner/key/digest/set/kind/time")
	}
	if err := ensureAgentRegistered(ctx, s.db, job.AgentName); err != nil {
		return k12.PracticePrintJob{}, false, err
	}
	job.Status = k12.PrintJobPreparing
	job.AttemptCount = 1
	job.CreatedAt, job.UpdatedAt, job.Version = job.PreparedAt, job.PreparedAt, 0
	job.PrinterSnapshot = "{}"

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.PracticePrintJob{}, false, fmt.Errorf("k12storage: 开启 PrintJob prepare 事务: %w", err)
	}
	defer tx.Rollback()

	// First transactional statement is a write: it serializes SQLite writers and
	// simultaneously establishes both idempotency and one-frozen-source boundaries.
	res, err := tx.ExecContext(ctx, `INSERT INTO k12_print_jobs (`+practicePrintJobColumns+`)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '{}', ?, ?, '', '', '{}', '', '', ?, 0, ?, ?, 0)
        ON CONFLICT DO NOTHING`,
		job.PrintJobID, job.AgentName, job.IdempotencyKey, job.RequestDigest,
		job.PracticeSetID, job.BaseSetVersion, job.ArtifactKind, job.ArtifactID,
		job.QuestionArtifactID, job.AnswerArtifactID, job.Status, job.AttemptCount,
		job.PreparedAt, job.CreatedAt, job.UpdatedAt)
	if err != nil {
		return k12.PracticePrintJob{}, false, fmt.Errorf("k12storage: 建立 PrintJob 幂等点: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, qErr := scanPracticePrintJob(tx.QueryRowContext(ctx, `SELECT `+practicePrintJobColumns+`
            FROM k12_print_jobs WHERE agent_name=? AND idempotency_key=?`, job.AgentName, job.IdempotencyKey))
		if qErr == sql.ErrNoRows {
			existing, qErr = scanPracticePrintJob(tx.QueryRowContext(ctx, `SELECT `+practicePrintJobColumns+`
                FROM k12_print_jobs WHERE agent_name=? AND practice_set_id=? AND base_set_version=?`,
				job.AgentName, job.PracticeSetID, job.BaseSetVersion))
		}
		if qErr != nil {
			return k12.PracticePrintJob{}, false, fmt.Errorf("k12storage: 回查 PrintJob 幂等点: %w", qErr)
		}
		if existing.RequestDigest != job.RequestDigest {
			return k12.PracticePrintJob{}, false, fmt.Errorf("k12storage: PrintJob 幂等键或卷源已绑定其他请求")
		}
		return existing, true, nil
	}

	var status string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT status, version FROM k12_practice_sets
        WHERE record_id=? AND agent_name=?`, job.PracticeSetID, job.AgentName).Scan(&status, &version); err != nil {
		if err == sql.ErrNoRows {
			return k12.PracticePrintJob{}, false, records.ErrNotFound
		}
		return k12.PracticePrintJob{}, false, fmt.Errorf("k12storage: 校验 PrintJob 源卷: %w", err)
	}
	if status != k12.PracticeStatusDraft || version != job.BaseSetVersion {
		return k12.PracticePrintJob{}, false, records.ErrVersionConflict
	}

	paperNo, err := reservePracticePaperNoTx(ctx, tx, job.AgentName, job.PreparedAt)
	if err != nil {
		return k12.PracticePrintJob{}, false, err
	}
	prepared.PaperNo = paperNo
	preparedJSON, err := json.Marshal(prepared)
	if err != nil {
		return k12.PracticePrintJob{}, false, fmt.Errorf("k12storage: freeze PrintJob source: %w", err)
	}
	sum := sha256.Sum256(preparedJSON)
	job.PaperNo = paperNo
	job.SourceDigest = hex.EncodeToString(sum[:])
	job.PreparedFieldsJSON = string(preparedJSON)
	if _, err := tx.ExecContext(ctx, `UPDATE k12_print_jobs SET paper_no=?, source_digest=?,
        prepared_fields_json=? WHERE print_job_id=? AND agent_name=?`, job.PaperNo,
		job.SourceDigest, job.PreparedFieldsJSON, job.PrintJobID, job.AgentName); err != nil {
		return k12.PracticePrintJob{}, false, fmt.Errorf("k12storage: 完成 PrintJob prepare: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return k12.PracticePrintJob{}, false, fmt.Errorf("k12storage: 提交 PrintJob prepare: %w", err)
	}
	return job, false, nil
}

// ReservePracticePaperNo is the compatibility allocator for the old finalize
// endpoint. New Desktop callers reserve through PreparePracticePrintJob.
func (s *Store) ReservePracticePaperNo(ctx context.Context, agentName string, at int64) (string, error) {
	if err := ensureAgentRegistered(ctx, s.db, agentName); err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	no, err := reservePracticePaperNoTx(ctx, tx, agentName, at)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return no, nil
}

func reservePracticePaperNoTx(ctx context.Context, tx *sql.Tx, agentName string, at int64) (string, error) {
	inserted, err := tx.ExecContext(ctx, `INSERT INTO k12_paper_no_counters(agent_name,next_sequence,updated_at)
        VALUES(?,0,?) ON CONFLICT(agent_name) DO NOTHING`, agentName, at)
	if err != nil {
		return "", fmt.Errorf("k12storage: 初始化卷面号计数器: %w", err)
	}
	var sequence int
	if n, _ := inserted.RowsAffected(); n > 0 {
		// Upgrade-safe seed: retain the largest historical/reserved suffix rather
		// than COUNT, so gaps and sequences above 99 can never be reused.
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM (
            SELECT CAST(substr(paper_no,8) AS INTEGER) AS seq FROM k12_practice_sets
              WHERE agent_name=? AND paper_no!=''
            UNION ALL
            SELECT CAST(substr(paper_no,8) AS INTEGER) AS seq FROM k12_print_jobs
              WHERE agent_name=? AND paper_no!=''
        )`, agentName, agentName).Scan(&sequence); err != nil {
			return "", fmt.Errorf("k12storage: 初始化历史卷面序号: %w", err)
		}
	} else if err := tx.QueryRowContext(ctx, `SELECT next_sequence FROM k12_paper_no_counters
        WHERE agent_name=?`, agentName).Scan(&sequence); err != nil {
		return "", fmt.Errorf("k12storage: 读取卷面号计数器: %w", err)
	}
	if sequence < 1 {
		sequence = 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_paper_no_counters SET next_sequence=?, updated_at=?
        WHERE agent_name=?`, sequence+1, at, agentName); err != nil {
		return "", fmt.Errorf("k12storage: 推进卷面号计数器: %w", err)
	}
	return k12.FormatPaperNo(time.Unix(at, 0), sequence), nil
}

func canAdvancePracticePrintJob(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case k12.PrintJobPreparing:
		return to == k12.PrintJobDialogOpen || to == k12.PrintJobCancelled ||
			to == k12.PrintJobFailed || to == k12.PrintJobOutcomeUnknown
	case k12.PrintJobDialogOpen:
		return to == k12.PrintJobSubmitted || to == k12.PrintJobCancelled ||
			to == k12.PrintJobFailed || to == k12.PrintJobOutcomeUnknown
	case k12.PrintJobSubmitted:
		return to == k12.PrintJobFailed || to == k12.PrintJobOutcomeUnknown
	case k12.PrintJobOutcomeUnknown:
		return to == k12.PrintJobCancelled || to == k12.PrintJobFailed
	default:
		return false
	}
}

func validatePrintReceiptCommitBoundary(status, storedNativeJobID, nativeJobID string) error {
	switch status {
	case k12.PrintJobDialogOpen, k12.PrintJobSubmitted:
		if storedNativeJobID != "" && storedNativeJobID != nativeJobID {
			return fmt.Errorf("k12storage: 原生 receipt 与 PrintJob 的 native_job_id 冲突")
		}
		return nil
	case k12.PrintJobOutcomeUnknown:
		if storedNativeJobID == "" || storedNativeJobID != nativeJobID {
			return fmt.Errorf("k12storage: outcome_unknown 仅允许同 native_job_id 的明确 reconciliation receipt")
		}
		return nil
	default:
		return fmt.Errorf("k12storage: %s PrintJob 未证明 dialog_open，不能提交 printed receipt", status)
	}
}

// AdvancePracticePrintJob persists non-success native events. Printed is handled
// by CommitPracticePrintJob so it cannot diverge from PracticeSet finalization.
func (s *Store) AdvancePracticePrintJob(ctx context.Context, agentName, jobID, status,
	nativeJobID, failureKind, failureDetail string, at int64) (k12.PracticePrintJob, error) {
	if status == k12.PrintJobPrinted {
		return k12.PracticePrintJob{}, fmt.Errorf("k12storage: printed 必须走原生 receipt 原子提交")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE k12_print_jobs SET version=version
        WHERE print_job_id=? AND agent_name=?`, jobID, agentName)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return k12.PracticePrintJob{}, records.ErrNotFound
	}
	job, err := getPracticePrintJobVia(ctx, tx, agentName, jobID)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	if job.Status == status {
		if nativeJobID != "" && job.NativeJobID != "" && nativeJobID != job.NativeJobID {
			return k12.PracticePrintJob{}, fmt.Errorf("k12storage: 同一 PrintJob 收到冲突 native_job_id")
		}
		return job, nil
	}
	if !canAdvancePracticePrintJob(job.Status, status) {
		return k12.PracticePrintJob{}, fmt.Errorf("k12storage: PrintJob 不允许 %s→%s", job.Status, status)
	}
	if status == k12.PrintJobSubmitted && strings.TrimSpace(nativeJobID) == "" {
		return k12.PracticePrintJob{}, fmt.Errorf("k12storage: submitted 必须携带 native_job_id")
	}
	if nativeJobID == "" {
		nativeJobID = job.NativeJobID
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_print_jobs SET status=?, native_job_id=?,
        failure_kind=?, failure_detail=?, updated_at=?, version=version+1
        WHERE print_job_id=? AND agent_name=? AND version=?`, status, nativeJobID,
		failureKind, failureDetail, at, jobID, agentName, job.Version); err != nil {
		return k12.PracticePrintJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return k12.PracticePrintJob{}, err
	}
	return s.GetPracticePrintJob(ctx, agentName, jobID)
}

// RetryPracticePrintJob retries only a definitive cancellation/failure, retaining
// the same immutable job, paper number and source. outcome_unknown is reconciled,
// never blindly retried.
func (s *Store) RetryPracticePrintJob(ctx context.Context, agentName, jobID string, at int64) (k12.PracticePrintJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE k12_print_jobs SET version=version
        WHERE print_job_id=? AND agent_name=?`, jobID, agentName)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return k12.PracticePrintJob{}, records.ErrNotFound
	}
	job, err := getPracticePrintJobVia(ctx, tx, agentName, jobID)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	if job.Status != k12.PrintJobCancelled && job.Status != k12.PrintJobFailed {
		return k12.PracticePrintJob{}, fmt.Errorf("k12storage: 只有 cancelled/failed PrintJob 可普通重试，当前 %s", job.Status)
	}
	var setStatus string
	var setVersion int
	if err := tx.QueryRowContext(ctx, `SELECT status,version FROM k12_practice_sets
        WHERE record_id=? AND agent_name=?`, job.PracticeSetID, agentName).Scan(&setStatus, &setVersion); err != nil {
		return k12.PracticePrintJob{}, err
	}
	if setStatus != k12.PracticeStatusDraft || setVersion != job.BaseSetVersion {
		return k12.PracticePrintJob{}, fmt.Errorf("k12storage: 待打印内容已变化，不能用旧 PrintJob 重试")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_print_jobs SET status=?, attempt_count=attempt_count+1,
        native_job_id='', native_receipt_id='', printer_snapshot_json='{}', failure_kind='',
        failure_detail='', updated_at=?, version=version+1 WHERE print_job_id=? AND agent_name=? AND version=?`,
		k12.PrintJobPreparing, at, jobID, agentName, job.Version); err != nil {
		return k12.PracticePrintJob{}, err
	}
	if err := tx.Commit(); err != nil {
		return k12.PracticePrintJob{}, err
	}
	return s.GetPracticePrintJob(ctx, agentName, jobID)
}

// CommitPracticePrintJob is the only printed transition. PracticeSet fields/status
// and the native receipt are updated in one SQLite transaction; any injected error
// after the set update rolls the entire transaction back.
func (s *Store) CommitPracticePrintJob(ctx context.Context, agentName, jobID, nativeJobID,
	nativeReceiptID, printerSnapshot string, at int64) (k12.PracticePrintJob, error) {
	if strings.TrimSpace(nativeJobID) == "" || strings.TrimSpace(nativeReceiptID) == "" ||
		!validPrinterSnapshotJSON(printerSnapshot) {
		return k12.PracticePrintJob{}, fmt.Errorf("k12storage: printed 必须携带 native_job_id、native_receipt_id 与 printer snapshot")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE k12_print_jobs SET version=version
        WHERE print_job_id=? AND agent_name=?`, jobID, agentName)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return k12.PracticePrintJob{}, records.ErrNotFound
	}
	job, err := getPracticePrintJobVia(ctx, tx, agentName, jobID)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	if job.Status == k12.PrintJobPrinted {
		if job.NativeJobID != nativeJobID || job.NativeReceiptID != nativeReceiptID ||
			!printerSnapshotsEqual(job.PrinterSnapshot, printerSnapshot) {
			return k12.PracticePrintJob{}, fmt.Errorf("k12storage: PrintJob 已由其他原生 receipt 提交")
		}
		return job, nil
	}
	if err := validatePrintReceiptCommitBoundary(job.Status, job.NativeJobID, nativeJobID); err != nil {
		return k12.PracticePrintJob{}, err
	}

	var prepared k12.PracticeSetFields
	if err := json.Unmarshal([]byte(job.PreparedFieldsJSON), &prepared); err != nil {
		return k12.PracticePrintJob{}, fmt.Errorf("k12storage: 解析冻结卷源: %w", err)
	}
	prepared.FinalizedAt = at
	prepared.FinalizedVia = "print"
	fieldsJSON, err := json.Marshal(prepared)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	schema, err := s.registry.Get(k12.CollectionPracticeSet)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	if schema.ValidateFields != nil {
		if err := schema.ValidateFields(string(fieldsJSON)); err != nil {
			return k12.PracticePrintJob{}, fmt.Errorf("%w: PrintJob 冻结卷源: %v", records.ErrInvalidFields, err)
		}
	}
	mp, err := s.mapperFor(k12.CollectionPracticeSet)
	if err != nil {
		return k12.PracticePrintJob{}, err
	}
	cur, err := s.getVia(ctx, tx, job.PracticeSetID)
	if err != nil || cur.AgentName != agentName || cur.Collection != k12.CollectionPracticeSet {
		if err == nil {
			err = records.ErrNotFound
		}
		return k12.PracticePrintJob{}, err
	}
	if cur.Status == k12.PracticeStatusAssigned {
		currentFields, _ := k12.ParsePracticeSetFields(cur.Fields)
		if currentFields.PaperNo != job.PaperNo || currentFields.QuestionArtifact != job.QuestionArtifactID ||
			currentFields.AnswerArtifact != job.AnswerArtifactID {
			return k12.PracticePrintJob{}, fmt.Errorf("k12storage: PracticeSet 已由其他卷源固化")
		}
	} else {
		if cur.Status != k12.PracticeStatusDraft || cur.Version != job.BaseSetVersion {
			return k12.PracticePrintJob{}, records.ErrVersionConflict
		}
		domainVals, err := mp.encode(string(fieldsJSON))
		if err != nil {
			return k12.PracticePrintJob{}, err
		}
		assigns := make([]string, 0, len(mp.domainCols()))
		for _, col := range mp.domainCols() {
			assigns = append(assigns, col+"=?")
		}
		dedupe := cur.DedupeKey
		if !strings.Contains(dedupe, dedupeTombstoneSep) {
			dedupe += dedupeTombstoneSep + cur.RecordID
		}
		q := fmt.Sprintf(`UPDATE %s SET status=?, dedupe_key=?, %s, version=version+1, updated_at=?
            WHERE record_id=? AND agent_name=? AND status=? AND version=?`, mp.table(), strings.Join(assigns, ", "))
		args := append([]any{k12.PracticeStatusAssigned, dedupe}, domainVals...)
		args = append(args, at, cur.RecordID, agentName, k12.PracticeStatusDraft, job.BaseSetVersion)
		updated, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return k12.PracticePrintJob{}, fmt.Errorf("k12storage: 原子固化 PracticeSet: %w", err)
		}
		if n, _ := updated.RowsAffected(); n == 0 {
			return k12.PracticePrintJob{}, records.ErrVersionConflict
		}
		if err := mp.syncChildren(ctx, tx, cur.RecordID, string(fieldsJSON)); err != nil {
			return k12.PracticePrintJob{}, err
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE k12_print_jobs SET status=?, native_job_id=?,
        native_receipt_id=?, printer_snapshot_json=?, failure_kind='', failure_detail='',
        printed_at=?, updated_at=?, version=version+1 WHERE print_job_id=? AND agent_name=? AND version=?`,
		k12.PrintJobPrinted, nativeJobID, nativeReceiptID, printerSnapshot, at, at,
		jobID, agentName, job.Version); err != nil {
		return k12.PracticePrintJob{}, fmt.Errorf("k12storage: 提交原生 PrintJob receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return k12.PracticePrintJob{}, err
	}
	return s.GetPracticePrintJob(ctx, agentName, jobID)
}
