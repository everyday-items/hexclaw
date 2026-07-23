package k12storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// dbExecer 抽象 *sql.DB / *sql.Tx 的写。
type dbExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// dbQueryer 抽象 *sql.DB / *sql.Tx 的读。
type dbQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// dbHandle 读写全能（*sql.DB 与 *sql.Tx 均满足）。
type dbHandle interface {
	dbExecer
	dbQueryer
}

// rowMapper 一个 K12 记录集 ↔ 一张（组）类型化表的映射。
//
// 领域字段在写入时从 Fields JSON 拍平为 typed 列（encode），读取时从 typed 列重建
// Fields JSON（newScan + attachChildren）——重建走该记录集的领域结构体 json.Marshal，
// 与所有写入方产出的 JSON 逐字节同构（数据层替换、行为等价）。
type rowMapper interface {
	collection() string
	table() string
	// domainCols 领域列名（不含通用基建列），与 encode 返回值一一对齐。
	domainCols() []string
	encode(fieldsJSON string) ([]any, error)
	// newScan 返回领域列的扫描目标 + 从扫描结果重建 Fields JSON 的收尾函数。
	newScan() (dest []any, finish func() (string, error))
	// syncChildren 以聚合根为整体重写子表行（无子表实现为空操作）。
	syncChildren(ctx context.Context, ex dbExecer, recordID, fieldsJSON string) error
	// attachChildren 读取子表行并入 Fields JSON（无子表原样返回）。
	attachChildren(ctx context.Context, q dbQueryer, recordID, fieldsJSON string) (string, error)
}

// allMappers 五个记录集的映射（顺序无关，Export 时按 collection 字节序排）。
func allMappers() []rowMapper {
	return []rowMapper{
		mistakeMapper{}, accumMapper{}, practiceSetMapper{}, creativeWorkMapper{}, gradingJobMapper{},
	}
}

// ---------- 错题本 → k12_mistakes ----------

type mistakeMapper struct{}

func (mistakeMapper) collection() string { return k12.CollectionMistakes }
func (mistakeMapper) table() string      { return "k12_mistakes" }
func (mistakeMapper) domainCols() []string {
	return []string{"subject", "question", "knowledge_point", "error_cause", "wrong_process",
		"canonical_answer", "review_stage", "last_retried_at", "spot_check_state", "entry_source"}
}

func (mistakeMapper) encode(fieldsJSON string) ([]any, error) {
	f, err := k12.ParseMistakeFields(fieldsJSON)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 解析错题字段: %w", err)
	}
	return []any{f.Subject, f.Question, f.KnowledgePoint, f.ErrorCause, f.WrongProcess,
		f.CanonicalAnswer, f.ReviewStage, f.LastRetriedAt, f.SpotCheckState, f.EntrySource}, nil
}

func (mistakeMapper) newScan() ([]any, func() (string, error)) {
	var f k12.MistakeFields
	dest := []any{&f.Subject, &f.Question, &f.KnowledgePoint, &f.ErrorCause, &f.WrongProcess,
		&f.CanonicalAnswer, &f.ReviewStage, &f.LastRetriedAt, &f.SpotCheckState, &f.EntrySource}
	return dest, func() (string, error) { return marshalFields(f) }
}

func (mistakeMapper) syncChildren(context.Context, dbExecer, string, string) error { return nil }
func (mistakeMapper) attachChildren(_ context.Context, _ dbQueryer, _ string, fieldsJSON string) (string, error) {
	return fieldsJSON, nil
}

// ---------- 积累本 → k12_accumulations ----------

type accumMapper struct{}

func (accumMapper) collection() string { return k12.CollectionAccumulation }
func (accumMapper) table() string      { return "k12_accumulations" }
func (accumMapper) domainCols() []string {
	return []string{"subject", "entry_type", "content", "source_ref", "review_stage", "last_retried_at"}
}

func (accumMapper) encode(fieldsJSON string) ([]any, error) {
	f, err := k12.ParseAccumFields(fieldsJSON)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 解析积累字段: %w", err)
	}
	return []any{f.Subject, f.EntryType, f.Content, f.Source, f.ReviewStage, f.LastRetriedAt}, nil
}

func (accumMapper) newScan() ([]any, func() (string, error)) {
	var f k12.AccumFields
	dest := []any{&f.Subject, &f.EntryType, &f.Content, &f.Source, &f.ReviewStage, &f.LastRetriedAt}
	return dest, func() (string, error) { return marshalFields(f) }
}

func (accumMapper) syncChildren(context.Context, dbExecer, string, string) error { return nil }
func (accumMapper) attachChildren(_ context.Context, _ dbQueryer, _ string, fieldsJSON string) (string, error) {
	return fieldsJSON, nil
}

// ---------- 练习集 → k12_practice_sets + k12_practice_set_items ----------

type practiceSetMapper struct{}

func (practiceSetMapper) collection() string { return k12.CollectionPracticeSet }
func (practiceSetMapper) table() string      { return "k12_practice_sets" }
func (practiceSetMapper) domainCols() []string {
	return []string{"source_kind", "title", "paper_no", "question_artifact_id", "answer_artifact_id",
		"skipped_blocked_count", "finalized_at", "finalized_via", "reminder_sent_at",
		"reminder_dismissed", "closed_reason", "delivery_status", "delivery_target"}
}

func (practiceSetMapper) encode(fieldsJSON string) ([]any, error) {
	f, err := k12.ParsePracticeSetFields(fieldsJSON)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 解析练习集字段: %w", err)
	}
	return []any{f.SourceKind, f.Title, f.PaperNo, f.QuestionArtifact, f.AnswerArtifact,
		f.SkippedBlockedCount, f.FinalizedAt, f.FinalizedVia, f.ReminderSentAt,
		boolInt(f.ReminderDismissed), f.ClosedReason, f.DeliveryStatus, f.DeliveryTarget}, nil
}

func (practiceSetMapper) newScan() ([]any, func() (string, error)) {
	var f k12.PracticeSetFields
	var dismissed int64
	dest := []any{&f.SourceKind, &f.Title, &f.PaperNo, &f.QuestionArtifact, &f.AnswerArtifact,
		&f.SkippedBlockedCount, &f.FinalizedAt, &f.FinalizedVia, &f.ReminderSentAt,
		&dismissed, &f.ClosedReason, &f.DeliveryStatus, &f.DeliveryTarget}
	return dest, func() (string, error) {
		f.ReminderDismissed = dismissed != 0
		// Items 由 attachChildren 从 k12_practice_set_items 补齐。
		return marshalFields(f)
	}
}

func (practiceSetMapper) syncChildren(ctx context.Context, ex dbExecer, recordID, fieldsJSON string) error {
	f, err := k12.ParsePracticeSetFields(fieldsJSON)
	if err != nil {
		return fmt.Errorf("k12storage: 解析练习集字段: %w", err)
	}
	if _, err := ex.ExecContext(ctx, `DELETE FROM k12_practice_set_items WHERE set_record_id = ?`, recordID); err != nil {
		return fmt.Errorf("k12storage: 清理练习项: %w", err)
	}
	for i, it := range f.Items {
		var rc any // NULL = 尚无复批结论
		if it.ResultCorrect != nil {
			rc = boolInt(*it.ResultCorrect)
		}
		if _, err := ex.ExecContext(ctx, `INSERT INTO k12_practice_set_items
            (set_record_id, item_index, item_id, source_problem_id, subject, added_via,
             question_markdown, expected_answer_markdown, verification_status,
             verification_evidence, blocked_reason, paper_seq, returned, practice_problem_id, result_correct,
             generation_job_id, variant_index, requested_difficulty, actual_difficulty)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			recordID, i, it.ItemID, it.SourceProblemID, it.Subject, it.AddedVia,
			it.QuestionMarkdown, it.ExpectedAnswerMarkdown, it.VerificationStatus,
			it.VerificationEvidence, it.BlockedReason, it.PaperSeq, boolInt(it.Returned),
			it.PracticeProblemID, rc, it.GenerationJobID, it.VariantIndex,
			it.RequestedDifficulty, it.ActualDifficulty); err != nil {
			return fmt.Errorf("k12storage: 写练习项 #%d: %w", i, err)
		}
	}
	// DD-028：return_assets 是只追加审计表。普通聚合更新绝不 DELETE；既有 return_id
	// 只允许完全相同的载荷重放，任何改写尝试都让整个外层事务回滚。
	queryer, queryOK := ex.(dbQueryer)
	for i, ra := range f.ReturnAssets {
		itemIDsJSON, err := json.Marshal(ra.ItemIDs)
		if err != nil {
			return fmt.Errorf("k12storage: 编码回传资产 #%d item_ids: %w", i, err)
		}
		res, err := ex.ExecContext(ctx, `INSERT INTO k12_practice_return_assets
            (set_record_id, return_index, return_id, asset_id, item_ids_json, returned_at)
            VALUES (?, ?, ?, ?, ?, ?)
            ON CONFLICT(set_record_id, return_id) DO NOTHING`,
			recordID, i, ra.ReturnID, ra.AssetID, string(itemIDsJSON), ra.ReturnedAt)
		if err != nil {
			return fmt.Errorf("k12storage: 追加回传资产 #%d: %w", i, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if !queryOK {
				return fmt.Errorf("k12storage: 回查回传资产需要可查询事务句柄")
			}
			var storedAsset, storedItems string
			var storedAt int64
			if err := queryer.QueryRowContext(ctx, `SELECT asset_id, item_ids_json, returned_at
                    FROM k12_practice_return_assets WHERE set_record_id = ? AND return_id = ?`,
				recordID, ra.ReturnID).Scan(&storedAsset, &storedItems, &storedAt); err != nil {
				return fmt.Errorf("k12storage: 回查回传资产 #%d: %w", i, err)
			}
			if storedAsset != ra.AssetID || storedItems != string(itemIDsJSON) || storedAt != ra.ReturnedAt {
				return fmt.Errorf("k12storage: return_id %q 已存在且载荷不同，禁止覆盖", ra.ReturnID)
			}
		}
	}
	return nil
}

func (practiceSetMapper) attachChildren(ctx context.Context, q dbQueryer, recordID, fieldsJSON string) (string, error) {
	f, err := k12.ParsePracticeSetFields(fieldsJSON)
	if err != nil {
		return "", fmt.Errorf("k12storage: 解析练习集字段: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT item_id, source_problem_id, subject, added_via,
        question_markdown, expected_answer_markdown, verification_status, verification_evidence,
        blocked_reason, paper_seq, returned, practice_problem_id, result_correct,
        generation_job_id, variant_index, requested_difficulty, actual_difficulty
        FROM k12_practice_set_items WHERE set_record_id = ? ORDER BY item_index`, recordID)
	if err != nil {
		return "", fmt.Errorf("k12storage: 读练习项: %w", err)
	}
	f.Items = nil
	for rows.Next() {
		var it k12.PracticeItem
		var returned int64
		var rc *int64
		if err := rows.Scan(&it.ItemID, &it.SourceProblemID, &it.Subject, &it.AddedVia,
			&it.QuestionMarkdown, &it.ExpectedAnswerMarkdown, &it.VerificationStatus,
			&it.VerificationEvidence, &it.BlockedReason, &it.PaperSeq, &returned,
			&it.PracticeProblemID, &rc, &it.GenerationJobID, &it.VariantIndex,
			&it.RequestedDifficulty, &it.ActualDifficulty); err != nil {
			rows.Close()
			return "", fmt.Errorf("k12storage: 扫描练习项: %w", err)
		}
		it.Returned = returned != 0
		if rc != nil {
			b := *rc != 0
			it.ResultCorrect = &b
		}
		f.Items = append(f.Items, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}

	returnRows, err := q.QueryContext(ctx, `SELECT return_id, asset_id, item_ids_json, returned_at
        FROM k12_practice_return_assets WHERE set_record_id = ? ORDER BY return_index`, recordID)
	if err != nil {
		return "", fmt.Errorf("k12storage: 读回传资产: %w", err)
	}
	f.ReturnAssets = nil
	returnedItemIDs := make(map[string]struct{})
	for returnRows.Next() {
		var ra k12.PracticeReturnAsset
		var itemIDsJSON string
		if err := returnRows.Scan(&ra.ReturnID, &ra.AssetID, &itemIDsJSON, &ra.ReturnedAt); err != nil {
			returnRows.Close()
			return "", fmt.Errorf("k12storage: 扫描回传资产: %w", err)
		}
		if err := json.Unmarshal([]byte(itemIDsJSON), &ra.ItemIDs); err != nil {
			returnRows.Close()
			return "", fmt.Errorf("k12storage: 解析回传资产 item_ids: %w", err)
		}
		f.ReturnAssets = append(f.ReturnAssets, ra)
		for _, itemID := range ra.ItemIDs {
			returnedItemIDs[itemID] = struct{}{}
		}
	}
	if err := returnRows.Err(); err != nil {
		returnRows.Close()
		return "", err
	}
	if err := returnRows.Close(); err != nil {
		return "", err
	}
	// DD-028：return_assets 是回传事实，PracticeItem.Returned 只是兼容旧 DTO 的投影。
	// 旧调用方即使漏带 return_assets 或写回 false，也不能抹掉已有照片证据。
	for i := range f.Items {
		if _, covered := returnedItemIDs[f.Items[i].ItemID]; covered {
			f.Items[i].Returned = true
		}
	}
	// 零项保持 nil（与写入方零值 marshal 的 "items":null 同构）。
	return marshalFields(f)
}

// ---------- 作品 → k12_creative_works + versions + feedback ----------

type creativeWorkMapper struct{}

func (creativeWorkMapper) collection() string { return k12.CollectionCreativeWork }
func (creativeWorkMapper) table() string      { return "k12_creative_works" }
func (creativeWorkMapper) domainCols() []string {
	return []string{"work_type", "title", "task", "intent"}
}

func (creativeWorkMapper) encode(fieldsJSON string) ([]any, error) {
	f, err := k12.ParseCreativeWorkFields(fieldsJSON)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 解析作品字段: %w", err)
	}
	return []any{f.WorkType, f.Title, f.Task, f.Intent}, nil
}

func (creativeWorkMapper) newScan() ([]any, func() (string, error)) {
	var f k12.CreativeWorkFields
	dest := []any{&f.WorkType, &f.Title, &f.Task, &f.Intent}
	return dest, func() (string, error) { return marshalFields(f) }
}

func (creativeWorkMapper) syncChildren(ctx context.Context, ex dbExecer, recordID, fieldsJSON string) error {
	f, err := k12.ParseCreativeWorkFields(fieldsJSON)
	if err != nil {
		return fmt.Errorf("k12storage: 解析作品字段: %w", err)
	}
	if _, err := ex.ExecContext(ctx, `DELETE FROM k12_creative_work_versions WHERE work_record_id = ?`, recordID); err != nil {
		return fmt.Errorf("k12storage: 清理作品版本: %w", err)
	}
	if _, err := ex.ExecContext(ctx, `DELETE FROM k12_work_feedback WHERE work_record_id = ?`, recordID); err != nil {
		return fmt.Errorf("k12storage: 清理形成性反馈: %w", err)
	}
	for i, v := range f.Versions {
		if _, err := ex.ExecContext(ctx, `INSERT INTO k12_creative_work_versions
			(work_record_id, version_index, version_id, source_asset_id, content_markdown,
			 practice_card_done_at, ocr_job_id, ocr_raw, ocr_version, ocr_confirmed_digest,
			 content_confirmed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			recordID, i, v.VersionID, v.SourceAssetID, v.ContentMarkdown, v.PracticeCardDoneAt,
			v.OCRJobID, v.OCRRaw, v.OCRVersion, v.OCRConfirmedDigest, v.ContentConfirmedAt); err != nil {
			return fmt.Errorf("k12storage: 写作品版本 #%d: %w", i, err)
		}
		structuredJSON := ""
		if v.StructuredFeedback != nil {
			if err := v.StructuredFeedback.Validate(); err != nil {
				return fmt.Errorf("k12storage: 结构化作品点评 #%d 非法: %w", i, err)
			}
			raw, err := json.Marshal(v.StructuredFeedback)
			if err != nil {
				return fmt.Errorf("k12storage: marshal 结构化作品点评 #%d: %w", i, err)
			}
			structuredJSON = string(raw)
		}
		if v.Feedback != "" || v.FeedbackSource != "" || v.FeedbackSkill != "" || structuredJSON != "" {
			if _, err := ex.ExecContext(ctx, `INSERT INTO k12_work_feedback
				(work_record_id, version_index, feedback_markdown, feedback_source, feedback_skill,
				 structured_feedback_json)
				VALUES (?, ?, ?, ?, ?, ?)`,
				recordID, i, v.Feedback, v.FeedbackSource, v.FeedbackSkill, structuredJSON); err != nil {
				return fmt.Errorf("k12storage: 写形成性反馈 #%d: %w", i, err)
			}
		}
	}
	return nil
}

func (creativeWorkMapper) attachChildren(ctx context.Context, q dbQueryer, recordID, fieldsJSON string) (string, error) {
	f, err := k12.ParseCreativeWorkFields(fieldsJSON)
	if err != nil {
		return "", fmt.Errorf("k12storage: 解析作品字段: %w", err)
	}
	rows, err := q.QueryContext(ctx, `SELECT v.version_id, v.source_asset_id, v.content_markdown,
		v.practice_card_done_at, v.ocr_job_id, v.ocr_raw, v.ocr_version,
		v.ocr_confirmed_digest, v.content_confirmed_at,
		COALESCE(fb.feedback_markdown,''), COALESCE(fb.feedback_source,''), COALESCE(fb.feedback_skill,''),
		COALESCE(fb.structured_feedback_json,'')
        FROM k12_creative_work_versions v
        LEFT JOIN k12_work_feedback fb
          ON fb.work_record_id = v.work_record_id AND fb.version_index = v.version_index
        WHERE v.work_record_id = ? ORDER BY v.version_index`, recordID)
	if err != nil {
		return "", fmt.Errorf("k12storage: 读作品版本: %w", err)
	}
	defer rows.Close()
	f.Versions = nil
	for rows.Next() {
		var v k12.CreativeWorkVersion
		var structuredJSON string
		if err := rows.Scan(&v.VersionID, &v.SourceAssetID, &v.ContentMarkdown,
			&v.PracticeCardDoneAt, &v.OCRJobID, &v.OCRRaw, &v.OCRVersion,
			&v.OCRConfirmedDigest, &v.ContentConfirmedAt,
			&v.Feedback, &v.FeedbackSource, &v.FeedbackSkill,
			&structuredJSON); err != nil {
			return "", fmt.Errorf("k12storage: 扫描作品版本: %w", err)
		}
		if structuredJSON != "" {
			structured, err := k12.ParseWorkFeedbackJSON([]byte(structuredJSON))
			if err != nil {
				return "", fmt.Errorf("k12storage: 结构化作品点评非法: %w", err)
			}
			v.StructuredFeedback = &structured
		}
		f.Versions = append(f.Versions, v)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	// 零版本保持 nil（与写入方零值 marshal 同构）。
	return marshalFields(f)
}

// ---------- 批改任务 → k12_grading_jobs ----------

type gradingJobMapper struct{}

func (gradingJobMapper) collection() string { return k12.CollectionGradingJob }
func (gradingJobMapper) table() string      { return "k12_grading_jobs" }
func (gradingJobMapper) domainCols() []string {
	return []string{"submission_id", "source_kind", "idempotency_key", "confirmed_version",
		"confirmation_state", "anchor_state", "deadline", "model_snapshot_json",
		"budget_snapshot_json", "stage_checkpoints_json", "attempt_count", "failure_kind", "retryable", "failed_stage"}
}

func (gradingJobMapper) encode(fieldsJSON string) ([]any, error) {
	f, err := k12.ParseGradingJobFields(fieldsJSON)
	if err != nil {
		return nil, fmt.Errorf("k12storage: 解析批改任务字段: %w", err)
	}
	snap, err := json.Marshal(f.ModelSnapshot)
	if err != nil {
		return nil, fmt.Errorf("k12storage: marshal model_snapshot: %w", err)
	}
	budget := ""
	if f.BudgetSnapshot.IsFrozen() {
		raw, err := json.Marshal(f.BudgetSnapshot)
		if err != nil {
			return nil, fmt.Errorf("k12storage: marshal budget_snapshot: %w", err)
		}
		budget = string(raw)
	}
	checkpoints := ""
	if len(f.StageCheckpoints) > 0 {
		raw, err := json.Marshal(f.StageCheckpoints)
		if err != nil {
			return nil, fmt.Errorf("k12storage: marshal stage_checkpoints: %w", err)
		}
		checkpoints = string(raw)
	}
	return []any{f.SubmissionID, f.SourceKind, f.IdempotencyKey, f.ConfirmedVersion,
		f.ConfirmationState, f.AnchorState, f.Deadline, string(snap),
		budget, checkpoints, f.AttemptCount, f.FailureKind, boolInt(f.Retryable), f.FailedStage}, nil
}

func (gradingJobMapper) newScan() ([]any, func() (string, error)) {
	var f k12.GradingJobFields
	var snapJSON, budgetJSON, checkpointsJSON string
	var retryable int64
	dest := []any{&f.SubmissionID, &f.SourceKind, &f.IdempotencyKey, &f.ConfirmedVersion,
		&f.ConfirmationState, &f.AnchorState, &f.Deadline, &snapJSON,
		&budgetJSON, &checkpointsJSON, &f.AttemptCount, &f.FailureKind, &retryable, &f.FailedStage}
	return dest, func() (string, error) {
		f.Retryable = retryable != 0
		if snapJSON != "" {
			if err := json.Unmarshal([]byte(snapJSON), &f.ModelSnapshot); err != nil {
				return "", fmt.Errorf("k12storage: unmarshal model_snapshot: %w", err)
			}
		}
		if budgetJSON != "" && budgetJSON != "null" {
			if err := json.Unmarshal([]byte(budgetJSON), &f.BudgetSnapshot); err != nil {
				return "", fmt.Errorf("k12storage: unmarshal budget_snapshot: %w", err)
			}
			if err := f.BudgetSnapshot.Validate(); err != nil {
				return "", fmt.Errorf("k12storage: invalid budget_snapshot: %w", err)
			}
		}
		if checkpointsJSON != "" && checkpointsJSON != "null" {
			if err := json.Unmarshal([]byte(checkpointsJSON), &f.StageCheckpoints); err != nil {
				return "", fmt.Errorf("k12storage: unmarshal stage_checkpoints: %w", err)
			}
		}
		return marshalFields(f)
	}
}

func (gradingJobMapper) syncChildren(context.Context, dbExecer, string, string) error { return nil }
func (gradingJobMapper) attachChildren(_ context.Context, _ dbQueryer, _ string, fieldsJSON string) (string, error) {
	return fieldsJSON, nil
}

// ---------- 共用 ----------

func marshalFields(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("k12storage: 重建领域字段: %w", err)
	}
	return string(raw), nil
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
