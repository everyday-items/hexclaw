package usecase

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

// PracticeSetItem 练习集条目（记录 + 领域字段）。
type PracticeSetView struct {
	Record *records.AgentRecord
	Fields k12.PracticeSetFields
}

// CreatePracticeSet 新建练习集草稿（PRD §3.8）。幂等去重：同来源+标题+题目内容命中已存在则不重复入库。
func (d Deps) CreatePracticeSet(ctx context.Context, agentName, sourceSession string, f k12.PracticeSetFields) (recordID string, created bool, err error) {
	if f.GradeTerm == "" {
		f.GradeTerm = d.creationGradeTerm(ctx, agentName, "")
	}
	rec, err := k12.NewPracticeSetRecord(agentName, sourceSession, f)
	if err != nil {
		return "", false, err
	}
	created, err = d.Records.Put(ctx, rec)
	if err != nil {
		return "", false, fmt.Errorf("usecase: 练习集入库: %w", err)
	}
	return rec.RecordID, created, nil
}

// ListPracticeSets 列练习集（status 空 = 全部）。
func (d Deps) ListPracticeSets(ctx context.Context, agentName, status string) ([]PracticeSetView, error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionPracticeSet, status)
	if err != nil {
		return nil, fmt.Errorf("usecase: 列练习集: %w", err)
	}
	out := make([]PracticeSetView, 0, len(recs))
	for _, r := range recs {
		f, _ := k12.ParsePracticeSetFields(r.Fields)
		out = append(out, PracticeSetView{Record: r, Fields: f})
	}
	return out, nil
}

// GetPracticeSet 取单个练习集（带 owner 校验：跨实例返回 not found）。
func (d Deps) GetPracticeSet(ctx context.Context, agentName, recordID string) (PracticeSetView, error) {
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return PracticeSetView{}, fmt.Errorf("usecase: 取练习集: %w", err)
	}
	if rec == nil || rec.AgentName != agentName || rec.Collection != k12.CollectionPracticeSet {
		return PracticeSetView{}, fmt.Errorf("usecase: 练习集不存在或不属于该实例")
	}
	f, _ := k12.ParsePracticeSetFields(rec.Fields)
	if f.DeliveryBatchID != "" {
		batch, batchErr := d.Records.GetDeliveryBatch(ctx, agentName, f.DeliveryBatchID)
		if batchErr != nil {
			return PracticeSetView{}, fmt.Errorf("usecase: 取练习集投递批次: %w", batchErr)
		}
		f.DeliveryStatus = string(batch.Status)
	}
	return PracticeSetView{Record: rec, Fields: f}, nil
}

// VerifyPracticeItem 标记某练习项的验证状态（PRD §4.7）。verified 必须带验证方式证据，
// 且该学科的确定性验证器必须已过 eval 质量门（2026-07-18 裁决）——防 AI 判断冒充确定性验证。
func (d Deps) VerifyPracticeItem(ctx context.Context, agentName, recordID, itemID, status, evidence string) error {
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return err
	}
	if v.Record.Status != k12.PracticeStatusDraft {
		return fmt.Errorf("usecase: 只有草稿态练习集可修改题目验证状态，当前 %s", v.Record.Status)
	}
	if status == k12.PracticeItemVerified && evidence == "" {
		return fmt.Errorf("usecase: verified 必须提供验证方式证据（独立验算/字符比对/沙箱运行…）")
	}
	found := false
	for i := range v.Fields.Items {
		if v.Fields.Items[i].ItemID == itemID {
			if status == k12.PracticeItemVerified && !k12.SubjectVerifierGatePassed(v.Fields.Items[i].Subject) {
				// §4.11 家长向术语（2026-07-18 新增禁用）：错误文案不出现「验证器/质量门/达门」。
				return fmt.Errorf("usecase: 学科 %q 暂不支持自动验证，不得标记 verified；请用 needs_review 诚实标注", v.Fields.Items[i].Subject)
			}
			v.Fields.Items[i].VerificationStatus = status
			v.Fields.Items[i].VerificationEvidence = evidence
			if status != k12.PracticeItemVerified {
				v.Fields.Items[i].BlockedReason = blockedReasonFor(status)
			} else {
				v.Fields.Items[i].BlockedReason = ""
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("usecase: 练习项 %s 不存在", itemID)
	}
	return d.savePracticeFields(ctx, v, v.Record.Status)
}

func blockedReasonFor(status string) string {
	switch status {
	case k12.PracticeItemNeedsReview:
		return "只有模型判断或证据冲突，待复核"
	case k12.PracticeItemRejected:
		return "超纲、答案错误、重复或结构不合格"
	case k12.PracticeItemStale:
		return "来源题、教材或验证策略变更导致证据失效"
	default:
		return "验证尚未完成"
	}
}

// FinalizeBasket 固化出卷（2026-07-18 购物车裁决，架构设计 §3.8）：打印/发送即家长确认。
// 从 draft 一步到 assigned（待完成）；confirmed 仅作为审计中间态由本命令隐式写入，
// 不存在独立的家长确认命令。固化逐题跳过非 verified 项（skipped 返回并持久化），
// 整卷不被拒绝；仅当一道 verified 题都没有时拒绝出卷。
// via: "print"（打印，delivery 保持 not_sent）| "send"（服务端解析全部 active direct bindings）。
// 末尾可变参数只为源码兼容旧调用点；其中的历史 target 值会被忽略，绝不参与解析或持久化。
func (d Deps) FinalizeBasket(ctx context.Context, agentName, recordID, via string, _ ...string) (PracticeSetView, int, error) {
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return PracticeSetView{}, 0, err
	}
	var deliveryTargets []ResolvedDeliveryTarget
	switch via {
	case "print":
	case "send":
		// A completed HTTP replay reads the frozen batch and never consults the
		// current mutable binding set.
		if v.Record.Status == k12.PracticeStatusAssigned &&
			v.Fields.FinalizedVia == "send" && v.Fields.DeliveryBatchID != "" {
			return v, v.Fields.SkippedBlockedCount, nil
		}
		deliveryTargets, err = d.ResolveDeliveryTargets(ctx, agentName)
		if err != nil {
			return PracticeSetView{}, 0, err
		}
	default:
		return PracticeSetView{}, 0, fmt.Errorf("usecase: 固化方式非法 %q（print/send）", via)
	}
	if v.Record.Status != k12.PracticeStatusDraft {
		// Recovery for a crash after the PracticeSet reached assigned but before
		// its batch link was written. No provider request can have happened
		// without the batch, so resolving the current bindings is still safe.
		if via == "send" && v.Record.Status == k12.PracticeStatusAssigned &&
			v.Fields.FinalizedVia == "send" && v.Fields.DeliveryBatchID == "" {
			v, err = d.finishPracticeSetDelivery(ctx, v, deliveryTargets)
			return v, v.Fields.SkippedBlockedCount, err
		}
		return PracticeSetView{}, 0, fmt.Errorf("usecase: 只有待打印（draft）练习集可固化出卷，当前 %s", v.Record.Status)
	}
	publishable, skipped := k12.PublishableItems(v.Fields)
	if len(publishable) == 0 {
		// §4.11 家长向术语：不出现「篮子」，统一「待打印」。
		return PracticeSetView{}, 0, fmt.Errorf("usecase: 待打印中没有已验证题目，不能出卷；%d 道题待验证或已阻断", skipped)
	}
	// 阻断题保留在聚合内供审计与补验证，但绝不进入打印版本（INV-010 新表述）。
	for i := range v.Fields.Items {
		if !k12.PracticeItemPublishable(v.Fields.Items[i]) && v.Fields.Items[i].BlockedReason == "" {
			v.Fields.Items[i].BlockedReason = blockedReasonFor(v.Fields.Items[i].VerificationStatus)
		}
	}
	v.Fields.QuestionArtifact = "qsheet-" + recordID
	v.Fields.AnswerArtifact = "asheet-" + recordID
	v.Fields.SkippedBlockedCount = skipped
	ts := time.Now()
	if d.Now != nil { // 与 pipeline.go 同款防护：装配层可不注入 Now
		ts = time.Unix(d.Now(), 0)
	}
	// 卷名规则（§3.8 第 2 条）：篮子默认名不入历史；显式命名的卷（如测试/导入）保留原名。
	if v.Fields.Title == basketTitle {
		v.Fields.Title = k12.GeneratePaperTitle(v.Fields, ts)
	}
	// 呈现物（§4.13）：按学科分组的卷面题号、固化时间与方式。
	// send 的卷面号由下面的跨聚合事务分配；print 仍走原生打印命令的兼容路径。
	k12.AssignPaperSeqs(v.Fields.Items)
	// #4c（2026-07-18）：入卷题铸造独立 practice_problem_id——变式题面≠原题，
	// 复批 Attempt 归属铸造 Problem，不挂来源错题；SourceProblemID 保留为 derived_from。
	for i := range v.Fields.Items {
		if k12.PracticeItemPublishable(v.Fields.Items[i]) && v.Fields.Items[i].PracticeProblemID == "" {
			v.Fields.Items[i].PracticeProblemID = "pprob-" + recordID[:min(8, len(recordID))] + "-" + v.Fields.Items[i].ItemID
		}
	}
	v.Fields.FinalizedAt = ts.Unix()
	v.Fields.FinalizedVia = via
	v.Fields.SourceKind = k12.AggregateSourceKind(v.Fields, v.Fields.SourceKind)

	if via == "send" {
		term := ""
		if d.Profiles != nil {
			if profile, profileErr := d.GetProfile(ctx, agentName); profileErr == nil {
				term = profile.GradeTerm
			}
		}
		batch, replay, finalizeErr := d.Records.FinalizePracticeSetWithDeliveryBatch(
			ctx,
			agentName,
			recordID,
			v.Record.Version,
			ts.Unix(),
			v.Fields,
			func(factoryCtx context.Context, fields k12.PracticeSetFields) (k12.DeliveryBatch, error) {
				markdown := k12.RenderPaperMarkdown(
					fields,
					k12.PaperKindQuestion,
					k12.PaperMeta{Term: term, Date: ts, Preview: false},
				)
				return d.buildPreparedTextBatch(
					factoryCtx,
					agentName,
					"practice_set_question",
					recordID,
					markdown,
					deliveryTargets,
				)
			},
		)
		if finalizeErr != nil {
			return PracticeSetView{}, 0, finalizeErr
		}
		if !replay {
			if _, sendErr := d.sendDeliveryBatch(ctx, batch); sendErr != nil {
				current, getErr := d.GetPracticeSet(ctx, agentName, recordID)
				if getErr != nil {
					return PracticeSetView{}, 0, getErr
				}
				return current, skipped, sendErr
			}
		}
		current, getErr := d.GetPracticeSet(ctx, agentName, recordID)
		return current, skipped, getErr
	}

	v.Fields.PaperNo, err = d.Records.ReservePracticePaperNo(ctx, agentName, ts.Unix())
	if err != nil {
		return PracticeSetView{}, 0, fmt.Errorf("usecase: 预占卷面号: %w", err)
	}
	// 隐式 confirmed（审计中间态）→ assigned，两次状态写入同一命令内完成。
	if err := d.savePracticeFields(ctx, v, k12.PracticeStatusConfirmed); err != nil {
		return PracticeSetView{}, 0, err
	}
	v, err = d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return PracticeSetView{}, 0, err
	}
	if err := d.savePracticeFields(ctx, v, k12.PracticeStatusAssigned); err != nil {
		return PracticeSetView{}, 0, err
	}
	v, err = d.GetPracticeSet(ctx, agentName, recordID)
	return v, skipped, err
}

func (d Deps) finishPracticeSetDelivery(
	ctx context.Context,
	v PracticeSetView,
	targets []ResolvedDeliveryTarget,
) (PracticeSetView, error) {
	paper, err := d.RenderPracticePaper(
		ctx, v.Record.AgentName, v.Record.RecordID, k12.PaperKindQuestion,
	)
	if err != nil {
		return v, err
	}
	batch, _, err := d.prepareAndSendTextBatchWithTargets(
		ctx,
		v.Record.AgentName,
		"practice_set_question",
		v.Record.RecordID,
		paper.Markdown,
		targets,
	)
	if err != nil {
		return v, err
	}
	v, err = d.GetPracticeSet(ctx, v.Record.AgentName, v.Record.RecordID)
	if err != nil {
		return PracticeSetView{}, err
	}
	v.Fields.DeliveryBatchID = batch.BatchID
	v.Fields.DeliveryStatus = string(batch.Status)
	v.Fields.DeliveryTarget = ""
	if err := d.savePracticeFields(ctx, v, k12.PracticeStatusAssigned); err != nil {
		return PracticeSetView{}, err
	}
	return d.GetPracticeSet(ctx, v.Record.AgentName, v.Record.RecordID)
}

// basketTitle 待打印篮的固定标题；单 Learner 单篮的查找锚点之一。
const basketTitle = "待打印篮"

// findBasket 找当前 Learner 的待打印篮（draft 态练习集）。单 Learner 单篮：
// 存在多个 draft（历史遗留）时取最早创建的一个，不再新建。
func (d Deps) findBasket(ctx context.Context, agentName string) (*PracticeSetView, error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionPracticeSet, k12.PracticeStatusDraft)
	if err != nil {
		return nil, fmt.Errorf("usecase: 查找待打印练习集: %w", err)
	}
	if len(recs) == 0 {
		return nil, nil
	}
	r := recs[len(recs)-1] // ListByScope 倒序时末位为最早；顺序时也稳定取一端，保证篮唯一
	f, _ := k12.ParsePracticeSetFields(r.Fields)
	return &PracticeSetView{Record: r, Fields: f}, nil
}

// AddToBasket 装篮（2026-07-18 购物车裁决）：单 Learner 单篮、按题幂等去重。
// 篮不存在则创建（draft）；同题（同来源题 ID 或同规范化题面）重复装篮返回 added=false。
func (d Deps) AddToBasket(ctx context.Context, agentName, sourceSession string, item k12.PracticeItem) (string, bool, error) {
	basket, err := d.findBasket(ctx, agentName)
	if err != nil {
		return "", false, err
	}
	if basket == nil {
		f := k12.PracticeSetFields{SourceKind: k12.PracticeSourceMixed, Title: basketTitle,
			Items: []k12.PracticeItem{item}}
		id, _, err := d.CreatePracticeSet(ctx, agentName, sourceSession, f)
		if err != nil {
			return "", false, err
		}
		return id, true, nil
	}
	for _, it := range basket.Fields.Items {
		if samePracticeItem(it, item) {
			return basket.Record.RecordID, false, nil // 装篮幂等去重
		}
	}
	item = normalizeBasketItem(item, len(basket.Fields.Items))
	basket.Fields.Items = append(basket.Fields.Items, item)
	if err := d.savePracticeFields(ctx, *basket, basket.Record.Status); err != nil {
		return "", false, err
	}
	return basket.Record.RecordID, true, nil
}

// fillBasketWeeklyCap 单次自动装篮上限（§3.13 每周复习默认规模）：超出按 due_at 最早优先
// （ReviewQueue 本身按到期升序），其余留待下周。
const fillBasketWeeklyCap = 10

// FillBasketFromDue 每周自动装篮（§3.13 每周复习 · §3.8 装篮入口2，执行计划 §3.0 治本②）：
// 取当前 Learner 到期复习项（ReviewQueue 口径：错题本 + 积累本纠错型，due 升序），
// 逐题**原题重现**装篮（added_via=weekly）。验证态诚实标注（§4.7）：
//   - 有 canonical_answer 且学科验证器已过质量门 → verified
//     （数学「原题重现·已带批改答案」；语英「原词重现·字符比对」）；
//   - 语英默写类：题面「默写：X」原文自含 / 积累纠错型内容即答案 → 可 verified（字符比对）；
//   - 无答案（老记录）或学科未达门 → pending 诚实阻断，绝不虚标 verified。
//
// 幂等复用 AddToBasket 按题去重（同 source_problem_id / 同规范化题面）——cron 重触发安全。
// 返回 added（本次新装）/ skipped（去重命中或该科不可组卷而跳过）。
//
// 抽查复验衔接（§3.6，2026-07-18 落地）：spot_check_state=scheduled 的错题**优先入卷**、
// 每次 ≤2 道（规则 2a，超出顺延下周）、每题一道原题重现、added_via=spot_check（内部标识，
// 呈现不打抽查标签）；已在在途卷（固化未复批）中的抽查不重复安排。
// 节奏申报：文档节奏为「下一周期混入周卷」，此处以每次 FillBasketFromDue（周五 cron /
// 手动生成复习卷共用入口）为周期节拍——最保守解读，不新增独立调度。
func (d Deps) FillBasketFromDue(ctx context.Context, agentName, sourceSession string) (added, skipped int, err error) {
	if agentName == "" {
		return 0, 0, fmt.Errorf("%w: agentName 不可空", ErrInvalidInput)
	}
	spotItems, err := d.spotCheckCandidates(ctx, agentName)
	if err != nil {
		return 0, 0, err
	}
	for _, it := range spotItems {
		if added >= fillBasketWeeklyCap {
			break
		}
		pi := weeklyPracticeItem(it)
		pi.AddedVia = k12.PracticeAddedViaSpotCheck
		_, ok, aerr := d.AddToBasket(ctx, agentName, sourceSession, pi)
		if aerr != nil {
			return added, skipped, aerr
		}
		if ok {
			added++
		} else {
			skipped++
		}
	}
	items, err := d.ReviewQueue(ctx, agentName)
	if err != nil {
		return 0, 0, err
	}
	for _, it := range items {
		if added >= fillBasketWeeklyCap {
			break // §3.13：单次装篮上限 10 道，其余留待下周
		}
		pi := weeklyPracticeItem(it)
		if !k12.PracticeSubjectAllowed(pi.Subject) {
			skipped++ // 物理/化学等暂不可组卷学科：留在复习队列，不进打印卷
			continue
		}
		_, ok, aerr := d.AddToBasket(ctx, agentName, sourceSession, pi)
		if aerr != nil {
			return added, skipped, aerr
		}
		if ok {
			added++
		} else {
			skipped++
		}
	}
	return added, skipped, nil
}

// spotCheckWeeklyCap 每次装篮混入的抽查复验题上限（§3.6 规则 2a：每周 ≤2 道，
// 防「确认了一堆」把周卷塞满抽查题；超出的顺延到下次装篮）。
const spotCheckWeeklyCap = 2

// spotCheckCandidates 取本次应混入周卷的抽查复验错题（§3.6）。
// 家长确认不再篡改 evidence status，所以候选由独立 scheduled + due_at 事实选出；
// 历史 mastered+scheduled 行仍可继续完成一次抽查。
func (d Deps) spotCheckCandidates(ctx context.Context, agentName string) ([]ReviewItem, error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionMistakes, "")
	if err != nil {
		return nil, fmt.Errorf("usecase: 取抽查复验候选: %w", err)
	}
	inFlight, err := d.spotCheckInFlight(ctx, agentName)
	if err != nil {
		return nil, err
	}
	var out []ReviewItem
	now := d.now()
	for _, r := range recs {
		f, _ := k12.ParseMistakeFields(r.Fields)
		if r.Status == k12.StatusArchived ||
			f.SpotCheckState != k12.SpotCheckScheduled ||
			r.DueAt == nil || *r.DueAt > now ||
			inFlight[r.RecordID] {
			continue
		}
		out = append(out, ReviewItem{Record: r, Fields: f})
	}
	sort.SliceStable(out, func(i, j int) bool {
		fi, _ := k12.ParseMistakeFields(out[i].Record.Fields)
		fj, _ := k12.ParseMistakeFields(out[j].Record.Fields)
		ti, tj := fi.ParentConfirmedAt, fj.ParentConfirmedAt
		if ti == 0 {
			ti = out[i].Record.CreatedAt
		}
		if tj == 0 {
			tj = out[j].Record.CreatedAt
		}
		return ti < tj
	})
	if len(out) > spotCheckWeeklyCap {
		out = out[:spotCheckWeeklyCap]
	}
	return out, nil
}

// spotCheckInFlight 返回已固化未复批（在途）卷中的抽查来源题集合——一次抽查安排只对应
// 一次卷面机会，在途期间不重复装篮（结论落地后 scheduled 会翻成 passed/failed，自然退出候选）。
func (d Deps) spotCheckInFlight(ctx context.Context, agentName string) (map[string]bool, error) {
	sets, err := d.Records.ListByScope(ctx, agentName, k12.CollectionPracticeSet, "")
	if err != nil {
		return nil, fmt.Errorf("usecase: 查在途抽查: %w", err)
	}
	out := map[string]bool{}
	for _, r := range sets {
		switch r.Status {
		case k12.PracticeStatusConfirmed, k12.PracticeStatusAssigned, k12.PracticeStatusSubmitted:
		default:
			continue // draft 由装篮幂等去重兜住；graded/closed/cancelled 结论已出或已作废
		}
		f, _ := k12.ParsePracticeSetFields(r.Fields)
		for _, it := range f.Items {
			if it.AddedVia == k12.PracticeAddedViaSpotCheck && it.SourceProblemID != "" && it.ResultCorrect == nil {
				out[it.SourceProblemID] = true
			}
		}
	}
	return out, nil
}

// weeklyPracticeItem 把一条到期复习项映射为原题重现练习项（验证态按 §4.7 诚实标注）。
func weeklyPracticeItem(it ReviewItem) k12.PracticeItem {
	subject := it.Subject()
	var question, answer, evidence string
	if it.IsAccum() {
		// 积累纠错型（默写错/错词/语法改错）：原词重现默写，答案 = 原文自含
		// （同 GenerateDictationToBasket 的字符比对口径）。
		question = "默写：" + strings.TrimSpace(it.Accum.Content)
		answer = strings.TrimSpace(it.Accum.Content)
		evidence = "原词重现·字符比对"
	} else {
		question = it.Fields.Question
		answer = strings.TrimSpace(it.Fields.CanonicalAnswer)
		evidence = "原题重现·已带批改答案"
		switch {
		case answer == "":
			// 语英默写类特判：题面「默写：X」原文自含 → 答案可从题面还原（字符比对可判）。
			if rest, ok := strings.CutPrefix(strings.TrimSpace(question), "默写："); ok && strings.TrimSpace(rest) != "" {
				answer = strings.TrimSpace(rest)
				evidence = "原词重现·字符比对"
			}
		case subject == "语文" || subject == "英语":
			evidence = "原词重现·字符比对"
		}
	}
	status, ev := k12.PracticeItemPending, ""
	if answer != "" && k12.SubjectVerifierGatePassed(subject) {
		status, ev = k12.PracticeItemVerified, evidence
	}
	return k12.PracticeItem{
		SourceProblemID:        it.Record.RecordID,
		Subject:                subject,
		AddedVia:               k12.PracticeAddedViaWeekly,
		QuestionMarkdown:       question,
		ExpectedAnswerMarkdown: answer,
		VerificationStatus:     status,
		VerificationEvidence:   ev,
	}
}

// RemoveFromBasket 篮内移除（§3.8 待打印第 4 条）：只出篮，不影响错题状态与复习安排。
func (d Deps) RemoveFromBasket(ctx context.Context, agentName, recordID, itemID string) error {
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return err
	}
	if v.Record.Status != k12.PracticeStatusDraft {
		return fmt.Errorf("usecase: 只有待打印（draft）练习集可移除题目，当前 %s", v.Record.Status)
	}
	// 先按原长度分配：空篮再次移除必须返回“项不存在”，不能因 len-1 为负触发 panic。
	// 实际少一个元素只影响至多一个 slot，不值得用不安全的容量算术换取。
	kept := make([]k12.PracticeItem, 0, len(v.Fields.Items))
	found := false
	var removed k12.PracticeItem
	for _, it := range v.Fields.Items {
		if it.ItemID == itemID {
			found = true
			removed = it
			continue
		}
		kept = append(kept, it)
	}
	if !found {
		return fmt.Errorf("usecase: 练习项 %s 不在待打印列表中", itemID)
	}
	v.Fields.Items = kept
	raw, err := marshalPracticeFields(v.Fields)
	if err != nil {
		return err
	}
	if err := d.Records.RemovePracticeItemAndRetireGeneration(
		ctx, v.Record.AgentName, v.Record.RecordID, itemID,
		removed.GenerationJobID, raw, v.Record.Version,
	); err != nil {
		return fmt.Errorf("usecase: 移除练习项: %w", err)
	}
	return nil
}

func samePracticeItem(a, b k12.PracticeItem) bool {
	if a.SourceProblemID != "" && a.SourceProblemID == b.SourceProblemID {
		return true
	}
	return normalizeQuestion(a.QuestionMarkdown) == normalizeQuestion(b.QuestionMarkdown)
}

func normalizeQuestion(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), ""))
}

func normalizeBasketItem(it k12.PracticeItem, idx int) k12.PracticeItem {
	if it.VerificationStatus == "" {
		it.VerificationStatus = k12.PracticeItemPending
	}
	if it.ItemID == "" {
		it.ItemID = fmt.Sprintf("item-basket-%d-%s", idx, normalizeQuestionDigest(it.QuestionMarkdown))
	}
	return it
}

func normalizeQuestionDigest(q string) string {
	sum := sha1.Sum([]byte(normalizeQuestion(q)))
	return hex.EncodeToString(sum[:4])
}

// countFinalizedSets 该 Learner 已固化（含历史全部非 draft/cancelled）卷数，卷面号序号依据。
func (d Deps) countFinalizedSets(ctx context.Context, agentName string) (int, error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionPracticeSet, "")
	if err != nil {
		return 0, fmt.Errorf("usecase: 统计已固化卷: %w", err)
	}
	n := 0
	for _, r := range recs {
		f, _ := k12.ParsePracticeSetFields(r.Fields)
		if f.PaperNo != "" {
			n++
		}
	}
	return n, nil
}

// PracticeReturnInput 是一次只追加回传批次。Webhook 可以在一个已验签事件中携带
// 多批；SubmitReturns 会先验证完整集合，再用一次聚合写事务提交，禁止“前一批成功、
// 后一批失败”的半状态。
type PracticeReturnInput struct {
	ReturnID string
	AssetID  string
	ItemIDs  []string
}

// SubmitReturn 保留单批命令面，内部统一进入原子多批实现。
func (d Deps) SubmitReturn(ctx context.Context, agentName, recordID, returnID, assetID string, itemIDs []string) (PracticeSetView, error) {
	return d.SubmitReturns(ctx, agentName, recordID, []PracticeReturnInput{{
		ReturnID: returnID, AssetID: assetID, ItemIDs: itemIDs,
	}})
}

// SubmitReturns 原子追加一组作答照片证据（DD-019/DD-028）。所有资产归属、题号、
// return_id 冲突和卷状态在写入前完成验证；最终只调用一次 savePracticeFields。
func (d Deps) SubmitReturns(ctx context.Context, agentName, recordID string, inputs []PracticeReturnInput) (PracticeSetView, error) {
	agentName = strings.TrimSpace(agentName)
	recordID = strings.TrimSpace(recordID)
	if agentName == "" || recordID == "" || len(inputs) == 0 {
		return PracticeSetView{}, fmt.Errorf("%w: agent / record_id / return_assets 均必填", ErrInvalidInput)
	}
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return PracticeSetView{}, err
	}
	if v.Record.Status != k12.PracticeStatusAssigned && v.Record.Status != k12.PracticeStatusSubmitted {
		return PracticeSetView{}, fmt.Errorf("usecase: 只有待完成/已回传卷可回传作答，当前 %s", v.Record.Status)
	}
	byID := make(map[string]int, len(v.Fields.Items))
	for i := range v.Fields.Items {
		byID[v.Fields.Items[i].ItemID] = i
	}
	changed := false
	for _, input := range inputs {
		returnID := strings.TrimSpace(input.ReturnID)
		assetID := strings.TrimSpace(input.AssetID)
		if returnID == "" || assetID == "" || len(input.ItemIDs) == 0 {
			return PracticeSetView{}, fmt.Errorf("%w: return_id / asset_id / item_ids 均必填", ErrInvalidInput)
		}
		owner, ok := assetstore.OwnerOf(assetID)
		if !ok || owner != agentName {
			return PracticeSetView{}, fmt.Errorf("%w: 回传照片不存在或不属于当前辅导实例", ErrInvalidInput)
		}
		if _, err := assetstore.PathFromID(assetID); err != nil {
			return PracticeSetView{}, fmt.Errorf("%w: 回传照片不可用: %v", ErrInvalidInput, err)
		}
		requested := make(map[string]struct{}, len(input.ItemIDs))
		for _, rawID := range input.ItemIDs {
			id := strings.TrimSpace(rawID)
			if id == "" {
				return PracticeSetView{}, fmt.Errorf("%w: item_ids 不得含空值", ErrInvalidInput)
			}
			if _, exists := requested[id]; exists {
				return PracticeSetView{}, fmt.Errorf("%w: item_id %q 重复", ErrInvalidInput, id)
			}
			requested[id] = struct{}{}
		}
		canonical := make([]string, 0, len(requested))
		for _, item := range v.Fields.Items {
			if _, selected := requested[item.ItemID]; selected {
				canonical = append(canonical, item.ItemID)
			}
		}
		if len(canonical) != len(requested) {
			for id := range requested {
				if _, exists := byID[id]; !exists {
					return PracticeSetView{}, fmt.Errorf("%w: 练习项 %s 不在本卷内", ErrInvalidInput, id)
				}
			}
		}
		replayed := false
		for _, prior := range v.Fields.ReturnAssets {
			if prior.ReturnID != returnID {
				continue
			}
			if prior.AssetID == assetID && equalStringSlice(prior.ItemIDs, canonical) {
				replayed = true
				break
			}
			return PracticeSetView{}, fmt.Errorf("%w: return_id %q 已绑定其他回传载荷", ErrInvalidInput, returnID)
		}
		if replayed {
			continue
		}
		for _, id := range canonical {
			i := byID[id]
			if !k12.PracticeItemPublishable(v.Fields.Items[i]) {
				return PracticeSetView{}, fmt.Errorf("%w: 练习项 %s 是被跳过的阻断题，不在卷面上", ErrInvalidInput, id)
			}
			v.Fields.Items[i].Returned = true
		}
		v.Fields.ReturnAssets = append(v.Fields.ReturnAssets, k12.PracticeReturnAsset{
			ReturnID: returnID, AssetID: assetID, ItemIDs: canonical, ReturnedAt: d.now(),
			RegradeStatus: k12.PracticeRegradeQueued, RegradeUpdatedAt: d.now(),
		})
		changed = true
	}
	if !changed {
		return v, nil
	}
	if err := d.savePracticeFields(ctx, v, k12.PracticeStatusSubmitted); err != nil {
		// 并发完全重放的输家回读赢家；所有批次都完全一致才按幂等成功。
		if errors.Is(err, records.ErrVersionConflict) {
			latest, getErr := d.GetPracticeSet(ctx, agentName, recordID)
			if getErr == nil && practiceReturnsContain(latest.Fields.ReturnAssets, inputs) {
				return latest, nil
			}
			if getErr == nil {
				for _, input := range inputs {
					for _, prior := range latest.Fields.ReturnAssets {
						if prior.ReturnID == strings.TrimSpace(input.ReturnID) && prior.AssetID != strings.TrimSpace(input.AssetID) {
							return PracticeSetView{}, fmt.Errorf("%w: return_id %q 已绑定其他回传载荷", ErrInvalidInput, input.ReturnID)
						}
					}
				}
			}
		}
		return PracticeSetView{}, err
	}
	return d.GetPracticeSet(ctx, agentName, recordID)
}

func practiceReturnsContain(prior []k12.PracticeReturnAsset, inputs []PracticeReturnInput) bool {
	for _, input := range inputs {
		found := false
		for _, item := range prior {
			if item.ReturnID == strings.TrimSpace(input.ReturnID) && item.AssetID == strings.TrimSpace(input.AssetID) {
				requested := append([]string(nil), input.ItemIDs...)
				sort.Strings(requested)
				actual := append([]string(nil), item.ItemIDs...)
				sort.Strings(actual)
				if equalStringSlice(actual, requested) {
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// GradePracticeSet 复批（submitted → graded），空结论旧行为：整卷按已回传直接转已批改，
// 不写逐题结论、不联动来源错题（后向兼容入口；新契约走 GradePracticeSetItems）。
// “全部题有结论后卷才转已批改”（§3.8）：尚有入卷题未回传时拒绝整卷 graded，提示可补传。
func (d Deps) GradePracticeSet(ctx context.Context, agentName, recordID string) error {
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return err
	}
	if v.Record.Status != k12.PracticeStatusSubmitted {
		return fmt.Errorf("usecase: 练习集状态须为 submitted 才能复批，当前 %s", v.Record.Status)
	}
	missing := 0
	for _, it := range v.Fields.Items {
		if k12.PracticeItemPublishable(it) && !it.Returned {
			missing++
		}
	}
	if missing > 0 {
		return fmt.Errorf("usecase: 尚有 %d 题未回传，可补传后再复批；已回传题结果不受影响", missing)
	}
	return d.savePracticeFields(ctx, v, k12.PracticeStatusGraded)
}

// PracticeGradeResult 复批逐题结论（§3.8 历史与回传复批 第 3、4 条）。
type PracticeGradeResult struct {
	ItemID  string `json:"item_id"`
	Correct bool   `json:"correct"`
}

// GradePracticeSetItems 逐题结论复批（§3.8 第 3-4 条，2026-07-18 裁决）：
//   - 结论只允许落在入卷（verified）且已有 return_assets 覆盖证据的题上；复批不得
//     反向伪造照片回传事实。
//   - 结论（复批 Attempt 的等价物）挂在练习项自身——其归属对象是固化时铸造的
//     practice_problem_id（#4c 裁决：变式题面≠原题，不得挂来源错题 Problem）；
//     source_problem_id 仅作 derived_from 来源链，用于向来源错题回写复习信号：
//     通过题 → 走 MarkRetried 链路积掌握证据（→retried/mastered）；
//     未通过题 → 对应来源错题 due_at 重置回本周（due=now）、复习轮次重置首档（§4.6）。
//   - 部分回传合法：允许多次调用，每次覆盖已给结论的题，重复同一结论幂等（不二次推进阶梯）；
//     全部入卷题都有结论后卷才转 graded；graded 后仍可修正重批（覆盖结论，卷保持 graded）。
func (d Deps) GradePracticeSetItems(ctx context.Context, agentName, recordID string, results []PracticeGradeResult) (PracticeSetView, error) {
	return d.gradePracticeSetItems(ctx, agentName, recordID, results, k12.PracticeResultSystemVerified)
}

// ConfirmPracticeSetItems is the no-photo/manual fallback. It advances the
// spaced-review schedule so a truthful parent check is useful, but records
// human_confirmed and can never promote a source record to system mastery.
func (d Deps) ConfirmPracticeSetItems(ctx context.Context, agentName, recordID string, results []PracticeGradeResult) (PracticeSetView, error) {
	view, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return PracticeSetView{}, err
	}
	if view.Record.Status == k12.PracticeStatusAssigned {
		if err := d.savePracticeFields(ctx, view, k12.PracticeStatusSubmitted); err != nil {
			return PracticeSetView{}, err
		}
	}
	return d.gradePracticeSetItems(ctx, agentName, recordID, results, k12.PracticeResultHumanConfirmed)
}

func (d Deps) gradePracticeSetItems(
	ctx context.Context,
	agentName, recordID string,
	results []PracticeGradeResult,
	evidence string,
) (PracticeSetView, error) {
	if evidence != k12.PracticeResultSystemVerified && evidence != k12.PracticeResultHumanConfirmed {
		return PracticeSetView{}, fmt.Errorf("%w: 复批证据等级非法 %q", ErrInvalidInput, evidence)
	}
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return PracticeSetView{}, err
	}
	if v.Record.Status != k12.PracticeStatusSubmitted && v.Record.Status != k12.PracticeStatusGraded {
		return PracticeSetView{}, fmt.Errorf("usecase: 练习集状态须为 submitted/graded 才能逐题复批，当前 %s", v.Record.Status)
	}
	if len(results) == 0 {
		return PracticeSetView{}, fmt.Errorf("%w: 逐题复批必须携带至少一条结论（空数组走整卷复批旧入口）", ErrInvalidInput)
	}
	byID := map[string]int{}
	for i := range v.Fields.Items {
		byID[v.Fields.Items[i].ItemID] = i
	}
	for _, res := range results {
		i, ok := byID[res.ItemID]
		if !ok {
			return PracticeSetView{}, fmt.Errorf("%w: 练习项 %s 不在本卷内", ErrInvalidInput, res.ItemID)
		}
		it := &v.Fields.Items[i]
		if !k12.PracticeItemPublishable(*it) {
			return PracticeSetView{}, fmt.Errorf("%w: 练习项 %s 是被跳过的阻断题，不在卷面上，不能给结论", ErrInvalidInput, res.ItemID)
		}
		if evidence == k12.PracticeResultSystemVerified && !it.Returned {
			return PracticeSetView{}, fmt.Errorf("%w: 练习项 %s 尚无 return_assets 照片覆盖证据，不能给复批结论", ErrInvalidInput, res.ItemID)
		}
		if it.ResultCorrect != nil && *it.ResultCorrect == res.Correct && it.ResultEvidence == evidence {
			continue // 幂等：重复同一结论不重复联动错题
		}
		correct := res.Correct
		it.ResultCorrect = &correct
		it.ResultEvidence = evidence
		// Returned=return_assets 照片覆盖投影、ResultCorrect=复批结论（对错），两字段
		// 独立不合并；DD-028 后复批不能反向伪造 Returned，证据必须先由 SubmitReturn 追加。
		if it.SourceProblemID != "" {
			var projectionErr error
			if evidence == k12.PracticeResultSystemVerified {
				projectionErr = d.applyRegradeOutcome(ctx, it.SourceProblemID, correct)
			} else {
				projectionErr = d.applyHumanConfirmedRegradeOutcome(ctx, it.SourceProblemID, correct)
			}
			if projectionErr != nil {
				return PracticeSetView{}, projectionErr
			}
		}
	}
	// 全部入卷题都有结论后卷才转 graded（§3.8 第 4 条）；否则同态写回等待补传。
	newStatus := v.Record.Status
	if allItemsConcluded(v.Fields) {
		newStatus = k12.PracticeStatusGraded
	}
	if err := d.savePracticeFields(ctx, v, newStatus); err != nil {
		return PracticeSetView{}, err
	}
	return d.GetPracticeSet(ctx, agentName, recordID)
}

// applyHumanConfirmedRegradeOutcome mirrors the scheduling effect of a
// truthful manual check without ever writing LastRetriedAt or mastered. Those
// two facts are reserved for system-verified attempts.
func (d Deps) applyHumanConfirmedRegradeOutcome(ctx context.Context, sourceID string, correct bool) error {
	if !correct {
		return d.applyRegradeOutcome(ctx, sourceID, false)
	}
	rec, err := d.Records.Get(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("usecase: 取人工复批来源题 %s: %w", sourceID, err)
	}
	now := d.now()
	switch rec.Collection {
	case k12.CollectionMistakes:
		if rec.Status == k12.StatusMastered {
			return nil
		}
		f, err := k12.ParseMistakeFields(rec.Fields)
		if err != nil {
			return fmt.Errorf("usecase: 解析人工复批错题字段: %w", err)
		}
		f.ReviewStage++
		due := now + reviewIntervalForStage(f.ReviewStage)
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("usecase: marshal 人工复批错题字段: %w", err)
		}
		return d.Records.UpdateStatusFields(
			ctx, rec.RecordID, k12.StatusRetried, &due, string(raw), rec.Version,
		)
	case k12.CollectionAccumulation:
		if rec.Status == k12.AccumStatusMastered || rec.Status == k12.AccumStatusKept {
			return nil
		}
		f, err := k12.ParseAccumFields(rec.Fields)
		if err != nil {
			return fmt.Errorf("usecase: 解析人工复批积累字段: %w", err)
		}
		f.ReviewStage++
		due := now + reviewIntervalForStage(f.ReviewStage)
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("usecase: marshal 人工复批积累字段: %w", err)
		}
		return d.Records.UpdateStatusFields(
			ctx, rec.RecordID, k12.AccumStatusReviewing, &due, string(raw), rec.Version,
		)
	default:
		return fmt.Errorf("usecase: 人工复批来源 %s 不属于可复习记录集（%s）", sourceID, rec.Collection)
	}
}

// allItemsConcluded 是否全部入卷（verified）题都已有逐题结论。
func allItemsConcluded(f k12.PracticeSetFields) bool {
	for _, it := range f.Items {
		if k12.PracticeItemPublishable(it) && it.ResultCorrect == nil {
			return false
		}
	}
	return true
}

// applyRegradeOutcome 复批结论联动来源错题（§3.8 第 3 条）：
// 通过 → MarkRetried 链路（进下一档间隔 / 满足条件升 mastered，积掌握证据）；
// 未通过 → due_at 重置回本周（due=now）、复习轮次重置首档（§4.6：未通过重置回首档），
// 状态不变——回到本周复习队列由 due 驱动，不倒退状态机。
func (d Deps) applyRegradeOutcome(ctx context.Context, sourceID string, correct bool) error {
	rec, err := d.Records.Get(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("usecase: 取复批来源题 %s: %w", sourceID, err)
	}
	// 抽查复验结论优先路由（§3.6）：scheduled 的错题在卷面上的那道题就是抽查题，
	// 结论写 spot_check_state 而非普通复习联动。
	if rec.Collection == k12.CollectionMistakes {
		if f, _ := k12.ParseMistakeFields(rec.Fields); f.SpotCheckState == k12.SpotCheckScheduled {
			return d.applySpotCheckOutcome(ctx, rec, f, correct)
		}
	}
	if correct {
		return d.MarkRetried(ctx, sourceID, rec.Version)
	}
	now := d.now()
	firstDue := now + reviewIntervalForStage(0)
	switch rec.Collection {
	case k12.CollectionMistakes:
		f, _ := k12.ParseMistakeFields(rec.Fields)
		f.ReviewStage = 0
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("usecase: marshal 错题字段: %w", err)
		}
		return d.Records.UpdateStatusFields(ctx, rec.RecordID, rec.Status, &firstDue, string(raw), rec.Version)
	case k12.CollectionAccumulation:
		f, _ := k12.ParseAccumFields(rec.Fields)
		f.ReviewStage = 0
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("usecase: marshal 积累字段: %w", err)
		}
		return d.Records.UpdateStatusFields(ctx, rec.RecordID, rec.Status, &firstDue, string(raw), rec.Version)
	default:
		return fmt.Errorf("usecase: 复批来源 %s 不属于可复习记录集（%s）", sourceID, rec.Collection)
	}
}

// applySpotCheckOutcome 抽查复验结论落地（§3.6 规则 2/3/4）：
//   - 通过 → passed：把本次真实作答按正常 mastery policy 累积，不能把家长确认冒充证据；
//   - 未过 → failed：回到本周复习队列（due=now、mastered→retried——状态机唯一合法回退），
//     间隔档保持确认前档位（确认动作未改 ReviewStage，不清零）；档案据 failed 呈现
//     「家长确认（复验未过）」标注，话术温和不指责由前端文案承担；
//   - 无论通过与否，抽查只此一次：scheduled 翻出后不再进入候选（规则 4 幂等基座）。
func (d Deps) applySpotCheckOutcome(ctx context.Context, rec *records.AgentRecord, f k12.MistakeFields, correct bool) error {
	if correct {
		f.SpotCheckState = k12.SpotCheckPassed
		now := d.now()
		status := rec.Status
		var due *int64
		if status == k12.StatusMastered {
			due = nil // 兼容旧 mastered+scheduled 数据；系统证据已在旧状态中。
		} else if status == k12.StatusRetried && f.LastRetriedAt > 0 &&
			now-f.LastRetriedAt >= MasteryGapInterval {
			f.LastRetriedAt = now
			status = k12.StatusMastered
			due = nil
		} else {
			f.ReviewStage++
			f.LastRetriedAt = now
			nextDue := now + reviewIntervalForStage(f.ReviewStage)
			due = &nextDue
			status = k12.StatusRetried
		}
		raw, err := json.Marshal(f)
		if err != nil {
			return fmt.Errorf("usecase: marshal 错题字段: %w", err)
		}
		return d.Records.UpdateStatusFields(ctx, rec.RecordID, status, due, string(raw), rec.Version)
	}
	f.SpotCheckState = k12.SpotCheckFailed
	raw, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("usecase: marshal 错题字段: %w", err)
	}
	now := d.now()
	status := rec.Status
	if status == k12.StatusMastered {
		status = k12.StatusRetried
	}
	return d.Records.UpdateStatusFields(ctx, rec.RecordID, status, &now, string(raw), rec.Version)
}

// ClosePracticeSet 关闭练习集（graded → closed）。reason 记录触发原因（§3.8，2026-07-18 裁决）：
// manual=家长手动关闭（缺省）/ semester=学期归档；非法值拒绝——closed 必须可审计。
func (d Deps) ClosePracticeSet(ctx context.Context, agentName, recordID, reason string) error {
	if reason == "" {
		reason = k12.PracticeClosedManual
	}
	if reason != k12.PracticeClosedManual && reason != k12.PracticeClosedSemester {
		return fmt.Errorf("usecase: closed_reason 非法 %q（manual/semester）", reason)
	}
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return err
	}
	if v.Record.Status != k12.PracticeStatusGraded {
		return fmt.Errorf("usecase: 练习集状态须为 graded 才能关闭，当前 %s", v.Record.Status)
	}
	v.Fields.ClosedReason = reason
	return d.savePracticeFields(ctx, v, k12.PracticeStatusClosed)
}

// CancelPracticeSet 取消练习集（仅 draft/confirmed → cancelled，PRD §3.8）。
func (d Deps) CancelPracticeSet(ctx context.Context, agentName, recordID string) error {
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return err
	}
	if v.Record.Status != k12.PracticeStatusDraft && v.Record.Status != k12.PracticeStatusConfirmed {
		return fmt.Errorf("usecase: 只有草稿/已确认可取消，当前 %s", v.Record.Status)
	}
	return d.savePracticeFields(ctx, v, k12.PracticeStatusCancelled)
}

func (d Deps) advancePractice(ctx context.Context, agentName, recordID, from, to string) error {
	v, err := d.GetPracticeSet(ctx, agentName, recordID)
	if err != nil {
		return err
	}
	if v.Record.Status != from {
		return fmt.Errorf("usecase: 练习集状态须为 %s 才能进入 %s，当前 %s", from, to, v.Record.Status)
	}
	return d.savePracticeFields(ctx, v, to)
}

// savePracticeFields 以乐观锁写回练习集字段与状态。newStatus 等于当前态时为同态字段更新。
func (d Deps) savePracticeFields(ctx context.Context, v PracticeSetView, newStatus string) error {
	raw, err := marshalPracticeFields(v.Fields)
	if err != nil {
		return err
	}
	if err := d.Records.UpdateStatusFields(ctx, v.Record.RecordID, newStatus, v.Record.DueAt, raw, v.Record.Version); err != nil {
		return fmt.Errorf("usecase: 练习集写回: %w", err)
	}
	return nil
}

func marshalPracticeFields(f k12.PracticeSetFields) (string, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return "", fmt.Errorf("usecase: marshal 练习集字段: %w", err)
	}
	return string(raw), nil
}
