package builtin

import (
	"context"
	"strings"
	"testing"
)

type fakeHealer struct{ resumed, paused, triggered string }

func (f *fakeHealer) ResumeJob(_ context.Context, id string) error  { f.resumed = id; return nil }
func (f *fakeHealer) PauseJob(_ context.Context, id string) error   { f.paused = id; return nil }
func (f *fakeHealer) TriggerJob(_ context.Context, id string) error { f.triggered = id; return nil }

type fakeRunner struct {
	ran   string
	calls int
}

func (r *fakeRunner) RunWorkflowByID(id, _ string) (string, error) {
	r.ran = id
	r.calls++
	return "run-x", nil
}

func TestAppHeal_WhitelistedActionsRouted(t *testing.T) {
	f := &fakeHealer{}
	s := NewAppHealSkill(f, nil)
	for _, action := range []string{"cron_retry", "cron_resume", "cron_pause"} {
		if _, err := s.Execute(context.Background(), map[string]any{"action": action, "job_id": "j1"}); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if f.triggered != "j1" || f.resumed != "j1" || f.paused != "j1" {
		t.Fatalf("actions not routed: %+v", f)
	}
}

// 🔴 安全：非白名单动作（如任意 config 改写）必须被拒，缺 job_id 必须报错。
func TestAppHeal_RejectsNonWhitelistedAndMissingJob(t *testing.T) {
	s := NewAppHealSkill(&fakeHealer{}, nil)
	if _, err := s.Execute(context.Background(), map[string]any{"action": "config_rewrite", "job_id": "j1"}); err == nil {
		t.Fatal("非白名单动作应被拒（防 LLM 幻觉任意改 config）")
	}
	if _, err := s.Execute(context.Background(), map[string]any{"action": "cron_retry"}); err == nil {
		t.Fatal("缺 job_id 应报错")
	}
}

// P2: workflow_run 走真实 runner（差分：与 HTTP handleRunWorkflow 同一执行内核），缺 workflow_id 报错。
func TestAppHeal_WorkflowRun(t *testing.T) {
	r := &fakeRunner{}
	s := NewAppHealSkill(&fakeHealer{}, r)
	if _, err := s.Execute(context.Background(), map[string]any{"action": "workflow_run", "workflow_id": "wf1"}); err != nil {
		t.Fatalf("workflow_run: %v", err)
	}
	if r.ran != "wf1" {
		t.Fatalf("workflow not run: %q", r.ran)
	}
	if _, err := s.Execute(context.Background(), map[string]any{"action": "workflow_run"}); err == nil {
		t.Fatal("缺 workflow_id 应报错")
	}
}

// #11 限频：同一工作流窗口内重复触发被拒（防审批后被循环 / 误连点重复启动），runner 只真正跑一次。
func TestAppHeal_WorkflowRun_RateLimited(t *testing.T) {
	r := &fakeRunner{}
	s := NewAppHealSkill(&fakeHealer{}, r)
	if _, err := s.Execute(context.Background(), map[string]any{"action": "workflow_run", "workflow_id": "wf1"}); err != nil {
		t.Fatalf("首次 workflow_run 应成功: %v", err)
	}
	// 立即重复触发同一工作流 → 限频拒绝
	if _, err := s.Execute(context.Background(), map[string]any{"action": "workflow_run", "workflow_id": "wf1"}); err == nil {
		t.Fatal("窗口内重复触发同一工作流应被限频拒绝")
	}
	if r.calls != 1 {
		t.Fatalf("限频后 runner 应只执行一次，实际 %d 次", r.calls)
	}
	// 不同工作流不受影响
	if _, err := s.Execute(context.Background(), map[string]any{"action": "workflow_run", "workflow_id": "wf2"}); err != nil {
		t.Fatalf("不同工作流不应被限频: %v", err)
	}
}

// #11 回滚指针：可逆动作的结果须告诉用户如何撤销（cron_pause ↔ cron_resume）。
func TestAppHeal_ReversibleActionSurfacesRollback(t *testing.T) {
	s := NewAppHealSkill(&fakeHealer{}, nil)
	res, err := s.Execute(context.Background(), map[string]any{"action": "cron_pause", "job_id": "j9"})
	if err != nil {
		t.Fatalf("cron_pause: %v", err)
	}
	if !strings.Contains(res.Content, "cron_resume") {
		t.Fatalf("cron_pause 结果应提示用 cron_resume 回滚: %q", res.Content)
	}
	if hint := rollbackHint("cron_resume", "j9"); !strings.Contains(hint, "cron_pause") {
		t.Fatalf("cron_resume 的回滚指针应指向 cron_pause: %q", hint)
	}
	if hint := rollbackHint("cron_retry", "j9"); hint != "" {
		t.Fatalf("不可逆动作回滚指针应为空: %q", hint)
	}
}
