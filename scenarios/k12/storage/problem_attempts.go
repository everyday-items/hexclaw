package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ErrProblemAttemptConflict means a retry tried to rewrite an immutable OCR fact,
// reuse an identity in another submission, or move a canonical version backwards.
var ErrProblemAttemptConflict = errors.New("problem/attempt immutable fact conflict")

const problemColumns = `problem_id,agent_name,submission_id,page_asset_id,ordinal,
    problem_kind,parent_problem_id,subproblem_no,source_number_path_json,display_label,
    source_section_path_json,source_section_label,system_section_ordinal,system_display_label,
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
	problem.SourceSectionPath = append([]string(nil), problem.SourceSectionPath...)
	for i := range problem.SourceSectionPath {
		problem.SourceSectionPath[i] = strings.TrimSpace(problem.SourceSectionPath[i])
		if problem.SourceSectionPath[i] == "" {
			return k12.Problem{}, fmt.Errorf("k12storage: source_section_path 含空 token")
		}
	}
	problem.SourceSectionLabel = strings.TrimSpace(problem.SourceSectionLabel)
	if (len(problem.SourceSectionPath) == 0) != (problem.SourceSectionLabel == "") {
		return k12.Problem{}, fmt.Errorf("k12storage: source_section_path/source_section_label 必须同时存在或同时为空")
	}
	problem.SystemDisplayLabel = strings.TrimSpace(problem.SystemDisplayLabel)
	if problem.SystemSectionOrdinal < 0 {
		return k12.Problem{}, fmt.Errorf("k12storage: system_section_ordinal 不可为负数")
	}
	if problem.SystemSectionOrdinal == 0 {
		if problem.SystemDisplayLabel != "" {
			return k12.Problem{}, fmt.Errorf("k12storage: 无 system_section_ordinal 不可携带 system_display_label")
		}
	} else {
		if len(problem.SourceNumberPath) != 0 || len(problem.SourceSectionPath) == 0 ||
			problem.SystemDisplayLabel != fmt.Sprintf("第 %d 题（系统序号）", problem.SystemSectionOrdinal) {
			return k12.Problem{}, fmt.Errorf("k12storage: system order 必须是无原卷题号且有来源分区的显式服务端标签")
		}
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

// validateDurableSystemSectionOrder keeps imported/restored snapshots subject to
// the same server-owned derivation as live recognition.  A system ordinal is not
// a caller-owned label: it is exactly the source-order position among unnumbered
// answerable items in one visible source section.
func validateDurableSystemSectionOrder(problems []k12.Problem) error {
	ordered := append([]k12.Problem(nil), problems...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Ordinal != ordered[j].Ordinal {
			return ordered[i].Ordinal < ordered[j].Ordinal
		}
		return ordered[i].ProblemID < ordered[j].ProblemID
	})
	sectionLabels := make(map[string]string)
	sectionCounts := make(map[string]int)
	for _, problem := range ordered {
		sectionKey := strings.Join(problem.SourceSectionPath, "\x00")
		if sectionKey != "" {
			if prior, exists := sectionLabels[sectionKey]; exists && prior != problem.SourceSectionLabel {
				return fmt.Errorf("k12storage: 同一 source_section_path 不可对应不同 source_section_label")
			}
			sectionLabels[sectionKey] = problem.SourceSectionLabel
		}
		needsSystemOrder := problem.ProblemKind != k12.ProblemKindCompoundParent &&
			len(problem.SourceSectionPath) != 0 && len(problem.SourceNumberPath) == 0
		if !needsSystemOrder {
			if problem.SystemSectionOrdinal != 0 || problem.SystemDisplayLabel != "" {
				return fmt.Errorf("k12storage: 非无印刷题号的可作答题不可携带 system order")
			}
			continue
		}
		expected := sectionCounts[sectionKey] + 1
		if problem.SystemSectionOrdinal != expected ||
			problem.SystemDisplayLabel != fmt.Sprintf("第 %d 题（系统序号）", expected) {
			return fmt.Errorf("k12storage: system order 必须按来源分区和原卷顺序由服务端精确派生")
		}
		sectionCounts[sectionKey] = expected
	}
	return nil
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
	if err := validateDurableSystemSectionOrder(out.Problems); err != nil {
		return k12.ProblemAttemptSnapshot{}, err
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
	if err := alignProblemAttemptReplayWithCurrentLegacyHeadsTx(
		ctx,
		tx,
		&normalized,
	); err != nil {
		return fmt.Errorf("k12storage: align Problem/Attempt structure replay: %w", err)
	}

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
	if err := syncProblemInputRevisionHeadsTx(ctx, tx, normalized, nowUnix()); err != nil {
		return fmt.Errorf("k12storage: freeze immutable Problem input heads: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("k12storage: 提交 Problem/Attempt 事务: %w", err)
	}
	return nil
}

// alignProblemAttemptReplayWithCurrentLegacyHeadsTx recognizes only the
// server-created revision barrier produced when an otherwise stable Problem is
// carried into a new authoritative structure version. The caller may replay
// the original recognition snapshot, whose Attempt still names the pre-barrier
// revision; accepting that exact replay is safe only while the current V72 head
// is legacy_unverified and every immutable/canonical fact still matches.
// Command-origin heads are deliberately excluded so stale OCR snapshots can
// never overwrite a parent correction, crop, retake, or resume.
func alignProblemAttemptReplayWithCurrentLegacyHeadsTx(
	ctx context.Context,
	tx *sql.Tx,
	snapshot *k12.ProblemAttemptSnapshot,
) error {
	if snapshot == nil || len(snapshot.Problems) == 0 {
		return nil
	}
	agentName := snapshot.Problems[0].AgentName
	submissionID := snapshot.Problems[0].SubmissionID
	_, digest, err := problemStructureFacts(*snapshot)
	if err != nil {
		return err
	}
	current, err := getCurrentProblemStructureTx(
		ctx,
		tx,
		agentName,
		submissionID,
	)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current.Digest != digest) {
		return nil
	}
	if err != nil {
		return err
	}
	problems := make(map[string]k12.Problem, len(snapshot.Problems))
	for _, problem := range snapshot.Problems {
		problems[problem.ProblemID] = problem
	}
	for index := range snapshot.Attempts {
		attempt := &snapshot.Attempts[index]
		member, ok := current.Members[attempt.ProblemID]
		if !ok || attempt.ConfirmedVersion < 1 ||
			attempt.ConfirmedVersion >= member.InputRevision {
			continue
		}
		problem, ok := problems[attempt.ProblemID]
		if !ok {
			return fmt.Errorf("problem %s is missing for replayed attempt", attempt.ProblemID)
		}
		bboxJSON := ""
		if attempt.BBox != nil {
			bboxJSON = mustJSON(attempt.BBox)
		}
		var pageAssetID, sourceRegionJSON, stemRaw, answerRaw string
		var answerBBoxJSON, questionMarkdown, answerMarkdown string
		var inputDigest, originKind string
		var originReceipt sql.NullString
		err := tx.QueryRowContext(ctx, `
			SELECT page_asset_id,COALESCE(source_region_json,''),stem_raw,answer_raw,
			       answer_bbox_json,question_canonical_markdown,
			       answer_canonical_markdown,input_digest,
			       origin_command_receipt_id,origin_kind
			FROM k12_problem_input_revisions
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND problem_id=? AND input_revision=?
			  AND current_disposition='current'`,
			agentName,
			submissionID,
			current.Version,
			attempt.ProblemID,
			member.InputRevision,
		).Scan(
			&pageAssetID,
			&sourceRegionJSON,
			&stemRaw,
			&answerRaw,
			&answerBBoxJSON,
			&questionMarkdown,
			&answerMarkdown,
			&inputDigest,
			&originReceipt,
			&originKind,
		)
		if errors.Is(err, sql.ErrNoRows) {
			// Older V72 writers may have left precisely this missing-head shape.
			// syncProblemInputRevisionHeadsTx repairs it later in this transaction.
			continue
		}
		if err != nil {
			return err
		}
		if originKind != "legacy_unverified" || originReceipt.Valid ||
			sourceRegionJSON != "" || pageAssetID != problem.PageAssetID ||
			stemRaw != problem.StemRaw || answerRaw != attempt.AnswerRaw ||
			answerBBoxJSON != bboxJSON || questionMarkdown != problem.StemMarkdown ||
			answerMarkdown != attempt.AnswerMarkdown || inputDigest != attempt.InputDigest {
			// Leave the incoming lower version untouched. putAttemptTx will reject
			// it as an ordinary immutable-fact regression.
			continue
		}
		attempt.ConfirmedVersion = member.InputRevision
	}
	return nil
}

type legacyProblemInputHead struct {
	pageAssetID      string
	sourceRegionJSON sql.NullString
	stemRaw          string
	answerRaw        string
	inputDigest      string
	disposition      string
	originReceipt    sql.NullString
	originKind       string
}

// appendConfirmedProblemInputAfterLegacyTx converges a pre-fix synthetic head
// without rewriting it. The synthetic row remains immutable audit evidence and
// is superseded by a new server-owned revision that exactly binds V19 Attempt
// and V51 member state. Existing durable decisions make that convergence
// ambiguous, so they fail closed rather than being silently invalidated.
func appendConfirmedProblemInputAfterLegacyTx(
	ctx context.Context,
	tx *sql.Tx,
	agentName, submissionID string,
	structureVersion, legacyRevision int,
	problem k12.Problem,
	attempt k12.Attempt,
	legacy legacyProblemInputHead,
	bboxJSON string,
	now int64,
) error {
	durableRevision, err := currentDurableProblemInputRevisionTx(
		ctx,
		tx,
		agentName,
		problem.ProblemID,
	)
	if err != nil {
		return err
	}
	if durableRevision >= legacyRevision {
		return fmt.Errorf(
			"%w: legacy input revision %s/v%d already has durable decision v%d",
			ErrProblemAttemptConflict,
			problem.ProblemID,
			legacyRevision,
			durableRevision,
		)
	}

	nextRevision := legacyRevision + 1
	result, err := tx.ExecContext(ctx, `
		UPDATE k12_problem_input_revisions
		SET current_disposition='superseded',updated_at=MAX(updated_at,?)
		WHERE agent_name=? AND submission_id=? AND structure_version=?
		  AND problem_id=? AND input_revision=?
		  AND current_disposition='current'
		  AND input_digest=? AND origin_kind='legacy_unverified'
		  AND origin_command_receipt_id IS NULL AND source_region_json IS NULL`,
		now,
		agentName,
		submissionID,
		structureVersion,
		problem.ProblemID,
		legacyRevision,
		legacy.inputDigest,
	)
	if err != nil {
		return err
	}
	if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if rows != 1 {
		return fmt.Errorf(
			"%w: legacy input revision %s/v%d CAS lost",
			ErrProblemAttemptConflict,
			problem.ProblemID,
			legacyRevision,
		)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO k12_problem_input_revisions (
			agent_name,submission_id,structure_version,problem_id,input_revision,
			page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
			question_canonical_markdown,answer_canonical_markdown,input_digest,
			current_disposition,origin_command_receipt_id,origin_kind,
			created_at,updated_at
		) VALUES (?,?,?,?,?,?,NULL,?,?,?,?,?,?,'current',NULL,'legacy_unverified',?,?)`,
		agentName,
		submissionID,
		structureVersion,
		problem.ProblemID,
		nextRevision,
		problem.PageAssetID,
		problem.StemRaw,
		attempt.AnswerRaw,
		bboxJSON,
		problem.StemMarkdown,
		attempt.AnswerMarkdown,
		attempt.InputDigest,
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
		nextRevision,
		agentName,
		submissionID,
		structureVersion,
		problem.ProblemID,
		legacyRevision,
	)
	if err != nil {
		return err
	}
	if rows, rowsErr := memberResult.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if rows != 1 {
		return fmt.Errorf(
			"%w: structure input revision %s/v%d CAS lost",
			ErrProblemAttemptConflict,
			problem.ProblemID,
			legacyRevision,
		)
	}

	attemptResult, err := tx.ExecContext(ctx, `
		UPDATE k12_attempts
		SET confirmed_version=?,updated_at=MAX(updated_at,?)
		WHERE agent_name=? AND submission_id=? AND attempt_id=? AND problem_id=?
		  AND confirmed_version=? AND input_digest=?`,
		nextRevision,
		now,
		agentName,
		submissionID,
		attempt.AttemptID,
		attempt.ProblemID,
		attempt.ConfirmedVersion,
		attempt.InputDigest,
	)
	if err != nil {
		return err
	}
	if rows, rowsErr := attemptResult.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if rows != 1 {
		return fmt.Errorf(
			"%w: Attempt %s legacy revision barrier CAS lost",
			ErrProblemAttemptConflict,
			attempt.AttemptID,
		)
	}
	return nil
}

// syncProblemInputRevisionHeadsTx keeps V72's append-only confirmed-input
// ledger in the same transaction as the canonical Problem/Attempt and
// structure snapshot. A v0 Attempt is recognition output awaiting confirmation,
// not immutable confirmed evidence, so it deliberately has no V72 head.
func syncProblemInputRevisionHeadsTx(
	ctx context.Context,
	tx *sql.Tx,
	snapshot k12.ProblemAttemptSnapshot,
	now int64,
) error {
	if len(snapshot.Problems) == 0 {
		return nil
	}
	agentName := snapshot.Problems[0].AgentName
	submissionID := snapshot.Problems[0].SubmissionID
	var structureVersion int
	if err := tx.QueryRowContext(ctx, `
		SELECT structure_version
		FROM k12_problem_structure_snapshots
		WHERE agent_name=? AND submission_id=? AND current_disposition='current'
		ORDER BY structure_version DESC LIMIT 1`,
		agentName,
		submissionID,
	).Scan(&structureVersion); err != nil {
		return err
	}
	problems := make(map[string]k12.Problem, len(snapshot.Problems))
	for _, problem := range snapshot.Problems {
		problems[problem.ProblemID] = problem
	}
	for _, attempt := range snapshot.Attempts {
		problem, ok := problems[attempt.ProblemID]
		if !ok {
			return fmt.Errorf("problem %s missing for attempt %s", attempt.ProblemID, attempt.AttemptID)
		}
		if attempt.ConfirmedVersion < 1 {
			continue
		}
		inputRevision := attempt.ConfirmedVersion
		if err := tx.QueryRowContext(ctx, `
			SELECT input_revision
			FROM k12_problem_structure_members
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND problem_id=?`,
			agentName,
			submissionID,
			structureVersion,
			attempt.ProblemID,
		).Scan(&inputRevision); err != nil {
			return err
		}
		if inputRevision < 1 {
			inputRevision = 1
		}
		if inputRevision < attempt.ConfirmedVersion {
			return fmt.Errorf(
				"structure input revision %d is behind attempt %s revision %d",
				inputRevision,
				attempt.AttemptID,
				attempt.ConfirmedVersion,
			)
		}
		inputDigest := attempt.InputDigest
		if inputDigest == "" {
			return fmt.Errorf("%w: confirmed Attempt %s has no input digest",
				ErrProblemAttemptConflict, attempt.AttemptID)
		}
		bboxJSON := ""
		if attempt.BBox != nil {
			raw, err := json.Marshal(attempt.BBox)
			if err != nil {
				return err
			}
			bboxJSON = string(raw)
		}

		var existing legacyProblemInputHead
		err := tx.QueryRowContext(ctx, `
			SELECT page_asset_id,source_region_json,stem_raw,answer_raw,input_digest,
			       current_disposition,origin_command_receipt_id,origin_kind
			FROM k12_problem_input_revisions
			WHERE agent_name=? AND submission_id=? AND structure_version=?
			  AND problem_id=? AND input_revision=?`,
			agentName,
			submissionID,
			structureVersion,
			problem.ProblemID,
			inputRevision,
		).Scan(
			&existing.pageAssetID,
			&existing.sourceRegionJSON,
			&existing.stemRaw,
			&existing.answerRaw,
			&existing.inputDigest,
			&existing.disposition,
			&existing.originReceipt,
			&existing.originKind,
		)
		if err == nil {
			if existing.disposition != "current" {
				return fmt.Errorf("%w: input revision %s/v%d is not current",
					ErrProblemAttemptConflict, problem.ProblemID, inputRevision)
			}
			legacyDigest := problemSourceInputDigest(
				"legacy",
				problem.ProblemID,
				inputRevision,
			)
			isSyntheticPlaceholder := existing.originKind == "legacy_unverified" &&
				!existing.originReceipt.Valid && !existing.sourceRegionJSON.Valid &&
				(existing.inputDigest == "" || existing.inputDigest == legacyDigest)
			if isSyntheticPlaceholder && existing.inputDigest != inputDigest {
				if existing.pageAssetID != problem.PageAssetID ||
					existing.stemRaw != problem.StemRaw ||
					existing.answerRaw != attempt.AnswerRaw {
					return fmt.Errorf(
						"%w: legacy input revision %s/v%d raw identity drifted",
						ErrProblemAttemptConflict,
						problem.ProblemID,
						inputRevision,
					)
				}
				if err := appendConfirmedProblemInputAfterLegacyTx(
					ctx,
					tx,
					agentName,
					submissionID,
					structureVersion,
					inputRevision,
					problem,
					attempt,
					existing,
					bboxJSON,
					now,
				); err != nil {
					return err
				}
				continue
			}
			if existing.inputDigest != inputDigest {
				return fmt.Errorf(
					"%w: input revision %s/v%d digest %q != %q",
					ErrProblemAttemptConflict,
					problem.ProblemID,
					inputRevision,
					existing.inputDigest,
					inputDigest,
				)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else {
			result, err := tx.ExecContext(ctx, `
				UPDATE k12_problem_input_revisions
				SET current_disposition='superseded',updated_at=?
				WHERE agent_name=? AND submission_id=? AND structure_version=?
				  AND problem_id=? AND current_disposition='current'
				  AND input_revision<?`,
				now,
				agentName,
				submissionID,
				structureVersion,
				problem.ProblemID,
				inputRevision,
			)
			if err != nil {
				return err
			}
			if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
				return rowsErr
			} else if rows > 1 {
				return fmt.Errorf("multiple current input heads for problem %s", problem.ProblemID)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO k12_problem_input_revisions (
					agent_name,submission_id,structure_version,problem_id,input_revision,
					page_asset_id,source_region_json,stem_raw,answer_raw,answer_bbox_json,
					question_canonical_markdown,answer_canonical_markdown,input_digest,
					current_disposition,origin_command_receipt_id,origin_kind,
					created_at,updated_at
				) VALUES (?,?,?,?,?,?,NULL,?,?,?,?,?,?,'current',NULL,'legacy_unverified',?,?)`,
				agentName,
				submissionID,
				structureVersion,
				problem.ProblemID,
				inputRevision,
				problem.PageAssetID,
				problem.StemRaw,
				attempt.AnswerRaw,
				bboxJSON,
				problem.StemMarkdown,
				attempt.AnswerMarkdown,
				inputDigest,
				now,
				now,
			); err != nil {
				return err
			}
		}
		if attempt.ConfirmedVersion >= 1 && inputRevision > attempt.ConfirmedVersion {
			result, err := tx.ExecContext(ctx, `
				UPDATE k12_attempts
				SET confirmed_version=?,input_digest=?,updated_at=MAX(updated_at,?)
				WHERE agent_name=? AND attempt_id=? AND problem_id=?
				  AND confirmed_version=?`,
				inputRevision,
				inputDigest,
				attempt.UpdatedAt,
				agentName,
				attempt.AttemptID,
				attempt.ProblemID,
				attempt.ConfirmedVersion,
			)
			if err != nil {
				return err
			}
			if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
				return rowsErr
			} else if rows != 1 {
				return fmt.Errorf("attempt %s structure revision barrier CAS lost", attempt.AttemptID)
			}
		}
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
	sourceSectionPathJSON, _ := json.Marshal(problem.SourceSectionPath)
	var confidence any
	if problem.TranscriptionConfidence != nil {
		confidence = *problem.TranscriptionConfidence
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO k12_problems (`+problemColumns+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(agent_name,problem_id) DO NOTHING`,
		problem.ProblemID, problem.AgentName, problem.SubmissionID, problem.PageAssetID, problem.Ordinal,
		problem.ProblemKind, nullableString(problem.ParentProblemID), problem.SubproblemNo,
		string(sourceNumberPathJSON), problem.DisplayLabel,
		string(sourceSectionPathJSON), problem.SourceSectionLabel,
		problem.SystemSectionOrdinal, problem.SystemDisplayLabel, problem.Subject,
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
		!reflect.DeepEqual(existing.SourceSectionPath, problem.SourceSectionPath) ||
		existing.SourceSectionLabel != problem.SourceSectionLabel ||
		existing.SystemSectionOrdinal != problem.SystemSectionOrdinal ||
		existing.SystemDisplayLabel != problem.SystemDisplayLabel ||
		existing.StemRaw != problem.StemRaw {
		return fmt.Errorf("%w: Problem %s raw/structure", ErrProblemAttemptConflict, problem.ProblemID)
	}
	if problem.CanonicalVersion < existing.CanonicalVersion {
		return fmt.Errorf("%w: Problem %s canonical version %d < %d", ErrProblemAttemptConflict,
			problem.ProblemID, problem.CanonicalVersion, existing.CanonicalVersion)
	}
	if problem.CanonicalVersion == existing.CanonicalVersion {
		if problemCanonicalFactsEqual(existing, problem) {
			return nil
		}
		if !problemCanonicalContentFactsEqual(existing, problem) {
			return fmt.Errorf("%w: Problem %s same canonical version changed facts", ErrProblemAttemptConflict, problem.ProblemID)
		}
		// 确认策略由当前识别证据重新计算，不属于题目规范正文的版本化事实。
		_, err = tx.ExecContext(ctx, `UPDATE k12_problems SET confirmation_required=?,
            confirmation_reasons_json=?,updated_at=? WHERE agent_name=? AND problem_id=?`,
			boolInt(problem.ConfirmationRequired), string(reasonsJSON), problem.UpdatedAt,
			problem.AgentName, problem.ProblemID)
		if err != nil {
			return fmt.Errorf("k12storage: update Problem %s confirmation policy: %w", problem.ProblemID, err)
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
	return problemCanonicalContentFactsEqual(a, b) &&
		a.ConfirmationRequired == b.ConfirmationRequired &&
		reflect.DeepEqual(a.ConfirmationReasons, b.ConfirmationReasons)
}

func problemCanonicalContentFactsEqual(a, b k12.Problem) bool {
	return a.Subject == b.Subject && a.StemMarkdown == b.StemMarkdown &&
		reflect.DeepEqual(a.ConceptIDs, b.ConceptIDs) &&
		floatPtrEqual(a.TranscriptionConfidence, b.TranscriptionConfidence)
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
	var sourceNumberPathJSON, sourceSectionPathJSON, conceptsJSON, reasonsJSON string
	var confidence sql.NullFloat64
	var confirmationRequired int
	err := row.Scan(&problem.ProblemID, &problem.AgentName, &problem.SubmissionID,
		&problem.PageAssetID, &problem.Ordinal, &problem.ProblemKind, &parent,
		&problem.SubproblemNo, &sourceNumberPathJSON, &problem.DisplayLabel,
		&sourceSectionPathJSON, &problem.SourceSectionLabel,
		&problem.SystemSectionOrdinal, &problem.SystemDisplayLabel,
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
	if err := json.Unmarshal([]byte(sourceSectionPathJSON), &problem.SourceSectionPath); err != nil {
		return k12.Problem{}, fmt.Errorf("decode source_section_path_json: %w", err)
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
