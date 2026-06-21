package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/cron"
)

// 审计 C-5：30/user 配额闸原先只挂在 unified create（cronActionCreate），
// 桌面端主路径（恒走 SSE /api/v1/cron/jobs/stream）完全绕过 → 上限形同虚设。
// 修复后两条创建路径共用同一配额闸。本测试 seed 满配额后断言 SSE 创建返 429。

func seedActiveCronJobs(t *testing.T, sch *cron.Scheduler, userID string, n int) {
	t.Helper()
	for i := range n {
		_, err := sch.AddJobFromScript(t.Context(), cron.AddJobRequest{
			Name:     "seed-" + cronItoa(i),
			Schedule: "@daily",
			UserID:   userID,
			Prompt:   "seed",
		}, "", `emit({"status": "success"})`) // 纯 Go Starlark，仅注册为 active，不执行
		if err != nil {
			t.Fatalf("seed job %d: %v", i, err)
		}
	}
}

func TestHandleAddCronJobSSE_EnforcesQuota(t *testing.T) {
	srv, scheduler := newSSETestServer(t)

	// seed 恰好 CronQuotaPerUser 个活跃任务 → 已达上限
	seedActiveCronJobs(t, scheduler, "u1", CronQuotaPerUser)

	body := `{"name":"over","schedule":"@daily","prompt":"x","user_id":"u1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron/jobs", strings.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleAddCronJob(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("满配额时 SSE 创建应返 429，得到 %d；body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "上限") {
		t.Errorf("429 响应应含配额超限提示，得到 %s", w.Body.String())
	}
}

func TestHandleAddCronJobSSE_UnderQuotaPasses(t *testing.T) {
	srv, scheduler := newSSETestServer(t)

	// 未达上限（CronQuotaPerUser-1 个）→ 应正常创建，不被配额闸拦
	seedActiveCronJobs(t, scheduler, "u1", CronQuotaPerUser-1)

	body := `{"name":"ok","schedule":"@daily","prompt":"x","user_id":"u1"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron/jobs", strings.NewReader(body))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.handleAddCronJob(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("未达上限时应正常创建（200 SSE），得到 %d；body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "event: done") {
		t.Errorf("未达上限应走完整 SSE 到 done，得到 %s", w.Body.String())
	}
}
