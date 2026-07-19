package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// CreateCreativeWorkOCRJob inserts the durable pending checkpoint or returns
// the request-id match. A reused request id may only name the same asset.
func (s *Store) CreateCreativeWorkOCRJob(
	ctx context.Context, agentName, requestID, assetID, sourceDigest string, now int64,
) (k12.CreativeWorkOCRJob, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.CreativeWorkOCRJob{}, false, err
	}
	defer tx.Rollback()
	if err := ensureAgentRegistered(ctx, tx, agentName); err != nil {
		return k12.CreativeWorkOCRJob{}, false, err
	}

	existing, err := getCreativeWorkOCRJobVia(ctx, tx, agentName, "", requestID)
	if err == nil {
		if existing.SourceAssetID != assetID || existing.SourceDigest != sourceDigest {
			return k12.CreativeWorkOCRJob{}, false, fmt.Errorf("k12storage: OCR request_id 已绑定另一张原稿")
		}
		return existing, false, nil
	}
	if !isNotFound(err) {
		return k12.CreativeWorkOCRJob{}, false, err
	}

	jobID := "cwocr-" + idgen.NanoID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_creative_work_ocr_jobs
		(job_id,agent_name,request_id,source_asset_id,source_digest,status,ocr_raw,error_message,
		 attempt_count,confirmed_version,confirmed_digest,created_at,updated_at)
		VALUES (?,?,?,?,?,'pending','','',0,0,'',?,?)`,
		jobID, agentName, requestID, assetID, sourceDigest, now, now); err != nil {
		return k12.CreativeWorkOCRJob{}, false, fmt.Errorf("k12storage: 创建作文 OCR Job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return k12.CreativeWorkOCRJob{}, false, err
	}
	job, err := s.GetCreativeWorkOCRJob(ctx, agentName, jobID)
	return job, true, err
}

func isNotFound(err error) bool { return errors.Is(err, records.ErrNotFound) }

// GetCreativeWorkOCRJob enforces owner isolation in the SQL predicate, so a
// caller cannot distinguish a foreign job from an absent one.
func (s *Store) GetCreativeWorkOCRJob(ctx context.Context, agentName, jobID string) (k12.CreativeWorkOCRJob, error) {
	return getCreativeWorkOCRJobVia(ctx, s.db, agentName, jobID, "")
}

// GetCreativeWorkOCRArchiveEvidence resolves one exact confirmed canonical
// version for .hexbak. It never exports pending/processing/failed jobs.
func (s *Store) GetCreativeWorkOCRArchiveEvidence(
	ctx context.Context, agentName, jobID string, version int,
) (k12.CreativeWorkOCRArchiveEvidence, error) {
	return getCreativeWorkOCRArchiveEvidenceVia(ctx, s.db, agentName, jobID, version)
}

// GetCreativeWorkOCRArchiveEvidenceTx resolves evidence from the caller's
// snapshot transaction (restore-as pre-state capture).
func (s *Store) GetCreativeWorkOCRArchiveEvidenceTx(
	ctx context.Context, tx *sql.Tx, agentName, jobID string, version int,
) (k12.CreativeWorkOCRArchiveEvidence, error) {
	if tx == nil {
		return k12.CreativeWorkOCRArchiveEvidence{}, fmt.Errorf("k12storage: nil OCR evidence transaction")
	}
	return getCreativeWorkOCRArchiveEvidenceVia(ctx, tx, agentName, jobID, version)
}

func getCreativeWorkOCRArchiveEvidenceVia(
	ctx context.Context, q dbQueryer, agentName, jobID string, version int,
) (k12.CreativeWorkOCRArchiveEvidence, error) {
	var item k12.CreativeWorkOCRArchiveEvidence
	err := q.QueryRowContext(ctx, `SELECT
		j.job_id,j.agent_name,j.request_id,j.source_asset_id,j.source_digest,j.ocr_raw,
		v.version,v.content_markdown,v.content_digest,v.confirmed_at,
		j.attempt_count,j.created_at,j.updated_at
		FROM k12_creative_work_ocr_jobs j
		JOIN k12_creative_work_ocr_versions v ON v.job_id=j.job_id
		WHERE j.agent_name=? AND j.job_id=? AND v.version=? AND j.status='confirmed'`,
		agentName, jobID, version).Scan(
		&item.JobID, &item.AgentName, &item.RequestID, &item.SourceAssetID, &item.SourceDigest,
		&item.OCRRaw, &item.Version, &item.ContentMarkdown, &item.ContentDigest, &item.ConfirmedAt,
		&item.AttemptCount, &item.JobCreatedAt, &item.JobLastUpdatedAt,
	)
	if err == sql.ErrNoRows {
		return k12.CreativeWorkOCRArchiveEvidence{}, fmt.Errorf("%w: confirmed OCR evidence", records.ErrNotFound)
	}
	if err != nil {
		return k12.CreativeWorkOCRArchiveEvidence{}, fmt.Errorf("k12storage: read confirmed OCR evidence: %w", err)
	}
	return item, nil
}

// ImportCreativeWorkOCREvidenceTx merges confirmed archive evidence without
// weakening V20 immutability. Existing versions must match byte-for-byte;
// missing versions append, and a newer local confirmed pointer is never rolled
// back by an older archive.
func (s *Store) ImportCreativeWorkOCREvidenceTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	items []k12.CreativeWorkOCRArchiveEvidence,
) error {
	if tx == nil {
		return fmt.Errorf("k12storage: nil OCR import transaction")
	}
	if len(items) == 0 {
		return nil
	}
	if err := ensureAgentRegistered(ctx, tx, agentName); err != nil {
		return err
	}
	groups := make(map[string][]k12.CreativeWorkOCRArchiveEvidence)
	for i, item := range items {
		if item.AgentName != agentName || strings.TrimSpace(item.JobID) == "" ||
			strings.TrimSpace(item.RequestID) == "" || item.Version <= 0 || item.ConfirmedAt <= 0 ||
			strings.TrimSpace(item.SourceAssetID) == "" || strings.TrimSpace(item.SourceDigest) == "" ||
			strings.TrimSpace(item.ContentMarkdown) == "" || item.AttemptCount < 0 {
			return fmt.Errorf("k12storage: OCR archive evidence #%d invalid", i)
		}
		sum := sha256.Sum256([]byte(item.ContentMarkdown))
		if hex.EncodeToString(sum[:]) != item.ContentDigest {
			return fmt.Errorf("k12storage: OCR archive evidence #%d digest mismatch", i)
		}
		groups[item.JobID] = append(groups[item.JobID], item)
	}
	jobIDs := make([]string, 0, len(groups))
	for jobID := range groups {
		jobIDs = append(jobIDs, jobID)
	}
	sort.Strings(jobIDs)
	for _, jobID := range jobIDs {
		versions := groups[jobID]
		sort.Slice(versions, func(i, j int) bool { return versions[i].Version < versions[j].Version })
		identity := versions[0]
		maxVersion := versions[len(versions)-1]
		for _, item := range versions[1:] {
			if item.AgentName != identity.AgentName || item.RequestID != identity.RequestID ||
				item.SourceAssetID != identity.SourceAssetID || item.SourceDigest != identity.SourceDigest ||
				item.OCRRaw != identity.OCRRaw {
				return fmt.Errorf("k12storage: OCR job %q identity/raw differs across versions", jobID)
			}
		}

		var existing struct {
			agent, requestID, assetID, sourceDigest, status, raw, confirmedDigest string
			confirmedVersion, attemptCount                                        int
			createdAt, updatedAt                                                  int64
		}
		err := tx.QueryRowContext(ctx, `SELECT agent_name,request_id,source_asset_id,source_digest,
			status,ocr_raw,confirmed_version,confirmed_digest,attempt_count,created_at,updated_at
			FROM k12_creative_work_ocr_jobs WHERE job_id=?`, jobID).Scan(
			&existing.agent, &existing.requestID, &existing.assetID, &existing.sourceDigest,
			&existing.status, &existing.raw, &existing.confirmedVersion, &existing.confirmedDigest,
			&existing.attemptCount, &existing.createdAt, &existing.updatedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			createdAt := identity.JobCreatedAt
			if createdAt <= 0 {
				createdAt = identity.ConfirmedAt
			}
			updatedAt := maxVersion.JobLastUpdatedAt
			if updatedAt < maxVersion.ConfirmedAt {
				updatedAt = maxVersion.ConfirmedAt
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO k12_creative_work_ocr_jobs
				(job_id,agent_name,request_id,source_asset_id,source_digest,status,ocr_raw,error_message,
				 attempt_count,confirmed_version,confirmed_digest,created_at,updated_at)
				VALUES (?,?,?,?,?,'confirmed',?,'',?,?,?,?,?)`,
				jobID, agentName, identity.RequestID, identity.SourceAssetID, identity.SourceDigest,
				identity.OCRRaw, identity.AttemptCount, maxVersion.Version, maxVersion.ContentDigest,
				createdAt, updatedAt); err != nil {
				return fmt.Errorf("k12storage: import OCR job %q: %w", jobID, err)
			}
			existing.confirmedVersion = maxVersion.Version
			existing.confirmedDigest = maxVersion.ContentDigest
		} else if err != nil {
			return fmt.Errorf("k12storage: read OCR import conflict %q: %w", jobID, err)
		} else {
			if existing.agent != agentName || existing.assetID != identity.SourceAssetID ||
				existing.sourceDigest != identity.SourceDigest || existing.raw != identity.OCRRaw ||
				existing.status != string(k12.CreativeWorkOCRConfirmed) {
				return fmt.Errorf("k12storage: OCR job %q conflicts with existing owner/identity/raw", jobID)
			}
		}

		for _, item := range versions {
			var content, digest string
			var confirmedAt int64
			err := tx.QueryRowContext(ctx, `SELECT content_markdown,content_digest,confirmed_at
				FROM k12_creative_work_ocr_versions WHERE job_id=? AND version=?`, jobID, item.Version).
				Scan(&content, &digest, &confirmedAt)
			if errors.Is(err, sql.ErrNoRows) {
				if _, err := tx.ExecContext(ctx, `INSERT INTO k12_creative_work_ocr_versions
					(job_id,version,content_markdown,content_digest,confirmed_at) VALUES (?,?,?,?,?)`,
					jobID, item.Version, item.ContentMarkdown, item.ContentDigest, item.ConfirmedAt); err != nil {
					return fmt.Errorf("k12storage: import OCR version %s/v%d: %w", jobID, item.Version, err)
				}
			} else if err != nil {
				return fmt.Errorf("k12storage: read OCR version %s/v%d: %w", jobID, item.Version, err)
			} else if content != item.ContentMarkdown || digest != item.ContentDigest || confirmedAt != item.ConfirmedAt {
				return fmt.Errorf("k12storage: OCR version %s/v%d conflicts with existing evidence", jobID, item.Version)
			}
		}
		if maxVersion.Version > existing.confirmedVersion {
			updatedAt := maxVersion.JobLastUpdatedAt
			if updatedAt < maxVersion.ConfirmedAt {
				updatedAt = maxVersion.ConfirmedAt
			}
			if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_work_ocr_jobs
				SET confirmed_version=?,confirmed_digest=?,updated_at=?
				WHERE job_id=? AND agent_name=? AND status='confirmed'`,
				maxVersion.Version, maxVersion.ContentDigest, updatedAt, jobID, agentName); err != nil {
				return fmt.Errorf("k12storage: advance imported OCR job %q: %w", jobID, err)
			}
		} else if maxVersion.Version == existing.confirmedVersion && maxVersion.ContentDigest != existing.confirmedDigest {
			return fmt.Errorf("k12storage: OCR job %q confirmed pointer conflicts", jobID)
		}
	}
	return nil
}

func (s *Store) CreativeWorkOCRJobExistsTx(ctx context.Context, tx *sql.Tx, jobID string) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("k12storage: nil OCR existence transaction")
	}
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM k12_creative_work_ocr_jobs WHERE job_id=?`, jobID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("k12storage: check OCR job existence: %w", err)
	}
	return true, nil
}

// DeleteCreativeWorkOCRJobsTx removes only jobs proven by the restore journal
// to have been created by that migration. Target operational/unreferenced jobs
// are never swept by owner.
func (s *Store) DeleteCreativeWorkOCRJobsTx(
	ctx context.Context, tx *sql.Tx, agentName string, jobIDs []string,
) error {
	if tx == nil {
		return fmt.Errorf("k12storage: nil OCR delete transaction")
	}
	for _, jobID := range jobIDs {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM k12_creative_work_ocr_jobs WHERE job_id=? AND agent_name=?`, jobID, agentName)
		if err != nil {
			return fmt.Errorf("k12storage: delete migrated OCR job %q: %w", jobID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n > 1 {
			return fmt.Errorf("k12storage: deleted multiple OCR jobs for id %q", jobID)
		}
	}
	return nil
}

func getCreativeWorkOCRJobVia(
	ctx context.Context, q dbQueryer, agentName, jobID, requestID string,
) (k12.CreativeWorkOCRJob, error) {
	where := "j.agent_name = ? AND j.job_id = ?"
	arg := jobID
	if requestID != "" {
		where = "j.agent_name = ? AND j.request_id = ?"
		arg = requestID
	}
	var job k12.CreativeWorkOCRJob
	err := q.QueryRowContext(ctx, `SELECT
		j.job_id,j.agent_name,j.request_id,j.source_asset_id,j.source_digest,j.status,
		j.ocr_raw,j.error_message,j.attempt_count,j.confirmed_version,j.confirmed_digest,
		COALESCE(v.content_markdown,''),COALESCE(v.confirmed_at,0),j.created_at,j.updated_at
		FROM k12_creative_work_ocr_jobs j
		LEFT JOIN k12_creative_work_ocr_versions v
		  ON v.job_id=j.job_id AND v.version=j.confirmed_version
		WHERE `+where, agentName, arg).Scan(
		&job.JobID, &job.AgentName, &job.RequestID, &job.SourceAssetID, &job.SourceDigest, &job.Status,
		&job.OCRRaw, &job.ErrorMessage, &job.AttemptCount, &job.ConfirmedVersion, &job.ConfirmedDigest,
		&job.ConfirmedContent, &job.ConfirmedAt, &job.CreatedAt, &job.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: 作文 OCR Job 不存在", records.ErrNotFound)
	}
	if err != nil {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("k12storage: 读取作文 OCR Job: %w", err)
	}
	return job, nil
}

func (s *Store) MarkCreativeWorkOCRProcessing(ctx context.Context, agentName, jobID string, now int64) (k12.CreativeWorkOCRJob, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_creative_work_ocr_jobs
		SET status='processing', error_message='', attempt_count=attempt_count+1, updated_at=?
		WHERE job_id=? AND agent_name=? AND status IN ('pending','failed')`, now, jobID, agentName)
	if err != nil {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("k12storage: 开始作文 OCR: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: 作文 OCR 当前状态不可开始", records.ErrIllegalTransition)
	}
	return s.GetCreativeWorkOCRJob(ctx, agentName, jobID)
}

func (s *Store) MarkCreativeWorkOCRFailed(ctx context.Context, agentName, jobID, message string, now int64) (k12.CreativeWorkOCRJob, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_creative_work_ocr_jobs
		SET status='failed', error_message=?, updated_at=?
		WHERE job_id=? AND agent_name=? AND status='processing'`, message, now, jobID, agentName)
	if err != nil {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("k12storage: 记录作文 OCR 失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: 作文 OCR 不在 processing", records.ErrIllegalTransition)
	}
	return s.GetCreativeWorkOCRJob(ctx, agentName, jobID)
}

func (s *Store) MarkCreativeWorkOCRAwaiting(
	ctx context.Context, agentName, jobID, raw string, now int64,
) (k12.CreativeWorkOCRJob, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE k12_creative_work_ocr_jobs
		SET status='awaiting_confirmation', ocr_raw=?, error_message='', updated_at=?
		WHERE job_id=? AND agent_name=? AND status='processing' AND (ocr_raw='' OR ocr_raw=?)`,
		raw, now, jobID, agentName, raw)
	if err != nil {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("k12storage: 固化作文 OCR 原文: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: OCR 原文冲突或状态不可确认", records.ErrIllegalTransition)
	}
	return s.GetCreativeWorkOCRJob(ctx, agentName, jobID)
}

// ConfirmCreativeWorkOCR appends a canonical version. Repeating the same
// content is idempotent; edited content receives a new version and digest.
func (s *Store) ConfirmCreativeWorkOCR(
	ctx context.Context, agentName, jobID, content, digest string, now int64,
) (k12.CreativeWorkOCRJob, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return k12.CreativeWorkOCRJob{}, err
	}
	defer tx.Rollback()
	job, err := getCreativeWorkOCRJobVia(ctx, tx, agentName, jobID, "")
	if err != nil {
		return k12.CreativeWorkOCRJob{}, err
	}
	if job.Status != k12.CreativeWorkOCRAwaitingConfirmation &&
		job.Status != k12.CreativeWorkOCRFailed && job.Status != k12.CreativeWorkOCRConfirmed {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("%w: 作文 OCR 当前状态不可确认", records.ErrIllegalTransition)
	}
	if job.ConfirmedVersion > 0 && job.ConfirmedDigest == digest && job.ConfirmedContent == content {
		return job, nil
	}
	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version),0)+1 FROM k12_creative_work_ocr_versions WHERE job_id=?`, jobID).Scan(&next); err != nil {
		return k12.CreativeWorkOCRJob{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO k12_creative_work_ocr_versions
		(job_id,version,content_markdown,content_digest,confirmed_at) VALUES (?,?,?,?,?)`,
		jobID, next, content, digest, now); err != nil {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("k12storage: 追加家长确认稿: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE k12_creative_work_ocr_jobs
		SET status='confirmed', confirmed_version=?, confirmed_digest=?, error_message='', updated_at=?
		WHERE job_id=? AND agent_name=?`, next, digest, now, jobID, agentName); err != nil {
		return k12.CreativeWorkOCRJob{}, fmt.Errorf("k12storage: 推进家长确认稿: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return k12.CreativeWorkOCRJob{}, err
	}
	return s.GetCreativeWorkOCRJob(ctx, agentName, jobID)
}
