package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

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
	items := toItems(mrecs) // 错题本（数理化）
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

// MarkMastered 家长「他会了」/复测掌握：状态 → 已掌握，清到期（移出复习队列）。**按 collection 分派**
// 掌握态（错题本 mastered / 积累本 已掌握）——否则会撞记录集状态机校验。
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
	if rec.Collection == k12.CollectionAccumulation {
		return d.Records.UpdateStatusScoped(ctx, agentName, recordID, k12.AccumStatusMastered, nil, expectedVersion)
	}
	f, _ := k12.ParseMistakeFields(rec.Fields)
	if f.SpotCheckState == "" || f.SpotCheckState == k12.SpotCheckNone {
		f.SpotCheckState = k12.SpotCheckScheduled // 最多自动安排一次；间隔档不动（复验未过要恢复原档）
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("usecase: marshal 错题字段: %w", err)
	}
	return d.Records.UpdateStatusFields(ctx, recordID, k12.StatusMastered, nil, string(raw), expectedVersion)
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

// GenerateRetry 「再练一道」：基于某错题出一道**同知识点相似练习题** + 合理答案。
// 返回相似题解 + 证据对象；只读，不改错题状态。
//
// BUG-20260712 治本（真机 68s → 目标 <10s）：练习变式题非高风险批改，走 RetryGenerator 的
// **单次 reasoning 出题**，不再复用为批改正确性设计的**全套对抗多智能体链**（变式生成 → solver →
// verifier → code_exec → self-consistency → 不合格重出）——那套链路重、慢，出练习题用不上。
// Solver 实现 RetryGenerator 即走轻量口（保 subject 路由 + grade 约束）；只实现 Solver 的
// 老实现回退全链，向后兼容。
func (d Deps) GenerateRetry(ctx context.Context, item ReviewItem, grade string) (SolveResult, error) {
	if err := validateGradeInput(grade); err != nil {
		return SolveResult{}, err
	}
	if d.Solver == nil {
		return SolveResult{}, fmt.Errorf("usecase: 未配置 Solver")
	}
	prompt := fmt.Sprintf("参照这道错题出一道同知识点(%s)的相似题并解答：%s", item.Fields.KnowledgePoint, item.Fields.Question)
	// 轻量优先：单次出题，不跑全对抗验算链（延迟优先，练习题容错高）。
	if rg, ok := d.Solver.(RetryGenerator); ok {
		sr, err := rg.GenerateSimilar(ctx, item.Subject(), prompt, grade)
		if err != nil {
			// BUG-2：下游解题执行失败标记为 ErrSolveFailed，HTTP 层据此回 502（非 400）。
			return SolveResult{}, fmt.Errorf("%w: %v", ErrSolveFailed, err)
		}
		return sr, nil
	}
	// 回退：老 Solver（未实现轻量出题）仍走全链，保持向后兼容。
	sr, err := d.solveProblem(ctx, item.Subject(), prompt, grade)
	if err != nil {
		return SolveResult{}, fmt.Errorf("%w: %v", ErrSolveFailed, err)
	}
	return sr, nil
}

// GenerateRetryByRecord 按错题 record_id「再练一道」（HTTP/前端入口）。grade 空时从档案取。
func (d Deps) GenerateRetryByRecord(ctx context.Context, agentName, recordID, grade string) (SolveResult, error) {
	if recordID == "" {
		return SolveResult{}, fmt.Errorf("usecase: recordID 不可空")
	}
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return SolveResult{}, fmt.Errorf("usecase: 取错题: %w", err)
	}
	if rec == nil || rec.AgentName != agentName {
		return SolveResult{}, fmt.Errorf("usecase: 错题不属于该实例: %w", records.ErrNotFound)
	}
	// 语英纠错型：**原词重现 + 确定性字符比对**，不走 solve 验算链（PRD §3.5.4：语英字词再练机制适配）。
	if rec.Collection == k12.CollectionAccumulation {
		af, _ := k12.ParseAccumFields(rec.Fields)
		return SolveResult{
			Solution: fmt.Sprintf("再默一遍（%s·%s）：%s\n判定：一字不差即正确（确定性字符比对，非验算链）。", af.Subject, af.EntryType, af.Content),
			Evidence: SolveEvidence{Verdict: VerdictVerbatim, EvidenceType: EvidenceVerbatim},
		}, nil
	}
	if rec.Collection != k12.CollectionMistakes {
		return SolveResult{}, fmt.Errorf("usecase: 记录不属于 K12 复习集: %w", records.ErrNotFound)
	}
	if grade == "" && d.Profiles != nil {
		if p, perr := d.GetProfile(ctx, agentName); perr == nil {
			grade = p.GradeTerm
		}
	}
	f, _ := k12.ParseMistakeFields(rec.Fields)
	return d.GenerateRetry(ctx, ReviewItem{Record: rec, Fields: f}, grade)
}

func toItems(recs []*records.AgentRecord) []ReviewItem {
	out := make([]ReviewItem, 0, len(recs))
	for _, r := range recs {
		f, _ := k12.ParseMistakeFields(r.Fields)
		out = append(out, ReviewItem{Record: r, Fields: f})
	}
	return out
}
