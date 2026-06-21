package cron

import (
	"testing"
	"time"
)

// BUG-20260622 / 审计 C-3：cron 五段解析器原先只接受 `*` 或单整数，
// 拒绝标准 crontab 的步进/范围/列表/命名表达式（*/5、9-18、1,3,5、MON）。
// 用户手填或 LLM 生成的标准 cron 必然 400。本测试锁定修复后这些表达式全部可解析且语义正确。

func TestParseCron5_StandardExpressions(t *testing.T) {
	// 固定基准时间：2026-06-23 是周二（Tuesday）。用 UTC 避免本地时区漂移。
	from := time.Date(2026, 6, 23, 10, 7, 0, 0, time.UTC) // 周二 10:07

	cases := []struct {
		expr   string
		verify func(t *testing.T, next time.Time)
	}{
		{"*/15 * * * *", func(t *testing.T, n time.Time) {
			if n.Minute()%15 != 0 {
				t.Fatalf("*/15 期望分钟为 15 的倍数，得到 %d", n.Minute())
			}
			if !n.After(from) || n.Sub(from) > 15*time.Minute {
				t.Fatalf("*/15 下次执行应在 15 分钟内，得到 %v", n)
			}
		}},
		{"0 9-18 * * *", func(t *testing.T, n time.Time) {
			if n.Minute() != 0 || n.Hour() < 9 || n.Hour() > 18 {
				t.Fatalf("0 9-18 期望整点且 9..18 时，得到 %02d:%02d", n.Hour(), n.Minute())
			}
		}},
		{"0 9 * * 1,3,5", func(t *testing.T, n time.Time) {
			wd := n.Weekday()
			if n.Hour() != 9 || n.Minute() != 0 || (wd != time.Monday && wd != time.Wednesday && wd != time.Friday) {
				t.Fatalf("0 9 * * 1,3,5 期望 周一/三/五 09:00，得到 %v %02d:%02d", wd, n.Hour(), n.Minute())
			}
		}},
		{"0 0 1 */2 *", func(t *testing.T, n time.Time) {
			if n.Day() != 1 || n.Hour() != 0 || n.Minute() != 0 {
				t.Fatalf("0 0 1 */2 * 期望某月 1 日 00:00，得到 %v", n)
			}
			if int(n.Month())%2 == 0 { // */2 从 1 起 → 奇数月（1,3,5,7,9,11）
				t.Fatalf("0 0 1 */2 * 期望奇数月，得到 %d 月", n.Month())
			}
		}},
		{"0 9 * * MON", func(t *testing.T, n time.Time) {
			if n.Weekday() != time.Monday || n.Hour() != 9 || n.Minute() != 0 {
				t.Fatalf("0 9 * * MON 期望周一 09:00，得到 %v %02d:%02d", n.Weekday(), n.Hour(), n.Minute())
			}
		}},
		{"0 9 * * 7", func(t *testing.T, n time.Time) { // 7 = 周日（POSIX）
			if n.Weekday() != time.Sunday || n.Hour() != 9 {
				t.Fatalf("0 9 * * 7 期望周日 09:00（7→0 归一），得到 %v %02d:00", n.Weekday(), n.Hour())
			}
		}},
		{"30 8 * * 1-5", func(t *testing.T, n time.Time) { // 工作日范围
			wd := n.Weekday()
			if n.Hour() != 8 || n.Minute() != 30 || wd == time.Saturday || wd == time.Sunday {
				t.Fatalf("30 8 * * 1-5 期望工作日 08:30，得到 %v %02d:%02d", wd, n.Hour(), n.Minute())
			}
		}},
	}

	for _, c := range cases {
		next, err := parseCron5(c.expr, from)
		if err != nil {
			t.Errorf("标准表达式 %q 应被接受，却报错: %v", c.expr, err)
			continue
		}
		c.verify(t, next)
	}
}

func TestParseCron5_SingleValueStillWorks(t *testing.T) {
	from := time.Date(2026, 6, 23, 10, 7, 0, 0, time.UTC)
	for _, expr := range []string{"0 9 * * *", "30 8 * * 1"} {
		if _, err := parseCron5(expr, from); err != nil {
			t.Errorf("单值表达式 %q 回归失败: %v", expr, err)
		}
	}
}

func TestParseCron5_RejectsInvalid(t *testing.T) {
	from := time.Date(2026, 6, 23, 10, 7, 0, 0, time.UTC)
	for _, expr := range []string{
		"*/0 * * * *",   // 步进 0
		"99 * * * *",    // 分钟越界
		"0 9 * * 8",     // 周越界（>7）
		"abc * * * *",   // 非法 token
		"0 9 * *",       // 字段数不足
		"0 13-9 * * *",  // 倒序范围
	} {
		if _, err := parseCron5(expr, from); err == nil {
			t.Errorf("非法表达式 %q 应被拒绝，却通过了", expr)
		}
	}
}
