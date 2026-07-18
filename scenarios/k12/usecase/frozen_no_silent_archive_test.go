package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// v0.5.0 冻结守卫（架构设计-v0.5.0 §3.7 明令）：
// 「未掌握题不因长时间未练习自动隐藏；只按学期策略或家长显式操作归档。」
//
// 原 T2.3 的 30 天静默归档（ArchiveStaleMistakes）与 PRD 冲突，已整体删除；
// 归档只剩三条路径：学期策略 / 家长手动 / 证据已掌握（mastered）。
// 本测试钉死新语义：30 天不练**不归档**——每日提醒跑过之后错题仍留在活跃复习池。
func TestFrozen_NoSilentArchiveAfter30DaysIdle(t *testing.T) {
	ctx := context.Background()
	clock := int64(100_000_000)
	d, _ := newClockDeps(t, &clock)

	// 一条 40 天没动过的错题（早已过期 due）+ 一条顶档今天恰好到期的卡片。
	staleDue := clock - 35*86400
	stale := &records.AgentRecord{
		RecordID: "idle40d", AgentName: "xiaoming", Collection: k12.CollectionMistakes,
		SchemaVersion: 1, Status: k12.StatusNew,
		Fields:    `{"question":"旧题","knowledge_point":"小数乘法"}`,
		DedupeKey: "d-idle", DueAt: &staleDue,
		UpdatedAt: clock - 40*86400, CreatedAt: clock - 50*86400,
	}
	due := clock
	rung4 := &records.AgentRecord{
		RecordID: "rung4", AgentName: "xiaoming", Collection: k12.CollectionMistakes,
		SchemaVersion: 1, Status: k12.StatusRetried,
		Fields:    `{"question":"3.8×3=?","knowledge_point":"小数乘法","review_stage":4}`,
		DedupeKey: "d-rung4", DueAt: &due,
		UpdatedAt: clock - 30*86400, CreatedAt: clock - 90*86400,
	}
	if err := d.Records.ImportRecords(ctx, []*records.AgentRecord{stale, rung4}); err != nil {
		t.Fatal(err)
	}

	text, skip, err := d.DailyReminder(ctx, "xiaoming")
	if err != nil {
		t.Fatal(err)
	}

	// §3.7：长时间未练习不得被自动隐藏——两条都必须保持原状态、不被 archived。
	for id, want := range map[string]string{"idle40d": k12.StatusNew, "rung4": k12.StatusRetried} {
		got, err := d.Records.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != want {
			t.Errorf("§3.7 冻结：%s 不应被静默归档, status=%q want=%q", id, got.Status, want)
		}
	}
	// 且都仍在活跃复习队列里（对家长可见，未被隐藏）。
	q, err := d.ReviewQueue(ctx, "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, it := range q {
		if it.Record != nil {
			found[it.Record.RecordID] = true
		}
	}
	if !found["idle40d"] || !found["rung4"] {
		t.Errorf("过期错题应留在复习队列; queue=%d found=%v skip=%v text=%q", len(q), found, skip, text)
	}
}
