package adapter

import (
	"testing"
	"time"
)

// TestStreamEditLimiter_Fix_v0_3_12_H3 回归测试：
// 修复前各 IM 适配器自己实现限流（飞书 50 字符/1s 硬编码），无连续失败降级、无自适应。
// 若平台限流变严（429 频繁），固定策略会持续撞墙，无法退化。
// 修复后 StreamEditLimiter 提供：字符阈值 + 时间间隔 + 连续 3 次失败自适应降级。
func TestStreamEditLimiter_Fix_v0_3_12_H3(t *testing.T) {
	t.Run("before_fix_behavior_fixed_rate_cannot_adapt", func(t *testing.T) {
		// 修复前：adapter 硬编码的常量策略，遇到 429 仍按相同频率重试 → 撞墙
		t.Logf("修复前：各适配器硬编码限流常量，无失败降级")
	})

	t.Run("after_fix_respects_char_threshold", func(t *testing.T) {
		l := NewStreamEditLimiterWith(40, 10*time.Millisecond, 100*time.Millisecond)
		if l.ShouldEmit(39) {
			t.Error("39 字符 < 40 阈值，不应 emit")
		}
		if !l.ShouldEmit(40) {
			t.Error("40 字符 = 阈值，首次应允许")
		}
	})

	t.Run("after_fix_respects_time_interval", func(t *testing.T) {
		l := NewStreamEditLimiterWith(40, 50*time.Millisecond, 500*time.Millisecond)
		// 首次 emit
		if !l.ShouldEmit(40) {
			t.Fatal("首次应允许")
		}
		l.Emitted(40)

		// 距上次 < 50ms → 不允许（即使字符够）
		if l.ShouldEmit(100) {
			t.Error("时间间隔未到，不应 emit")
		}

		// 等到时间窗
		time.Sleep(60 * time.Millisecond)
		if !l.ShouldEmit(100) {
			t.Error("时间窗已过，应允许")
		}
	})

	t.Run("after_fix_degrades_after_3_consecutive_failures", func(t *testing.T) {
		l := NewStreamEditLimiterWith(40, 10*time.Millisecond, 80*time.Millisecond)
		initialDelay := l.CurrentDelay()

		// 2 次失败 → 未触发降级
		for i := 0; i < 2; i++ {
			if degraded := l.Failed(); degraded {
				t.Errorf("第 %d 次失败不应触发降级", i+1)
			}
		}
		if l.CurrentDelay() != initialDelay {
			t.Errorf("前 2 次失败后 delay 不应改变")
		}

		// 第 3 次失败 → 触发降级，delay × 2
		if degraded := l.Failed(); !degraded {
			t.Error("第 3 次失败应触发降级")
		}
		if l.CurrentDelay() != initialDelay*2 {
			t.Errorf("降级后 delay 应 × 2，期望 %v，实际 %v", initialDelay*2, l.CurrentDelay())
		}

		t.Logf("修复后：连续 3 次失败 → delay %v → %v ✓", initialDelay, l.CurrentDelay())
	})

	t.Run("after_fix_degrade_caps_at_max", func(t *testing.T) {
		l := NewStreamEditLimiterWith(40, 10*time.Millisecond, 30*time.Millisecond)
		// 连续失败触发多轮降级
		for i := 0; i < 20; i++ {
			l.Failed()
		}
		if l.CurrentDelay() > 30*time.Millisecond {
			t.Errorf("delay 应封顶到 maxInterval (30ms)，实际 %v", l.CurrentDelay())
		}
		if !l.IsDegraded() {
			t.Error("应已降级到上限")
		}
	})

	t.Run("after_fix_success_resets_fail_counter", func(t *testing.T) {
		l := NewStreamEditLimiterWith(40, 10*time.Millisecond, 100*time.Millisecond)
		// 2 次失败
		l.Failed()
		l.Failed()
		// 1 次成功 → 清零
		l.Emitted(40)
		// 再 2 次失败（总共才 2 次，不应降级）
		l.Failed()
		if degraded := l.Failed(); degraded {
			t.Error("Emitted 后 fail 计数应清零，2 次失败不应降级")
		}
		if l.CurrentDelay() != 10*time.Millisecond {
			t.Errorf("Emitted 后应恢复 minInterval，实际 %v", l.CurrentDelay())
		}
	})

	t.Run("after_fix_concurrent_safe", func(t *testing.T) {
		l := NewStreamEditLimiterWith(10, 1*time.Millisecond, 100*time.Millisecond)
		done := make(chan struct{})
		// 并发 goroutine 混合 ShouldEmit / Failed / Emitted
		for i := 0; i < 10; i++ {
			go func(n int) {
				for j := 0; j < 100; j++ {
					l.ShouldEmit(n * j)
					if j%3 == 0 {
						l.Failed()
					}
					if j%5 == 0 {
						l.Emitted(n * j)
					}
				}
				done <- struct{}{}
			}(i)
		}
		for i := 0; i < 10; i++ {
			<-done
		}
		// 不 panic / 无 race 就算过
	})

	t.Run("c5_rejects_negative_and_retraction", func(t *testing.T) {
		// C5 防御：调用方回退 currentLen < lastLen 时拒绝 emit，避免死锁
		l := NewStreamEditLimiterWith(40, 10*time.Millisecond, 100*time.Millisecond)
		l.Emitted(100) // lastLen=100
		if l.ShouldEmit(50) {
			t.Error("currentLen=50 < lastLen=100 应拒绝 emit")
		}
		if l.ShouldEmit(-10) {
			t.Error("负 currentLen 应拒绝 emit")
		}
		// Emitted 收到回退值应忽略，不污染 lastLen（等时间窗后验证）
		l.Emitted(50)
		time.Sleep(15 * time.Millisecond) // 等时间窗过
		if !l.ShouldEmit(200) {
			t.Error("200 >= 100 + 40 应允许 emit（Emitted(50) 应被忽略，lastLen 保持 100）")
		}
	})

	t.Run("after_fix_defaults_are_sensible", func(t *testing.T) {
		l := NewStreamEditLimiter()
		if l.CurrentDelay() != time.Second {
			t.Errorf("默认 minInterval 应为 1s，实际 %v", l.CurrentDelay())
		}
		// 触发降级，看 max 是否合理
		for i := 0; i < 30; i++ {
			l.Failed()
		}
		if l.CurrentDelay() > 5*time.Second {
			t.Errorf("默认 maxInterval 应 ≤ 5s，实际 %v", l.CurrentDelay())
		}
	})
}
