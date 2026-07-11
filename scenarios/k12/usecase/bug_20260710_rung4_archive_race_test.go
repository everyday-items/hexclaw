package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// BUG-20260710（顶档 30 天到期即被归档）：rung 4 的复习间隔 = 30 天 == StaleArchiveInterval，
// 而 DailyReminder 先跑 ArchiveStaleMistakes 再取 ReviewQueue，且归档判据只看
// UpdatedAt ≤ cutoff、不排除"仍有未到期或刚到期 due"的记录 → 顶档卡片在 due 时刻
// （UpdatedAt = now-30d，DueAt = now）恰好满足归档条件，先于进队列被 archived，
// 家长永远见不到这张卡。PRD §5.3.1 语义是「30 天无任何状态变化才归档」——一张按阶梯
// 正常排期、今天刚到期待练的卡片不属于"静默"记录。
func TestBug20260710_Rung4DueCardNotArchivedByDailyReminder(t *testing.T) {
	ctx := context.Background()
	clock := int64(100_000_000)
	d, _ := newClockDeps(t, &clock)

	due := clock // 顶档卡片恰好在今天到期
	rung4 := &records.AgentRecord{
		RecordID: "rung4", AgentName: "xiaoming", Collection: k12.CollectionMistakes,
		SchemaVersion: 1, Status: k12.StatusRetried,
		Fields:    `{"question":"3.8×3=?","knowledge_point":"小数乘法","review_stage":4}`,
		DedupeKey: "d-rung4", DueAt: &due,
		UpdatedAt: clock - 30*86400, CreatedAt: clock - 90*86400,
	}
	if err := d.Records.ImportRecords(ctx, []*records.AgentRecord{rung4}); err != nil {
		t.Fatal(err)
	}

	// DailyReminder 实际顺序：先 ArchiveStaleMistakes，后 ReviewQueue。
	text, skip, err := d.DailyReminder(ctx, "xiaoming")
	if err != nil {
		t.Fatal(err)
	}

	got, err := d.Records.Get(ctx, "rung4")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == k12.StatusArchived {
		t.Fatalf("rung4 到期卡片被 DailyReminder 先行归档（应先进复习队列）: status=%q", got.Status)
	}
	q, err := d.ReviewQueue(ctx, "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range q {
		if it.Record != nil && it.Record.RecordID == "rung4" {
			found = true
		}
	}
	if !found {
		t.Errorf("rung4 到期卡片应出现在复习队列且未被归档；queue=%d skip=%v text=%q", len(q), skip, text)
	}
}
