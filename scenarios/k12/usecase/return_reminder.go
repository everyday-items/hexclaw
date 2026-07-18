package usecase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// 回传提醒（架构设计-v0.5.0 §3.13 回传提醒规则，2026-07-18 补）：闭环最后一米，
// 关键转化不交给家长记性。cron 每日 20:00 扫描节拍（cronspec.go KindReturnReminder），
// "T+1 事件"语义由本层按 finalized_at=昨日 筛选实现。

// reminderLoc 提醒窗口时区（§3.13 默认 Asia/Shanghai；无夏令时，固定 +8，
// 免依赖系统 tzdata）。
var reminderLoc = time.FixedZone("Asia/Shanghai", 8*3600)

// ReturnReminder 生成昨日固化未回传卷的提醒文案（skip=本期无需提醒）。
//
// 规则（§3.13）：
//   - 只扫 finalized_at ∈ 昨日（Asia/Shanghai 日界）且仍为 assigned（待完成，
//     未回传）的卷；已回传（submitted+）不提醒；
//   - 每卷最多提醒一次：发出即写 reminder_sent_at（持久幂等，cron 重触发安全）；
//   - 家长手动关闭（reminder_dismissed）不提醒；
//   - 文案含 paper_no 与题数，家长向用语（§4.11：只说后果不说机制），温和不催促。
func (d Deps) ReturnReminder(ctx context.Context, agentName string) (text string, skip bool, err error) {
	if agentName == "" {
		return "", false, fmt.Errorf("%w: agentName 不可空", ErrInvalidInput)
	}
	recs, err := d.Records.ListByScope(ctx, agentName, k12.CollectionPracticeSet, k12.PracticeStatusAssigned)
	if err != nil {
		return "", false, fmt.Errorf("usecase: 扫描待完成卷: %w", err)
	}
	now := time.Unix(d.now(), 0).In(reminderLoc)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, reminderLoc)
	yesterdayStart := todayStart.AddDate(0, 0, -1)

	var lines []string
	for _, r := range recs {
		f, perr := k12.ParsePracticeSetFields(r.Fields)
		if perr != nil {
			continue // 字段损坏的卷不阻断整轮提醒
		}
		if f.FinalizedAt < yesterdayStart.Unix() || f.FinalizedAt >= todayStart.Unix() {
			continue // 非昨日固化：过窗不补发，未到不早发
		}
		if f.ReminderSentAt != 0 || f.ReminderDismissed {
			continue // 每卷最多一次；家长手动关闭后不再发
		}
		publishable, _ := k12.PublishableItems(f)
		// §3.13 文案口径「昨天的卷子做完了吗？拍照回传就能自动批改」+ paper_no + 题数；
		// 温和、不催促、不制造焦虑（§4.11 家长向用语）。
		lines = append(lines, fmt.Sprintf("昨天的练习卷 %s（共 %d 题）做完了吗？拍照回传就能自动批改。",
			f.PaperNo, len(publishable)))
		f.ReminderSentAt = d.now()
		if err := d.savePracticeFields(ctx, PracticeSetView{Record: r, Fields: f}, r.Status); err != nil {
			return "", false, err
		}
	}
	if len(lines) == 0 {
		return "", true, nil
	}
	return strings.Join(lines, "\n"), false, nil
}
