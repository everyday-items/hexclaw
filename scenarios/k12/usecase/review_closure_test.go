package usecase

import (
	"context"
	"errors"
	"math/rand"
	"strconv"
	"testing"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// ── 测试闭环 Method 1：不变量 / property-based fuzz ──────────────────────────
// 随机操作序列跑复习飞轮，断言 5 条业务不变量恒成立。固定种子可复现。
func TestReviewFlywheel_Invariants_Property(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{}) // now()=1000
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))
	const capV1 = int64(14 * 86400)
	allowedV1 := map[int64]bool{
		1 * 86400:  true,
		3 * 86400:  true,
		7 * 86400:  true,
		14 * 86400: true,
	}
	const iterations = 400

	for iter := 0; iter < iterations; iter++ {
		id := seedMistake(t, d, "s"+strconv.Itoa(iter), "小数乘法", "计算失误", 500)
		var prevInterval int64
		steps := rng.Intn(7) // 0~6 次重做
		for s := 0; s < steps; s++ {
			cur, err := d.Records.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			f0, _ := k12.ParseMistakeFields(cur.Fields)
			if err := d.MarkRetried(ctx, id, cur.Version); err != nil {
				t.Fatalf("iter %d step %d: %v", iter, s, err)
			}
			got, _ := d.Records.Get(ctx, id)
			f1, _ := k12.ParseMistakeFields(got.Fields)

			// 不变量①：ReviewStage 严格 +1
			if f1.ReviewStage != f0.ReviewStage+1 {
				t.Fatalf("不变量① stage 应 +1: %d→%d", f0.ReviewStage, f1.ReviewStage)
			}
			interval := *got.DueAt - 1000
			// 不变量②：间隔单调不减（越记得牢推得越远）
			if interval < prevInterval {
				t.Fatalf("不变量② 间隔应单调不减: %d < %d", interval, prevInterval)
			}
			// 不变量③：v1 exact-set 只能是 1/3/7/14 天，末档封顶 14 天；
			// 不能用旧的 30 天宽松上限放过 15/30 天策略漂移。
			if interval > capV1 {
				t.Fatalf("不变量③ v1 间隔应封顶 14 天: %d", interval)
			}
			if !allowedV1[interval] {
				t.Fatalf("不变量③ v1 间隔漂移，必须属于 1/3/7/14 天 exact-set: %d", interval)
			}
			// 不变量④：due 恒在未来
			if *got.DueAt <= 1000 {
				t.Fatalf("不变量④ due 应在未来: %d", *got.DueAt)
			}
			// 不变量⑤：version 严格 +1（乐观锁推进）
			if got.Version != cur.Version+1 {
				t.Fatalf("不变量⑤ version 应 +1: %d→%d", cur.Version, got.Version)
			}
			prevInterval = interval
		}
		// 随机以两次相隔 ≥3 天的系统正确证据收尾 → 断言清 due。
		if rng.Intn(2) == 0 {
			cur, _ := d.Records.Get(ctx, id)
			if cur.Status != k12.StatusRetried {
				d.Now = func() int64 { return 1000 }
				if err := d.MarkRetried(ctx, id, cur.Version); err != nil {
					t.Fatal(err)
				}
				cur, _ = d.Records.Get(ctx, id)
			}
			d.Now = func() int64 { return 1000 + MasteryGapInterval }
			if err := d.MarkRetried(ctx, id, cur.Version); err != nil {
				t.Fatal(err)
			}
			got, _ := d.Records.Get(ctx, id)
			if got.DueAt != nil {
				t.Fatalf("不变量⑥ evidence mastery 应清 due, got %v", *got.DueAt)
			}
			d.Now = func() int64 { return 1000 }
		}
	}
}

// ── 测试闭环 Method 3：乐观锁 / lost-update 防护 ─────────────────────────────
// 陈旧 version 写入（模拟并发另一方读旧版本后写）必须被拒，状态不被二次污染。
// 补充 `go test -race`（数据竞争检测）——两者共同覆盖并发正确性。
func TestMarkRetried_OptimisticLock_RejectsStaleVersion(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()
	id := seedMistake(t, d, "a", "小数乘法", "计算失误", 500)

	cur, _ := d.Records.Get(ctx, id) // version 0
	if err := d.MarkRetried(ctx, id, cur.Version); err != nil {
		t.Fatal(err) // 0→1 成功
	}
	// 再用陈旧 version 0 → 必须冲突（防丢更新）。
	if err := d.MarkRetried(ctx, id, cur.Version); !errors.Is(err, records.ErrVersionConflict) {
		t.Fatalf("陈旧 version 应返回 ErrVersionConflict, got %v", err)
	}
	// 冲突写不得改脏状态：stage 仍为 1。
	got, _ := d.Records.Get(ctx, id)
	f, _ := k12.ParseMistakeFields(got.Fields)
	if f.ReviewStage != 1 {
		t.Errorf("冲突写不应改 stage, got %d", f.ReviewStage)
	}
}

// ── 测试闭环 Method 9：schema 前向兼容矩阵 ──────────────────────────────────
// 老记录（fields JSON 无 review_stage 键）→ 解析为 0，阶梯从 rung 0 正常起步，
// 字段被补齐。证明加 ReviewStage 字段对存量数据零破坏（无需迁移）。
func TestMistakeFields_LegacyWithoutReviewStage_Compat(t *testing.T) {
	d, _ := newPipeline(t, fakeSolver{}, fakeGrader{}, &fakeInsights{})
	ctx := context.Background()

	rec := &records.AgentRecord{
		AgentName: "mingming", Collection: k12.CollectionMistakes, SourceSession: "legacy",
		Fields: `{"question":"3.8×3","knowledge_point":"小数乘法","error_cause":"计算失误"}`,
	}
	due := int64(500)
	rec.DueAt = &due
	if _, err := d.Records.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// 缺字段 → 默认 0。
	f0, _ := k12.ParseMistakeFields(rec.Fields)
	if f0.ReviewStage != 0 {
		t.Errorf("老记录 ReviewStage 应默认 0, got %d", f0.ReviewStage)
	}
	// MarkRetried：rung 0 → rung 1（3 天），字段补齐。
	cur, _ := d.Records.Get(ctx, rec.RecordID)
	if err := d.MarkRetried(ctx, rec.RecordID, cur.Version); err != nil {
		t.Fatal(err)
	}
	got, _ := d.Records.Get(ctx, rec.RecordID)
	f1, _ := k12.ParseMistakeFields(got.Fields)
	if f1.ReviewStage != 1 {
		t.Errorf("老记录重做后 stage 应 1, got %d", f1.ReviewStage)
	}
	if got.DueAt == nil || *got.DueAt != 1000+3*86400 {
		t.Errorf("到期应 3 天（rung 1）, got %v", got.DueAt)
	}
}
