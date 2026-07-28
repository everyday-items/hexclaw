package k12storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

var (
	ErrProblemSourceActionNotFound          = errors.New("problem source action scope not found")
	ErrProblemSourceActionConflict          = errors.New("problem source action conflict")
	ErrGradingProgressiveProjectionConflict = errors.New(
		"grading progressive projection conflicts with final artifact",
	)
)

type ProblemSourceActionCommand struct {
	OwnerScope            string
	TrustedAgentName      string
	DispatchID            string
	ProblemID             string
	IdempotencyKey        string
	Action                string
	StructureVersion      int
	ExpectedInputRevision int
	Payload               json.RawMessage
}

type ProblemSourceActionResult struct {
	CommandReceiptID    string                           `json:"command_receipt_id"`
	InputRevision       int                              `json:"input_revision"`
	ProgressiveSnapshot ProblemSourceProgressiveSnapshot `json:"progressive_snapshot"`
}

type ProblemSourceProgressiveSnapshot struct {
	StructureVersion int                              `json:"structure_version"`
	SnapshotRevision int                              `json:"snapshot_revision"`
	ProblemProgress  []ProblemSourceProgress          `json:"problem_progress"`
	Coverage         ProblemSourceProgressiveCoverage `json:"coverage"`
}

type GradingProgressiveProjection struct {
	ProgressiveSnapshot ProblemSourceProgressiveSnapshot
	FinalArtifact       *k12.GradingFinalArtifact
}

type ProblemSourceProgress struct {
	ProblemID          string `json:"problem_id"`
	Status             string `json:"status"`
	InputRevision      int    `json:"input_revision"`
	PublishedRevision  int    `json:"published_revision"`
	CurrentDisposition string `json:"current_disposition"`
}

type ProblemSourceProgressiveCoverage struct {
	Total              int    `json:"total"`
	Published          int    `json:"published"`
	Skipped            int    `json:"skipped"`
	Awaiting           int    `json:"awaiting"`
	Failed             int    `json:"failed"`
	Status             string `json:"status"`
	ProjectionRevision int    `json:"projection_revision"`
}

type problemSourceActionScope struct {
	AgentName        string
	SubmissionID     string
	JobID            string
	AttemptRevision  int
	StructureVersion int
}

type problemSourceActionDigestInput struct {
	OwnerScope            string          `json:"owner_scope"`
	AgentName             string          `json:"agent_name"`
	DispatchID            string          `json:"dispatch_id"`
	ProblemID             string          `json:"problem_id"`
	Action                string          `json:"action"`
	StructureVersion      int             `json:"structure_version"`
	ExpectedInputRevision int             `json:"expected_input_revision"`
	Payload               json.RawMessage `json:"payload"`
}

type correctTextProblemSourceActionPayload struct {
	QuestionCanonicalMarkdown string `json:"question_canonical_markdown"`
	AnswerCanonicalMarkdown   string `json:"answer_canonical_markdown"`
}

type sourcePixelRegion struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type selectRegionProblemSourceActionPayload struct {
	PageAssetID string            `json:"page_asset_id"`
	Region      sourcePixelRegion `json:"region"`
}

type retakeProblemSourceActionPayload struct {
	PageAssetID string `json:"page_asset_id"`
}

func canonicalProblemSourceActionPayload(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func problemSourceActionDigest(
	command ProblemSourceActionCommand,
	agentName string,
) (string, error) {
	payload, err := canonicalProblemSourceActionPayload(command.Payload)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(problemSourceActionDigestInput{
		OwnerScope:            command.OwnerScope,
		AgentName:             agentName,
		DispatchID:            command.DispatchID,
		ProblemID:             command.ProblemID,
		Action:                command.Action,
		StructureVersion:      command.StructureVersion,
		ExpectedInputRevision: command.ExpectedInputRevision,
		Payload:               payload,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func validateProblemSourceActionCommand(command ProblemSourceActionCommand) error {
	if strings.TrimSpace(command.OwnerScope) == "" ||
		strings.TrimSpace(command.DispatchID) == "" ||
		strings.TrimSpace(command.ProblemID) == "" ||
		strings.TrimSpace(command.IdempotencyKey) == "" ||
		command.StructureVersion < 1 ||
		command.ExpectedInputRevision < 1 {
		return fmt.Errorf("%w: incomplete command identity", ErrProblemSourceActionConflict)
	}
	switch command.Action {
	case "correct_text", "select_region", "retake", "skip", "resume":
		return nil
	default:
		return fmt.Errorf("%w: action %q has no committed state transition",
			ErrProblemSourceActionConflict, command.Action)
	}
}

func lockAndResolveProblemSourceActionScope(
	ctx context.Context,
	tx *sql.Tx,
	command ProblemSourceActionCommand,
) (problemSourceActionScope, error) {
	result, err := tx.ExecContext(ctx, `UPDATE k12_image_task_dispatches
		SET updated_at=updated_at
		WHERE dispatch_id=?`, command.DispatchID)
	if err != nil {
		return problemSourceActionScope{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return problemSourceActionScope{}, err
	}
	if rows != 1 {
		return problemSourceActionScope{}, ErrProblemSourceActionNotFound
	}

	var scope problemSourceActionScope
	err = tx.QueryRowContext(ctx, `
		SELECT d.agent_name,j.submission_id,s.grading_job_id,
		       sm.input_revision,ss.structure_version
		FROM k12_image_task_dispatches d
		JOIN k12_homework_submissions s
		  ON s.agent_name=d.agent_name
		 AND s.dispatch_id=d.dispatch_id
		 AND s.submission_id=d.target_object_id
		JOIN k12_grading_jobs j
		  ON j.agent_name=s.agent_name
		 AND j.record_id=s.grading_job_id
		JOIN k12_problems p
		  ON p.agent_name=j.agent_name
		 AND p.submission_id=j.submission_id
		 AND p.problem_id=?
		JOIN k12_problem_structure_snapshots ss
		  ON ss.agent_name=p.agent_name
		 AND ss.submission_id=p.submission_id
		 AND ss.current_disposition='current'
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=ss.agent_name
		 AND sm.submission_id=ss.submission_id
		 AND sm.structure_version=ss.structure_version
		 AND sm.problem_id=p.problem_id
		WHERE d.dispatch_id=?
		  AND d.target_object_type='homework_submission'`,
		command.ProblemID,
		command.DispatchID,
	).Scan(
		&scope.AgentName,
		&scope.SubmissionID,
		&scope.JobID,
		&scope.AttemptRevision,
		&scope.StructureVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return problemSourceActionScope{}, ErrProblemSourceActionNotFound
	}
	if err != nil {
		return problemSourceActionScope{}, err
	}
	return scope, nil
}

func replayProblemSourceAction(
	ctx context.Context,
	tx *sql.Tx,
	ownerScope string,
	idempotencyKey string,
	requestDigest string,
) (ProblemSourceActionResult, bool, error) {
	var storedDigest, responseJSON string
	err := tx.QueryRowContext(ctx, `
		SELECT request_digest,response_json
		FROM k12_problem_source_action_receipts
		WHERE owner_scope=? AND idempotency_key=?`,
		ownerScope,
		idempotencyKey,
	).Scan(&storedDigest, &responseJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ProblemSourceActionResult{}, false, nil
	}
	if err != nil {
		return ProblemSourceActionResult{}, false, err
	}
	if storedDigest != requestDigest {
		return ProblemSourceActionResult{}, false, fmt.Errorf(
			"%w: Idempotency-Key is bound to another request digest",
			ErrProblemSourceActionConflict,
		)
	}
	var result ProblemSourceActionResult
	if err := json.Unmarshal([]byte(responseJSON), &result); err != nil {
		return ProblemSourceActionResult{}, false, err
	}
	return result, true, nil
}

func currentProblemSourceActionRevision(
	ctx context.Context,
	tx *sql.Tx,
	scope problemSourceActionScope,
	problemID string,
) (int, error) {
	revision := scope.AttemptRevision
	var durableHead int
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(revision) FROM (
			SELECT COALESCE(MAX(input_revision),0) AS revision
			FROM k12_grading_assessment_items
			WHERE agent_name=? AND job_id=? AND problem_id=?
			  AND current_disposition='current' AND structure_version=?
			UNION ALL
			SELECT COALESCE(MAX(input_revision),0)
			FROM k12_problem_skip_receipts
			WHERE agent_name=? AND job_id=? AND problem_id=?
			  AND current_disposition='current' AND structure_version=?
			UNION ALL
			SELECT COALESCE(MAX(result_input_revision),0)
			FROM k12_problem_source_action_receipts
			WHERE agent_name=? AND job_id=? AND problem_id=?
			  AND structure_version=?
		)`,
		scope.AgentName, scope.JobID, problemID, scope.StructureVersion,
		scope.AgentName, scope.JobID, problemID, scope.StructureVersion,
		scope.AgentName, scope.JobID, problemID, scope.StructureVersion,
	).Scan(&durableHead); err != nil {
		return 0, err
	}
	if durableHead > revision {
		revision = durableHead
	}
	if revision < 1 {
		revision = 1
	}
	return revision, nil
}

func nextProblemSourcePublishedRevision(
	ctx context.Context,
	tx *sql.Tx,
	scope problemSourceActionScope,
	problemID string,
) (int, error) {
	var revision int
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(revision) FROM (
			SELECT COALESCE(MAX(published_revision),0) AS revision
			FROM k12_grading_assessment_items
			WHERE agent_name=? AND job_id=? AND problem_id=?
			UNION ALL
			SELECT COALESCE(MAX(published_revision),0)
			FROM k12_problem_skip_receipts
			WHERE agent_name=? AND job_id=? AND problem_id=?
		)`,
		scope.AgentName, scope.JobID, problemID,
		scope.AgentName, scope.JobID, problemID,
	).Scan(&revision); err != nil {
		return 0, err
	}
	return revision + 1, nil
}

func commitProblemSkip(
	ctx context.Context,
	tx *sql.Tx,
	command ProblemSourceActionCommand,
	scope problemSourceActionScope,
	requestDigest string,
	now int64,
) error {
	var currentAssessment int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM k12_grading_assessment_items
		WHERE agent_name=? AND job_id=? AND problem_id=?
		  AND current_disposition='current' AND structure_version=?`,
		scope.AgentName, scope.JobID, command.ProblemID, scope.StructureVersion,
	).Scan(&currentAssessment); err != nil {
		return err
	}
	if currentAssessment != 0 {
		return fmt.Errorf("%w: a published assessment already owns the current head",
			ErrProblemSourceActionConflict)
	}

	var existing int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM k12_problem_skip_receipts
		WHERE agent_name=? AND job_id=? AND problem_id=?
		  AND current_disposition='current' AND structure_version=?`,
		scope.AgentName, scope.JobID, command.ProblemID, scope.StructureVersion,
	).Scan(&existing); err != nil {
		return err
	}
	if existing != 0 {
		return fmt.Errorf("%w: a skip receipt already owns the current head",
			ErrProblemSourceActionConflict)
	}
	publishedRevision, err := nextProblemSourcePublishedRevision(
		ctx, tx, scope, command.ProblemID,
	)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO k12_problem_skip_receipts (
			skip_receipt_id,agent_name,job_id,problem_id,structure_version,
			input_revision,result_digest,current_disposition,published_revision,
			superseded_at,created_at,updated_at
		) VALUES (?,?,?,?,?,?,?,'current',?,0,?,?)`,
		idgen.NanoID(),
		scope.AgentName,
		scope.JobID,
		command.ProblemID,
		command.StructureVersion,
		command.ExpectedInputRevision,
		requestDigest,
		publishedRevision,
		now,
		now,
	)
	return err
}

func commitProblemResume(
	ctx context.Context,
	tx *sql.Tx,
	command ProblemSourceActionCommand,
	scope problemSourceActionScope,
	now int64,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_skip_receipts
		SET current_disposition='superseded',superseded_at=?,updated_at=?
		WHERE agent_name=? AND job_id=? AND problem_id=?
		  AND current_disposition='current' AND input_revision=?
		  AND structure_version=?`,
		now,
		now,
		scope.AgentName,
		scope.JobID,
		command.ProblemID,
		command.ExpectedInputRevision,
		scope.StructureVersion,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: resume requires the current skip head",
			ErrProblemSourceActionConflict)
	}
	return nil
}

func affectedProblemSourceActionMembers(
	ctx context.Context,
	tx *sql.Tx,
	command ProblemSourceActionCommand,
	scope problemSourceActionScope,
) ([]string, string, error) {
	var problemKind, dependencyGroupID string
	if err := tx.QueryRowContext(ctx, `
		SELECT problem_kind,dependency_group_id
		FROM k12_problem_structure_members
		WHERE agent_name=? AND submission_id=? AND structure_version=?
		  AND problem_id=?`,
		scope.AgentName,
		scope.SubmissionID,
		scope.StructureVersion,
		command.ProblemID,
	).Scan(&problemKind, &dependencyGroupID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, "", ErrProblemSourceActionNotFound
		}
		return nil, "", err
	}
	if problemKind != "compound_parent" {
		return []string{command.ProblemID}, dependencyGroupID, nil
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT problem_id,input_revision
		FROM k12_problem_structure_members
		WHERE agent_name=? AND submission_id=? AND structure_version=?
		  AND dependency_group_id=?
		ORDER BY ordinal,problem_id`,
		scope.AgentName,
		scope.SubmissionID,
		scope.StructureVersion,
		dependencyGroupID,
	)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	problemIDs := make([]string, 0)
	for rows.Next() {
		var problemID string
		var inputRevision int
		if err := rows.Scan(&problemID, &inputRevision); err != nil {
			return nil, "", err
		}
		if inputRevision != command.ExpectedInputRevision {
			return nil, "", fmt.Errorf(
				"%w: dependency group member %s input_revision=%d expected=%d",
				ErrProblemSourceActionConflict,
				problemID,
				inputRevision,
				command.ExpectedInputRevision,
			)
		}
		problemIDs = append(problemIDs, problemID)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(problemIDs) == 0 {
		return nil, "", ErrProblemSourceActionNotFound
	}
	return problemIDs, dependencyGroupID, nil
}

func problemSourceInputDigest(
	requestDigest string,
	problemID string,
	inputRevision int,
) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf(
		"%s\x00%s\x00%d",
		requestDigest,
		problemID,
		inputRevision,
	)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func supersedeProblemSourceActionHeads(
	ctx context.Context,
	tx *sql.Tx,
	scope problemSourceActionScope,
	problemID string,
	resultRevision int,
	now int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE k12_grading_assessment_items
		SET current_disposition='superseded',updated_at=?
		WHERE agent_name=? AND job_id=? AND problem_id=?
		  AND structure_version=? AND current_disposition='current'
		  AND input_revision<?`,
		now,
		scope.AgentName,
		scope.JobID,
		problemID,
		scope.StructureVersion,
		resultRevision,
	); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_skip_receipts
		SET current_disposition='superseded',superseded_at=?,updated_at=?
		WHERE agent_name=? AND job_id=? AND problem_id=?
		  AND structure_version=? AND current_disposition='current'
		  AND input_revision<?`,
		now,
		now,
		scope.AgentName,
		scope.JobID,
		problemID,
		scope.StructureVersion,
		resultRevision,
	)
	return err
}

func commitProblemInputChange(
	ctx context.Context,
	tx *sql.Tx,
	command ProblemSourceActionCommand,
	scope problemSourceActionScope,
	requestDigest string,
	resultRevision int,
	now int64,
) error {
	problemIDs, dependencyGroupID, err := affectedProblemSourceActionMembers(
		ctx,
		tx,
		command,
		scope,
	)
	if err != nil {
		return err
	}

	switch command.Action {
	case "correct_text":
		var payload correctTextProblemSourceActionPayload
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			return fmt.Errorf("%w: invalid correct_text payload", ErrProblemSourceActionConflict)
		}
		payload.QuestionCanonicalMarkdown = strings.TrimSpace(payload.QuestionCanonicalMarkdown)
		payload.AnswerCanonicalMarkdown = strings.TrimSpace(payload.AnswerCanonicalMarkdown)
		if payload.QuestionCanonicalMarkdown == "" && payload.AnswerCanonicalMarkdown == "" {
			return fmt.Errorf("%w: correct_text requires corrected text",
				ErrProblemSourceActionConflict)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE k12_problems
			SET canonical_version=?,confirmation_required=0,
			    confirmation_reasons_json='[]',updated_at=?
			WHERE agent_name=? AND submission_id=? AND problem_id=?`,
			resultRevision,
			now,
			scope.AgentName,
			scope.SubmissionID,
			command.ProblemID,
		); err != nil {
			return err
		}
		if payload.QuestionCanonicalMarkdown != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE k12_problems
				SET stem_raw=?,stem_markdown=?,updated_at=?
				WHERE agent_name=? AND submission_id=? AND problem_id=?`,
				payload.QuestionCanonicalMarkdown,
				payload.QuestionCanonicalMarkdown,
				now,
				scope.AgentName,
				scope.SubmissionID,
				command.ProblemID,
			); err != nil {
				return err
			}
		}
		if payload.AnswerCanonicalMarkdown != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE k12_attempts
				SET answer_state='present',answer_raw=?,answer_markdown=?,updated_at=?
				WHERE agent_name=? AND submission_id=? AND problem_id=?`,
				payload.AnswerCanonicalMarkdown,
				payload.AnswerCanonicalMarkdown,
				now,
				scope.AgentName,
				scope.SubmissionID,
				command.ProblemID,
			); err != nil {
				return err
			}
		}
	case "select_region":
		var payload selectRegionProblemSourceActionPayload
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			return fmt.Errorf("%w: invalid select_region payload",
				ErrProblemSourceActionConflict)
		}
		payload.PageAssetID = strings.TrimSpace(payload.PageAssetID)
		if payload.PageAssetID == "" || payload.Region.X < 0 ||
			payload.Region.Y < 0 || payload.Region.Width <= 0 ||
			payload.Region.Height <= 0 {
			return fmt.Errorf("%w: invalid select_region source",
				ErrProblemSourceActionConflict)
		}
		regionJSON, err := json.Marshal(payload.Region)
		if err != nil {
			return err
		}
		for _, problemID := range problemIDs {
			if _, err := tx.ExecContext(ctx, `
				UPDATE k12_problems SET page_asset_id=?,updated_at=?
				WHERE agent_name=? AND submission_id=? AND problem_id=?`,
				payload.PageAssetID,
				now,
				scope.AgentName,
				scope.SubmissionID,
				problemID,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE k12_attempts SET bbox_json=?,updated_at=?
				WHERE agent_name=? AND submission_id=? AND problem_id=?`,
				string(regionJSON),
				now,
				scope.AgentName,
				scope.SubmissionID,
				problemID,
			); err != nil {
				return err
			}
		}
	case "retake":
		var payload retakeProblemSourceActionPayload
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			return fmt.Errorf("%w: invalid retake payload", ErrProblemSourceActionConflict)
		}
		payload.PageAssetID = strings.TrimSpace(payload.PageAssetID)
		if payload.PageAssetID == "" {
			return fmt.Errorf("%w: retake requires page asset",
				ErrProblemSourceActionConflict)
		}
		for _, problemID := range problemIDs {
			if _, err := tx.ExecContext(ctx, `
				UPDATE k12_problems SET page_asset_id=?,updated_at=?
				WHERE agent_name=? AND submission_id=? AND problem_id=?`,
				payload.PageAssetID,
				now,
				scope.AgentName,
				scope.SubmissionID,
				problemID,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE k12_attempts SET bbox_json='',updated_at=?
				WHERE agent_name=? AND submission_id=? AND problem_id=?`,
				now,
				scope.AgentName,
				scope.SubmissionID,
				problemID,
			); err != nil {
				return err
			}
		}
	}

	for _, problemID := range problemIDs {
		inputDigest := problemSourceInputDigest(
			requestDigest,
			problemID,
			resultRevision,
		)
		if _, err := tx.ExecContext(ctx, `
			UPDATE k12_problem_structure_members
			SET input_revision=?
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND problem_id=? AND input_revision=?`,
			resultRevision,
			scope.AgentName,
			scope.SubmissionID,
			scope.StructureVersion,
			problemID,
			command.ExpectedInputRevision,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE k12_attempts
			SET confirmed_version=?,input_digest=?,updated_at=?
			WHERE agent_name=? AND submission_id=? AND problem_id=?`,
			resultRevision,
			inputDigest,
			now,
			scope.AgentName,
			scope.SubmissionID,
			problemID,
		); err != nil {
			return err
		}
		if err := supersedeProblemSourceActionHeads(
			ctx,
			tx,
			scope,
			problemID,
			resultRevision,
			now,
		); err != nil {
			return err
		}
	}
	if dependencyGroupID != "" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE k12_problem_dependency_groups
			SET state='pending',state_revision=state_revision+1,updated_at=?
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND dependency_group_id=?`,
			now,
			scope.AgentName,
			scope.SubmissionID,
			scope.StructureVersion,
			dependencyGroupID,
		); err != nil {
			return err
		}
	}
	return nil
}

func buildProblemSourceProgressiveSnapshot(
	ctx context.Context,
	q dbQueryer,
	scope problemSourceActionScope,
) (ProblemSourceProgressiveSnapshot, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT p.problem_id,sm.input_revision
		FROM k12_problem_structure_snapshots ss
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=ss.agent_name
		 AND sm.submission_id=ss.submission_id
		 AND sm.structure_version=ss.structure_version
		JOIN k12_problems p
		  ON p.agent_name=sm.agent_name
		 AND p.submission_id=sm.submission_id
		 AND p.problem_id=sm.problem_id
		WHERE ss.agent_name=? AND ss.submission_id=?
		  AND ss.structure_version=? AND ss.current_disposition='current'
		  AND p.problem_kind!='compound_parent'
		ORDER BY sm.ordinal,sm.problem_id`,
		scope.AgentName,
		scope.SubmissionID,
		scope.StructureVersion,
	)
	if err != nil {
		return ProblemSourceProgressiveSnapshot{}, err
	}
	type problemHead struct {
		problemID       string
		attemptRevision int
	}
	heads := make([]problemHead, 0)
	for rows.Next() {
		var head problemHead
		if err := rows.Scan(&head.problemID, &head.attemptRevision); err != nil {
			rows.Close()
			return ProblemSourceProgressiveSnapshot{}, err
		}
		heads = append(heads, head)
	}
	if err := rows.Close(); err != nil {
		return ProblemSourceProgressiveSnapshot{}, err
	}
	if err := rows.Err(); err != nil {
		return ProblemSourceProgressiveSnapshot{}, err
	}

	snapshot := ProblemSourceProgressiveSnapshot{
		StructureVersion: scope.StructureVersion,
		ProblemProgress:  make([]ProblemSourceProgress, 0, len(heads)),
	}
	for _, head := range heads {
		progress := ProblemSourceProgress{
			ProblemID:          head.problemID,
			Status:             "awaiting_source",
			InputRevision:      head.attemptRevision,
			CurrentDisposition: "current",
		}
		var assessmentStatus string
		err := q.QueryRowContext(ctx, `
			SELECT status,input_revision,published_revision,current_disposition
			FROM k12_grading_assessment_items
			WHERE agent_name=? AND job_id=? AND problem_id=?
			  AND current_disposition='current' AND structure_version=?
			LIMIT 1`,
			scope.AgentName, scope.JobID, head.problemID, scope.StructureVersion,
		).Scan(
			&assessmentStatus,
			&progress.InputRevision,
			&progress.PublishedRevision,
			&progress.CurrentDisposition,
		)
		if err == nil {
			progress.Status = assessmentStatus
			snapshot.Coverage.Published++
		} else if !errors.Is(err, sql.ErrNoRows) {
			return ProblemSourceProgressiveSnapshot{}, err
		} else {
			err = q.QueryRowContext(ctx, `
				SELECT input_revision,published_revision,current_disposition
				FROM k12_problem_skip_receipts
				WHERE agent_name=? AND job_id=? AND problem_id=?
				  AND current_disposition='current' AND structure_version=?
				LIMIT 1`,
				scope.AgentName, scope.JobID, head.problemID, scope.StructureVersion,
			).Scan(
				&progress.InputRevision,
				&progress.PublishedRevision,
				&progress.CurrentDisposition,
			)
			if err == nil {
				progress.Status = "skipped"
				snapshot.Coverage.Skipped++
			} else if !errors.Is(err, sql.ErrNoRows) {
				return ProblemSourceProgressiveSnapshot{}, err
			} else {
				var commandRevision int
				if err := q.QueryRowContext(ctx, `
						SELECT COALESCE(MAX(result_input_revision),0)
						FROM k12_problem_source_action_receipts
						WHERE agent_name=? AND job_id=? AND problem_id=?
						  AND structure_version=?`,
					scope.AgentName, scope.JobID, head.problemID, scope.StructureVersion,
				).Scan(&commandRevision); err != nil {
					return ProblemSourceProgressiveSnapshot{}, err
				}
				if commandRevision > progress.InputRevision {
					progress.InputRevision = commandRevision
				}
				if progress.InputRevision < 1 {
					progress.InputRevision = 1
				}
				snapshot.Coverage.Awaiting++
			}
		}
		if progress.PublishedRevision > snapshot.SnapshotRevision {
			snapshot.SnapshotRevision = progress.PublishedRevision
		}
		if progress.InputRevision > snapshot.SnapshotRevision {
			snapshot.SnapshotRevision = progress.InputRevision
		}
		snapshot.ProblemProgress = append(snapshot.ProblemProgress, progress)
	}
	snapshot.Coverage.Total = len(snapshot.ProblemProgress)
	snapshot.Coverage.ProjectionRevision = snapshot.SnapshotRevision
	switch {
	case snapshot.Coverage.Total == 0:
		snapshot.Coverage.Status = "empty"
	case snapshot.Coverage.Awaiting == 0:
		snapshot.Coverage.Status = "complete"
	default:
		snapshot.Coverage.Status = "in_progress"
	}
	return snapshot, nil
}

func validateGradingProgressiveProjection(
	snapshot ProblemSourceProgressiveSnapshot,
	artifact *k12.GradingFinalArtifact,
) error {
	coverage := snapshot.Coverage
	if len(snapshot.ProblemProgress) != coverage.Total ||
		coverage.Published+coverage.Skipped+coverage.Awaiting+coverage.Failed != coverage.Total {
		return ErrGradingProgressiveProjectionConflict
	}
	if artifact == nil {
		return nil
	}
	if snapshot.StructureVersion != artifact.StructureVersion ||
		coverage.Total != artifact.TotalCount ||
		coverage.Published != artifact.PublishedCount ||
		coverage.Skipped != artifact.SkippedCount ||
		coverage.Awaiting != 0 ||
		coverage.Failed != 0 ||
		coverage.Status != "complete" {
		return ErrGradingProgressiveProjectionConflict
	}
	return nil
}

// GetGradingProgressiveProjection reads the current structure, per-problem
// assessment/skip receipts and optional final artifact in one read
// transaction. Both the public ImageTask GET and source-action commands use
// the same snapshot builder; neither may derive progress from the artifact.
func (s *Store) GetGradingProgressiveProjection(
	ctx context.Context,
	agentName, jobID string,
) (GradingProgressiveProjection, error) {
	agentName = strings.TrimSpace(agentName)
	jobID = strings.TrimSpace(jobID)
	if agentName == "" || jobID == "" {
		return GradingProgressiveProjection{}, records.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return GradingProgressiveProjection{}, err
	}
	defer tx.Rollback()

	var submissionID string
	if err := tx.QueryRowContext(ctx, `
		SELECT submission_id
		FROM k12_grading_jobs
		WHERE agent_name=? AND record_id=?`,
		agentName,
		jobID,
	).Scan(&submissionID); errors.Is(err, sql.ErrNoRows) {
		return GradingProgressiveProjection{}, records.ErrNotFound
	} else if err != nil {
		return GradingProgressiveProjection{}, err
	}

	var structureVersion int
	structureErr := tx.QueryRowContext(ctx, `
		SELECT structure_version
		FROM k12_problem_structure_snapshots
		WHERE agent_name=? AND submission_id=? AND current_disposition='current'
		ORDER BY structure_version DESC
		LIMIT 1`,
		agentName,
		submissionID,
	).Scan(&structureVersion)
	if structureErr != nil && !errors.Is(structureErr, sql.ErrNoRows) {
		return GradingProgressiveProjection{}, structureErr
	}

	var artifact *k12.GradingFinalArtifact
	storedArtifact, artifactErr := getGradingFinalArtifactByJobVia(
		ctx,
		tx,
		agentName,
		jobID,
	)
	if artifactErr == nil {
		artifact = &storedArtifact
	} else if !errors.Is(artifactErr, records.ErrNotFound) {
		return GradingProgressiveProjection{}, artifactErr
	}
	if errors.Is(structureErr, sql.ErrNoRows) {
		if artifact != nil {
			return GradingProgressiveProjection{}, ErrGradingProgressiveProjectionConflict
		}
		if err := tx.Commit(); err != nil {
			return GradingProgressiveProjection{}, err
		}
		return GradingProgressiveProjection{}, nil
	}

	snapshot, err := buildProblemSourceProgressiveSnapshot(
		ctx,
		tx,
		problemSourceActionScope{
			AgentName:        agentName,
			SubmissionID:     submissionID,
			JobID:            jobID,
			StructureVersion: structureVersion,
		},
	)
	if err != nil {
		return GradingProgressiveProjection{}, err
	}
	if err := validateGradingProgressiveProjection(snapshot, artifact); err != nil {
		return GradingProgressiveProjection{}, err
	}
	if err := tx.Commit(); err != nil {
		return GradingProgressiveProjection{}, err
	}
	return GradingProgressiveProjection{
		ProgressiveSnapshot: snapshot,
		FinalArtifact:       artifact,
	}, nil
}

// CommitProblemSourceAction resolves the agent only from the durable dispatch
// and commits the command receipt plus state transition in one SQLite
// transaction. OwnerScope is supplied by the server runtime/auth middleware;
// no request-controlled owner or agent is accepted by this API.
func (s *Store) CommitProblemSourceAction(
	ctx context.Context,
	command ProblemSourceActionCommand,
) (ProblemSourceActionResult, error) {
	command.OwnerScope = strings.TrimSpace(command.OwnerScope)
	command.TrustedAgentName = strings.TrimSpace(command.TrustedAgentName)
	command.DispatchID = strings.TrimSpace(command.DispatchID)
	command.ProblemID = strings.TrimSpace(command.ProblemID)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.Action = strings.TrimSpace(command.Action)
	if err := validateProblemSourceActionCommand(command); err != nil {
		return ProblemSourceActionResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	defer tx.Rollback()
	scope, err := lockAndResolveProblemSourceActionScope(ctx, tx, command)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	if command.TrustedAgentName != "" &&
		command.TrustedAgentName != scope.AgentName {
		return ProblemSourceActionResult{}, ErrProblemSourceActionNotFound
	}
	requestDigest, err := problemSourceActionDigest(command, scope.AgentName)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	if replay, ok, err := replayProblemSourceAction(
		ctx, tx, command.OwnerScope, command.IdempotencyKey, requestDigest,
	); err != nil {
		return ProblemSourceActionResult{}, err
	} else if ok {
		if err := tx.Commit(); err != nil {
			return ProblemSourceActionResult{}, err
		}
		return replay, nil
	}
	if command.StructureVersion != scope.StructureVersion {
		return ProblemSourceActionResult{}, fmt.Errorf(
			"%w: structure_version=%d current=%d",
			ErrProblemSourceActionConflict,
			command.StructureVersion,
			scope.StructureVersion,
		)
	}
	currentRevision, err := currentProblemSourceActionRevision(
		ctx, tx, scope, command.ProblemID,
	)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	if command.ExpectedInputRevision != currentRevision {
		return ProblemSourceActionResult{}, fmt.Errorf(
			"%w: expected_input_revision=%d current=%d",
			ErrProblemSourceActionConflict,
			command.ExpectedInputRevision,
			currentRevision,
		)
	}
	now := nowUnix()
	resultRevision := currentRevision
	switch command.Action {
	case "correct_text", "select_region", "retake":
		resultRevision++
		err = commitProblemInputChange(
			ctx,
			tx,
			command,
			scope,
			requestDigest,
			resultRevision,
			now,
		)
	case "skip":
		err = commitProblemSkip(ctx, tx, command, scope, requestDigest, now)
	case "resume":
		err = commitProblemResume(ctx, tx, command, scope, now)
		resultRevision++
	}
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	commandReceiptID := idgen.NanoID()

	// Persist a provisional command row before building the projection. Resume
	// revisions therefore participate in the snapshot even though they have no
	// current skip receipt.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO k12_problem_source_action_receipts (
			command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,
			idempotency_key,request_digest,action,structure_version,
			expected_input_revision,result_input_revision,response_json,created_at,updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?, ?,?)`,
		commandReceiptID,
		command.OwnerScope,
		scope.AgentName,
		command.DispatchID,
		scope.JobID,
		command.ProblemID,
		command.IdempotencyKey,
		requestDigest,
		command.Action,
		command.StructureVersion,
		command.ExpectedInputRevision,
		resultRevision,
		`{}`,
		now,
		now,
	)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	snapshot, err := buildProblemSourceProgressiveSnapshot(ctx, tx, scope)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	result := ProblemSourceActionResult{
		CommandReceiptID:    commandReceiptID,
		InputRevision:       resultRevision,
		ProgressiveSnapshot: snapshot,
	}
	responseJSON, err := json.Marshal(result)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_source_action_receipts
		SET response_json=?,updated_at=?
		WHERE command_receipt_id=?`,
		string(responseJSON),
		now,
		commandReceiptID,
	); err != nil {
		return ProblemSourceActionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProblemSourceActionResult{}, err
	}
	return result, nil
}
