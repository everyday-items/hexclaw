package memory

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// ⑦ 优雅停机（cancel+wait）保证后台相 goroutine 必退出、不泄漏。
func TestStartScheduledPhase_GracefulStopNoLeak(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	fm := newFM(t)
	stop := fm.StartScheduledPhase(context.Background(), PhaseReflect, time.Hour, time.Hour,
		func(_ context.Context, _ time.Time) error { return nil })
	stop() // 返回后 goroutine 必已退出
}

// 交接「做梦只在 app 开着时跑、关机不补」局限的修复回归锁：墙钟持久化 + 开机补跑。

// ① 纯决策核：首次→锚定、到期(含恰好等于 interval)→补跑、未到期→等待。
func TestDecidePhaseStart(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		last time.Time
		want phaseStartAction
	}{
		{"首次无记录→锚定", time.Time{}, phaseAnchor},
		{"刚跑过→等待", now.Add(-30 * time.Minute), phaseWait},
		{"恰好到期→补跑", now.Add(-1 * time.Hour), phaseCatchUp},
		{"早已过期→补跑", now.Add(-49 * time.Hour), phaseCatchUp},
	}
	for _, c := range cases {
		if got := decidePhaseStart(c.last, time.Hour, now); got != c.want {
			t.Errorf("%s：want %d got %d", c.name, c.want, got)
		}
	}
}

// ② 持久化跨「重启」：mark 后用新 clock（模拟重启）读同一目录，时间戳仍在，且不冲掉兄弟相。
func TestPhaseClock_PersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	c1 := newPhaseClock(dir)
	tReflect := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	tDream := time.Now().Add(-200 * time.Hour).Truncate(time.Second)
	c1.mark(PhaseReflect, tReflect)
	c1.mark(PhaseDream, tDream) // 兄弟相，read-modify-write 不应互相冲掉

	c2 := newPhaseClock(dir) // 「重启」：新进程新 clock，同一持久化文件
	if got := c2.lastRun(PhaseReflect); !got.Equal(tReflect) {
		t.Errorf("reflect 墙钟未持久化：want %v got %v", tReflect, got)
	}
	if got := c2.lastRun(PhaseDream); !got.Equal(tDream) {
		t.Errorf("dream 墙钟未持久化（兄弟相被冲掉？）：want %v got %v", tDream, got)
	}
	if got := c2.lastRun(PhaseProfile); !got.IsZero() {
		t.Errorf("未记录的相应为零值，得 %v", got)
	}
}

// ③ 开机补跑：上次运行已超 interval → 启动即在后台静默补跑一次，且把水位(since)透传给 run。
func TestStartScheduledPhase_CatchUpOnOverdue(t *testing.T) {
	fm := newFM(t)
	seeded := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	fm.clock.mark(PhaseDream, seeded) // 模拟上次做梦在 3 小时前

	var count int32
	gotSince := make(chan time.Time, 1)
	stop := fm.StartScheduledPhase(context.Background(), PhaseDream, time.Hour, time.Hour,
		func(_ context.Context, since time.Time) error {
			atomic.AddInt32(&count, 1)
			gotSince <- since
			return nil
		})
	defer stop()

	select {
	case since := <-gotSince:
		if !since.Equal(seeded) {
			t.Errorf("补跑应把上次水位透传给 run：want %v got %v", seeded, since)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("到期未触发开机补跑")
	}
	time.Sleep(150 * time.Millisecond) // 确认只补跑 1 次（interval=1h 不会再 tick）
	if n := atomic.LoadInt32(&count); n != 1 {
		t.Errorf("应只补跑 1 次，得 %d", n)
	}
	if last := fm.clock.lastRun(PhaseDream); !last.After(seeded) {
		t.Errorf("补跑成功后墙钟应推进，仍为 %v", last)
	}
}

// ④ 首次启动：无历史 → 仅锚定墙钟、不补跑（避免装机首启即跑空活/无谓 LLM）。
func TestStartScheduledPhase_AnchorOnFirstStart_NoRun(t *testing.T) {
	fm := newFM(t)
	var count int32
	stop := fm.StartScheduledPhase(context.Background(), PhaseDream, time.Hour, time.Hour,
		func(_ context.Context, _ time.Time) error {
			atomic.AddInt32(&count, 1)
			return nil
		})
	defer stop()

	time.Sleep(250 * time.Millisecond)
	if n := atomic.LoadInt32(&count); n != 0 {
		t.Errorf("首次启动不应补跑，却跑了 %d 次", n)
	}
	if fm.clock.lastRun(PhaseDream).IsZero() {
		t.Error("首次启动应锚定墙钟（下次重启才能据此判断是否补跑）")
	}
}

// ⑤ 未到期：上次刚跑过 → 启动不补跑（等下一个 ticker），墙钟不变。
func TestStartScheduledPhase_WaitWhenRecent(t *testing.T) {
	fm := newFM(t)
	recent := time.Now().Add(-1 * time.Minute).Truncate(time.Second)
	fm.clock.mark(PhaseReflect, recent)

	var count int32
	stop := fm.StartScheduledPhase(context.Background(), PhaseReflect, time.Hour, time.Hour,
		func(_ context.Context, _ time.Time) error {
			atomic.AddInt32(&count, 1)
			return nil
		})
	defer stop()

	time.Sleep(250 * time.Millisecond)
	if n := atomic.LoadInt32(&count); n != 0 {
		t.Errorf("未到期不应补跑，却跑了 %d 次", n)
	}
	if got := fm.clock.lastRun(PhaseReflect); !got.Equal(recent) {
		t.Errorf("未跑则墙钟不应变：want %v got %v", recent, got)
	}
}

// ⑥ run 失败不推进墙钟 → 下次（重启/tick）仍会补跑（不静默吞掉一整轮做梦）。
func TestStartScheduledPhase_FailureDoesNotAdvanceClock(t *testing.T) {
	fm := newFM(t)
	seeded := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	fm.clock.mark(PhaseDream, seeded)

	ran := make(chan struct{}, 1)
	stop := fm.StartScheduledPhase(context.Background(), PhaseDream, time.Hour, time.Hour,
		func(_ context.Context, _ time.Time) error {
			ran <- struct{}{}
			return context.Canceled // 模拟失败
		})
	defer stop()

	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("到期未触发补跑")
	}
	time.Sleep(150 * time.Millisecond)
	if got := fm.clock.lastRun(PhaseDream); !got.Equal(seeded) {
		t.Errorf("补跑失败不应推进墙钟（否则丢一整轮）：want %v got %v", seeded, got)
	}
}
