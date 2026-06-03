package cron

import (
	"context"
	"testing"
	"time"
)

// TestHistoryPersistence_RoundtripStdout 验证 Sprint 1.3 JobHistory 扩展字段：
// stdout / stderr / exit_code / data 写入后能通过 GetJobHistory 还原。
func TestHistoryPersistence_RoundtripStdout(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	// 必须先有 job（GetJobHistory 会做存在性校验）
	job := &Job{
		Name:         "脚本任务",
		Schedule:     "@hourly",
		SourcePrompt: "测试",
		Spec:         &JobSpec{Runtime: "python3", Script: "print('hi')"},
		UserID:       "u1",
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// 写入一条带完整字段的 history
	now := time.Now().Truncate(time.Second)
	data := map[string]any{"items": []any{"a", "b"}, "count": float64(2)}
	if err := s.persistHistory(ctx, job.ID,
		"success", "结果摘要", "", 1234, now,
		"stdout 内容\n第二行", "stderr 警告", 0, data); err != nil {
		t.Fatalf("persistHistory: %v", err)
	}

	// 读回
	history, err := s.GetJobHistory(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJobHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("应有 1 条历史，实际 %d", len(history))
	}
	h := history[0]
	if h.Status != "success" {
		t.Errorf("Status: 期望 success 实际 %s", h.Status)
	}
	if h.Stdout != "stdout 内容\n第二行" {
		t.Errorf("Stdout: %q", h.Stdout)
	}
	if h.Stderr != "stderr 警告" {
		t.Errorf("Stderr: %q", h.Stderr)
	}
	if h.ExitCode != 0 {
		t.Errorf("ExitCode: %d", h.ExitCode)
	}
	if h.DurationMs != 1234 {
		t.Errorf("DurationMs: %d", h.DurationMs)
	}
	// Data 反序列化为 map[string]any
	got, ok := h.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data 应为 map[string]any，实际 %T: %v", h.Data, h.Data)
	}
	if got["count"] != float64(2) {
		t.Errorf("Data.count: %v", got["count"])
	}
	items, ok := got["items"].([]any)
	if !ok || len(items) != 2 || items[0] != "a" {
		t.Errorf("Data.items: %v", got["items"])
	}
}

// TestHistoryPersistence_ExitCodeAndTimeout 覆盖 exit_code 非零 + timeout 状态。
func TestHistoryPersistence_ExitCodeAndTimeout(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}

	job := &Job{
		Name: "失败任务", Schedule: "@hourly", SourcePrompt: "x",
		Spec: &JobSpec{Runtime: "python3", Script: "import sys; sys.exit(2)"},
		UserID: "u1",
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	now := time.Now().Truncate(time.Second)
	if err := s.persistHistory(ctx, job.ID,
		"timeout", "", "脚本执行超过 30 秒", 30000, now,
		"", "Traceback...", 137, nil); err != nil {
		t.Fatalf("persistHistory: %v", err)
	}

	history, err := s.GetJobHistory(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJobHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("应 1 条")
	}
	h := history[0]
	if h.Status != "timeout" {
		t.Errorf("Status: %s", h.Status)
	}
	if h.ExitCode != 137 {
		t.Errorf("ExitCode: %d", h.ExitCode)
	}
	if h.Stderr != "Traceback..." {
		t.Errorf("Stderr: %q", h.Stderr)
	}
	if h.Data != nil {
		t.Errorf("Data 应为 nil，实际 %v", h.Data)
	}
}
