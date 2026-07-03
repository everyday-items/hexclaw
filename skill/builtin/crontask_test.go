package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/cron"
)

type fakeCronScheduler struct {
	addReq    *cron.AddJobRequest
	addErr    error
	jobs      []*cron.Job
	listedFor string
	paused    []string
	resumed   []string
	removed   []string
}

func (f *fakeCronScheduler) AddJobFromPrompt(_ context.Context, req cron.AddJobRequest) (*cron.Job, error) {
	f.addReq = &req
	if f.addErr != nil {
		return nil, f.addErr
	}
	return &cron.Job{
		ID:        "job-1",
		Name:      req.Name,
		Schedule:  req.Schedule,
		UserID:    req.UserID,
		NextRunAt: time.Date(2026, 6, 11, 9, 0, 0, 0, time.Local),
	}, nil
}

func (f *fakeCronScheduler) ListJobs(_ context.Context, userID string) ([]*cron.Job, error) {
	f.listedFor = userID
	return f.jobs, nil
}

func (f *fakeCronScheduler) GetJob(_ context.Context, jobID string) (*cron.Job, bool) {
	for _, j := range f.jobs {
		if j.ID == jobID {
			return j, true
		}
	}
	return nil, false
}

func (f *fakeCronScheduler) RemoveJob(_ context.Context, id string) error {
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeCronScheduler) PauseJob(_ context.Context, id string) error {
	f.paused = append(f.paused, id)
	return nil
}

func (f *fakeCronScheduler) ResumeJob(_ context.Context, id string) error {
	f.resumed = append(f.resumed, id)
	return nil
}

func TestCronTaskSkill_CreateBuildsRequestAndDefaultsUser(t *testing.T) {
	fake := &fakeCronScheduler{}
	s := NewCronTaskSkill(fake, "http://127.0.0.1:16060")

	res, err := s.Execute(context.Background(), map[string]any{
		"action":   "create",
		"name":     "百度热搜采集",
		"schedule": "@every 5m",
		"prompt":   "采集百度热搜 TOP10，整理为 markdown 并加入知识库",
	})
	if err != nil {
		t.Fatalf("create 失败: %v", err)
	}
	if fake.addReq == nil {
		t.Fatal("未调用 AddJobFromPrompt")
	}
	if fake.addReq.UserID != "desktop-user" {
		t.Errorf("缺省 user 应为 desktop-user（桌面端任务列表按此过滤），got %q", fake.addReq.UserID)
	}
	if fake.addReq.LocalAPIBase != "http://127.0.0.1:16060" {
		t.Errorf("LocalAPIBase 未注入: %q", fake.addReq.LocalAPIBase)
	}
	if !strings.Contains(res.Content, "job-1") || !strings.Contains(res.Content, "自动化") {
		t.Errorf("结果应含任务 ID 与管理入口提示: %s", res.Content)
	}
}

func TestCronTaskSkill_CreateRequiresFields(t *testing.T) {
	s := NewCronTaskSkill(&fakeCronScheduler{}, "")
	_, err := s.Execute(context.Background(), map[string]any{
		"action": "create",
		"name":   "缺字段",
	})
	if err == nil || !strings.Contains(err.Error(), "schedule") {
		t.Fatalf("缺 schedule/prompt 应报错并指明字段: %v", err)
	}
}

func TestCronTaskSkill_CreateToleratesLLMArgumentDrift(t *testing.T) {
	fake := &fakeCronScheduler{}
	s := NewCronTaskSkill(fake, "")

	_, err := s.Execute(context.Background(), map[string]any{
		"action":      "create",
		"title":       "百度热搜采集",
		"description": "每天早上9点采集百度热搜榜并写入知识库",
		"schedule":    "每天早上9点",
	})
	if err != nil {
		t.Fatalf("LLM 参数轻微漂移不应导致创建失败: %v", err)
	}
	if fake.addReq == nil {
		t.Fatal("未调用 AddJobFromPrompt")
	}
	if fake.addReq.Name != "百度热搜采集" {
		t.Fatalf("name 未从 title 别名提取: %#v", fake.addReq)
	}
	if fake.addReq.Prompt != "每天早上9点采集百度热搜榜并写入知识库" {
		t.Fatalf("prompt 未从 description 别名提取: %#v", fake.addReq)
	}
	if fake.addReq.Schedule != "0 9 * * *" {
		t.Fatalf("自然语言 schedule 未规范化: %#v", fake.addReq)
	}
}

func TestCronTaskSkill_CreateDefaultsPromptFromNameWhenModelOmitsPrompt(t *testing.T) {
	fake := &fakeCronScheduler{}
	s := NewCronTaskSkill(fake, "")

	_, err := s.Execute(context.Background(), map[string]any{
		"action":   "create",
		"name":     "百度热搜采集",
		"schedule": "0 9 * * *",
	})
	if err != nil {
		t.Fatalf("name+schedule 已足够表达任务时不应因 prompt 缺失失败: %v", err)
	}
	if fake.addReq == nil {
		t.Fatal("未调用 AddJobFromPrompt")
	}
	if fake.addReq.Prompt != "百度热搜采集" {
		t.Fatalf("缺 prompt 时应退回 name: %#v", fake.addReq)
	}
}

func TestCronTaskSkill_CreateErrorWrapped(t *testing.T) {
	s := NewCronTaskSkill(&fakeCronScheduler{addErr: errors.New("compile boom")}, "")
	_, err := s.Execute(context.Background(), map[string]any{
		"action":   "create",
		"name":     "x",
		"schedule": "@daily",
		"prompt":   "y",
	})
	if err == nil || !strings.Contains(err.Error(), "compile boom") {
		t.Fatalf("编译错误应透传: %v", err)
	}
}

func TestCronTaskSkill_ListPauseResumeRemove(t *testing.T) {
	fake := &fakeCronScheduler{
		jobs: []*cron.Job{{ID: "job-9", Name: "九号任务", Schedule: "@daily"}},
	}
	s := NewCronTaskSkill(fake, "")

	res, err := s.Execute(context.Background(), map[string]any{"action": "list", "user_id": "u-7"})
	if err != nil {
		t.Fatalf("list 失败: %v", err)
	}
	if fake.listedFor != "u-7" {
		t.Errorf("list 应透传 user_id, got %q", fake.listedFor)
	}
	if !strings.Contains(res.Content, "job-9") {
		t.Errorf("list 结果应含任务: %s", res.Content)
	}

	for _, tc := range []struct {
		action string
		got    *[]string
	}{
		{"pause", &fake.paused},
		{"resume", &fake.resumed},
		{"remove", &fake.removed},
	} {
		if _, err := s.Execute(context.Background(), map[string]any{"action": tc.action, "job_id": "job-9"}); err != nil {
			t.Fatalf("%s 失败: %v", tc.action, err)
		}
		if len(*tc.got) != 1 || (*tc.got)[0] != "job-9" {
			t.Errorf("%s 未作用到 job-9: %v", tc.action, *tc.got)
		}
		// Missing job_id must be an error.
		if _, err := s.Execute(context.Background(), map[string]any{"action": tc.action}); err == nil {
			t.Errorf("%s 缺 job_id 应报错", tc.action)
		}
	}
}

func TestCronTaskSkill_UnknownActionAndNilScheduler(t *testing.T) {
	s := NewCronTaskSkill(&fakeCronScheduler{}, "")
	if _, err := s.Execute(context.Background(), map[string]any{"action": "explode"}); err == nil {
		t.Error("未知 action 应报错")
	}
	nilSkill := NewCronTaskSkill(nil, "")
	if _, err := nilSkill.Execute(context.Background(), map[string]any{"action": "list"}); err == nil {
		t.Error("scheduler 未注入应报错")
	}
}
