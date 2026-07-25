package usecase

import (
	"context"
	"fmt"
	"strings"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// AccumItem 积累本条目（记录 + 领域字段）。
type AccumItem struct {
	Record *records.AgentRecord
	Fields k12.AccumFields
}

// AddAccumulation 往积累本写一条（语文/英语沉淀，幂等去重）。
// **纠错型**（默写错/错词/语法改错）设首次复习到期 → 进统一复习队列（与错题本同飞轮，PRD §3.5.4）；
// 积累/留档型不设到期。
func (d Deps) AddAccumulation(ctx context.Context, agentName, sourceSession string, f k12.AccumFields) (recordID string, created bool, err error) {
	rec, err := k12.NewAccumRecord(agentName, sourceSession, f)
	if err != nil {
		return "", false, err
	}
	if k12.AccumIsCorrective(f.EntryType) {
		due := d.now() + FirstReviewInterval
		rec.DueAt = &due
	}
	created, err = d.Records.Put(ctx, rec)
	if err != nil {
		return "", false, fmt.Errorf("usecase: 积累入库: %w", err)
	}
	return rec.RecordID, created, nil
}

// SendAccumulation freezes the stored accumulation content and sends it to the
// complete current direct-binding snapshot. The caller cannot supply or
// override message text.
func (d Deps) SendAccumulation(
	ctx context.Context,
	agentName, recordID string,
) (k12.DeliveryBatch, bool, error) {
	agentName = strings.TrimSpace(agentName)
	recordID = strings.TrimSpace(recordID)
	if agentName == "" || recordID == "" {
		return k12.DeliveryBatch{}, false, fmt.Errorf("%w: agentName / recordID 不可空", ErrInvalidInput)
	}
	if d.Records == nil {
		return k12.DeliveryBatch{}, false, ErrDeliveryUnavailable
	}
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf("usecase: 取积累: %w", err)
	}
	if rec == nil || rec.AgentName != agentName || rec.Collection != k12.CollectionAccumulation {
		return k12.DeliveryBatch{}, false, fmt.Errorf("%w: 积累不存在或不属于该实例", records.ErrNotFound)
	}
	fields, err := k12.ParseAccumFields(rec.Fields)
	if err != nil {
		return k12.DeliveryBatch{}, false, fmt.Errorf("usecase: 解析积累: %w", err)
	}
	content := strings.TrimSpace(fields.Content)
	if content == "" {
		return k12.DeliveryBatch{}, false, fmt.Errorf("%w: 积累内容不可空", ErrInvalidInput)
	}
	return d.PrepareAndSendTextBatch(ctx, agentName, "accumulation", rec.RecordID, content)
}

// 默写出题格式参数（架构设计 §3.9，2026-07-18 定死）。
const (
	dictationFullTextMaxRunes = 20  // ≤20 字（单词/短句）→ 全文默写
	dictationMaxRunes         = 100 // >100 字长文不生成默写练习
)

// GenerateDictationToBasket 积累检验出口（§3.9「生成默写题，加入练习集」）：
// 按默写出题格式（2026-07-18 定死）从一条积累生成一道默写题并装篮（added_via=accumulation）：
//   - ≤20 字（单词/短句）→ 全文默写题「默写：{内容}」，答案 = 内容；
//   - 古诗默认补空式（逐句留 1～2 个关键字空）；整首默写须家长显式选择（fullDictation=true）；
//   - >100 字长文拒绝：“内容过长，不适合默写练习”；
//   - 语/英字符级比对验证器已过质量门（§4.7）→ 直接 verified，可入打印卷。
//
// 积累本身无状态机：生成默写题不改积累状态（取用不污染闭环）；装篮幂等去重，家长可移除。
func (d Deps) GenerateDictationToBasket(ctx context.Context, agentName, sourceSession, recordID string, fullDictation bool) (basketID string, added bool, err error) {
	if agentName == "" || recordID == "" {
		return "", false, fmt.Errorf("%w: agentName / recordID 不可空", ErrInvalidInput)
	}
	rec, err := d.Records.Get(ctx, recordID)
	if err != nil {
		return "", false, fmt.Errorf("usecase: 取积累: %w", err)
	}
	if rec == nil || rec.AgentName != agentName || rec.Collection != k12.CollectionAccumulation {
		return "", false, fmt.Errorf("%w: 积累不存在或不属于该实例", records.ErrNotFound)
	}
	f, _ := k12.ParseAccumFields(rec.Fields)
	content := strings.TrimSpace(f.Content)
	runes := []rune(content)
	if len(runes) > dictationMaxRunes {
		return "", false, fmt.Errorf("%w: 内容过长，不适合默写练习", ErrInvalidInput)
	}
	isPoem := f.EntryType == "古诗" || f.EntryType == "古诗积累"
	question := "默写：" + content // 全文默写（≤20 字单词/短句；或家长显式选择整首默写的古诗）
	if isPoem && !fullDictation {
		question = "补空默写：" + blankFillClauses(content) // 古诗默认补空式
	} else if !isPoem && len(runes) > dictationFullTextMaxRunes {
		question = "补空默写：" + blankFillClauses(content) // 20～100 字长句：补空，不整段抄题
	}
	status, evidence := k12.PracticeItemNeedsReview, ""
	if k12.SubjectVerifierGatePassed(f.Subject) {
		status, evidence = k12.PracticeItemVerified, "字符级比对（确定性默写判定，一字不差即正确）"
	}
	item := k12.PracticeItem{
		Subject:                f.Subject,
		AddedVia:               k12.PracticeAddedViaAccumulation,
		QuestionMarkdown:       question,
		ExpectedAnswerMarkdown: content,
		VerificationStatus:     status,
		VerificationEvidence:   evidence,
	}
	return d.AddToBasket(ctx, agentName, sourceSession, item)
}

// blankFillClauses 古诗/长句补空（§3.9）：按标点逐句留 1～2 个关键字空（句末关键字挖空，
// 短句 1 空、≥5 字句 2 空），标点保留——确定性生成，不走模型。
func blankFillClauses(content string) string {
	var b strings.Builder
	clause := []rune{}
	flush := func() {
		n := len(clause)
		if n == 0 {
			return
		}
		blanks := 1
		if n >= 5 {
			blanks = 2
		}
		if blanks >= n { // 单字句保底留原字
			blanks = n - 1
		}
		for i, r := range clause {
			if i >= n-blanks {
				b.WriteRune('＿')
			} else {
				b.WriteRune(r)
			}
		}
		clause = clause[:0]
	}
	for _, r := range content {
		if strings.ContainsRune("，。、；：！？,.;:!?\n", r) {
			flush()
			b.WriteRune(r)
			continue
		}
		clause = append(clause, r)
	}
	flush()
	return b.String()
}

// ListAccumulation 列积累本（可按学科过滤；subject 空 = 全部）。
func (d Deps) ListAccumulation(ctx context.Context, agentName, subject string) ([]AccumItem, error) {
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionAccumulation, "")
	if err != nil {
		return nil, fmt.Errorf("usecase: 列积累本: %w", err)
	}
	out := make([]AccumItem, 0, len(recs))
	for _, r := range recs {
		f, _ := k12.ParseAccumFields(r.Fields)
		if subject != "" && f.Subject != subject {
			continue
		}
		out = append(out, AccumItem{Record: r, Fields: f})
	}
	return out, nil
}
