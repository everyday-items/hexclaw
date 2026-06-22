package dingtalk

import (
	"testing"
	"time"
)

// 回归测试 — A-1（2026-06-22 hex-test 审计 / 复用分层）：
// connectLoop 由手写 `backoff = min(backoff*2, 30s)` 改为复用 toolkit/util/retry。
// 本测试钉死退避序列**逐项不变**，防止复用重构悄悄改掉重连节奏。
func TestReconnectBackoff_A1_SequencePreserved(t *testing.T) {
	want := []time.Duration{
		1 * time.Second,  // attempt 1（原实现首次 backoff=1s）
		2 * time.Second,  // attempt 2
		4 * time.Second,  // attempt 3
		8 * time.Second,  // attempt 4
		16 * time.Second, // attempt 5
		30 * time.Second, // attempt 6（min(32s,30s) 封顶）
		30 * time.Second, // attempt 7（持续封顶）
	}
	for i, w := range want {
		attempt := i + 1
		if got := reconnectBackoff(attempt); got != w {
			t.Errorf("reconnectBackoff(%d)=%v，期望=%v（须与原手写 backoff*2 封顶 30s 序列逐项一致）", attempt, got, w)
		}
	}
}
