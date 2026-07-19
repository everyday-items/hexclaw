package usecase_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// 回传提醒契约（架构设计-v0.5.0 §3.13 回传提醒规则，2026-07-18 补）：
//   - 扫 finalized_at=昨日（T+1）且仍未回传（assigned）的固化卷 → 生成提醒文案；
//   - 文案含 paper_no 与题数，家长向用语（§4.11：禁「篮子/验证器」等机制词）；
//   - 已回传（submitted+）不提醒；每卷最多提醒一次（reminder_sent_at 持久幂等）；
//   - 家长手动关闭（reminder_dismissed）不提醒；非昨日固化不提醒。

// 回传提醒时区口径：§3.13 默认时区 Asia/Shanghai（无夏令时，固定 +8）。
var reminderLoc = time.FixedZone("Asia/Shanghai", 8*3600)

// finalizeYesterdaySet 建一张验证过的卷并在 finalizeAt 固化（print），返回 recordID 与 paper_no。
func finalizeYesterdaySet(t *testing.T, d usecase.Deps, agent string, finalizeAt time.Time) (string, string) {
	t.Helper()
	ctx := context.Background()
	f := k12.PracticeSetFields{
		SourceKind: k12.PracticeSourceWeekly, Title: "回传提醒契约卷",
		Items: []k12.PracticeItem{
			verifiedItem("q1", "2.8 × 0.65 = ?", "1.82"),
			verifiedItem("q2", "12 ÷ 4 = ?", "3"),
		},
	}
	id, _, err := d.CreatePracticeSet(ctx, agent, "s-reminder", f)
	if err != nil {
		t.Fatal(err)
	}
	d.Now = func() int64 { return finalizeAt.Unix() }
	v, _, err := d.FinalizeBasket(ctx, agent, id, "print", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Fields.PaperNo == "" {
		t.Fatal("固化卷应有 paper_no")
	}
	return id, v.Fields.PaperNo
}

func TestReturnReminder_YesterdayUnreturnedHasText(t *testing.T) {
	d := newDataDeps(t)
	finalizeAt := time.Date(2026, 7, 16, 15, 0, 0, 0, reminderLoc)
	remindAt := time.Date(2026, 7, 17, 20, 0, 0, 0, reminderLoc)
	_, paperNo := finalizeYesterdaySet(t, d, "xiaoming", finalizeAt)

	d.Now = func() int64 { return remindAt.Unix() }
	text, skip, err := d.ReturnReminder(context.Background(), "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	if skip {
		t.Fatal("昨日固化未回传卷应生成提醒，不应 skip")
	}
	if !strings.Contains(text, paperNo) {
		t.Errorf("提醒应含卷面号 %s, got %q", paperNo, text)
	}
	if !strings.Contains(text, "2 题") {
		t.Errorf("提醒应含题数（2 题）, got %q", text)
	}
	// §4.11 家长向用语：机制词禁入。
	for _, banned := range []string{"篮子", "验证器", "质量门", "assigned"} {
		if strings.Contains(text, banned) {
			t.Errorf("提醒文案不得出现机制词 %q, got %q", banned, text)
		}
	}
}

func TestReturnReminder_AlreadyReturnedSkips(t *testing.T) {
	d := newDataDeps(t)
	finalizeAt := time.Date(2026, 7, 16, 15, 0, 0, 0, reminderLoc)
	remindAt := time.Date(2026, 7, 17, 20, 0, 0, 0, reminderLoc)
	id, _ := finalizeYesterdaySet(t, d, "xiaoming", finalizeAt)
	// 昨日卷已回传（哪怕部分回传，状态离开 assigned）→ 不提醒。
	t.Setenv("HEXCLAW_ASSET_ROOT", t.TempDir())
	assetID := saveReturnAsset(t, "xiaoming")
	if _, err := d.SubmitReturn(context.Background(), "xiaoming", id, "return-reminder", assetID, []string{"q1"}); err != nil {
		t.Fatal(err)
	}

	d.Now = func() int64 { return remindAt.Unix() }
	text, skip, err := d.ReturnReminder(context.Background(), "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	if !skip || text != "" {
		t.Fatalf("已回传卷不应提醒, got skip=%v text=%q", skip, text)
	}
}

func TestReturnReminder_OncePerPaper(t *testing.T) {
	d := newDataDeps(t)
	finalizeAt := time.Date(2026, 7, 16, 15, 0, 0, 0, reminderLoc)
	remindAt := time.Date(2026, 7, 17, 20, 0, 0, 0, reminderLoc)
	finalizeYesterdaySet(t, d, "xiaoming", finalizeAt)

	d.Now = func() int64 { return remindAt.Unix() }
	if _, skip, err := d.ReturnReminder(context.Background(), "xiaoming"); err != nil || skip {
		t.Fatalf("第一次应有提醒, skip=%v err=%v", skip, err)
	}
	// 每卷最多提醒一次：reminder_sent_at 持久化，第二次调用（cron 重触发）静默。
	text, skip, err := d.ReturnReminder(context.Background(), "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	if !skip || text != "" {
		t.Fatalf("同卷第二次不应再提醒, got skip=%v text=%q", skip, text)
	}
}

func TestReturnReminder_NotYesterdaySkips(t *testing.T) {
	d := newDataDeps(t)
	finalizeAt := time.Date(2026, 7, 16, 15, 0, 0, 0, reminderLoc)
	finalizeYesterdaySet(t, d, "xiaoming", finalizeAt)

	// 当天（T+0）：还没到 T+1，不提醒。
	d.Now = func() int64 { return time.Date(2026, 7, 16, 20, 0, 0, 0, reminderLoc).Unix() }
	if text, skip, err := d.ReturnReminder(context.Background(), "xiaoming"); err != nil || !skip || text != "" {
		t.Fatalf("固化当天不应提醒, skip=%v text=%q err=%v", skip, text, err)
	}
	// T+2：提醒窗口只有昨日一天，过窗不补发。
	d.Now = func() int64 { return time.Date(2026, 7, 18, 20, 0, 0, 0, reminderLoc).Unix() }
	if text, skip, err := d.ReturnReminder(context.Background(), "xiaoming"); err != nil || !skip || text != "" {
		t.Fatalf("T+2 过窗不应提醒, skip=%v text=%q err=%v", skip, text, err)
	}
}

func TestReturnReminder_DismissedSkips(t *testing.T) {
	d := newDataDeps(t)
	ctx := context.Background()
	finalizeAt := time.Date(2026, 7, 16, 15, 0, 0, 0, reminderLoc)
	remindAt := time.Date(2026, 7, 17, 20, 0, 0, 0, reminderLoc)
	id, _ := finalizeYesterdaySet(t, d, "xiaoming", finalizeAt)

	// 家长手动关闭本卷提醒（reminder_dismissed，§3.13）。
	rec, err := d.Records.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := k12.ParsePracticeSetFields(rec.Fields)
	f.ReminderDismissed = true
	raw, _ := json.Marshal(f)
	if err := d.Records.UpdateStatusFields(ctx, rec.RecordID, rec.Status, rec.DueAt, string(raw), rec.Version); err != nil {
		t.Fatal(err)
	}

	d.Now = func() int64 { return remindAt.Unix() }
	if text, skip, err := d.ReturnReminder(ctx, "xiaoming"); err != nil || !skip || text != "" {
		t.Fatalf("家长关闭本卷提醒后不应提醒, skip=%v text=%q err=%v", skip, text, err)
	}
}
