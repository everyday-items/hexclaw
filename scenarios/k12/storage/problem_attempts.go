package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ErrProblemAttemptConflict means a retry tried to rewrite an immutable OCR fact,
// reuse an identity in another submission, or move a canonical version backwards.
var ErrProblemAttemptConflict = errors.New("problem/attempt immutable fact conflict")

const problemColumns = `problem_id,agent_name,submission_id,page_asset_id,ordinal,
    problem_kind,parent_problem_id,subproblem_no,source_number_path_json,display_label,
    subject,stem_raw,stem_markdown,
    concept_ids_json,transcription_confidence,confirmation_required,
    confirmation_reasons_json,canonical_version,created_at,updated_at`

const attemptColumns = `attempt_id,agent_name,submission_id,problem_id,answer_state,
    answer_raw,answer_markdown,confirmed_version,input_digest,bbox_json,created_at,updated_at`

func normalizeProblem(problem k12.Problem, at int64) (k12.Problem, error) {
	problem.ProblemID = strings.TrimSpace(problem.ProblemID)
	problem.AgentName = strings.TrimSpace(problem.AgentName)
	problem.SubmissionID = strings.TrimSpace(problem.SubmissionID)
	problem.PageAssetID = strings.TrimSpace(problem.PageAssetID)
	problem.ProblemKind = strings.TrimSpace(problem.ProblemKind)
	problem.ParentProblemID = strings.TrimSpace(problem.ParentProblemID)
	problem.SubproblemNo = strings.TrimSpace(problem.SubproblemNo)
	problem.SourceNumberPath = append([]string(nil), problem.SourceNumberPath...)
	for i := range problem.SourceNumberPath {
		problem.SourceNumberPath[i] = strings.TrimSpace(problem.SourceNumberPath[i])
		if problem.SourceNumberPath[i] == "" {
			return k12.Problem{}, fmt.Errorf("k12storage: source_number_path 含空 token")
		}
	}
	problem.DisplayLabel = strings.TrimSpace(problem.DisplayLabel)
	if (len(problem.SourceNumberPath) == 0) != (problem.DisplayLabel == "") {
		return k12.Problem{}, fmt.Errorf("k12storage: source_number_path/display_label 必须同时存在或同时为空")
	}
	problem.Subject = strings.TrimSpace(problem.Subject)
	problem.StemMarkdown = strings.TrimSpace(problem.StemMarkdown)
	if problem.ProblemID == "" || problem.AgentName == "" || problem.SubmissionID == "" ||
		problem.PageAssetID == "" || problem.StemRaw == "" || problem.StemMarkdown == "" ||
		problem.Ordinal < 0 || problem.CanonicalVersion < 1 {
		return k12.Problem{}, fmt.Errorf("k12storage: Problem 缺少 id/owner/submission/page/stem/version/ordinal")
	}
	switch problem.ProblemKind {
	case k12.ProblemKindStandalone, k12.ProblemKindCompoundParent:
		if problem.ParentProblemID != "" || problem.SubproblemNo != "" {
			return k12.Problem{}, fmt.Errorf("k12storage: %s Problem 不可拥有 parent/subproblem_no", problem.ProblemKind)
		}
	case k12.ProblemKindSubproblem:
		if problem.ParentProblemID == "" || problem.SubproblemNo == "" {
			return k12.Problem{}, fmt.Errorf("k12storage: subproblem 缺少 parent/subproblem_no")
		}
	default:
		return k12.Problem{}, fmt.Errorf("k12storage: 未知 problem_kind %q", problem.ProblemKind)
	}
	if problem.TranscriptionConfidence != nil &&
		(*problem.TranscriptionConfidence < 0 || *problem.TranscriptionConfidence > 1) {
		return k12.Problem{}, fmt.Errorf("k12storage: transcription_confidence 必须位于 0..1")
	}
	if problem.CreatedAt <= 0 {
		problem.CreatedAt = at
	}
	if problem.UpdatedAt <= 0 {
		problem.UpdatedAt = problem.CreatedAt
	}
	problem.ConceptIDs = append([]string(nil), problem.ConceptIDs...)
	problem.ConfirmationReasons = append([]string(nil), problem.ConfirmationReasons...)
	return problem, nil
}

func normalizeAttempt(attempt k12.Attempt, at int64) (k12.Attempt, error) {
	attempt.AttemptID = strings.TrimSpace(attempt.AttemptID)
	attempt.AgentName = strings.TrimSpace(attempt.AgentName)
	attempt.SubmissionID = strings.TrimSpace(attempt.SubmissionID)
	attempt.ProblemID = strings.TrimSpace(attempt.ProblemID)
	attempt.AnswerState = strings.TrimSpace(attempt.AnswerState)
	attempt.AnswerMarkdown = strings.TrimSpace(attempt.AnswerMarkdown)
	attempt.InputDigest = strings.TrimSpace(attempt.InputDigest)
	if attempt.AttemptID == "" || attempt.AgentName == "" || attempt.SubmissionID == "" ||
		attempt.ProblemID == "" || attempt.ConfirmedVersion < 0 {
		return k12.Attempt{}, fmt.Errorf("k12storage: Attempt 缺少 id/owner/submission/problem 或版本非法")
	}
	switch attempt.AnswerState {
	case "blank":
		if attempt.AnswerMarkdown != "" {
			return k12.Attempt{}, fmt.Errorf("k12storage: blank Attempt 不可携带 canonical answer")
		}
	case "present":
		if attempt.AnswerMarkdown == "" {
			return k12.Attempt{}, fmt.Errorf("k12storage: present Attempt 缺少 canonical answer")
		}
	case "unclear":
		if attempt.AnswerMarkdown != "" {
			return k12.Attempt{}, fmt.Errorf("k12storage: unclear Attempt 不可伪造 canonical answer")
		}
	default:
		return k12.Attempt{}, fmt.Errorf("k12storage: 未知 answer_state %q", attempt.AnswerState)
	}
	if (attempt.ConfirmedVersion == 0) != (attempt.InputDigest == "") {
		return k12.Attempt{}, fmt.Errorf("k12storage: confirmed_version 与 input_digest 不一致")
	}
	if attempt.BBox != nil && !validAttemptBBox(*attempt.BBox) {
		return k12.Attempt{}, fmt.Errorf("k12storage: Attempt bbox 必须是合法归一化坐标")
	}
	if attempt.CreatedAt <= 0 {
		attempt.CreatedAt = at
	}
	if attempt.UpdatedAt <= 0 {
		attempt.UpdatedAt = attempt.CreatedAt
	}
	return attempt, nil
}

func validAttemptBBox(box k12.AttemptBBox) bool {
	return box.X >= 0 && box.Y >= 0 && box.W > 0 && box.H > 0 &&
		box.X <= 1 && box.Y <= 1 && box.W <= 1 && box.H <= 1 &&
		box.X+box.W <= 1.0000001 && box.Y+box.H <= 1.0000001
}

func normalizeProblemAttemptSnapshot(snapshot k12.ProblemAttemptSnapshot, at int64) (k12.ProblemAttemptSnapshot, error) {
	if len(snapshot.Problems) == 0 {
		return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: ProblemAttemptSnapshot 不可为空")
	}
	out := k12.ProblemAttemptSnapshot{
		Problems: make([]k12.Problem, len(snapshot.Problems)),
		Attempts: make([]k12.Attempt, len(snapshot.Attempts)),
	}
	problems := make(map[string]k12.Problem, len(snapshot.Problems))
	ordinals := make(map[int]struct{}, len(snapshot.Problems))
	answerable := make(map[string]struct{}, len(snapshot.Problems))
	var owner, submission string
	for i, input := range snapshot.Problems {
		problem, err := normalizeProblem(input, at)
		if err != nil {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("problem[%d]: %w", i, err)
		}
		if i == 0 {
			owner, submission = problem.AgentName, problem.SubmissionID
		}
		if problem.AgentName != owner || problem.SubmissionID != submission {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: ProblemAttemptSnapshot 跨 owner/submission")
		}
		if _, exists := problems[problem.ProblemID]; exists {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: duplicate problem_id %q", problem.ProblemID)
		}
		if _, exists := ordinals[problem.Ordinal]; exists {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: duplicate Problem ordinal %d", problem.Ordinal)
		}
		problems[problem.ProblemID] = problem
		ordinals[problem.Ordinal] = struct{}{}
		if problem.ProblemKind != k12.ProblemKindCompoundParent {
			answerable[problem.ProblemID] = struct{}{}
		}
		out.Problems[i] = problem
	}
	childNos := make(map[string]map[string]struct{})
	for _, problem := range out.Problems {
		if problem.ProblemKind != k12.ProblemKindSubproblem {
			continue
		}
		parent, ok := problems[problem.ParentProblemID]
		if !ok || parent.ProblemKind != k12.ProblemKindCompoundParent || parent.PageAssetID != problem.PageAssetID {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: subproblem %q 的 parent/page 非法", problem.ProblemID)
		}
		if childNos[parent.ProblemID] == nil {
			childNos[parent.ProblemID] = map[string]struct{}{}
		}
		if _, exists := childNos[parent.ProblemID][problem.SubproblemNo]; exists {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: parent %q 下 subproblem_no %q 重复", parent.ProblemID, problem.SubproblemNo)
		}
		childNos[parent.ProblemID][problem.SubproblemNo] = struct{}{}
	}
	attemptIDs := make(map[string]struct{}, len(snapshot.Attempts))
	attemptByProblem := make(map[string]struct{}, len(snapshot.Attempts))
	for i, input := range snapshot.Attempts {
		attempt, err := normalizeAttempt(input, at)
		if err != nil {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("attempt[%d]: %w", i, err)
		}
		if attempt.AgentName != owner || attempt.SubmissionID != submission {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: ProblemAttemptSnapshot Attempt 跨 owner/submission")
		}
		if _, ok := answerable[attempt.ProblemID]; !ok {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: Attempt %q 不属于可作答 Problem", attempt.AttemptID)
		}
		if _, exists := attemptIDs[attempt.AttemptID]; exists {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: duplicate attempt_id %q", attempt.AttemptID)
		}
		if _, exists := attemptByProblem[attempt.ProblemID]; exists {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: Problem %q 拥有多个 Attempt", attempt.ProblemID)
		}
		attemptIDs[attempt.AttemptID] = struct{}{}
		attemptByProblem[attempt.ProblemID] = struct{}{}
		out.Attempts[i] = attempt
	}
	for problemID := range answerable {
		if _, ok := attemptByProblem[problemID]; !ok {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: 可作答 Problem %q 缺少 Attempt", problemID)
		}
	}
	return out, nil
}

// ValidateProblemAttemptArchive validates a complete, already-durable owner-scoped
// Problem/Attempt ledger without silently normalizing signed archive facts. Stable
// problem/attempt IDs are unique inside one Tutor even when submissions differ.
func ValidateProblemAttemptArchive(agentName string, snapshots []k12.ProblemAttemptSnapshot) error {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return fmt.Errorf("k12storage: Problem/Attempt archive owner 不可空")
	}
	submissions := make(map[string]struct{}, len(snapshots))
	problemIDs := make(map[string]struct{})
	attemptIDs := make(map[string]struct{})
	for i, snapshot := range snapshots {
		normalized, err := normalizeProblemAttemptSnapshot(snapshot, 0)
		if err != nil {
			return fmt.Errorf("problem_attempts[%d]: %w", i, err)
		}
		if !reflect.DeepEqual(normalized, snapshot) {
			return fmt.Errorf("k12storage: problem_attempts[%d] 含未规范化或非持久化字段", i)
		}
		submissionID := snapshot.Problems[0].SubmissionID
		if snapshot.Problems[0].AgentName != agentName {
			return fmt.Errorf("k12storage: problem_attempts[%d] owner %q 不属于 %q", i, snapshot.Problems[0].AgentName, agentName)
		}
		if _, duplicate := submissions[submissionID]; duplicate {
			return fmt.Errorf("k12storage: duplicate submission_id %q", submissionID)
		}
		submissions[submissionID] = struct{}{}
		for _, problem := range snapshot.Problems {
			if problem.CreatedAt <= 0 || problem.UpdatedAt <= 0 || problem.UpdatedAt < problem.CreatedAt {
				return fmt.Errorf("k12storage: Problem %q 缺少合法持久化时间", problem.ProblemID)
			}
			if _, duplicate := problemIDs[problem.ProblemID]; duplicate {
				return fmt.Errorf("k12storage: duplicate archive problem_id %q", problem.ProblemID)
			}
			problemIDs[problem.ProblemID] = struct{}{}
		}
		for _, attempt := range snapshot.Attempts {
			if attempt.CreatedAt <= 0 || attempt.UpdatedAt <= 0 || attempt.UpdatedAt < attempt.CreatedAt {
				return fmt.Errorf("k12storage: Attempt %q 缺少合法持久化时间", attempt.AttemptID)
			}
			if _, duplicate := attemptIDs[attempt.AttemptID]; duplicate {
				return fmt.Errorf("k12storage: duplicate archive attempt_id %q", attempt.AttemptID)
			}
			attemptIDs[attempt.AttemptID] = struct{}{}
		}
	}
	return nil
}

// PutProblemAttemptSnapshot atomically writes one submission's typed OCR facts.
// Raw transcription and structural identity are immutable; canonical and confirmation
// versions may only advance. A compound parent never owns an Attempt.
func (s *Store) PutProblemAttemptSnapshot(ctx context.Context, snapshot k12.ProblemAttemptSnapshot) error {
	normalized, err := normalizeProblemAttemptSnapshot(snapshot, nowUnix())
	if err != nil {
		return err
	}
	owner := normalized.Problems[0].AgentName
	if err := ensureAgentRegistered(ctx, s.db, owner); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("k12storage: 开启 Problem/Attempt 事务: %w", err)
	}
	defer tx.Rollback()

	// Parents are inserted first so the self-referencing FK is valid even when the
	// recognizer returned children before their shared stem.
	for _, kind := range []string{k12.ProblemKindCompoundParent, k12.ProblemKindStandalone, k12.ProblemKindSubproblem} {
		for _, problem := range normalized.Problems {
			if problem.ProblemKind != kind {
				continue
			}
			if err := putProblemTx(ctx, tx, problem); err != nil {
				return err
			}
		}
	}
	for _, attempt := range normalized.Attempts {
		if err := putAttemptTx(ctx, tx, attempt); err != nil {
			return err
		}
	}
	if err := advanceProblemStructureSnapshotTx(
		ctx,
		tx,
		normalized,
		nowUnix(),
	); err != nil {
		return fmt.Errorf("k12storage: freeze Problem structure snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("k12storage: 提交 Problem/Attempt 事务: %w", err)
	}
	return nil
}

// ExportProblemAttemptSnapshots exports the complete V19 canonical ledger for one
// Tutor. Ordering is deterministic so the same database state seals to the same
// .hexbak semantic payload.
func (s *Store) ExportProblemAttemptSnapshots(ctx context.Context, agentName string) ([]k12.ProblemAttemptSnapshot, error) {
	return s.exportProblemAttemptSnapshotsVia(ctx, s.db, agentName)
}

// ExportProblemAttemptSnapshotsTx exports inside the caller's transaction so a
// restore-as pre-snapshot covers records, Problem/Attempt and profile atomically.
func (s *Store) ExportProblemAttemptSnapshotsTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
) ([]k12.ProblemAttemptSnapshot, error) {
	if tx == nil {
		return nil, fmt.Errorf("k12storage: nil Problem/Attempt export transaction")
	}
	return s.exportProblemAttemptSnapshotsVia(ctx, tx, agentName)
}

func (s *Store) exportProblemAttemptSnapshotsVia(
	ctx context.Context,
	q dbQueryer,
	agentName string,
) ([]k12.ProblemAttemptSnapshot, error) {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return nil, fmt.Errorf("k12storage: Problem/Attempt export owner 不可空")
	}
	rows, err := q.QueryContext(ctx, `SELECT `+problemColumns+` FROM k12_problems
        WHERE agent_name=? ORDER BY submission_id,ordinal,problem_id`, agentName)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 导出 Problem 列表: %w", err)
	}
	bySubmission := make(map[string]int)
	out := make([]k12.ProblemAttemptSnapshot, 0)
	for rows.Next() {
		problem, scanErr := scanProblem(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("k12storage: 扫描归档 Problem: %w", scanErr)
		}
		index, ok := bySubmission[problem.SubmissionID]
		if !ok {
			index = len(out)
			bySubmission[problem.SubmissionID] = index
			out = append(out, k12.ProblemAttemptSnapshot{
				Problems: make([]k12.Problem, 0), Attempts: make([]k12.Attempt, 0),
			})
		}
		out[index].Problems = append(out[index].Problems, problem)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("k12storage: 遍历归档 Problem: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("k12storage: 关闭归档 Problem 游标: %w", err)
	}

	attemptRows, err := q.QueryContext(ctx, `SELECT `+attemptColumns+` FROM k12_attempts
        WHERE agent_name=? ORDER BY submission_id,problem_id,attempt_id`, agentName)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 导出 Attempt 列表: %w", err)
	}
	for attemptRows.Next() {
		attempt, scanErr := scanAttempt(attemptRows)
		if scanErr != nil {
			_ = attemptRows.Close()
			return nil, fmt.Errorf("k12storage: 扫描归档 Attempt: %w", scanErr)
		}
		index, ok := bySubmission[attempt.SubmissionID]
		if !ok {
			_ = attemptRows.Close()
			return nil, fmt.Errorf("k12storage: Attempt %q 没有同 submission Problem", attempt.AttemptID)
		}
		out[index].Attempts = append(out[index].Attempts, attempt)
	}
	if err := attemptRows.Err(); err != nil {
		_ = attemptRows.Close()
		return nil, fmt.Errorf("k12storage: 遍历归档 Attempt: %w", err)
	}
	if err := attemptRows.Close(); err != nil {
		return nil, fmt.Errorf("k12storage: 关闭归档 Attempt 游标: %w", err)
	}
	if err := ValidateProblemAttemptArchive(agentName, out); err != nil {
		return nil, fmt.Errorf("k12storage: 数据库 Problem/Attempt ledger 非法: %w", err)
	}
	return out, nil
}

// ImportProblemAttemptSnapshots merges a complete archive batch atomically.
// Same stable IDs and facts are idempotent; immutable fact changes fail closed.
func (s *Store) ImportProblemAttemptSnapshots(
	ctx context.Context,
	agentName string,
	snapshots []k12.ProblemAttemptSnapshot,
) error {
	if err := ValidateProblemAttemptArchive(agentName, snapshots); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("k12storage: 开启 Problem/Attempt 归档恢复事务: %w", err)
	}
	defer tx.Rollback()
	if err := s.ImportProblemAttemptSnapshotsTx(ctx, tx, agentName, snapshots); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("k12storage: 提交 Problem/Attempt 归档恢复事务: %w", err)
	}
	return nil
}

// ImportProblemAttemptSnapshotsTx merges a Problem/Attempt archive inside a
// caller-owned durability boundary.
func (s *Store) ImportProblemAttemptSnapshotsTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	snapshots []k12.ProblemAttemptSnapshot,
) error {
	if tx == nil {
		return fmt.Errorf("k12storage: nil Problem/Attempt merge transaction")
	}
	if err := ValidateProblemAttemptArchive(agentName, snapshots); err != nil {
		return err
	}
	if err := ensureAgentRegistered(ctx, tx, agentName); err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		for _, kind := range []string{k12.ProblemKindCompoundParent, k12.ProblemKindStandalone, k12.ProblemKindSubproblem} {
			for _, problem := range snapshot.Problems {
				if problem.ProblemKind != kind {
					continue
				}
				if err := putProblemTx(ctx, tx, problem); err != nil {
					return err
				}
			}
		}
		for _, attempt := range snapshot.Attempts {
			if err := putAttemptTx(ctx, tx, attempt); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReplaceProblemAttemptSnapshotsTx exact-replaces one Tutor's V19 ledger. It is
// used only by restore-as rollback after the immutable pre-snapshot verifies.
func (s *Store) ReplaceProblemAttemptSnapshotsTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName string,
	snapshots []k12.ProblemAttemptSnapshot,
) error {
	if tx == nil {
		return fmt.Errorf("k12storage: nil Problem/Attempt replace transaction")
	}
	if err := ValidateProblemAttemptArchive(agentName, snapshots); err != nil {
		return err
	}
	if err := ensureAgentRegistered(ctx, tx, agentName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM k12_attempts WHERE agent_name=?`, agentName); err != nil {
		return fmt.Errorf("k12storage: 清理旧 Attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM k12_problems WHERE agent_name=?`, agentName); err != nil {
		return fmt.Errorf("k12storage: 清理旧 Problem: %w", err)
	}
	return s.ImportProblemAttemptSnapshotsTx(ctx, tx, agentName, snapshots)
}

func putProblemTx(ctx context.Context, tx *sql.Tx, problem k12.Problem) error {
	conceptsJSON, _ := json.Marshal(problem.ConceptIDs)
	reasonsJSON, _ := json.Marshal(problem.ConfirmationReasons)
	sourceNumberPathJSON, _ := json.Marshal(problem.SourceNumberPath)
	var confidence any
	if problem.TranscriptionConfidence != nil {
		confidence = *problem.TranscriptionConfidence
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO k12_problems (`+problemColumns+`)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(agent_name,problem_id) DO NOTHING`,
		problem.ProblemID, problem.AgentName, problem.SubmissionID, problem.PageAssetID, problem.Ordinal,
		problem.ProblemKind, nullableString(problem.ParentProblemID), problem.SubproblemNo,
		string(sourceNumberPathJSON), problem.DisplayLabel, problem.Subject,
		problem.StemRaw, problem.StemMarkdown, string(conceptsJSON), confidence, boolInt(problem.ConfirmationRequired),
		string(reasonsJSON), problem.CanonicalVersion, problem.CreatedAt, problem.UpdatedAt)
	if err != nil {
		return fmt.Errorf("k12storage: 写 Problem %s: %w", problem.ProblemID, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	existing, err := scanProblem(tx.QueryRowContext(ctx, `SELECT `+problemColumns+`
        FROM k12_problems WHERE agent_name=? AND problem_id=?`, problem.AgentName, problem.ProblemID))
	if err != nil {
		return fmt.Errorf("k12storage: 回读 Problem %s: %w", problem.ProblemID, err)
	}
	if existing.SubmissionID != problem.SubmissionID || existing.PageAssetID != problem.PageAssetID ||
		existing.Ordinal != problem.Ordinal || existing.ProblemKind != problem.ProblemKind ||
		existing.ParentProblemID != problem.ParentProblemID || existing.SubproblemNo != problem.SubproblemNo ||
		!reflect.DeepEqual(existing.SourceNumberPath, problem.SourceNumberPath) ||
		existing.DisplayLabel != problem.DisplayLabel ||
		existing.StemRaw != problem.StemRaw {
		return fmt.Errorf("%w: Problem %s raw/structure", ErrProblemAttemptConflict, problem.ProblemID)
	}
	if problem.CanonicalVersion < existing.CanonicalVersion {
		return fmt.Errorf("%w: Problem %s canonical version %d < %d", ErrProblemAttemptConflict,
			problem.ProblemID, problem.CanonicalVersion, existing.CanonicalVersion)
	}
	if problem.CanonicalVersion == existing.CanonicalVersion {
		if !problemCanonicalFactsEqual(existing, problem) {
			return fmt.Errorf("%w: Problem %s same canonical version changed facts", ErrProblemAttemptConflict, problem.ProblemID)
		}
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE k12_problems SET subject=?,stem_markdown=?,concept_ids_json=?,
        transcription_confidence=?,confirmation_required=?,confirmation_reasons_json=?,
        canonical_version=?,updated_at=? WHERE agent_name=? AND problem_id=?`,
		problem.Subject, problem.StemMarkdown, string(conceptsJSON), confidence,
		boolInt(problem.ConfirmationRequired), string(reasonsJSON), problem.CanonicalVersion,
		problem.UpdatedAt, problem.AgentName, problem.ProblemID)
	if err != nil {
		return fmt.Errorf("k12storage: 更新 Problem %s canonical: %w", problem.ProblemID, err)
	}
	return nil
}

func problemCanonicalFactsEqual(a, b k12.Problem) bool {
	return a.Subject == b.Subject && a.StemMarkdown == b.StemMarkdown &&
		reflect.DeepEqual(a.ConceptIDs, b.ConceptIDs) &&
		floatPtrEqual(a.TranscriptionConfidence, b.TranscriptionConfidence) &&
		a.ConfirmationRequired == b.ConfirmationRequired &&
		reflect.DeepEqual(a.ConfirmationReasons, b.ConfirmationReasons)
}

func putAttemptTx(ctx context.Context, tx *sql.Tx, attempt k12.Attempt) error {
	bboxJSON := ""
	if attempt.BBox != nil {
		raw, _ := json.Marshal(attempt.BBox)
		bboxJSON = string(raw)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO k12_attempts (`+attemptColumns+`)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(agent_name,attempt_id) DO NOTHING`,
		attempt.AttemptID, attempt.AgentName, attempt.SubmissionID, attempt.ProblemID,
		attempt.AnswerState, attempt.AnswerRaw, attempt.AnswerMarkdown, attempt.ConfirmedVersion,
		attempt.InputDigest, bboxJSON, attempt.CreatedAt, attempt.UpdatedAt)
	if err != nil {
		return fmt.Errorf("k12storage: 写 Attempt %s: %w", attempt.AttemptID, err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	existing, err := scanAttempt(tx.QueryRowContext(ctx, `SELECT `+attemptColumns+`
        FROM k12_attempts WHERE agent_name=? AND attempt_id=?`, attempt.AgentName, attempt.AttemptID))
	if err != nil {
		return fmt.Errorf("k12storage: 回读 Attempt %s: %w", attempt.AttemptID, err)
	}
	if existing.SubmissionID != attempt.SubmissionID || existing.ProblemID != attempt.ProblemID ||
		existing.AnswerRaw != attempt.AnswerRaw {
		return fmt.Errorf("%w: Attempt %s raw/identity", ErrProblemAttemptConflict, attempt.AttemptID)
	}
	if attempt.ConfirmedVersion < existing.ConfirmedVersion {
		return fmt.Errorf("%w: Attempt %s confirmed version %d < %d", ErrProblemAttemptConflict,
			attempt.AttemptID, attempt.ConfirmedVersion, existing.ConfirmedVersion)
	}
	if attempt.ConfirmedVersion == existing.ConfirmedVersion {
		if existing.AnswerState != attempt.AnswerState || existing.AnswerMarkdown != attempt.AnswerMarkdown ||
			existing.InputDigest != attempt.InputDigest {
			return fmt.Errorf("%w: Attempt %s same confirmed version changed facts", ErrProblemAttemptConflict, attempt.AttemptID)
		}
		return enrichAttemptBBoxTx(ctx, tx, existing, attempt)
	}
	if existing.BBox != nil && attempt.BBox == nil {
		bboxJSON = mustJSON(existing.BBox)
	}
	_, err = tx.ExecContext(ctx, `UPDATE k12_attempts SET answer_state=?,answer_markdown=?,
        confirmed_version=?,input_digest=?,bbox_json=?,updated_at=?
        WHERE agent_name=? AND attempt_id=?`, attempt.AnswerState, attempt.AnswerMarkdown,
		attempt.ConfirmedVersion, attempt.InputDigest, bboxJSON, attempt.UpdatedAt,
		attempt.AgentName, attempt.AttemptID)
	if err != nil {
		return fmt.Errorf("k12storage: 更新 Attempt %s canonical: %w", attempt.AttemptID, err)
	}
	return nil
}

func enrichAttemptBBoxTx(ctx context.Context, tx *sql.Tx, existing, incoming k12.Attempt) error {
	if incoming.BBox == nil {
		return nil
	}
	if existing.BBox != nil {
		if *existing.BBox != *incoming.BBox {
			return fmt.Errorf("%w: Attempt %s bbox changed without new confirmation", ErrProblemAttemptConflict, incoming.AttemptID)
		}
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE k12_attempts SET bbox_json=?,updated_at=?
        WHERE agent_name=? AND attempt_id=?`, mustJSON(incoming.BBox), incoming.UpdatedAt,
		incoming.AgentName, incoming.AttemptID)
	if err != nil {
		return fmt.Errorf("k12storage: 补写 Attempt %s bbox: %w", incoming.AttemptID, err)
	}
	return nil
}

// GetProblemAttemptSnapshot reads one submission inside its immutable Tutor scope.
func (s *Store) GetProblemAttemptSnapshot(ctx context.Context, agentName, submissionID string) (k12.ProblemAttemptSnapshot, error) {
	agentName = strings.TrimSpace(agentName)
	submissionID = strings.TrimSpace(submissionID)
	if agentName == "" || submissionID == "" {
		return k12.ProblemAttemptSnapshot{}, records.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+problemColumns+` FROM k12_problems
        WHERE agent_name=? AND submission_id=? ORDER BY ordinal,problem_id`, agentName, submissionID)
	if err != nil {
		return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: 读 Problem 列表: %w", err)
	}
	defer rows.Close()
	result := k12.ProblemAttemptSnapshot{Problems: make([]k12.Problem, 0), Attempts: make([]k12.Attempt, 0)}
	for rows.Next() {
		problem, scanErr := scanProblem(rows)
		if scanErr != nil {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: 扫描 Problem: %w", scanErr)
		}
		result.Problems = append(result.Problems, problem)
	}
	if err := rows.Err(); err != nil {
		return k12.ProblemAttemptSnapshot{}, err
	}
	if len(result.Problems) == 0 {
		return k12.ProblemAttemptSnapshot{}, records.ErrNotFound
	}
	attemptRows, err := s.db.QueryContext(ctx, `SELECT `+attemptColumns+` FROM k12_attempts
        WHERE agent_name=? AND submission_id=? ORDER BY problem_id,attempt_id`, agentName, submissionID)
	if err != nil {
		return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: 读 Attempt 列表: %w", err)
	}
	defer attemptRows.Close()
	for attemptRows.Next() {
		attempt, scanErr := scanAttempt(attemptRows)
		if scanErr != nil {
			return k12.ProblemAttemptSnapshot{}, fmt.Errorf("k12storage: 扫描 Attempt: %w", scanErr)
		}
		result.Attempts = append(result.Attempts, attempt)
	}
	if err := attemptRows.Err(); err != nil {
		return k12.ProblemAttemptSnapshot{}, err
	}
	return result, nil
}

func scanProblem(row rowScanner) (k12.Problem, error) {
	var problem k12.Problem
	var parent sql.NullString
	var sourceNumberPathJSON, conceptsJSON, reasonsJSON string
	var confidence sql.NullFloat64
	var confirmationRequired int
	err := row.Scan(&problem.ProblemID, &problem.AgentName, &problem.SubmissionID,
		&problem.PageAssetID, &problem.Ordinal, &problem.ProblemKind, &parent,
		&problem.SubproblemNo, &sourceNumberPathJSON, &problem.DisplayLabel,
		&problem.Subject, &problem.StemRaw, &problem.StemMarkdown,
		&conceptsJSON, &confidence, &confirmationRequired, &reasonsJSON,
		&problem.CanonicalVersion, &problem.CreatedAt, &problem.UpdatedAt)
	if err != nil {
		return k12.Problem{}, err
	}
	problem.ParentProblemID = parent.String
	if err := json.Unmarshal([]byte(sourceNumberPathJSON), &problem.SourceNumberPath); err != nil {
		return k12.Problem{}, fmt.Errorf("decode source_number_path_json: %w", err)
	}
	problem.ConfirmationRequired = confirmationRequired != 0
	if confidence.Valid {
		value := confidence.Float64
		problem.TranscriptionConfidence = &value
	}
	if err := json.Unmarshal([]byte(conceptsJSON), &problem.ConceptIDs); err != nil {
		return k12.Problem{}, fmt.Errorf("decode concept_ids_json: %w", err)
	}
	if err := json.Unmarshal([]byte(reasonsJSON), &problem.ConfirmationReasons); err != nil {
		return k12.Problem{}, fmt.Errorf("decode confirmation_reasons_json: %w", err)
	}
	return problem, nil
}

func scanAttempt(row rowScanner) (k12.Attempt, error) {
	var attempt k12.Attempt
	var bboxJSON string
	err := row.Scan(&attempt.AttemptID, &attempt.AgentName, &attempt.SubmissionID,
		&attempt.ProblemID, &attempt.AnswerState, &attempt.AnswerRaw,
		&attempt.AnswerMarkdown, &attempt.ConfirmedVersion, &attempt.InputDigest,
		&bboxJSON, &attempt.CreatedAt, &attempt.UpdatedAt)
	if err != nil {
		return k12.Attempt{}, err
	}
	if bboxJSON != "" {
		var box k12.AttemptBBox
		if err := json.Unmarshal([]byte(bboxJSON), &box); err != nil {
			return k12.Attempt{}, fmt.Errorf("decode bbox_json: %w", err)
		}
		attempt.BBox = &box
	}
	return attempt, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
