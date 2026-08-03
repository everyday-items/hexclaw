package k12storage

import (
	"context"
	"fmt"
	"strings"
)

// HasPendingCurrentProblemSourceRecognition reports whether the Job's current
// immutable V72 input exact-set is still the provisional head created by a
// select_region/retake command and that command has no committed V73 result.
// Superseded heads cannot block a later revision, while correct_text/resume do
// not require an OCR result and are deliberately excluded.
func (s *Store) HasPendingCurrentProblemSourceRecognition(
	ctx context.Context,
	agentName string,
	jobID string,
) (bool, error) {
	agentName = strings.TrimSpace(agentName)
	jobID = strings.TrimSpace(jobID)
	if agentName == "" || jobID == "" {
		return false, fmt.Errorf("k12storage: pending source recognition scope is incomplete")
	}
	var pending int
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1
			FROM k12_problem_source_reprocess_jobs work
			JOIN k12_problem_source_action_receipts receipt
			  ON receipt.command_receipt_id=work.command_receipt_id
			 AND receipt.agent_name=work.agent_name
			 AND receipt.job_id=work.job_id
			 AND receipt.action=work.action
			 AND receipt.structure_version=work.structure_version
			 AND receipt.result_input_revision=work.input_revision
			JOIN k12_grading_jobs job
			  ON job.record_id=work.job_id
			 AND job.agent_name=work.agent_name
			JOIN k12_problem_input_revisions input
			  ON input.agent_name=work.agent_name
			 AND input.submission_id=job.submission_id
			 AND input.structure_version=work.structure_version
			 AND input.input_revision=work.input_revision
			 AND input.origin_command_receipt_id=work.command_receipt_id
			WHERE work.agent_name=?
			  AND work.job_id=?
			  AND work.action IN ('select_region','retake')
			  AND input.current_disposition='current'
			  AND input.origin_kind='command'
			  AND EXISTS (
				SELECT 1
				FROM json_each(work.affected_problem_ids_json) affected
				WHERE CAST(affected.value AS TEXT)=input.problem_id
			  )
			  AND NOT EXISTS (
				SELECT 1
				FROM k12_problem_source_recognition_results result
				WHERE result.work_id=work.work_id
				  AND result.command_receipt_id=work.command_receipt_id
				  AND result.agent_name=work.agent_name
				  AND result.job_id=work.job_id
			  )
			LIMIT 1
		)`,
		agentName,
		jobID,
	).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("k12storage: inspect pending current source recognition: %w", err)
	}
	return pending != 0, nil
}
