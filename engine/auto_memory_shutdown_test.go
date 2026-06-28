package engine

import (
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// 交接 D 回归锁：①本地慢模型抽取超时放宽（慢硬件兜底，避免 >120s 抽取静默丢记忆）
//                ②Stop 有界宽限等待（停机不被单条在途慢抽取拖到分钟级）。

// ① 本地抽取超时须放宽到足够覆盖病态慢模型（实测 qwen3.5:9b 抽取 >120s）；云端保持精简基线。
func TestMemoryExtractionTimeout_LocalRaisedForSlowHardware(t *testing.T) {
	if localMemoryExtractTimeout <= 120*time.Second {
		t.Errorf("交接 D：本地抽取超时须从 120s 放宽（>120s 兜底慢硬件），得 %v", localMemoryExtractTimeout)
	}
	if got := memoryExtractionTimeout(true); got < 300*time.Second {
		t.Errorf("本地抽取超时应 ≥5min，得 %v", got)
	}
	if got := memoryExtractionTimeout(false); got != cloudMemoryExtractTimeout {
		t.Errorf("云端抽取超时应保持基线 %v，得 %v", cloudMemoryExtractTimeout, got)
	}
}

// ② 在途后台 goroutine 永不结束时，waitGroupBounded 必须等满宽限后放弃，而非被拖死。
//
//	并经 goleak 证明：waitGroupBounded 内部为 wg.Wait 起的 waiter goroutine 在 wg 排空后会净退、不永久泄漏。
func TestWaitGroupBounded_ReturnsWithinGrace(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	var wg sync.WaitGroup
	wg.Add(1)
	release := make(chan struct{})
	go func() { <-release; wg.Done() }() // 模拟病态慢的在途抽取

	grace := 150 * time.Millisecond
	start := time.Now()
	waitGroupBounded(&wg, grace)
	elapsed := time.Since(start)

	if elapsed < grace {
		t.Errorf("应至少等满宽限再放弃，得 %v < 宽限 %v", elapsed, grace)
	}
	if elapsed > grace+800*time.Millisecond {
		t.Errorf("超宽限必须放弃等待（停机不被在途 goroutine 拖死），得 %v 远超宽限 %v", elapsed, grace)
	}

	close(release) // 收尾，防 goroutine 泄漏到其它测试
	wg.Wait()
}

// ② 后台已排空时立即返回，不空等满宽限。
func TestWaitGroupBounded_ReturnsImmediatelyWhenDrained(t *testing.T) {
	var wg sync.WaitGroup // 空 wg
	start := time.Now()
	waitGroupBounded(&wg, 5*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("已排空应立即返回不空等宽限，得 %v", elapsed)
	}
}

// 宽限常量须为有界的小窗口（停机体验：不应等到分钟级）。
func TestBgShutdownGracePeriod_Bounded(t *testing.T) {
	if bgShutdownGracePeriod <= 0 || bgShutdownGracePeriod > 30*time.Second {
		t.Errorf("停机宽限须为有界小窗口(0,30s]，得 %v", bgShutdownGracePeriod)
	}
}
