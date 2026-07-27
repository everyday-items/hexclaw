package usecase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ReviewPolicyVersion 生效复习策略版本（架构设计 §4.6：复习与掌握策略参数化为版本化
// review policy；策略版本变更只影响其后的计算，不回溯改写既有状态）。
const ReviewPolicyVersion = "v1"

// reviewIntervalLadder 默认策略 v1 的间隔阶梯（秒，§4.6）：due_at 间隔序列 1、3、7、14 天，
// 每次复练通过取下一档，未通过重置回首档（重置在 applyRegradeOutcome 执行）。
// 索引 = ReviewStage 轮次；超出末档按 14 天封顶。
//
//	rung 0 = 1 天（入库首排，与 pipeline.FirstReviewInterval 对齐）
//	rung 1 = 3 天（首次重做做对）· rung 2 = 7 天 · rung 3+ = 14 天封顶
var reviewIntervalLadder = []int64{1 * 86400, 3 * 86400, 7 * 86400, 14 * 86400}

// reviewIntervalForStage 取某复习轮次的到期间隔（秒），末档封顶、下界 0。
func reviewIntervalForStage(stage int) int64 {
	if stage < 0 {
		stage = 0
	}
	if stage >= len(reviewIntervalLadder) {
		stage = len(reviewIntervalLadder) - 1
	}
	return reviewIntervalLadder[stage]
}

// ReviewItem 复习队列项（跨 collection）：错题本用 Fields，积累本纠错型用 Accum。
type ReviewItem struct {
	Record *records.AgentRecord
	Fields k12.MistakeFields // Collection=错题本 时有效
	Accum  k12.AccumFields   // Collection=积累本 时有效
}

// IsAccum 该项是否来自积累本（语英纠错型）。
func (it ReviewItem) IsAccum() bool {
	return it.Record != nil && it.Record.Collection == k12.CollectionAccumulation
}

// Subject 学科（家长侧跨科混排展示）：积累本取 subject；错题本主路径=数学（物化 M4-2 后带 subject）。
func (it ReviewItem) Subject() string {
	if it.IsAccum() {
		return it.Accum.Subject
	}
	if it.Fields.Subject != "" {
		return it.Fields.Subject
	}
	return "数学"
}

// Title 题面/内容。
func (it ReviewItem) Title() string {
	if it.IsAccum() {
		return it.Accum.Content
	}
	return it.Fields.Question
}

// Point 知识点/类型（错题本=知识点；积累本=entry_type 如"默写错/错词"）。
func (it ReviewItem) Point() string {
	if it.IsAccum() {
		return it.Accum.EntryType
	}
	return it.Fields.KnowledgePoint
}

// dueOf 取到期（无到期排末尾）。
func dueOf(it ReviewItem) int64 {
	if it.Record != nil && it.Record.DueAt != nil {
		return *it.Record.DueAt
	}
	return 1 << 62
}

// ReviewQueue 返回某实例**到期该练**的复习项——**跨 collection 混排**（错题本 + 积累本纠错型），
// 按到期升序。这是家长侧"本周该练"的统一队列（PRD §3.5.4：语英客观错误同进复习引擎）。
func (d Deps) ReviewQueue(ctx context.Context, agentName string) ([]ReviewItem, error) {
	now := d.now()
	mrecs, err := d.Records.ListDue(ctx, agentName, k12.CollectionMistakes, now)
	if err != nil {
		return nil, fmt.Errorf("usecase: 取错题复习队列: %w", err)
	}
	arecs, err := d.Records.ListDue(ctx, agentName, k12.CollectionAccumulation, now)
	if err != nil {
		return nil, fmt.Errorf("usecase: 取积累复习队列: %w", err)
	}
	// 已安排抽查的错题只能由 FillBasketFromDue 的 spot-check 通道投放。
	// 若同时留在普通到期队列，同一来源题会既作为抽查题、又作为普通复习题入卷，
	// 破坏“最多混入两道且在途不重复”的产品不变量。
	items := make([]ReviewItem, 0, len(mrecs)+len(arecs))
	reviewStates, err := d.Records.ListMistakeReviewStates(ctx, agentName)
	if err != nil {
		return nil, fmt.Errorf("usecase: 取错题复习状态: %w", err)
	}
	location := time.UTC
	if settings, settingsErr := d.Records.GetWeeklyPracticeSettings(
		ctx, agentName,
	); settingsErr == nil {
		if loaded, loadErr := time.LoadLocation(settings.Timezone); loadErr == nil {
			location = loaded
		}
	}
	currentYear, currentWeek := time.Unix(now, 0).In(location).ISOWeek()
	for _, r := range mrecs {
		if review, ok := reviewStates[r.RecordID]; ok {
			if review.State == k12.MistakeReviewSuppressed ||
				review.State == k12.MistakeReviewMastered ||
				(review.State == k12.MistakeReviewDeferredThisWeek &&
					review.DeferredISOYear == currentYear &&
					review.DeferredISOWeek == currentWeek) {
				continue
			}
		}
		f, _ := k12.ParseMistakeFields(r.Fields)
		if f.SpotCheckState == k12.SpotCheckScheduled {
			continue
		}
		items = append(items, ReviewItem{Record: r, Fields: f})
	}
	for _, r := range arecs {
		f, _ := k12.ParseAccumFields(r.Fields)
		if !k12.AccumIsCorrective(f.EntryType) {
			continue // 只纳入语英纠错型（积累/留档型不进复习队列，双保险：它们本就无 due）
		}
		items = append(items, ReviewItem{Record: r, Accum: f})
	}
	sort.SliceStable(items, func(i, j int) bool { return dueOf(items[i]) < dueOf(items[j]) })
	return items, nil
}

// MarkMastered retains its legacy command name for API compatibility, but its
// current product meaning is strictly “家长确认已会”. It records an independent
// parent fact and schedules one later spot check; it never promotes evidence
// mastery or mutates the current learning status.
//
// 抽查复验安排（§3.6，2026-07-18 落地）：错题确认已会时 spot_check_state none→scheduled，
// 下一周期周卷混入一道抽查（FillBasketFromDue）。**最多自动安排一次**：已 scheduled/passed/
// failed 的再次确认不重复安排——特别是 failed（复验未过）后家长再次确认，尊重家长判断
// 不再强制抽查，仅保留「家长确认（复验未过）」事实标注（规则 4）。
func (d Deps) MarkMastered(ctx context.Context, agentName, recordID string, expectedVersion int) error {
	if agentName == "" || recordID == "" {
		return fmt.Errorf("%w: agentName / recordID 不可空", ErrInvalidInput)
	}
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return fmt.Errorf("usecase: 取记录: %w", err)
	}
	if rec.AgentName != agentName {
		return fmt.Errorf("usecase: 取记录: %w", records.ErrNotFound)
	}
	if rec.Collection != k12.CollectionMistakes {
		return fmt.Errorf("%w: 家长确认只适用于错题", ErrInvalidInput)
	}
	f, _ := k12.ParseMistakeFields(rec.Fields)
	now := d.now()
	f.ParentConfirmedAt = now
	if f.SpotCheckState == "" || f.SpotCheckState == k12.SpotCheckNone {
		f.SpotCheckState = k12.SpotCheckScheduled
	}
	due := now + reviewIntervalForStage(f.ReviewStage+1)
	raw, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("usecase: marshal 错题字段: %w", err)
	}
	return d.Records.UpdateStatusFields(ctx, recordID, rec.Status, &due, string(raw), expectedVersion)
}

// DeleteMistake 家长「删除这条错题」（UX-3 · 数据纠错，非逃避难题）：移除记错的 / 重复的条目。
// 校验 agent 归属后删除对应 k12_mistakes 记录（Store.Delete 内按 agent_name 圈定）——
// 不存在 / 不属于该实例 → records.ErrNotFound（HTTP 404）。产品评审定案为详情弹层内的克制入口
// （二次确认），故此处只做归属校验 + 删除，不做任何状态机流转。
func (d Deps) DeleteMistake(ctx context.Context, agentName, recordID string) error {
	if agentName == "" || recordID == "" {
		return fmt.Errorf("%w: agentName / recordID 不可空", ErrInvalidInput)
	}
	return d.Records.Delete(ctx, agentName, recordID)
}

// ArchiveMistake 将错题从复习调度中软归档。它冻结归档前状态、到期和抽查状态，
// 但不删除题目、不写掌握证据。idempotencyKey 钉住一次用户意图：同一命令即使在
// Undo 之后迟到重放，也只返回当前事实，绝不把已恢复的题再次归档。
func (d Deps) ArchiveMistake(
	ctx context.Context,
	agentName, recordID string,
	expectedVersion int,
	idempotencyKey string,
) (*records.AgentRecord, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(recordID) == "" ||
		strings.TrimSpace(idempotencyKey) == "" {
		return nil, fmt.Errorf("%w: agentName / recordID / idempotencyKey 不可空", ErrInvalidInput)
	}
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return nil, fmt.Errorf("usecase: 取错题记录: %w", err)
	}
	if rec.AgentName != agentName || rec.Collection != k12.CollectionMistakes {
		return nil, fmt.Errorf("usecase: 取错题记录: %w", records.ErrNotFound)
	}
	f, err := k12.ParseMistakeFields(rec.Fields)
	if err != nil {
		return nil, fmt.Errorf("usecase: 解析错题字段: %w", err)
	}
	if f.ArchiveCommandID == idempotencyKey ||
		(f.LastArchive != nil && f.LastArchive.ArchiveCommandID == idempotencyKey) {
		return rec, nil
	}
	if rec.Version != expectedVersion {
		return nil, records.ErrVersionConflict
	}
	if rec.Status == k12.StatusArchived {
		return nil, fmt.Errorf("%w: 错题已由另一命令归档", records.ErrIllegalTransition)
	}
	fromDue := cloneUnixPointer(rec.DueAt)
	f.ArchivedReason = k12.MistakeArchivedReasonManual
	f.ArchivedAt = d.now()
	f.ArchiveCommandID = idempotencyKey
	f.ArchivedFromStatus = rec.Status
	f.ArchivedFromDueAt = fromDue
	f.ArchivedFromSpotCheckState = f.SpotCheckState
	f.LastArchive = &k12.MistakeArchiveSnapshot{
		Reason:             f.ArchivedReason,
		ArchivedAt:         f.ArchivedAt,
		ArchiveCommandID:   f.ArchiveCommandID,
		FromStatus:         f.ArchivedFromStatus,
		FromDueAt:          cloneUnixPointer(f.ArchivedFromDueAt),
		FromSpotCheckState: f.ArchivedFromSpotCheckState,
	}
	// 已归档对象不参与下一周期抽查；原值已冻结，恢复时原样写回。
	f.SpotCheckState = k12.SpotCheckNone
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("usecase: marshal 错题归档快照: %w", err)
	}
	if err := d.Records.UpdateStatusFields(
		ctx, recordID, k12.StatusArchived, nil, string(raw), expectedVersion,
	); err != nil {
		if errors.Is(err, records.ErrVersionConflict) {
			latest, latestErr := d.Records.Get(ctx, recordID)
			if latestErr == nil {
				latestFields, parseErr := k12.ParseMistakeFields(latest.Fields)
				if parseErr == nil && (latestFields.ArchiveCommandID == idempotencyKey ||
					(latestFields.LastArchive != nil &&
						latestFields.LastArchive.ArchiveCommandID == idempotencyKey)) {
					return latest, nil
				}
			}
		}
		return nil, err
	}
	return d.Records.Get(ctx, recordID)
}

// RestoreMistake 是 8 秒 Undo 与「已归档」长期恢复的唯一命令。它只恢复冻结的
// 学习状态和调度；绝不升级状态、增加复习轮次或制造掌握证据。历史 due 已过期时，
// 原样恢复会使它立即回到待练队列，避免无证据地推迟复习。
func (d Deps) RestoreMistake(
	ctx context.Context,
	agentName, recordID string,
	expectedVersion int,
	idempotencyKey string,
) (*records.AgentRecord, error) {
	if strings.TrimSpace(agentName) == "" || strings.TrimSpace(recordID) == "" ||
		strings.TrimSpace(idempotencyKey) == "" {
		return nil, fmt.Errorf("%w: agentName / recordID / idempotencyKey 不可空", ErrInvalidInput)
	}
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return nil, fmt.Errorf("usecase: 取错题记录: %w", err)
	}
	if rec.AgentName != agentName || rec.Collection != k12.CollectionMistakes {
		return nil, fmt.Errorf("usecase: 取错题记录: %w", records.ErrNotFound)
	}
	f, err := k12.ParseMistakeFields(rec.Fields)
	if err != nil {
		return nil, fmt.Errorf("usecase: 解析错题字段: %w", err)
	}
	if rec.Status != k12.StatusArchived &&
		f.LastArchive != nil && f.LastArchive.RestoreCommandID == idempotencyKey {
		return rec, nil
	}
	if rec.Version != expectedVersion {
		return nil, records.ErrVersionConflict
	}
	if rec.Status != k12.StatusArchived {
		return nil, fmt.Errorf("%w: 错题未归档", records.ErrIllegalTransition)
	}
	if !k12.MistakeRestorable(rec.Status, f) {
		return nil, fmt.Errorf("%w: 缺少可恢复的归档前状态", records.ErrInvalidFields)
	}
	snapshot := f.LastArchive
	restoredStatus := snapshot.FromStatus
	restoredDueAt := cloneUnixPointer(snapshot.FromDueAt)
	restoredSpotCheckState := snapshot.FromSpotCheckState
	f.SpotCheckState = restoredSpotCheckState
	if f.SpotCheckState == "" {
		f.SpotCheckState = k12.SpotCheckNone
	}
	f.LastArchive.RestoredAt = d.now()
	f.LastArchive.RestoreCommandID = idempotencyKey
	// 当前归档态字段与 status 严格同生命周期；审计事实已进入 LastArchive。
	f.ArchivedReason = ""
	f.ArchivedAt = 0
	f.ArchiveCommandID = ""
	f.ArchivedFromStatus = ""
	f.ArchivedFromDueAt = nil
	f.ArchivedFromSpotCheckState = ""
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("usecase: marshal 错题恢复快照: %w", err)
	}
	if err := d.Records.RestoreArchivedMistake(
		ctx, recordID, restoredStatus, restoredDueAt,
		string(raw), expectedVersion, agentName,
	); err != nil {
		if errors.Is(err, records.ErrVersionConflict) ||
			errors.Is(err, records.ErrIllegalTransition) {
			latest, latestErr := d.Records.Get(ctx, recordID)
			if latestErr == nil {
				latestFields, parseErr := k12.ParseMistakeFields(latest.Fields)
				if parseErr == nil && latestFields.LastArchive != nil &&
					latestFields.LastArchive.RestoreCommandID == idempotencyKey {
					return latest, nil
				}
			}
		}
		return nil, err
	}
	return d.Records.Get(ctx, recordID)
}

func cloneUnixPointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// MasteryGapInterval 掌握判定的最小间隔（秒）：第二次做对距上次 ≥ 此值 → mastered（PRD §5.3.1）。
const MasteryGapInterval int64 = 3 * 86400 // 3 天

// MarkRetried 重做做对：状态 → retried，复习轮次 +1，按艾宾浩斯阶梯重排下次到期
// （轮次越高间隔越长）。轮次写回卡片 fields（乐观锁校验 expectedVersion）。
//
// T2.2（PRD §5.3.1）：若已是 retried（做对过一次）且距上次做对 ≥3 天 → 升 mastered、清到期
// （连对两次且隔天才算掌握，抗隔天遗忘）；否则停留/进入 retried 并记录本次做对时间。
func (d Deps) MarkRetried(ctx context.Context, recordID string, expectedVersion int) error {
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return fmt.Errorf("usecase: 取错题记录: %w", err)
	}
	if rec.Collection == k12.CollectionAccumulation {
		return d.markRetriedAccum(ctx, rec, expectedVersion) // 语英纠错型：同艾宾浩斯阶梯，用积累本状态
	}
	// 已掌握（家长确认/证据掌握）再做对：幂等不倒退——mastered→retried 仅保留给
	// 抽查复验未过路径（§3.6 规则 3，applySpotCheckOutcome），普通复批不得触发。
	if rec.Status == k12.StatusMastered {
		return nil
	}
	f, _ := k12.ParseMistakeFields(rec.Fields)
	now := d.now()

	if rec.Status == k12.StatusRetried && f.LastRetriedAt > 0 && now-f.LastRetriedAt >= MasteryGapInterval {
		f.LastRetriedAt = now
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("usecase: marshal 错题字段: %w", err)
		}
		// 掌握：清到期（移出复习队列）。
		return d.Records.UpdateStatusFields(ctx, recordID, k12.StatusMastered, nil, string(raw), expectedVersion)
	}

	f.ReviewStage++ // 完成一次成功重做 → 进阶到下一档间隔
	f.LastRetriedAt = now
	due := now + reviewIntervalForStage(f.ReviewStage)
	raw, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("usecase: marshal 错题字段: %w", err)
	}
	return d.Records.UpdateStatusFields(ctx, recordID, k12.StatusRetried, &due, string(raw), expectedVersion)
}

// markRetriedAccum 语英纠错型「重做做对」：同错题本艾宾浩斯口径，但走积累本状态（待复习/已掌握）。
// 连对两次且隔天 ≥MasteryGapInterval → 已掌握清到期；否则轮次 +1 按阶梯重排到期。
func (d Deps) markRetriedAccum(ctx context.Context, rec *records.AgentRecord, expectedVersion int) error {
	f, _ := k12.ParseAccumFields(rec.Fields)
	now := d.now()
	if rec.Status == k12.AccumStatusReviewing && f.LastRetriedAt > 0 && now-f.LastRetriedAt >= MasteryGapInterval {
		f.LastRetriedAt = now
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("usecase: marshal 积累字段: %w", err)
		}
		return d.Records.UpdateStatusFields(ctx, rec.RecordID, k12.AccumStatusMastered, nil, string(raw), expectedVersion)
	}
	f.ReviewStage++
	f.LastRetriedAt = now
	due := now + reviewIntervalForStage(f.ReviewStage)
	raw, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("usecase: marshal 积累字段: %w", err)
	}
	return d.Records.UpdateStatusFields(ctx, rec.RecordID, k12.AccumStatusReviewing, &due, string(raw), expectedVersion)
}

func toItems(recs []*records.AgentRecord) []ReviewItem {
	out := make([]ReviewItem, 0, len(recs))
	for _, r := range recs {
		f, _ := k12.ParseMistakeFields(r.Fields)
		out = append(out, ReviewItem{Record: r, Fields: f})
	}
	return out
}
