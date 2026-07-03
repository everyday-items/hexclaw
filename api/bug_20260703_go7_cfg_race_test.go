package api

// GO-7（BUG-20260703）：handleUpdateAutonomyProfile 先 `nextCfg := *s.cfg`（整结构
// 浅拷贝 = 读 s.cfg 全部字段）再 `s.cfg.Security.Autonomy.Profile = profile` 裸写。
// 两个并发请求即构成同址读写竞争（-race 可测）；且 read-copy-save-apply 无锁
// 还有 lost-update：B 基于 A 写入前的旧副本落盘会抹掉 A 的变更。
// 契约：配置的 read-copy-save-apply 必须在 cfgMu 下串行。

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestBug20260703_ConcurrentProfileUpdateRaceFree(t *testing.T) {
	srv, _, _, _ := newAutonomyTestServer(t)

	profiles := []string{"balanced", "strict", "function_first"}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body, _ := json.Marshal(map[string]string{"profile": profiles[i%len(profiles)]})
			req := httptest.NewRequest("PUT", "/api/v1/autonomy/profile", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			srv.handleUpdateAutonomyProfile(rec, req)
			// 状态码不断言具体值：本测试关注 -race 是否无同址读写；
			// 任一并发序都必须要么 200 要么 5xx（持久化竞争）而非 panic。
		}(i)
	}
	wg.Wait()

	// 收敛性：最终 profile 必须是三者之一（无撕裂写）。
	final := srv.cfg.Security.Autonomy.Profile
	valid := map[string]bool{"balanced": true, "strict": true, "function_first": true}
	if !valid[final] {
		t.Fatalf("并发切换后 profile 撕裂：%q", final)
	}
}
