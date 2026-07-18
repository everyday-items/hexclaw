package usecase

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// TestMarkRetried_EbbinghausLadder 验证间隔阶梯（§4.6 默认策略 v1：1/3/7/14 天）：
// 每次重做做对，ReviewStage +1，下次到期按 3/7/14 天推进（末档封顶），轮次持久化回卡片 fields。
func TestMarkRetried_EbbinghausLadder(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{}) // now()=1000
	ctx := context.Background()
	id := seedMistake(t, d, "a", "小数乘法", "计算失误", 500) // stage 0, version 0

	wantDays := []int64{3, 7, 14, 14, 14} // rung 1..5（第 4 次起封顶 14 天）
	for i, days := range wantDays {
		cur, err := d.Records.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.MarkRetried(ctx, id, cur.Version); err != nil {
			t.Fatalf("第 %d 次 MarkRetried: %v", i+1, err)
		}
		got, err := d.Records.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != k12.StatusRetried {
			t.Errorf("第 %d 次后状态应 retried, got %q", i+1, got.Status)
		}
		f, _ := k12.ParseMistakeFields(got.Fields)
		if f.ReviewStage != i+1 {
			t.Errorf("第 %d 次后 ReviewStage 应 %d, got %d", i+1, i+1, f.ReviewStage)
		}
		wantDue := int64(1000) + days*86400 // now 固定 1000
		if got.DueAt == nil || *got.DueAt != wantDue {
			t.Errorf("第 %d 次到期应 %d（%d 天）, got %v", i+1, wantDue, days, got.DueAt)
		}
		// 乐观锁：每次 UpdateStatusFields 应把 version 推进 1。
		if got.Version != cur.Version+1 {
			t.Errorf("第 %d 次后 version 应 %d, got %d", i+1, cur.Version+1, got.Version)
		}
	}
}

// TestReviewIntervalLadder_MonotonicAndCapped 属性检查（§4.6 策略 v1）：间隔单调不减、
// 封顶 14 天、负轮次退化到 rung 0。防未来调阶梯时不小心弄出"越复习越频繁"的反效果。
func TestReviewIntervalLadder_MonotonicAndCapped(t *testing.T) {
	const capV1 = int64(14 * 86400)
	prev := int64(0)
	for stage := 0; stage < 12; stage++ {
		got := reviewIntervalForStage(stage)
		if got < prev {
			t.Errorf("间隔应单调不减: stage %d = %d < prev %d", stage, got, prev)
		}
		if got > capV1 {
			t.Errorf("间隔应封顶 14 天（策略 v1）: stage %d = %d", stage, got)
		}
		prev = got
	}
	if reviewIntervalForStage(-5) != reviewIntervalForStage(0) {
		t.Error("负轮次应退化到 rung 0")
	}
	if reviewIntervalForStage(100) != capV1 {
		t.Error("超末档应封顶 14 天（策略 v1）")
	}
}

// TestMarkMastered_ClearsDue 掌握后清到期，移出复习队列（回归保护：阶梯改造不破坏掌握态）。
func TestMarkMastered_ClearsDue(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedMistake(t, d, "a", "小数乘法", "计算失误", 500)

	cur, _ := d.Records.Get(ctx, id)
	if err := d.MarkMastered(ctx, "mingming", id, cur.Version); err != nil {
		t.Fatal(err)
	}
	got, _ := d.Records.Get(ctx, id)
	if got.Status != k12.StatusMastered {
		t.Errorf("状态应 mastered, got %q", got.Status)
	}
	if got.DueAt != nil {
		t.Errorf("掌握后应清到期, got %v", *got.DueAt)
	}
	// 掌握的题不再出现在到期队列。
	q, _ := d.ReviewQueue(ctx, "mingming")
	if len(q) != 0 {
		t.Errorf("掌握后复习队列应空, got %d", len(q))
	}
}
