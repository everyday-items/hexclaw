package usecase_test

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// T2.3（hex-test 审计 · PRD §5.3.1）：超过 30 天未发生任何状态变化 → 自动归档（移出活跃复习池）。
// 原 year-archive cron 只产文案建议、不流转记录状态；archived 从无自动推进路径。
func TestT2_3_ArchiveStaleMistakes(t *testing.T) {
	ctx := context.Background()
	clock := int64(100_000_000)
	d, _ := newClockDeps(t, &clock)

	// 造两条错题：一条 31 天未动（过期），一条今天更新（活跃）。ImportRecord 保留 UpdatedAt。
	stale := &records.AgentRecord{
		RecordID: "stale1", AgentName: "xiaoming", Collection: k12.CollectionMistakes,
		SchemaVersion: 1, Status: k12.StatusNew, Fields: `{"question":"旧题","knowledge_point":"小数乘法"}`,
		DedupeKey: "d-stale", UpdatedAt: clock - 31*86400, CreatedAt: clock - 40*86400,
	}
	fresh := &records.AgentRecord{
		RecordID: "fresh1", AgentName: "xiaoming", Collection: k12.CollectionMistakes,
		SchemaVersion: 1, Status: k12.StatusNew, Fields: `{"question":"新题","knowledge_point":"小数乘法"}`,
		DedupeKey: "d-fresh", UpdatedAt: clock, CreatedAt: clock,
	}
	if err := d.Records.ImportRecords(ctx, []*records.AgentRecord{stale, fresh}); err != nil {
		t.Fatal(err)
	}

	n, err := d.ArchiveStaleMistakes(ctx, "xiaoming")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("应归档 1 条过期错题, got %d", n)
	}
	got, _ := d.Records.Get(ctx, "stale1")
	if got.Status != k12.StatusArchived {
		t.Errorf("31 天未动应 archived, got %q", got.Status)
	}
	gotFresh, _ := d.Records.Get(ctx, "fresh1")
	if gotFresh.Status != k12.StatusNew {
		t.Errorf("活跃错题不应归档, got %q", gotFresh.Status)
	}
}

var _ = usecase.StaleArchiveInterval
