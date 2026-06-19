package matrix

import (
	"context"
	"testing"
)

// TestBugMatrixStop_DoubleStopNoPanic 回归锁定 [BUG-MATRIX-STOP]：
// Stop 必须幂等——二次调用（热重载/优雅停机重入）不得因 close(已关闭 channel) 而 panic。
//
// 修复前：Stop 无条件 close(stopCh)，已有 stopped atomic.Bool 却未用于守卫，
// 二次 Stop → "close of closed channel" panic。
func TestBugMatrixStop_DoubleStopNoPanic(t *testing.T) {
	a := New(Config{Name: "m", HomeserverURL: "https://example.org", AccessToken: "t"})

	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("首次 Stop 不应报错: %v", err)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("二次 Stop 不应 panic（Stop 须幂等）: %v", r)
		}
	}()
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("二次 Stop 不应报错: %v", err)
	}
}
