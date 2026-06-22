package cron

import (
	"context"
	"testing"
)

// AUD-1 回归锁（2026-06-22 R3）：IM/连接投递目标需要 job.ChatID，否则 Deliverer
// (cmd/hexclaw/main.go) 在 ChatID 为空时硬失败 "has no chat_id for IM target"。
// 桌面端经统一 /api/v1/cronjob 创建：draft.chat_id → AddJobRequest.ChatID → job.ChatID。
// 本测试钉死「AddJobRequest.ChatID 透传到 job.ChatID」这一段（script 路径，跳过 LLM）。
func TestAddJobFromScript_PropagatesChatID(t *testing.T) {
	db := setupTestDB(t)
	s := newTestScheduler(t, db)
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("Init schema 失败: %v", err)
	}

	job, err := s.AddJobFromScript(context.Background(), AddJobRequest{
		Name:     "日报",
		Schedule: "0 9 * * *",
		Prompt:   "汇总",
		UserID:   "u1",
		Deliver:  []string{"feishu"},
		ChatID:   "oc_test_123",
	}, RuntimeStarlark, `emit({"status":"success"})`)
	if err != nil {
		t.Fatalf("AddJobFromScript 失败: %v", err)
	}
	if job.ChatID != "oc_test_123" {
		t.Fatalf("ChatID 未透传到 job：want oc_test_123, got %q —— IM 投递将因空 ChatID 失败", job.ChatID)
	}
	if len(job.Deliver) != 1 || job.Deliver[0] != "feishu" {
		t.Fatalf("Deliver 未透传：%v", job.Deliver)
	}
}
