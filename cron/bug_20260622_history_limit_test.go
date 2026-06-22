package cron

import (
	"context"
	"testing"
	"time"
)

// TestBug20260622_GetJobHistoryRespectsLimit 回归锁定 [BUG-CRON-HISTORY-LIMIT]：
// 前端 GET /cron/jobs/{id}/history?limit=N 传入的 limit 此前被后端忽略，
// GetJobHistory 固定 `LIMIT 50`。本测试钉死 limit 生效（含 [1,200] 夹取）。
func TestBug20260622_GetJobHistoryRespectsLimit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := newTestScheduler(t, db)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	job := &Job{Name: "hist-job", Schedule: "@hourly", SourcePrompt: "x", UserID: "u1", Spec: minimalSpec()}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// 写 8 条历史
	base := time.Now()
	for i := range 8 {
		if err := s.persistHistory(ctx, job.ID, "success", "", "", 1, base.Add(time.Duration(i)*time.Second), "", "", 0, nil); err != nil {
			t.Fatalf("persistHistory #%d: %v", i, err)
		}
	}

	// limit=5 → 恰好 5 条
	h5, err := s.GetJobHistory(ctx, job.ID, 5)
	if err != nil {
		t.Fatalf("GetJobHistory(limit=5): %v", err)
	}
	if len(h5) != 5 {
		t.Errorf("limit=5 应返回 5 条, got %d", len(h5))
	}

	// 缺省（无 limit）→ 全 8 条（≤50 默认）
	hAll, err := s.GetJobHistory(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJobHistory(default): %v", err)
	}
	if len(hAll) != 8 {
		t.Errorf("缺省应返回全部 8 条, got %d", len(hAll))
	}

	// 超大 limit 被夹到 200（不报错，返回现有 8 条）
	hBig, err := s.GetJobHistory(ctx, job.ID, 99999)
	if err != nil {
		t.Fatalf("GetJobHistory(limit=99999): %v", err)
	}
	if len(hBig) != 8 {
		t.Errorf("limit 超大夹到 200 后应返回现有 8 条, got %d", len(hBig))
	}
}
