package k12storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

const problemSkipReceiptColumns = `skip_receipt_id,agent_name,job_id,problem_id,
	structure_version,input_revision,result_digest,current_disposition,
	published_revision,superseded_at,created_at,updated_at`

func scanProblemSkipReceipt(row rowScanner) (k12.ProblemSkipReceipt, error) {
	var receipt k12.ProblemSkipReceipt
	err := row.Scan(
		&receipt.SkipReceiptID,
		&receipt.AgentName,
		&receipt.JobID,
		&receipt.ProblemID,
		&receipt.StructureVersion,
		&receipt.InputRevision,
		&receipt.ResultDigest,
		&receipt.CurrentDisposition,
		&receipt.PublishedRevision,
		&receipt.SupersededAt,
		&receipt.CreatedAt,
		&receipt.UpdatedAt,
	)
	return receipt, err
}

// GetCurrentProblemSkipRevision returns the immutable input revision selected
// by the current parent skip decision for one problem.
func (s *Store) GetCurrentProblemSkipRevision(
	ctx context.Context,
	agentName string,
	jobID string,
	problemID string,
) (int, error) {
	var revision int
	err := s.db.QueryRowContext(ctx, `
		SELECT input_revision
		FROM k12_problem_skip_receipts
		WHERE agent_name=? AND job_id=? AND problem_id=?
		  AND current_disposition='current'
		LIMIT 1`,
		agentName, jobID, problemID,
	).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, records.ErrNotFound
	}
	return revision, err
}

// ListCurrentProblemSkipReceipts returns the immutable parent skip decisions
// that currently participate in final grading coverage.
func (s *Store) ListCurrentProblemSkipReceipts(
	ctx context.Context,
	agentName string,
	jobID string,
) ([]k12.ProblemSkipReceipt, error) {
	agentName = strings.TrimSpace(agentName)
	jobID = strings.TrimSpace(jobID)
	if agentName == "" || jobID == "" {
		return []k12.ProblemSkipReceipt{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+problemSkipReceiptColumns+`
		FROM k12_problem_skip_receipts
		WHERE agent_name=? AND job_id=? AND current_disposition='current'
		ORDER BY problem_id,published_revision,skip_receipt_id`,
		agentName,
		jobID,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"k12storage: list current problem skip receipts: %w",
			err,
		)
	}
	defer rows.Close()
	receipts := make([]k12.ProblemSkipReceipt, 0)
	for rows.Next() {
		receipt, scanErr := scanProblemSkipReceipt(rows)
		if scanErr != nil {
			return nil, fmt.Errorf(
				"k12storage: scan current problem skip receipt: %w",
				scanErr,
			)
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"k12storage: iterate current problem skip receipts: %w",
			err,
		)
	}
	return receipts, nil
}
