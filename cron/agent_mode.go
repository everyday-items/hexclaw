// agent_mode implements the agent side of cron's dual-mode execution:
//
//   - Task-mode classification: mechanical I/O tasks (collect/scrape/download/
//     save/sync) compile to scripts (zero-LLM execution); cognitive tasks that
//     contain reasoning verbs are marked Runtime=agent and each tick runs a
//     full Agent round through the injected AgentRunner.
//   - Frequency guard: agent-mode jobs require a minimum interval of 1 hour
//     (each run burns a full LLM round).
//   - Self-heal bridge: after a script job fails consecutively past the
//     threshold, recompile once with the failure context (isomorphic to JIT
//     deoptimization: fast-path assumption broken → fall back to the smart
//     path → re-optimize). At most 2 heal ATTEMPTS per 24h cooldown window,
//     so a permanently broken upstream cannot burn tokens forever.
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
)

// RuntimeAgent marks a job as agent-executed (one full Agent round per tick
// instead of a sandboxed script).
const RuntimeAgent = "agent"

// agentModeMinInterval is the minimum execution interval for agent-mode jobs.
const agentModeMinInterval = time.Hour

// AgentIntervalGuardPrefix is the stable classification prefix carried by the
// agent-mode frequency-guard rejection. The API layer matches on it to map
// the error to HTTP 400 + the "validating" pipeline stage. Keep it stable.
const AgentIntervalGuardPrefix = "agent interval guard"

// selfHealThreshold is how many consecutive failures trigger a heal recompile.
const selfHealMaxPerWindow = 2
const selfHealWindow = 24 * time.Hour
const selfHealThreshold = 3

// Notification levels passed to the injected Notifier. The desktop side maps
// them to its own notification types.
const (
	NotifyLevelInfo    = "info"
	NotifyLevelSuccess = "success"
	NotifyLevelWarning = "warning"
)

// AgentRunner runs one Agent conversation round and returns the final text.
// Injected by the business side (cmd/hexclaw/main.go) wrapping engine.Process.
type AgentRunner func(ctx context.Context, job *Job) (string, error)

// Notifier pushes job-level notifications (heal results, agent job results)
// to the user. Injected by the business side (e.g. desktop notifications).
type Notifier func(job *Job, level, title, body string)

// agentSupport is the Scheduler's agent-mode extension state.
//
// Tradeoff: healTimes/quotaNotices are in-memory only — a process restart
// resets the 24h heal window. Acceptable for a desktop sidecar: restarts are
// rare and the worst case is a few extra heal attempts, never an unbounded
// loop within one process lifetime. Entries are pruned on RemoveJob.
type agentSupport struct {
	mu           sync.Mutex
	runner       AgentRunner
	notifier     Notifier
	healTimes    map[string][]time.Time // jobID → heal-attempt timestamps inside the 24h window
	quotaNotices map[string]time.Time   // jobID → last failure-class notification (anti-bombing)
}

// SetNotifier injects the notification callback. Without it notifications are
// silently dropped (history rows remain visible).
func (s *Scheduler) SetNotifier(fn Notifier) {
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	s.agent.notifier = fn
}

func (s *Scheduler) notify(job *Job, level, title, body string) {
	s.agent.mu.Lock()
	fn := s.agent.notifier
	s.agent.mu.Unlock()
	if fn != nil {
		fn(job, level, title, body)
	}
}

// SetAgentRunner injects the agent executor. Without it agent-mode job runs
// fail with an explicit error.
func (s *Scheduler) SetAgentRunner(runner AgentRunner) {
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	s.agent.runner = runner
}

// reasoningVerbs are the verbs that mark a task as cognitive — a hit means
// the task needs Agent reasoning on every run. Mechanical I/O verbs
// (collect/scrape/download/save/sync) are intentionally absent: those compile
// to scripts.
var reasoningVerbs = []string{
	"总结", "汇总并分析", "分析", "判断", "评估", "挑选", "筛选出最", "点评",
	"解读", "建议", "诊断", "审查", "归纳", "提炼", "撰写", "写一篇", "起草",
}

// reasoningVerbWordRe matches the English reasoning verbs as whole words —
// substring matching caused false positives ("review" inside "preview",
// "draft" inside "draftsman").
var reasoningVerbWordRe = regexp.MustCompile(`\b(summarize|analyze|review|evaluate|draft)\b`)

// ClassifyTaskMode decides the execution mode for a task: RuntimeAgent or ""
// (script mode).
//
// Design tradeoff: deterministic keyword classification (zero cost,
// testable); can later be upgraded to a compiler-LLM decision without
// changing the caller contract. Misclassification costs are asymmetric —
// labeling a cognitive task as script compiles a script that cannot work
// (the self-heal bridge backstops it), labeling a mechanical task as agent
// just burns extra tokens — so the verb list stays conservative.
func ClassifyTaskMode(prompt string) string {
	lower := strings.ToLower(prompt)
	for _, v := range reasoningVerbs {
		if strings.Contains(lower, v) {
			return RuntimeAgent
		}
	}
	if reasoningVerbWordRe.MatchString(lower) {
		return RuntimeAgent
	}
	return ""
}

// scheduleMinInterval estimates the schedule interval by advancing
// nextRunTime twice and taking the difference. Returns 0 on parse failure
// (AddJob's schedule validation reports the error).
func scheduleMinInterval(schedule string) time.Duration {
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.Local) // a Monday, avoids week-boundary ambiguity
	t1, err := nextRunTime(schedule, JobTypeCron, base)
	if err != nil {
		return 0
	}
	t2, err := nextRunTime(schedule, JobTypeCron, t1)
	if err != nil {
		return 0
	}
	return t2.Sub(t1)
}

// validateAgentModeSchedule is the frequency guard: agent-mode jobs require a
// minimum interval of 1 hour.
func validateAgentModeSchedule(schedule string) error {
	if iv := scheduleMinInterval(schedule); iv > 0 && iv < agentModeMinInterval {
		return fmt.Errorf(
			"%s: 该任务需要 AI 推理执行（每次消耗一轮对话），执行间隔不能小于 1 小时，当前为 %s——请调整执行计划（如 @hourly / @daily）",
			AgentIntervalGuardPrefix, iv,
		)
	}
	return nil
}

// runAgentJob runs one agent round and maps the outcome to the same RunResult
// shape the script path produces.
func (s *Scheduler) runAgentJob(ctx context.Context, job *Job) *RunResult {
	s.agent.mu.Lock()
	runner := s.agent.runner
	s.agent.mu.Unlock()

	if runner == nil {
		return &RunResult{Status: "error", Error: "agent 执行器未注入 — 引擎未就绪"}
	}

	start := time.Now()
	content, err := runner(ctx, job)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return &RunResult{Status: "error", Error: err.Error(), DurationMs: elapsed}
	}
	return &RunResult{Status: "success", Stdout: content, DurationMs: elapsed}
}

// deliverAgentResult routes a successful agent-mode run's output to the job's
// deliver targets. Script jobs self-deliver (the compiled script POSTs
// /api/v1/notify); agent jobs have no script, so the scheduler delivers here.
// Desktop-class targets ("chat"/"notify"/"push" and the empty default) become
// one desktop notification titled with the job name; IM channels without a
// wired path are logged so the result is at least traceable in history.
func (s *Scheduler) deliverAgentResult(job *Job, result *RunResult) {
	content := strings.TrimSpace(result.Stdout)
	if content == "" {
		return
	}
	notified := false
	for _, target := range EffectiveDeliver(job) {
		switch target {
		case "", "chat", "notify", "push":
			if !notified {
				s.notify(job, NotifyLevelInfo, job.Name, clipForHeal(content, 500))
				notified = true
			}
		default:
			slog.Warn("[cron] agent job deliver target has no wired path, result kept in history only",
				"source", "cron", "id", job.ID, "name", job.Name, "target", target)
		}
	}
}

// maybeSelfHeal is the self-heal bridge: after a script job fails
// selfHealThreshold times in a row (counting only runs newer than the last
// successful heal), recompile the script once with the failure context.
// Attempts beyond the cooldown-window quota are skipped and logged.
func (s *Scheduler) maybeSelfHeal(ctx context.Context, job *Job, lastResult *RunResult) {
	if job.Spec == nil || job.Spec.Runtime == RuntimeAgent || s.compiler == nil {
		return
	}
	// Self-healing runs on its own context: callers pass their short DB-write
	// ctx (10s dbCtx), which an LLM recompile always outlives — and a failure
	// record written with the already-expired ctx would vanish too, leaving
	// zero trace (BUG-20260611 finding #6, double annihilation).
	healCtx, healCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer healCancel()
	ctx = healCtx

	if !s.consecutiveFailures(ctx, job.ID, selfHealThreshold) {
		return
	}
	if !s.healQuotaOK(job.ID) {
		slog.Warn("[cron-heal] heal quota exhausted for the 24h window, skipping",
			"source", "cron", "id", job.ID, "name", job.Name)
		// A high-frequency job that keeps failing would bomb the user with
		// notifications — at most one failure-class notification per window.
		if s.shouldNotifyHealFailure(job.ID) {
			s.notify(job, NotifyLevelWarning, "定时任务持续失败",
				fmt.Sprintf("「%s」自动修复次数已用尽，请在自动化页面查看失败原因：%s", job.Name, clipForHeal(lastResult.Error, 120)))
		}
		return
	}
	// Count the ATTEMPT against the quota before compiling: a permanently
	// failing upstream must not trigger an unbounded recompile+notify loop
	// (the header promise is max 2 heals per 24h, attempts included).
	s.recordHealAttempt(job.ID)

	failCtx := lastResult.Error
	if lastResult.Stderr != "" {
		failCtx += "\nstderr: " + clipForHeal(lastResult.Stderr, 500)
	}
	enriched := fmt.Sprintf(
		"%s\n\n[自愈上下文] 该任务此前编译的脚本已连续失败 %d 次，最近错误：\n%s\n请根据错误原因生成修正后的脚本。",
		job.SourcePrompt, selfHealThreshold, failCtx,
	)

	slog.Info("[cron-heal] triggering self-heal recompile", "source", "cron", "id", job.ID, "name", job.Name)
	compileCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	spec, err := s.compiler.Compile(compileCtx, enriched, CompileHints{UserID: job.UserID})
	now := time.Now()
	if err != nil {
		slog.Warn("[cron-heal] self-heal recompile failed", "source", "cron", "id", job.ID, "err", err.Error())
		_ = s.persistHistory(ctx, job.ID, "heal_failed", "", "自愈重编译失败: "+err.Error(), 0, now, "", "", 0, nil)
		// Same anti-bombing budget as the quota-exhausted path: one
		// failure-class notification per cooldown window.
		if s.shouldNotifyHealFailure(job.ID) {
			s.notify(job, NotifyLevelWarning, "定时任务自动修复失败",
				fmt.Sprintf("「%s」连续失败且自动修复未成功，请在自动化页面处理。原因：%s", job.Name, clipForHeal(err.Error(), 120)))
		}
		return
	}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		slog.Warn("[cron-heal] spec marshal failed", "source", "cron", "id", job.ID, "err", err.Error())
		return
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE cron_jobs SET spec_json = ? WHERE id = ?`, string(specJSON), job.ID); err != nil {
		slog.Warn("[cron-heal] spec persist failed", "source", "cron", "id", job.ID, "err", err.Error())
		return
	}
	s.mu.Lock()
	if j, ok := s.jobs[job.ID]; ok {
		j.Spec = spec
	}
	s.mu.Unlock()
	_ = s.persistHistory(ctx, job.ID, "healed", "脚本已根据失败上下文自动重编译，下次执行使用新脚本", "", 0, now, "", "", 0, nil)
	s.notify(job, NotifyLevelSuccess, "定时任务已自动修复",
		fmt.Sprintf("「%s」连续失败后已自动重编译，下次执行使用修复后的脚本。", job.Name))
	slog.Info("[cron-heal] self-heal complete, script updated", "source", "cron", "id", job.ID, "name", job.Name)
}

// consecutiveFailures reports whether the n most recent runs all failed.
// Heal/pause rows are excluded, and only runs NEWER than the latest 'healed'
// row count — pre-heal failures must not satisfy the window again after a
// successful heal (review M3: premature re-heal). The cutoff uses MAX(id) of
// the healed rows: ids are insertion-ordered, which avoids DATETIME string
// comparison pitfalls.
func (s *Scheduler) consecutiveFailures(ctx context.Context, jobID string, n int) bool {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status FROM cron_job_runs
		 WHERE job_id = ? AND status IN ('success','failed','error','timeout')
		   AND id > COALESCE((SELECT MAX(id) FROM cron_job_runs WHERE job_id = ? AND status = 'healed'), 0)
		 ORDER BY run_at DESC, id DESC LIMIT ?`, jobID, jobID, n)
	if err != nil {
		return false
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var status string
		if rows.Scan(&status) != nil {
			return false
		}
		if status == "success" {
			return false
		}
		count++
	}
	return count >= n
}

func (s *Scheduler) healQuotaOK(jobID string) bool {
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	if s.agent.healTimes == nil {
		s.agent.healTimes = make(map[string][]time.Time)
	}
	cutoff := time.Now().Add(-selfHealWindow)
	var recent []time.Time
	for _, t := range s.agent.healTimes[jobID] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	s.agent.healTimes[jobID] = recent
	return len(recent) < selfHealMaxPerWindow
}

// recordHealAttempt charges one heal attempt (successful or not) against the
// job's cooldown-window quota.
func (s *Scheduler) recordHealAttempt(jobID string) {
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	if s.agent.healTimes == nil {
		s.agent.healTimes = make(map[string][]time.Time)
	}
	s.agent.healTimes[jobID] = append(s.agent.healTimes[jobID], time.Now())
}

// shouldNotifyHealFailure allows one failure-class notification (heal failed
// or quota exhausted) per cooldown window.
func (s *Scheduler) shouldNotifyHealFailure(jobID string) bool {
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	if s.agent.quotaNotices == nil {
		s.agent.quotaNotices = make(map[string]time.Time)
	}
	if last, ok := s.agent.quotaNotices[jobID]; ok && time.Since(last) < selfHealWindow {
		return false
	}
	s.agent.quotaNotices[jobID] = time.Now()
	return true
}

// pruneAgentState drops per-job heal/notice bookkeeping when a job is removed.
func (s *Scheduler) pruneAgentState(jobID string) {
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	delete(s.agent.healTimes, jobID)
	delete(s.agent.quotaNotices, jobID)
}

func clipForHeal(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
