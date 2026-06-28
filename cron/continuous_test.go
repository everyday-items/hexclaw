package cron

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// 持续型任务 + 跨 tick 检查点的 RED→GREEN 回归锁。
// 验证：渐进式推进（不重头做）、检查点累积/注入、三道终止闸（完成/停滞/总量）、重启可续、失败不推进。
//
// 循环机制单测直接调 runContinuousAgentJob（一个 tick = 一次调用），绕开 executeJob 的 claim/投递
// 调度外壳（其按到期时间领取，非本单元关注点）；末尾一个集成测覆盖 executeJob→continuous 派发线。

func newContinuousJob(prompt string) *Job {
	return &Job{
		Name:         "持续任务",
		Schedule:     "@daily",
		UserID:       "u1",
		SourcePrompt: prompt,
		Continuous:   true,
		Spec:         &JobSpec{Runtime: RuntimeAgent},
	}
}

func newContinuousScheduler(t *testing.T) (*Scheduler, context.Context) {
	t.Helper()
	db := setupTestDB(t)
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	s := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	if err := s.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return s, ctx
}

// 检查点跨 tick 累积，且每个 tick 把已完成的进度档案注入下次 prompt（渐进式推进，不重头做）。
func TestContinuous_CheckpointAccumulatesAndInjects(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	var prompts []string
	tick := 0
	s.SetAgentRunner(func(_ context.Context, job *Job) (AgentResult, error) {
		prompts = append(prompts, job.SourcePrompt)
		tick++
		return AgentResult{Content: fmt.Sprintf("已梳理第%d章。\nPROGRESS: 完成第%d章摘要\nTASK_COMPLETE: no", tick, tick)}, nil
	})

	job := newContinuousJob("持续逐章梳理《长报告》，每次只梳理下一章")
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	var last *RunResult
	for i := 0; i < 3; i++ {
		last = s.runContinuousAgentJob(ctx, job)
	}

	cp := s.loadContinuousCheckpoint(job.ID)
	if cp.Tick != 3 {
		t.Fatalf("应累积推进 3 次，实际 tick=%d", cp.Tick)
	}
	if len(cp.History) != 3 {
		t.Fatalf("进度档案应累积 3 条，实际 %d：%v", len(cp.History), cp.History)
	}
	// 首次 prompt 标注"第一次推进"；第 2 次起应注入上次进度（证明跨 tick 上下文，不重头做）。
	if !strings.Contains(prompts[0], "第一次推进") {
		t.Errorf("首次 prompt 应标注第一次推进，得：%q", prompts[0])
	}
	if !strings.Contains(prompts[1], "完成第1章摘要") {
		t.Errorf("回归(跨 tick 注入): 第2次 prompt 应含上次进度档案『完成第1章摘要』，得：%q", prompts[1])
	}
	// 投递正文剥掉控制标记。
	if strings.Contains(last.Stdout, "TASK_COMPLETE") || strings.Contains(last.Stdout, "PROGRESS:") {
		t.Errorf("回归: 投递正文不应含控制标记，得：%q", last.Stdout)
	}
}

// 进度档案有界：超过 continuousHistoryCap 条只保留最近 N 条（界 prompt 体积）。
func TestContinuous_HistoryBounded(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	n := 0
	s.SetAgentRunner(func(_ context.Context, _ *Job) (AgentResult, error) {
		n++
		return AgentResult{Content: fmt.Sprintf("PROGRESS: 第%d步\nTASK_COMPLETE: no", n)}, nil
	})
	job := newContinuousJob("持续推进《长报告》")
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for i := 0; i < continuousHistoryCap+5; i++ {
		s.runContinuousAgentJob(ctx, job)
	}
	cp := s.loadContinuousCheckpoint(job.ID)
	if len(cp.History) != continuousHistoryCap {
		t.Errorf("进度档案应界在 %d 条，实际 %d", continuousHistoryCap, len(cp.History))
	}
	if cp.History[len(cp.History)-1] != fmt.Sprintf("第%d步", continuousHistoryCap+5) {
		t.Errorf("应保留最近的进度，得末条：%q", cp.History[len(cp.History)-1])
	}
}

// 完成信号：TASK_COMPLETE: yes → 标记 completed + 任务收为 done（停止后续 tick）。
func TestContinuous_CompleteSignalMarksDone(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	s.SetAgentRunner(func(_ context.Context, _ *Job) (AgentResult, error) {
		return AgentResult{Content: "全部完成。\nPROGRESS: 完成最后一章\nTASK_COMPLETE: yes"}, nil
	})
	job := newContinuousJob("持续逐章梳理《长报告》")
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.runContinuousAgentJob(ctx, job)

	if cp := s.loadContinuousCheckpoint(job.ID); !cp.Completed {
		t.Error("回归(完成信号): TASK_COMPLETE: yes 应标记 checkpoint.Completed")
	}
	got, _ := s.GetJob(ctx, job.ID)
	if statusOf(got) != StatusDone {
		t.Errorf("回归(完成信号): 完成后任务应收为 done（停止调度），实际 %v", statusOf(got))
	}
}

// 无进展停滞：连续 maxNoProgressStreak 个 tick 无新进展 → 暂停（防空转烧 token）。
func TestContinuous_NoProgressStallPauses(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	s.SetAgentRunner(func(_ context.Context, _ *Job) (AgentResult, error) {
		// 空 PROGRESS（无 (.+) 匹配）→ 每 tick 无新进展。
		return AgentResult{Content: "没找到能推进的，卡住了。\nPROGRESS: \nTASK_COMPLETE: no"}, nil
	})
	job := newContinuousJob("持续逐章梳理《长报告》")
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for i := 0; i < maxNoProgressStreak; i++ {
		s.runContinuousAgentJob(ctx, job)
	}
	cp := s.loadContinuousCheckpoint(job.ID)
	if cp.NoProgress < maxNoProgressStreak {
		t.Fatalf("应累计无进展 %d 次，实际 %d", maxNoProgressStreak, cp.NoProgress)
	}
	got, _ := s.GetJob(ctx, job.ID)
	if statusOf(got) != StatusPaused {
		t.Errorf("回归(停滞闸): 连续无进展应暂停，实际 %v", statusOf(got))
	}
}

// F-2 最佳实践：agent 显式自报无进展（PROGRESS: NONE 等）即判停滞——即便每 tick 换不同措辞
// （字面去重抓不到），显式信号也兜得住。
func TestContinuous_ExplicitNoProgressSignalStalls(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	// 三个**互不相同**但都表示"无进展"的信号：字面去重判它们各不相同(会误判有进展)，显式信号才停滞。
	outs := []string{
		"实在没找到能做的。\nPROGRESS: NONE\nTASK_COMPLETE: no",
		"还是卡着。\nPROGRESS: 无进展\nTASK_COMPLETE: no",
		"没辙了。\nPROGRESS: stuck\nTASK_COMPLETE: no",
	}
	i := 0
	s.SetAgentRunner(func(_ context.Context, _ *Job) (AgentResult, error) {
		o := outs[i%len(outs)]
		i++
		return AgentResult{Content: o}, nil
	})
	job := newContinuousJob("持续推进《长报告》")
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	for k := 0; k < maxNoProgressStreak; k++ {
		s.runContinuousAgentJob(ctx, job)
	}
	cp := s.loadContinuousCheckpoint(job.ID)
	if cp.NoProgress < maxNoProgressStreak {
		t.Fatalf("回归(F-2): 显式 NONE 信号(换措辞)应判无进展累计 %d，实际 %d；进度档案=%v",
			maxNoProgressStreak, cp.NoProgress, cp.History)
	}
	if len(cp.History) != 0 {
		t.Errorf("回归(F-2): 无进展信号不应进进度档案，得 %v", cp.History)
	}
	got, _ := s.GetJob(ctx, job.ID)
	if statusOf(got) != StatusPaused {
		t.Errorf("回归(F-2): 显式无进展连续 %d 次应暂停，实际 %v", maxNoProgressStreak, statusOf(got))
	}
}

// 总量 backstop：推进次数达 maxContinuousTicks → 暂停（防无限推进）。
func TestContinuous_MaxTickBackstopPauses(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	n := 0
	s.SetAgentRunner(func(_ context.Context, _ *Job) (AgentResult, error) {
		n++
		return AgentResult{Content: fmt.Sprintf("PROGRESS: 第%d步又推进了一点\nTASK_COMPLETE: no", n)}, nil
	})
	job := newContinuousJob("持续逐章梳理《长报告》")
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	// 预置接近上限的检查点，跑一次即触顶（隔离测总量闸，不实跑 100 次）。
	s.saveContinuousCheckpoint(job.ID, continuousCheckpoint{Tick: maxContinuousTicks - 1, History: []string{"上一步"}})
	s.runContinuousAgentJob(ctx, job)

	cp := s.loadContinuousCheckpoint(job.ID)
	if cp.Tick != maxContinuousTicks {
		t.Fatalf("应推进到上限 %d，实际 %d", maxContinuousTicks, cp.Tick)
	}
	got, _ := s.GetJob(ctx, job.ID)
	if statusOf(got) != StatusPaused {
		t.Errorf("回归(总量 backstop): 达推进上限应暂停，实际 %v", statusOf(got))
	}
}

// 已完成的任务再被触发：不烧 LLM，确保收为 done（防手动恢复后空转）。
func TestContinuous_AlreadyCompletedShortCircuits(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	called := false
	s.SetAgentRunner(func(_ context.Context, _ *Job) (AgentResult, error) {
		called = true
		return AgentResult{Content: "PROGRESS: x\nTASK_COMPLETE: no"}, nil
	})
	job := newContinuousJob("持续推进《长报告》")
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.saveContinuousCheckpoint(job.ID, continuousCheckpoint{Tick: 5, Completed: true})
	s.runContinuousAgentJob(ctx, job)
	if called {
		t.Error("回归: 已完成任务不应再调用 agent（不烧 LLM）")
	}
	got, _ := s.GetJob(ctx, job.ID)
	if statusOf(got) != StatusDone {
		t.Errorf("回归: 已完成任务应收为 done，实际 %v", statusOf(got))
	}
}

// 重启可续：检查点存 sqlite（StateStore），新建 Scheduler（模拟桌面强退重启）仍能读回进度；
// 且 Continuous 标志经 meta JSON 往返不丢（重启后加载的 job 仍是持续型）。
func TestContinuous_CheckpointSurvivesRestartAndMetaRoundTrips(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s1 := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	if err := s1.Init(ctx); err != nil {
		t.Fatalf("Init s1: %v", err)
	}
	n := 0
	s1.SetAgentRunner(func(_ context.Context, _ *Job) (AgentResult, error) {
		n++
		return AgentResult{Content: fmt.Sprintf("PROGRESS: 完成第%d章\nTASK_COMPLETE: no", n)}, nil
	})
	job := newContinuousJob("持续逐章梳理《长报告》")
	if err := s1.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s1.runContinuousAgentJob(ctx, job)
	s1.runContinuousAgentJob(ctx, job)
	before := s1.loadContinuousCheckpoint(job.ID)
	if before.Tick != 2 {
		t.Fatalf("前置应推进 2 次，实际 %d", before.Tick)
	}

	// 模拟重启：同一 DB 起新 Scheduler。
	s2 := NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	if err := s2.Init(ctx); err != nil {
		t.Fatalf("Init s2: %v", err)
	}
	after := s2.loadContinuousCheckpoint(job.ID)
	if after.Tick != before.Tick || len(after.History) != len(before.History) {
		t.Errorf("回归(重启可续): 检查点应跨重启保留，before tick=%d hist=%d，after tick=%d hist=%d",
			before.Tick, len(before.History), after.Tick, len(after.History))
	}
	got, ok := s2.GetJob(ctx, job.ID)
	if !ok || !got.Continuous {
		t.Errorf("回归(meta 往返): 重启后 job.Continuous 应仍为 true，实际 %+v", got)
	}
}

// 失败 tick 不推进检查点（失败不算进度，交既有失败/告警路径处理）。
func TestContinuous_FailedTickDoesNotAdvance(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	s.SetAgentRunner(func(_ context.Context, _ *Job) (AgentResult, error) {
		return AgentResult{Content: "页面打不开。\nTASK_STATUS: failed - 上游不可达"}, nil
	})
	job := newContinuousJob("持续逐章梳理《长报告》")
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	res := s.runContinuousAgentJob(ctx, job)
	if res.Status != "failed" {
		t.Fatalf("失败 tick 应返回 failed，实际 %s", res.Status)
	}
	if cp := s.loadContinuousCheckpoint(job.ID); cp.Tick != 0 {
		t.Errorf("回归(失败不推进): 失败 tick 不应推进检查点，实际 tick=%d", cp.Tick)
	}
}

// 集成：executeJob 对 Continuous 任务派发到 continuous 路径（检查点被创建）。
func TestContinuous_ExecuteJobDispatchesContinuous(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	s.SetAgentRunner(func(_ context.Context, _ *Job) (AgentResult, error) {
		return AgentResult{Content: "已推进。\nPROGRESS: 完成第一块\nTASK_COMPLETE: no"}, nil
	})
	job := newContinuousJob("持续逐章梳理《长报告》")
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.executeJob(job)
	cp := s.loadContinuousCheckpoint(job.ID)
	if cp.Tick < 1 || len(cp.History) < 1 {
		t.Errorf("回归(派发): executeJob 应走 continuous 路径并建检查点，得 tick=%d hist=%d", cp.Tick, len(cp.History))
	}
}

// 创建路径契约：req.Continuous=true 强制 agent 模式（持续任务每 tick 需推理），并经 meta 持久化。
func TestContinuous_AddJobForcesAgentMode(t *testing.T) {
	s, ctx := newContinuousScheduler(t)
	// prompt 无推理动词（本会判脚本模式），但 Continuous=true 应强制 agent。
	job, err := s.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "持续整理", Schedule: "@daily", Prompt: "逐条把这批链接的内容存档", UserID: "u1",
		Continuous: true,
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	if job.Spec == nil || job.Spec.Runtime != RuntimeAgent {
		t.Errorf("回归(创建契约): Continuous 应强制 agent 模式，得 Spec=%+v", job.Spec)
	}
	if !job.Continuous {
		t.Error("回归(创建契约): job.Continuous 应为 true")
	}
}

// parseContinuousProgress 解析与剥离控制标记。
func TestContinuous_ParseProgressMarkers(t *testing.T) {
	prog, complete, cleaned := parseContinuousProgress("已梳理第3章。\nPROGRESS: 完成第3章摘要\nTASK_COMPLETE: no")
	if prog != "完成第3章摘要" {
		t.Errorf("PROGRESS 解析错：%q", prog)
	}
	if complete {
		t.Error("TASK_COMPLETE: no 不应判完成")
	}
	if strings.Contains(cleaned, "PROGRESS") || strings.Contains(cleaned, "TASK_COMPLETE") {
		t.Errorf("控制标记应从正文剥离，得：%q", cleaned)
	}
	if !strings.Contains(cleaned, "已梳理第3章") {
		t.Errorf("正文应保留，得：%q", cleaned)
	}
	if _, c2, _ := parseContinuousProgress("done\nTASK_COMPLETE: yes"); !c2 {
		t.Error("TASK_COMPLETE: yes 应判完成")
	}
}

func statusOf(j *Job) JobStatus {
	if j == nil {
		return "<nil>"
	}
	return j.Status
}
