package heartbeat

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestHeartbeat_StartStop 测试心跳启停
func TestHeartbeat_StartStop(t *testing.T) {
	hb := New(Config{
		Enabled:      true,
		Interval:     100 * time.Millisecond,
		Instructions: "测试巡查指令",
	})

	var count atomic.Int32
	hb.Start(context.Background(),
		func(_ context.Context, inst string) (string, error) {
			count.Add(1)
			return "NO_NOTIFY", nil
		},
		func(_ context.Context, result string) error {
			return nil
		},
	)

	// 等待几次心跳
	time.Sleep(350 * time.Millisecond)
	hb.Stop()

	if count.Load() < 2 {
		t.Errorf("心跳应至少执行 2 次，实际 %d 次", count.Load())
	}

	stats := hb.Stats()
	if stats.RunCount < 2 {
		t.Errorf("统计的执行次数应至少 2 次，实际 %d 次", stats.RunCount)
	}
}

// TestHeartbeat_Notify 测试有结果时通知
func TestHeartbeat_Notify(t *testing.T) {
	hb := New(Config{
		Enabled:      true,
		Interval:     100 * time.Millisecond,
		Instructions: "检查是否有紧急事项",
	})

	var notified atomic.Int32
	hb.Start(context.Background(),
		func(_ context.Context, inst string) (string, error) {
			return "你有 3 封未读邮件", nil
		},
		func(_ context.Context, result string) error {
			notified.Add(1)
			return nil
		},
	)

	time.Sleep(200 * time.Millisecond)
	hb.Stop()

	if notified.Load() == 0 {
		t.Error("应该触发至少一次通知")
	}
}

// TestHeartbeat_NoNotify 测试无需通知时不触发
func TestHeartbeat_NoNotify(t *testing.T) {
	hb := New(Config{
		Enabled:      true,
		Interval:     100 * time.Millisecond,
		Instructions: "检查状态",
	})

	var notified atomic.Int32
	hb.Start(context.Background(),
		func(_ context.Context, inst string) (string, error) {
			return "NO_NOTIFY", nil
		},
		func(_ context.Context, result string) error {
			notified.Add(1)
			return nil
		},
	)

	time.Sleep(250 * time.Millisecond)
	hb.Stop()

	if notified.Load() > 0 {
		t.Error("NO_NOTIFY 不应触发通知")
	}
}

// TestHeartbeat_QuietHours 测试免打扰时段
func TestHeartbeat_QuietHours(t *testing.T) {
	now := time.Now()
	// 设置免打扰时段为当前时间
	quietStart := now.Format("15:04")
	quietEnd := now.Add(time.Hour).Format("15:04")

	hb := New(Config{
		Enabled:      true,
		Interval:     50 * time.Millisecond,
		Instructions: "巡查",
		QuietHours: QuietHours{
			Enabled: true,
			Start:   quietStart,
			End:     quietEnd,
		},
	})

	var count atomic.Int32
	hb.Start(context.Background(),
		func(_ context.Context, inst string) (string, error) {
			count.Add(1)
			return "", nil
		},
		nil,
	)

	time.Sleep(200 * time.Millisecond)
	hb.Stop()

	if count.Load() > 0 {
		t.Error("免打扰时段不应执行心跳")
	}
}

// TestParseHourMinute 测试时间解析
func TestParseHourMinute(t *testing.T) {
	tests := []struct {
		input string
		h, m  int
	}{
		{"09:00", 9, 0},
		{"22:30", 22, 30},
		{"00:00", 0, 0},
		{"23:59", 23, 59},
	}

	for _, tt := range tests {
		h, m := parseHourMinute(tt.input)
		if h != tt.h || m != tt.m {
			t.Errorf("parseHourMinute(%q) = (%d, %d), want (%d, %d)", tt.input, h, m, tt.h, tt.m)
		}
	}
}

// TestDefaultInstructions 测试默认指令
func TestDefaultInstructions(t *testing.T) {
	hb := New(Config{
		Enabled:  true,
		Interval: time.Minute,
	})

	if hb.instructions == "" {
		t.Error("默认指令不应为空")
	}
	if hb.instructions != defaultInstructions {
		t.Error("应使用默认指令")
	}
}
