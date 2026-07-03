package api

// FS-8（BUG-20260703）：handleAddCronJobJSON（非 SSE / curl / 老前端路径）漏传
// Paused——SSE 路径带 Paused: req.Paused，JSON 路径不带。经此路径以 paused:true
// 创建的任务会被静默建成 active（创建即跑），绕过审批冻结。契约：两条路径一致。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/cron"
)

func TestBug20260703_CronJSONCreateHonorsPaused(t *testing.T) {
	srv, scheduler := newSSETestServer(t)

	// 不带 Accept: text/event-stream → 走 handleAddCronJobJSON。
	body := `{"name":"t","schedule":"@daily","prompt":"x","user_id":"u1","paused":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cron/jobs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleAddCronJob(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("创建应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	jobs, err := scheduler.ListJobs(t.Context(), "u1")
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("应有 1 个任务，实际 %d", len(jobs))
	}
	if jobs[0].Status != cron.StatusPaused {
		t.Errorf("[FS-8] JSON 路径 paused:true 未生效，任务状态=%q（应 paused，否则创建即跑绕过审批）", jobs[0].Status)
	}
}
