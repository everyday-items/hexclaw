package cron

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClassifyTaskMode(t *testing.T) {
	agents := []string{
		"每天早上总结我的待办和邮件，挑重点发我",
		"每周一分析上周的服务日志并给出建议",
		"每天写一篇行业简报",
		"review error logs daily and send me the highlights",
		"summarize my unread emails every morning",
	}
	for _, p := range agents {
		if ClassifyTaskMode(p) != RuntimeAgent {
			t.Errorf("应判定为 agent 模式: %q", p)
		}
	}
	scripts := []string{
		"每5分钟采集百度热搜 TOP10，整理为 markdown 并加入知识库",
		"每小时抓取美元汇率存到知识库",
		"每天 0 点备份配置文件",
		// Review L2: English keywords must match whole words only —
		// "preview"/"draftsman" must not trigger "review"/"draft".
		"preview the dashboard page and save a screenshot every hour",
		"每天下载 draftsman 教程页面存档",
	}
	for _, p := range scripts {
		if ClassifyTaskMode(p) != "" {
			t.Errorf("应判定为脚本模式: %q", p)
		}
	}
}

func TestValidateAgentModeSchedule(t *testing.T) {
	if err := validateAgentModeSchedule("@every 5m"); err == nil {
		t.Error("agent 模式 5 分钟间隔应被拒绝")
	}
	for _, ok := range []string{"@hourly", "@daily", "0 9 * * *"} {
		if err := validateAgentModeSchedule(ok); err != nil {
			t.Errorf("%s 应通过护栏: %v", ok, err)
		}
	}
}

func TestAddJobFromPrompt_AgentMode_SkipsCompiler(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	c := &stubCompiler{ret: &JobSpec{Runtime: "python3", Script: "x"}}
	s := NewScheduler(db, c, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "晨报", Schedule: "@daily", Prompt: "每天早上总结我的待办，挑重点发我", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	if job.Spec == nil || job.Spec.Runtime != RuntimeAgent {
		t.Fatalf("应为 agent 模式 Spec: %+v", job.Spec)
	}
	if c.last.Prompt != "" {
		t.Error("agent 模式不应调用脚本编译器")
	}
}

func TestAddJobFromPrompt_AgentMode_FrequencyGuard(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(context.Background())

	_, err := s.AddJobFromPrompt(context.Background(), AddJobRequest{
		Name: "高频总结", Schedule: "@every 5m", Prompt: "每5分钟总结一次日志", UserID: "u1",
	})
	if err == nil || !strings.Contains(err.Error(), "1 小时") {
		t.Fatalf("应触发频率护栏并提示最小间隔: %v", err)
	}
}

func TestExecuteJob_AgentRunnerPath(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	var gotPrompt string
	s.SetAgentRunner(func(_ context.Context, job *Job) (AgentResult, error) {
		gotPrompt = job.SourcePrompt
		return AgentResult{Content: "今日要点：A、B、C"}, nil
	})

	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "晨报", Schedule: "@daily", Prompt: "每天总结要点", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}

	s.executeJob(job)

	if gotPrompt != "每天总结要点" {
		t.Errorf("runner 未收到任务 prompt: %q", gotPrompt)
	}
	history, _ := s.GetJobHistory(ctx, job.ID)
	if len(history) == 0 || history[0].Status != "success" || !strings.Contains(history[0].Stdout, "今日要点") {
		t.Fatalf("agent 执行历史不正确: %+v", history)
	}
}

func TestExecuteJob_AgentRunnerMissing(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "晨报", Schedule: "@daily", Prompt: "每天总结要点", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	s.executeJob(job)

	history, _ := s.GetJobHistory(ctx, job.ID)
	if len(history) == 0 || history[0].Status == "success" {
		t.Fatal("runner 未注入时应记录失败而非成功")
	}
	if !strings.Contains(history[0].Error, "未注入") {
		t.Errorf("错误信息应可定位: %s", history[0].Error)
	}
}

func TestMaybeSelfHeal_RecompileAndCooldown(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	healed := &JobSpec{Runtime: "python3", Script: "print('fixed')", TimeoutSec: 60}
	c := &stubCompiler{ret: healed}
	s := NewScheduler(db, c, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	job := &Job{
		Name: "采集", Type: JobTypeCron, Schedule: "@daily", UserID: "u1",
		Status: StatusActive, SourcePrompt: "每天采集热搜",
		Spec: &JobSpec{Runtime: "python3", Script: "broken", TimeoutSec: 60},
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	now := time.Now()
	for i := 0; i < 3; i++ {
		_ = s.persistHistory(ctx, job.ID, "error", "", "boom", 10, now.Add(-time.Duration(10-i)*time.Second), "", "stderr-detail", 1, nil)
	}

	var notices []string
	var levels []string
	s.SetNotifier(func(_ *Job, level, title, body string) {
		notices = append(notices, title+"|"+body)
		levels = append(levels, level)
	})

	s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "boom", Stderr: "stderr-detail"})

	if len(notices) != 1 || !strings.Contains(notices[0], "已自动修复") {
		t.Errorf("自愈成功应推送通知: %v", notices)
	}
	// Review L6: a successful heal is not a warning.
	if len(levels) != 1 || levels[0] != NotifyLevelSuccess {
		t.Errorf("自愈成功通知应为 success 级别: %v", levels)
	}

	if !strings.Contains(c.last.Prompt, "自愈上下文") || !strings.Contains(c.last.Prompt, "stderr-detail") {
		t.Errorf("重编译应携带失败上下文: %q", c.last.Prompt)
	}
	if got, _ := s.GetJob(ctx, job.ID); got.Spec.Script != "print('fixed')" {
		t.Errorf("内存 Spec 未更新: %q", got.Spec.Script)
	}
	history, _ := s.GetJobHistory(ctx, job.ID)
	if history[0].Status != "healed" {
		t.Errorf("应写入 healed 历史: %+v", history[0])
	}

	// Review M3: after a successful heal the failure window resets. A second
	// maybeSelfHeal with NO failures newer than the healed row must NOT
	// recompile (the pre-heal failures no longer count).
	c.last.Prompt = ""
	s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "boom-stale"})
	if c.last.Prompt != "" {
		t.Error("自愈成功后无新失败不应再次重编译（premature re-heal）")
	}

	// 3 fresh failures AFTER the healed row → second heal allowed (quota 2/24h).
	for i := 0; i < 3; i++ {
		_ = s.persistHistory(ctx, job.ID, "error", "", "boom-second", 10, time.Now(), "", "", 1, nil)
	}
	s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "boom-second"})
	if !strings.Contains(c.last.Prompt, "boom-second") {
		t.Errorf("窗口内第 2 次自愈应放行: %q", c.last.Prompt)
	}

	// 3 more fresh failures → third heal must be quota-blocked.
	for i := 0; i < 3; i++ {
		_ = s.persistHistory(ctx, job.ID, "error", "", "boom-third", 10, time.Now(), "", "", 1, nil)
	}
	s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "boom-third"})
	if strings.Contains(c.last.Prompt, "boom-third") {
		t.Error("24h 窗口内第 3 次自愈应被配额拦截，不应再调编译器")
	}
	if len(notices) == 0 || !strings.Contains(notices[len(notices)-1], "次数已用尽") {
		t.Errorf("配额拦截应推送'持续失败'通知: %v", notices)
	}
	countAfterExhausted := len(notices)
	s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "boom-fourth"})
	if len(notices) != countAfterExhausted {
		t.Errorf("配额用尽后的重复失败不应再次推送（防通知轰炸）: %v", notices)
	}
}

// countingFailCompiler always fails compilation and counts the attempts.
type countingFailCompiler struct{ calls int }

func (c *countingFailCompiler) Compile(context.Context, string, CompileHints) (*JobSpec, error) {
	c.calls++
	return nil, errors.New("LLM 编译失败: invalid x-api-key")
}

// Review H1: a permanently failing upstream must not trigger a full LLM
// recompile + notification on EVERY failing run. Attempts (not successes)
// count against the 24h quota, and the heal-failed notification is
// rate-limited to one per window.
func TestMaybeSelfHeal_AttemptsBoundedOnPersistentCompileFailure(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	comp := &countingFailCompiler{}
	s := NewScheduler(db, comp, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	job := &Job{
		Name: "采集", Type: JobTypeCron, Schedule: "@daily", UserID: "u1",
		Status: StatusActive, SourcePrompt: "每天采集热搜",
		Spec: &JobSpec{Runtime: "python3", Script: "broken", TimeoutSec: 60},
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for i := 0; i < 3; i++ {
		_ = s.persistHistory(ctx, job.ID, "error", "", "boom", 10, time.Now(), "", "", 1, nil)
	}

	var notices []string
	s.SetNotifier(func(_ *Job, _, title, _ string) { notices = append(notices, title) })

	for i := 0; i < 5; i++ {
		s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "boom"})
	}

	if comp.calls > selfHealMaxPerWindow {
		t.Errorf("持续编译失败时 24h 窗口内最多 %d 次自愈尝试，实际调用编译器 %d 次", selfHealMaxPerWindow, comp.calls)
	}
	if len(notices) > 1 {
		t.Errorf("24h 窗口内失败类通知最多 1 条（防轰炸），实际 %d 条: %v", len(notices), notices)
	}
}

// Review H2: a successful agent-mode run must deliver its result to the user
// (script jobs self-deliver via the compiled script; agent jobs rely on the
// scheduler routing the output through EffectiveDeliver).
func TestExecuteJob_AgentResultDelivered(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	s.SetAgentRunner(func(context.Context, *Job) (AgentResult, error) {
		return AgentResult{Content: "今日要点：A、B、C"}, nil
	})
	var got []string
	s.SetNotifier(func(_ *Job, level, title, body string) { got = append(got, level+"|"+title+"|"+body) })

	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "晨报", Schedule: "@daily", Prompt: "每天总结要点", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	s.executeJob(job)

	if len(got) != 1 {
		t.Fatalf("成功的 agent 任务应推送 1 条结果通知，实际 %d 条: %v", len(got), got)
	}
	if !strings.Contains(got[0], "晨报") || !strings.Contains(got[0], "今日要点") {
		t.Errorf("结果通知应携带任务名与内容: %q", got[0])
	}
}

func TestExecuteJob_AgentResultNotDeliveredOnFailure(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	s.SetAgentRunner(func(context.Context, *Job) (AgentResult, error) {
		return AgentResult{}, errors.New("LLM unavailable")
	})
	var got []string
	s.SetNotifier(func(_ *Job, _, title, body string) { got = append(got, title+"|"+body) })

	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "晨报", Schedule: "@daily", Prompt: "每天总结要点", UserID: "u1",
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	s.executeJob(job)

	if len(got) != 0 {
		t.Errorf("失败的 agent 任务不应推送结果通知: %v", got)
	}
}

func TestMaybeSelfHeal_NotTriggeredBelowThreshold(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	c := &stubCompiler{ret: &JobSpec{Runtime: "python3", Script: "x", TimeoutSec: 60}}
	s := NewScheduler(db, c, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	_ = s.Init(ctx)

	job := &Job{
		Name: "采集", Type: JobTypeCron, Schedule: "@daily", UserID: "u1",
		Status: StatusActive, SourcePrompt: "每天采集",
		Spec: &JobSpec{Runtime: "python3", Script: "broken", TimeoutSec: 60},
	}
	_ = s.AddJob(ctx, job)
	// Only 2 failures with 1 success sandwiched in between.
	_ = s.persistHistory(ctx, job.ID, "error", "", "e1", 10, time.Now().Add(-3*time.Second), "", "", 1, nil)
	_ = s.persistHistory(ctx, job.ID, "success", "", "", 10, time.Now().Add(-2*time.Second), "", "", 0, nil)
	_ = s.persistHistory(ctx, job.ID, "error", "", "e2", 10, time.Now().Add(-1*time.Second), "", "", 1, nil)

	s.maybeSelfHeal(ctx, job, &RunResult{Status: "error", Error: "e2"})
	if c.last.Prompt != "" {
		t.Error("未达连续失败阈值不应触发重编译")
	}
}
