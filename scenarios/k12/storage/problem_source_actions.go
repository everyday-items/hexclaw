package k12storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/viewcontract"
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

type ProblemSourceActionResult = viewcontract.FrozenProblemSourceActionResponse

type ProblemSourceProgressiveSnapshot = viewcontract.ProblemSourceProgressiveSnapshot

type GradingProgressiveProjection struct {
	ProgressiveSnapshot ProblemSourceProgressiveSnapshot
	FinalArtifact       *k12.GradingFinalArtifact
}

type ProblemSourceProgress = viewcontract.ProblemSourceProgress

type ProblemSourceProgressiveCoverage = viewcontract.ProblemSourceProgressiveCoverage

type problemSourceActionScope struct {
	AgentName        string
	SubmissionID     string
	JobID            string
	AttemptRevision  int
	StructureVersion int
}

// ProblemSourceActionAssetScope is the durable, server-derived identity used
// by the usecase to validate a requested PageAsset before the write
// transaction. The client never supplies AgentName or the current asset head.
type ProblemSourceActionAssetScope struct {
	AgentName        string
	SubmissionID     string
	JobID            string
	StructureVersion int
	InputRevision    int
	PageAssetID      string
}

// GetProblemSourceActionAssetScope resolves one command path through the
// dispatch, homework, grading job and current structure. Commit repeats the
// same joins under its write lock, so a concurrent head change still loses the
// expected revision CAS.
func (s *Store) GetProblemSourceActionAssetScope(
	ctx context.Context,
	dispatchID, problemID string,
) (ProblemSourceActionAssetScope, error) {
	dispatchID = strings.TrimSpace(dispatchID)
	problemID = strings.TrimSpace(problemID)
	if dispatchID == "" || problemID == "" {
		return ProblemSourceActionAssetScope{}, ErrProblemSourceActionNotFound
	}
	var scope ProblemSourceActionAssetScope
	var revisionAsset sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT d.agent_name,j.submission_id,s.grading_job_id,
		       ss.structure_version,sm.input_revision,ir.page_asset_id,p.page_asset_id
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
		LEFT JOIN k12_problem_input_revisions ir
		  ON ir.agent_name=sm.agent_name
		 AND ir.submission_id=sm.submission_id
		 AND ir.structure_version=sm.structure_version
		 AND ir.problem_id=sm.problem_id
		 AND ir.input_revision=sm.input_revision
		 AND ir.current_disposition='current'
		WHERE d.dispatch_id=?
		  AND d.target_object_type='homework_submission'`,
		problemID,
		dispatchID,
	).Scan(
		&scope.AgentName,
		&scope.SubmissionID,
		&scope.JobID,
		&scope.StructureVersion,
		&scope.InputRevision,
		&revisionAsset,
		&scope.PageAssetID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ProblemSourceActionAssetScope{}, ErrProblemSourceActionNotFound
	}
	if err != nil {
		return ProblemSourceActionAssetScope{}, err
	}
	if revisionAsset.Valid && strings.TrimSpace(revisionAsset.String) != "" {
		scope.PageAssetID = strings.TrimSpace(revisionAsset.String)
	}
	return scope, nil
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

func decodeProblemSourceActionPayloadStrict(raw json.RawMessage, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("payload contains more than one JSON value")
		}
		return err
	}
	return nil
}

// canonicalProblemSourceActionPayload produces the exact bytes used both by
// the committed transition and its idempotency digest. Canonicalization is
// action-aware: values normalized by the domain transition (notably trimmed
// text and PageAsset IDs) must not produce a different request identity.
func canonicalProblemSourceActionPayload(
	action string,
	raw json.RawMessage,
) (json.RawMessage, error) {
	var value any
	switch strings.TrimSpace(action) {
	case "correct_text":
		var payload correctTextProblemSourceActionPayload
		if err := decodeProblemSourceActionPayloadStrict(raw, &payload); err != nil {
			return nil, fmt.Errorf("%w: invalid correct_text payload: %v",
				ErrProblemSourceActionConflict, err)
		}
		payload.QuestionCanonicalMarkdown = strings.TrimSpace(
			payload.QuestionCanonicalMarkdown,
		)
		payload.AnswerCanonicalMarkdown = strings.TrimSpace(
			payload.AnswerCanonicalMarkdown,
		)
		if payload.QuestionCanonicalMarkdown == "" &&
			payload.AnswerCanonicalMarkdown == "" {
			return nil, fmt.Errorf("%w: correct_text requires corrected text",
				ErrProblemSourceActionConflict)
		}
		value = payload
	case "select_region":
		var payload selectRegionProblemSourceActionPayload
		if err := decodeProblemSourceActionPayloadStrict(raw, &payload); err != nil {
			return nil, fmt.Errorf("%w: invalid select_region payload: %v",
				ErrProblemSourceActionConflict, err)
		}
		payload.PageAssetID = strings.TrimSpace(payload.PageAssetID)
		if payload.PageAssetID == "" || payload.Region.X < 0 ||
			payload.Region.Y < 0 || payload.Region.Width <= 0 ||
			payload.Region.Height <= 0 {
			return nil, fmt.Errorf("%w: invalid select_region source",
				ErrProblemSourceActionConflict)
		}
		value = payload
	case "retake":
		var payload retakeProblemSourceActionPayload
		if err := decodeProblemSourceActionPayloadStrict(raw, &payload); err != nil {
			return nil, fmt.Errorf("%w: invalid retake payload: %v",
				ErrProblemSourceActionConflict, err)
		}
		payload.PageAssetID = strings.TrimSpace(payload.PageAssetID)
		if payload.PageAssetID == "" {
			return nil, fmt.Errorf("%w: retake requires page asset",
				ErrProblemSourceActionConflict)
		}
		value = payload
	case "skip", "resume":
		var payload struct{}
		if err := decodeProblemSourceActionPayloadStrict(raw, &payload); err != nil {
			return nil, fmt.Errorf("%w: invalid %s payload: %v",
				ErrProblemSourceActionConflict, action, err)
		}
		value = payload
	default:
		return nil, fmt.Errorf("%w: unsupported source action %q",
			ErrProblemSourceActionConflict, action)
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
	payload, err := canonicalProblemSourceActionPayload(
		command.Action, command.Payload,
	)
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
	var commandReceiptID, dispatchID, problemID, action string
	var structureVersion, resultInputRevision int
	err := tx.QueryRowContext(ctx, `
		SELECT request_digest,response_json,command_receipt_id,dispatch_id,problem_id,action,
		       structure_version,result_input_revision
		FROM k12_problem_source_action_receipts
		WHERE owner_scope=? AND idempotency_key=?`,
		ownerScope,
		idempotencyKey,
	).Scan(
		&storedDigest,
		&responseJSON,
		&commandReceiptID,
		&dispatchID,
		&problemID,
		&action,
		&structureVersion,
		&resultInputRevision,
	)
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
	result, err := viewcontract.ParseFrozenProblemSourceActionResponse([]byte(responseJSON))
	if err != nil {
		return ProblemSourceActionResult{}, false, err
	}
	if result.CommandReceiptID != commandReceiptID ||
		result.DispatchID != dispatchID || result.ProblemID != problemID ||
		result.Action != action || result.StructureVersion != structureVersion ||
		result.InputRevision != resultInputRevision {
		return ProblemSourceActionResult{}, false, fmt.Errorf(
			"frozen problem source action response identity does not match receipt",
		)
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
	scope problemSourceActionScope,
	problemIDs []string,
	expectedInputRevision int,
	requestDigest string,
	now int64,
) error {
	for _, problemID := range problemIDs {
		var currentAssessment int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM k12_grading_assessment_items
			WHERE agent_name=? AND job_id=? AND problem_id=?
			  AND current_disposition='current' AND structure_version=?`,
			scope.AgentName, scope.JobID, problemID, scope.StructureVersion,
		).Scan(&currentAssessment); err != nil {
			return err
		}
		if currentAssessment != 0 {
			return fmt.Errorf("%w: problem %s already has a published assessment head",
				ErrProblemSourceActionConflict, problemID)
		}

		var existing int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM k12_problem_skip_receipts
			WHERE agent_name=? AND job_id=? AND problem_id=?
			  AND current_disposition='current' AND structure_version=?`,
			scope.AgentName, scope.JobID, problemID, scope.StructureVersion,
		).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			return fmt.Errorf("%w: problem %s already has a current skip head",
				ErrProblemSourceActionConflict, problemID)
		}
		publishedRevision, err := nextProblemSourcePublishedRevision(
			ctx, tx, scope, problemID,
		)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO k12_problem_skip_receipts (
				skip_receipt_id,agent_name,job_id,problem_id,structure_version,
				input_revision,result_digest,current_disposition,published_revision,
				superseded_at,created_at,updated_at
			) VALUES (?,?,?,?,?,?,?,'current',?,0,?,?)`,
			idgen.NanoID(),
			scope.AgentName,
			scope.JobID,
			problemID,
			scope.StructureVersion,
			expectedInputRevision,
			requestDigest,
			publishedRevision,
			now,
			now,
		); err != nil {
			return err
		}
	}
	return nil
}

func commitProblemResume(
	ctx context.Context,
	tx *sql.Tx,
	scope problemSourceActionScope,
	problemIDs []string,
	expectedInputRevision int,
	now int64,
) error {
	for _, problemID := range problemIDs {
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
			problemID,
			expectedInputRevision,
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
			return fmt.Errorf("%w: resume requires current skip head for problem %s",
				ErrProblemSourceActionConflict, problemID)
		}
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
	// A source ambiguity belongs to the server-owned dependency group, not to
	// whichever visible child the Desktop happened to use as the command path.
	// Always expand the group and exclude the non-answerable compound parent.
	// This keeps all answerable siblings on one input head without trusting a
	// client supplied problem_ids list.
	rows, err := tx.QueryContext(ctx, `
		SELECT problem_id,input_revision,problem_kind
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
		var problemID, memberKind string
		var inputRevision int
		if err := rows.Scan(&problemID, &inputRevision, &memberKind); err != nil {
			return nil, "", err
		}
		if memberKind == "compound_parent" {
			continue
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
	if problemKind != "compound_parent" {
		found := false
		for _, problemID := range problemIDs {
			if problemID == command.ProblemID {
				found = true
				break
			}
		}
		if !found {
			return nil, "", ErrProblemSourceActionNotFound
		}
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

type problemInputRevisionHead struct {
	PageAssetID               string
	SourceRegionJSON          string
	StemRaw                   string
	AnswerRaw                 string
	AnswerBBoxJSON            string
	QuestionCanonicalMarkdown string
	AnswerCanonicalMarkdown   string
}

// currentProblemInputRevisionHead lazily creates the legacy v1 input head.
// Tests and restores can materialize Problem/Attempt facts after V72 has run;
// command processing must still preserve that evidence before appending v2.
func currentProblemInputRevisionHead(
	ctx context.Context,
	tx *sql.Tx,
	scope problemSourceActionScope,
	problemID string,
	expectedRevision int,
	now int64,
) (problemInputRevisionHead, error) {
	var head problemInputRevisionHead
	var inputDigest string
	if err := tx.QueryRowContext(ctx, `
		SELECT p.page_asset_id,p.stem_raw,COALESCE(a.answer_raw,''),
		       COALESCE(a.bbox_json,''),p.stem_markdown,
		       COALESCE(a.answer_markdown,''),COALESCE(a.input_digest,'')
		FROM k12_problems p
		JOIN k12_attempts a
		  ON a.agent_name=p.agent_name
		 AND a.submission_id=p.submission_id
		 AND a.problem_id=p.problem_id
		WHERE p.agent_name=? AND p.submission_id=? AND p.problem_id=?`,
		scope.AgentName,
		scope.SubmissionID,
		problemID,
	).Scan(
		&head.PageAssetID,
		&head.StemRaw,
		&head.AnswerRaw,
		&head.AnswerBBoxJSON,
		&head.QuestionCanonicalMarkdown,
		&head.AnswerCanonicalMarkdown,
		&inputDigest,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return problemInputRevisionHead{}, ErrProblemSourceActionNotFound
		}
		return problemInputRevisionHead{}, err
	}
	if inputDigest == "" {
		inputDigest = problemSourceInputDigest("legacy", problemID, expectedRevision)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO k12_problem_input_revisions (
			agent_name,submission_id,structure_version,problem_id,input_revision,
			page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
			question_canonical_markdown,
			answer_canonical_markdown,input_digest,current_disposition,
			origin_command_receipt_id,origin_kind,created_at,updated_at
		) VALUES (?,?,?,?,?,?,NULL,?,?,?,?,?,?,'current',NULL,'legacy_unverified',?,?)`,
		scope.AgentName,
		scope.SubmissionID,
		scope.StructureVersion,
		problemID,
		expectedRevision,
		head.PageAssetID,
		head.StemRaw,
		head.AnswerRaw,
		head.AnswerBBoxJSON,
		head.QuestionCanonicalMarkdown,
		head.AnswerCanonicalMarkdown,
		inputDigest,
		now,
		now,
	); err != nil {
		return problemInputRevisionHead{}, err
	}
	var sourceRegion sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
		       question_canonical_markdown,answer_canonical_markdown
		FROM k12_problem_input_revisions
		WHERE agent_name=? AND submission_id=? AND structure_version=?
		  AND problem_id=? AND input_revision=?
		  AND current_disposition='current'`,
		scope.AgentName,
		scope.SubmissionID,
		scope.StructureVersion,
		problemID,
		expectedRevision,
	).Scan(
		&head.PageAssetID,
		&sourceRegion,
		&head.StemRaw,
		&head.AnswerRaw,
		&head.AnswerBBoxJSON,
		&head.QuestionCanonicalMarkdown,
		&head.AnswerCanonicalMarkdown,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return problemInputRevisionHead{}, fmt.Errorf(
				"%w: problem %s has no current immutable input revision %d",
				ErrProblemSourceActionConflict,
				problemID,
				expectedRevision,
			)
		}
		return problemInputRevisionHead{}, err
	}
	if sourceRegion.Valid {
		head.SourceRegionJSON = sourceRegion.String
	}
	return head, nil
}

func appendProblemInputRevision(
	ctx context.Context,
	tx *sql.Tx,
	scope problemSourceActionScope,
	problemID string,
	expectedRevision, resultRevision int,
	commandReceiptID, requestDigest string,
	head problemInputRevisionHead,
	now int64,
) error {
	inputDigest := problemSourceInputDigest(requestDigest, problemID, resultRevision)
	result, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_input_revisions
		SET current_disposition='superseded',updated_at=?
		WHERE agent_name=? AND submission_id=? AND structure_version=?
		  AND problem_id=? AND input_revision=? AND current_disposition='current'`,
		now,
		scope.AgentName,
		scope.SubmissionID,
		scope.StructureVersion,
		problemID,
		expectedRevision,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: immutable input CAS lost for problem %s",
			ErrProblemSourceActionConflict, problemID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO k12_problem_input_revisions (
			agent_name,submission_id,structure_version,problem_id,input_revision,
			page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
			question_canonical_markdown,
			answer_canonical_markdown,input_digest,current_disposition,
			origin_command_receipt_id,created_at,updated_at
		) VALUES (?,?,?,?,?,?,NULLIF(?,''),?,?,?,?,?,?,'current',?,?,?)`,
		scope.AgentName,
		scope.SubmissionID,
		scope.StructureVersion,
		problemID,
		resultRevision,
		head.PageAssetID,
		head.SourceRegionJSON,
		head.StemRaw,
		head.AnswerRaw,
		head.AnswerBBoxJSON,
		head.QuestionCanonicalMarkdown,
		head.AnswerCanonicalMarkdown,
		inputDigest,
		commandReceiptID,
		now,
		now,
	); err != nil {
		return err
	}

	memberResult, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_structure_members
		SET input_revision=?
		WHERE agent_name=? AND submission_id=? AND structure_version=?
		  AND problem_id=? AND input_revision=?`,
		resultRevision,
		scope.AgentName,
		scope.SubmissionID,
		scope.StructureVersion,
		problemID,
		expectedRevision,
	)
	if err != nil {
		return err
	}
	memberRows, err := memberResult.RowsAffected()
	if err != nil {
		return err
	}
	if memberRows != 1 {
		return fmt.Errorf("%w: structure input CAS lost for problem %s",
			ErrProblemSourceActionConflict, problemID)
	}
	attemptResult, err := tx.ExecContext(ctx, `
		UPDATE k12_attempts
		SET confirmed_version=?,input_digest=?,updated_at=?
		WHERE agent_name=? AND submission_id=? AND problem_id=?
		  AND confirmed_version=?`,
		resultRevision,
		inputDigest,
		now,
		scope.AgentName,
		scope.SubmissionID,
		problemID,
		expectedRevision,
	)
	if err != nil {
		return err
	}
	attemptRows, err := attemptResult.RowsAffected()
	if err != nil {
		return err
	}
	if attemptRows != 1 {
		return fmt.Errorf("%w: attempt input CAS lost for problem %s",
			ErrProblemSourceActionConflict, problemID)
	}
	return supersedeProblemSourceActionHeads(
		ctx,
		tx,
		scope,
		problemID,
		resultRevision,
		now,
	)
}

func commitProblemInputChange(
	ctx context.Context,
	tx *sql.Tx,
	command ProblemSourceActionCommand,
	scope problemSourceActionScope,
	problemIDs []string,
	dependencyGroupID string,
	commandReceiptID string,
	requestDigest string,
	resultRevision int,
	now int64,
) error {
	var correction correctTextProblemSourceActionPayload
	var selectedRegionJSON string
	var selectedPageAssetID string
	switch command.Action {
	case "correct_text":
		if err := json.Unmarshal(command.Payload, &correction); err != nil {
			return fmt.Errorf("%w: invalid correct_text payload", ErrProblemSourceActionConflict)
		}
		correction.QuestionCanonicalMarkdown = strings.TrimSpace(
			correction.QuestionCanonicalMarkdown,
		)
		correction.AnswerCanonicalMarkdown = strings.TrimSpace(
			correction.AnswerCanonicalMarkdown,
		)
		if correction.QuestionCanonicalMarkdown == "" && correction.AnswerCanonicalMarkdown == "" {
			return fmt.Errorf("%w: correct_text requires corrected text",
				ErrProblemSourceActionConflict)
		}
	case "select_region":
		var payload selectRegionProblemSourceActionPayload
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			return fmt.Errorf("%w: invalid select_region payload",
				ErrProblemSourceActionConflict)
		}
		selectedPageAssetID = strings.TrimSpace(payload.PageAssetID)
		if selectedPageAssetID == "" || payload.Region.X < 0 || payload.Region.Y < 0 ||
			payload.Region.Width <= 0 || payload.Region.Height <= 0 {
			return fmt.Errorf("%w: invalid select_region source",
				ErrProblemSourceActionConflict)
		}
		regionJSON, err := json.Marshal(payload.Region)
		if err != nil {
			return err
		}
		selectedRegionJSON = string(regionJSON)
	case "retake":
		var payload retakeProblemSourceActionPayload
		if err := json.Unmarshal(command.Payload, &payload); err != nil {
			return fmt.Errorf("%w: invalid retake payload", ErrProblemSourceActionConflict)
		}
		selectedPageAssetID = strings.TrimSpace(payload.PageAssetID)
		if selectedPageAssetID == "" {
			return fmt.Errorf("%w: retake requires page asset",
				ErrProblemSourceActionConflict)
		}
	case "resume":
	default:
		return fmt.Errorf("%w: action %s is not an input revision",
			ErrProblemSourceActionConflict, command.Action)
	}

	for _, problemID := range problemIDs {
		head, err := currentProblemInputRevisionHead(
			ctx,
			tx,
			scope,
			problemID,
			command.ExpectedInputRevision,
			now,
		)
		if err != nil {
			return err
		}
		switch command.Action {
		case "correct_text":
			// A correction is a canonical overlay. Raw OCR columns remain the
			// immutable observation from the original page.
			if problemID == command.ProblemID {
				if correction.QuestionCanonicalMarkdown != "" {
					head.QuestionCanonicalMarkdown = correction.QuestionCanonicalMarkdown
				}
				if correction.AnswerCanonicalMarkdown != "" {
					head.AnswerCanonicalMarkdown = correction.AnswerCanonicalMarkdown
				}
			}
		case "select_region":
			head.PageAssetID = selectedPageAssetID
			head.SourceRegionJSON = selectedRegionJSON
			// The selected crop is a new recognition input. Copying OCR or an
			// answer anchor produced from the previous input would falsely claim
			// that those observations came from this source region. The
			// superseded revision remains the immutable audit record; the worker
			// appends recognition output for this new input before assessment.
			head.StemRaw = ""
			head.AnswerRaw = ""
			head.AnswerBBoxJSON = ""
			head.QuestionCanonicalMarkdown = ""
			head.AnswerCanonicalMarkdown = ""
		case "retake":
			head.PageAssetID = selectedPageAssetID
			head.SourceRegionJSON = ""
			// A new immutable photo cannot inherit OCR or geometry observed on
			// the old photo. Retain those bytes only on the superseded revision.
			head.StemRaw = ""
			head.AnswerRaw = ""
			head.AnswerBBoxJSON = ""
			head.QuestionCanonicalMarkdown = ""
			head.AnswerCanonicalMarkdown = ""
		}
		if err := appendProblemInputRevision(
			ctx,
			tx,
			scope,
			problemID,
			command.ExpectedInputRevision,
			resultRevision,
			commandReceiptID,
			requestDigest,
			head,
			now,
		); err != nil {
			return err
		}
	}

	if command.Action == "correct_text" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE k12_problems
			SET stem_markdown=CASE WHEN ?='' THEN stem_markdown ELSE ? END,
			    canonical_version=?,confirmation_required=0,
			    confirmation_reasons_json='[]',updated_at=?
			WHERE agent_name=? AND submission_id=? AND problem_id=?`,
			correction.QuestionCanonicalMarkdown,
			correction.QuestionCanonicalMarkdown,
			resultRevision,
			now,
			scope.AgentName,
			scope.SubmissionID,
			command.ProblemID,
		); err != nil {
			return err
		}
		if correction.AnswerCanonicalMarkdown != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE k12_attempts
				SET answer_state='present',answer_markdown=?,updated_at=?
				WHERE agent_name=? AND submission_id=? AND problem_id=?`,
				correction.AnswerCanonicalMarkdown,
				now,
				scope.AgentName,
				scope.SubmissionID,
				command.ProblemID,
			); err != nil {
				return err
			}
		}
	}

	if dependencyGroupID != "" {
		result, err := tx.ExecContext(ctx, `
			UPDATE k12_problem_dependency_groups
			SET state='pending',state_revision=state_revision+1,updated_at=?
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND dependency_group_id=?`,
			now,
			scope.AgentName,
			scope.SubmissionID,
			scope.StructureVersion,
			dependencyGroupID,
		)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("%w: dependency group %s is missing",
				ErrProblemSourceActionConflict, dependencyGroupID)
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
		SELECT p.problem_id,sm.input_revision,p.confirmation_required,
		       COALESCE(ir.page_asset_id,p.page_asset_id),ir.source_region_json,
		       COALESCE(pa.pixel_width,0),COALESCE(pa.pixel_height,0)
		FROM k12_problem_structure_snapshots ss
		JOIN k12_problem_structure_members sm
		  ON sm.agent_name=ss.agent_name
		 AND sm.submission_id=ss.submission_id
		 AND sm.structure_version=ss.structure_version
		JOIN k12_problems p
		  ON p.agent_name=sm.agent_name
		 AND p.submission_id=sm.submission_id
		 AND p.problem_id=sm.problem_id
		LEFT JOIN k12_problem_input_revisions ir
		  ON ir.agent_name=sm.agent_name
		 AND ir.submission_id=sm.submission_id
		 AND ir.structure_version=sm.structure_version
		 AND ir.problem_id=sm.problem_id
		 AND ir.input_revision=sm.input_revision
		 AND ir.current_disposition='current'
		LEFT JOIN k12_page_assets pa
		  ON pa.agent_name=p.agent_name
		 AND pa.page_asset_id=COALESCE(ir.page_asset_id,p.page_asset_id)
		 AND pa.storage_state='ready'
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
		needsResolution bool
		pageAssetID     string
		sourceRegion    *k12.SourcePixelRegion
		sourceWidth     int
		sourceHeight    int
	}
	heads := make([]problemHead, 0)
	for rows.Next() {
		var head problemHead
		var sourceRegionJSON sql.NullString
		if err := rows.Scan(
			&head.problemID,
			&head.attemptRevision,
			&head.needsResolution,
			&head.pageAssetID,
			&sourceRegionJSON,
			&head.sourceWidth,
			&head.sourceHeight,
		); err != nil {
			rows.Close()
			return ProblemSourceProgressiveSnapshot{}, err
		}
		if sourceRegionJSON.Valid {
			var region k12.SourcePixelRegion
			if err := json.Unmarshal([]byte(sourceRegionJSON.String), &region); err != nil {
				rows.Close()
				return ProblemSourceProgressiveSnapshot{}, fmt.Errorf(
					"decode current problem source region: %w",
					err,
				)
			}
			head.sourceRegion = &region
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
		status := "processing"
		if head.needsResolution {
			status = "awaiting_source"
		}
		progress := ProblemSourceProgress{
			ProblemID:          head.problemID,
			Status:             status,
			InputRevision:      head.attemptRevision,
			CurrentDisposition: "current",
		}
		// V72 legacy_unverified heads can predate owner-scoped PageAsset
		// metadata. Keep their frozen response backward-compatible; every ready
		// PageAsset head emits the complete source-fact set atomically.
		if head.sourceWidth > 0 && head.sourceHeight > 0 {
			progress.PageAssetID = head.pageAssetID
			progress.SourceWidth = head.sourceWidth
			progress.SourceHeight = head.sourceHeight
			if head.sourceRegion != nil {
				progress.SourceRegion = &viewcontract.SourcePixelRegion{
					X:      head.sourceRegion.X,
					Y:      head.sourceRegion.Y,
					Width:  head.sourceRegion.Width,
					Height: head.sourceRegion.Height,
				}
			}
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
				var sourceWorkStatus string
				err = q.QueryRowContext(ctx, `
					SELECT work.status
					FROM k12_problem_source_reprocess_jobs AS work
					JOIN json_each(work.affected_problem_ids_json) AS affected
					  ON CAST(affected.value AS TEXT)=?
					WHERE work.agent_name=? AND work.job_id=?
					  AND work.structure_version=? AND work.input_revision=?
					ORDER BY work.created_at DESC,work.work_id DESC
					LIMIT 1`,
					head.problemID,
					scope.AgentName,
					scope.JobID,
					scope.StructureVersion,
					progress.InputRevision,
				).Scan(&sourceWorkStatus)
				switch {
				case err == nil:
					switch ProblemSourceReprocessStatus(sourceWorkStatus) {
					case ProblemSourceReprocessPrepared,
						ProblemSourceReprocessQueued,
						ProblemSourceReprocessRunning,
						ProblemSourceReprocessFailed,
						ProblemSourceReprocessOutcomeUnknown:
						progress.Status = "processing"
					case ProblemSourceReprocessNeedsConfirmation:
						progress.Status = "awaiting_source"
					case ProblemSourceReprocessSucceeded,
						ProblemSourceReprocessCancelled:
						// A queue terminal is not a published grading fact. Success
						// must already have produced a current assessment/skip head;
						// cancellation is only valid while the owning Agent is being
						// deleted and therefore must not remain publicly projectable.
						return ProblemSourceProgressiveSnapshot{},
							ErrGradingProgressiveProjectionConflict
					default:
						return ProblemSourceProgressiveSnapshot{},
							ErrGradingProgressiveProjectionConflict
					}
				case errors.Is(err, sql.ErrNoRows):
					// Keep the current immutable Problem confirmation fact when
					// this revision has no source-reprocess work.
				default:
					return ProblemSourceProgressiveSnapshot{}, err
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
	canonicalPayload, err := canonicalProblemSourceActionPayload(
		command.Action, command.Payload,
	)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	command.Payload = canonicalPayload

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
	problemIDs, dependencyGroupID, err := affectedProblemSourceActionMembers(
		ctx,
		tx,
		command,
		scope,
	)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	// The final artifact is an immutable, uniquely-addressed public result. A
	// source-changing command after it exists would leave print/export/delivery
	// pointing at stale evidence, so V72 fails closed until artifact versioning
	// exists as a separate product decision.
	var finalArtifactCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM k12_grading_final_artifacts
		WHERE agent_name=? AND job_id=?`,
		scope.AgentName,
		scope.JobID,
	).Scan(&finalArtifactCount); err != nil {
		return ProblemSourceActionResult{}, err
	}
	if finalArtifactCount != 0 {
		return ProblemSourceActionResult{}, fmt.Errorf(
			"%w: immutable final artifact already exists",
			ErrProblemSourceActionConflict,
		)
	}
	// Advance the same durable generation checked by the final-artifact commit.
	// This runs only after idempotent replay has returned, so one accepted source
	// command consumes exactly one generation. The surrounding transaction
	// rolls the increment back with every downstream source-state failure.
	generationResult, err := tx.ExecContext(ctx, `
		UPDATE k12_grading_jobs
		SET finalization_generation=finalization_generation+1
		WHERE agent_name=? AND record_id=?`,
		scope.AgentName,
		scope.JobID,
	)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	generationRows, err := generationResult.RowsAffected()
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	if generationRows != 1 {
		return ProblemSourceActionResult{}, fmt.Errorf(
			"%w: grading finalization aggregate is missing",
			ErrProblemSourceActionConflict,
		)
	}

	now := nowUnix()
	resultRevision := currentRevision
	if command.Action == "correct_text" || command.Action == "select_region" ||
		command.Action == "retake" || command.Action == "resume" {
		resultRevision++
	}
	commandReceiptID := idgen.NanoID()
	affectedJSON, err := json.Marshal(problemIDs)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	requestJSON, err := json.Marshal(struct {
		Action                string          `json:"action"`
		StructureVersion      int             `json:"structure_version"`
		ExpectedInputRevision int             `json:"expected_input_revision"`
		Payload               json.RawMessage `json:"payload"`
	}{
		Action:                command.Action,
		StructureVersion:      command.StructureVersion,
		ExpectedInputRevision: command.ExpectedInputRevision,
		Payload:               command.Payload,
	})
	if err != nil {
		return ProblemSourceActionResult{}, err
	}

	// The provisional receipt is inserted before its immutable input revisions
	// and queued work so every durable row can reference one command identity.
	// Nothing is externally visible until this transaction commits.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO k12_problem_source_action_receipts (
			command_receipt_id,owner_scope,agent_name,dispatch_id,job_id,problem_id,
			idempotency_key,request_digest,action,structure_version,
			expected_input_revision,result_input_revision,request_json,
			affected_problem_ids_json,response_json,created_at,updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
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
		string(requestJSON),
		string(affectedJSON),
		`{}`,
		now,
		now,
	)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}

	switch command.Action {
	case "correct_text", "select_region", "retake":
		err = commitProblemInputChange(
			ctx,
			tx,
			command,
			scope,
			problemIDs,
			dependencyGroupID,
			commandReceiptID,
			requestDigest,
			resultRevision,
			now,
		)
	case "skip":
		err = commitProblemSkip(
			ctx,
			tx,
			scope,
			problemIDs,
			command.ExpectedInputRevision,
			requestDigest,
			now,
		)
	case "resume":
		if err = commitProblemResume(
			ctx,
			tx,
			scope,
			problemIDs,
			command.ExpectedInputRevision,
			now,
		); err == nil {
			err = commitProblemInputChange(
				ctx,
				tx,
				command,
				scope,
				problemIDs,
				dependencyGroupID,
				commandReceiptID,
				requestDigest,
				resultRevision,
				now,
			)
		}
	}
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	if command.Action != "skip" {
		workDigest := problemSourceInputDigest(
			requestDigest,
			strings.Join(problemIDs, "\x00"),
			resultRevision,
		)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO k12_problem_source_reprocess_jobs (
				work_id,command_receipt_id,owner_scope,agent_name,dispatch_id,
				job_id,problem_id,action,structure_version,input_revision,input_digest,
				affected_problem_ids_json,request_json,status,
				created_at,updated_at
			) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,'queued',?,?)`,
			idgen.NanoID(),
			commandReceiptID,
			command.OwnerScope,
			scope.AgentName,
			command.DispatchID,
			scope.JobID,
			command.ProblemID,
			command.Action,
			scope.StructureVersion,
			resultRevision,
			workDigest,
			string(affectedJSON),
			string(requestJSON),
			now,
			now,
		); err != nil {
			return ProblemSourceActionResult{}, err
		}
	}
	snapshot, err := buildProblemSourceProgressiveSnapshot(ctx, tx, scope)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	result, err := viewcontract.FreezeProblemSourceActionResponse(
		viewcontract.ProblemSourceActionResponse{
			CommandReceiptID:    commandReceiptID,
			DispatchID:          command.DispatchID,
			ProblemID:           command.ProblemID,
			Action:              command.Action,
			StructureVersion:    scope.StructureVersion,
			InputRevision:       resultRevision,
			ProgressiveSnapshot: snapshot,
		},
	)
	if err != nil {
		return ProblemSourceActionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_source_action_receipts
		SET response_json=?,updated_at=?
		WHERE command_receipt_id=?`,
		string(result.JSON),
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
