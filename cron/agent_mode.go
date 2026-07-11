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

	"github.com/hexagon-codes/hexclaw/skill"
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

// AgentResult is one agent round's outcome: the final text plus the names of
// the tools the agent actually invoked. ToolNames lets the scheduler verify a
// self-reported success — a job whose purpose is to store knowledge but that
// never called knowledge_ingest stored nothing, so its "done" verdict is a
// hallucination, not a result (audit C1).
type AgentResult struct {
	Content   string
	ToolNames []string
}

// AgentRunner runs one Agent conversation round and returns its outcome.
// Injected by the business side (cmd/hexclaw/main.go) wrapping engine.Process.
type AgentRunner func(ctx context.Context, job *Job) (AgentResult, error)

// Notifier pushes job-level notifications (heal results, agent job results)
// to the user. Injected by the business side (e.g. desktop notifications).
type Notifier func(job *Job, level, title, body string)

// Deliverer routes a successful run's output to a non-desktop deliver target
// (an IM channel such as feishu/discord/wechat). Injected by the business side
// over the platform adapters. Without it, IM targets fall back to history-only
// (review L2).
type Deliverer func(job *Job, target, content string) error

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
	deliverer    Deliverer
	healTimes    map[string][]time.Time // jobID → heal-attempt timestamps inside the 24h window
	quotaNotices map[string]time.Time   // jobID → last failure-class notification (anti-bombing)
}

// SetDeliverer injects the IM delivery callback. Without it, non-desktop
// deliver targets are logged and kept in history only.
func (s *Scheduler) SetDeliverer(fn Deliverer) {
	s.agent.mu.Lock()
	defer s.agent.mu.Unlock()
	s.agent.deliverer = fn
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

	// Stable snapshot base title for any knowledge_ingest this run performs: a
	// scheduled job's name keys its KB snapshot series, so repeated runs (timer,
	// webhook trigger, or escalation — all funnel here) append under one coherent
	// title instead of fragmenting under the title the model improvises each run.
	// Stamped here (not in the cmd wiring) so the contract is package-testable.
	if name := strings.TrimSpace(job.Name); name != "" {
		ctx = skill.WithSnapshotBaseTitle(ctx, name)
	}

	start := time.Now()
	res, err := runner(ctx, job)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return &RunResult{Status: "error", Error: err.Error(), DurationMs: elapsed}
	}
	// The dispatch prompt (engine.NewCronDispatchMessage) asks the agent to
	// end with a TASK_STATUS line; map it to the run status so "replied but
	// could not do the task" no longer records success (BUG-20260613).
	failed, reason, cleaned := parseAgentOutcome(res.Content)
	if failed {
		return &RunResult{Status: "failed", Stdout: cleaned, Error: reason, DurationMs: elapsed}
	}
	// C1: a job whose stated purpose is to store knowledge must actually invoke
	// knowledge_ingest — the only channel that persists a document. A "done"
	// verdict with no such tool call means the agent hallucinated the ingest;
	// nothing was stored, so the run is a failure regardless of what it said.
	if jobRequiresIngest(job) && !containsToolFold(res.ToolNames, knowledgeIngestTool) {
		return &RunResult{
			Status:     "failed",
			Stdout:     cleaned,
			Error:      "agent reported success but never called " + knowledgeIngestTool + " — no document was stored",
			DurationMs: elapsed,
		}
	}
	return &RunResult{Status: "success", Stdout: cleaned, DurationMs: elapsed}
}

// runAgentJobWithPrompt runs one agent round with an explicit prompt (the H1
// escalation prompt) instead of the gate job's compiled SourcePrompt. The gate
// Script and EscalatePrompt share intent but are distinct artifacts — the Script
// is already compiled into the cheap Starlark gate — so escalation must drive the
// agent with EscalatePrompt, not the gate's source.
func (s *Scheduler) runAgentJobWithPrompt(ctx context.Context, job *Job, prompt string) *RunResult {
	cp := *job
	cp.SourcePrompt = prompt
	return s.runAgentJob(ctx, &cp)
}

// escalateMinInterval floors how often a wake_agent gate may escalate to the
// Agent, mirroring the 1h interval agent-mode jobs obey — a Starlark gate runs
// every tick, so without this a broken gate that always wakes would burn tokens
// every tick (P1-2 cost guard).
const escalateMinInterval = time.Hour

// canEscalate reports whether enough time has elapsed since this job last
// escalated. First escalation always allowed.
func (s *Scheduler) canEscalate(jobID string) bool {
	if v, ok := s.lastEscalated.Load(jobID); ok {
		if last, ok := v.(time.Time); ok && time.Since(last) < escalateMinInterval {
			return false
		}
	}
	return true
}

// markEscalated records the escalation time for the cost guard and surfaces it
// in run history via a structured log (does not mutate result.Data).
func (s *Scheduler) markEscalated(jobID string) {
	s.lastEscalated.Store(jobID, time.Now())
	slog.Info("[cron] escalated", "source", "cron", "id", jobID)
}

// knowledgeIngestTool is the only skill through which the Agent can persist a
// document to the knowledge base. Keep in sync with the skill registration
// (cmd/hexclaw/main.go) and engine.systemDispatchToolFloor.
const knowledgeIngestTool = "knowledge_ingest"

// ingestIntentKeywords mark a job whose stated purpose is to store something
// into the knowledge base. The set is deliberately conservative: each phrase
// is unambiguous about persistence, so a success verdict with no
// knowledge_ingest call is a provable hallucination, not a guess.
var ingestIntentKeywords = []string{
	"入库", "知识库", "收录到", "存入知识", "归档到知识",
	"knowledge base", knowledgeIngestTool, "ingest",
}

// jobRequiresIngest reports whether the job's source prompt declares an intent
// to persist into the knowledge base.
func jobRequiresIngest(job *Job) bool {
	if job == nil {
		return false
	}
	lower := strings.ToLower(job.SourcePrompt)
	for _, kw := range ingestIntentKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// containsToolFold reports whether target appears in names, case-insensitively
// and ignoring surrounding whitespace.
func containsToolFold(names []string, target string) bool {
	for _, n := range names {
		if strings.EqualFold(strings.TrimSpace(n), target) {
			return true
		}
	}
	return false
}

// outcomeMarkerPrefixes are the accepted forms of the scheduler outcome
// marker. The English ASCII token is pinned by the contract, but a
// Chinese-replying model (glm-4-flash) localizes it, so the localized form is
// accepted as a fallback (BUG-20260613: the live model emitted "任务状态：失败").
var outcomeMarkerPrefixes = []string{"task_status:", "任务状态：", "任务状态:"}

// outcomeFailureTokens mark a self-reported failure verdict, in either
// language. Anything else after the marker is treated as success.
var outcomeFailureTokens = []string{"failed", "fail", "失败", "未完成", "无法完成", "未成功"}

// parseAgentOutcome extracts the trailing outcome marker from an agent reply.
// Returns failed=true with the reason when the agent self-reported failure;
// cleaned is the reply with the marker line removed. A reply with no marker
// (model ignored the contract) is treated as success, unchanged.
//
// The marker is matched on any of the last few non-empty lines (models
// sometimes add a postscript after it) and in either language.
func parseAgentOutcome(content string) (failed bool, reason, cleaned string) {
	lines := strings.Split(strings.TrimRight(content, "\n \t"), "\n")
	scanned := 0
	for i := len(lines) - 1; i >= 0 && scanned < 4; i-- {
		raw := strings.TrimSpace(lines[i])
		if raw == "" {
			continue
		}
		scanned++
		// Models often wrap the marker in backticks or bold/italic markers.
		line := strings.Trim(raw, "`*_ ")
		lower := strings.ToLower(line)

		var verdict string
		matched := false
		for _, p := range outcomeMarkerPrefixes {
			if strings.HasPrefix(lower, p) {
				verdict = strings.TrimSpace(line[len(p):])
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		cleaned = strings.TrimSpace(strings.Join(append(append([]string{}, lines[:i]...), lines[i+1:]...), "\n"))
		lowerVerdict := strings.ToLower(verdict)
		for _, tok := range outcomeFailureTokens {
			// Match the verdict's LEADING token only — the contract puts the
			// verdict word first ("failed - reason"). Substring matching would
			// misclassify "done, failover path used" as a failure (review M1).
			if !strings.HasPrefix(lowerVerdict, tok) {
				continue
			}
			// reason = whatever follows the failure token, minus separators.
			reason = strings.TrimSpace(strings.TrimLeft(verdict[len(tok):], " -—:："))
			if reason == "" {
				reason = "agent reported the task as not accomplished"
			}
			return true, reason, cleaned
		}
		return false, "", cleaned
	}
	return false, "", content
}

// deliverableContent renders a successful run's user-facing payload. An agent
// job's text lives in Stdout (the cleaned reply); a script job's result lives
// in Data (the "data" field of its output-contract JSON, with Stdout being the
// raw JSON line). Preferring Data gives script jobs a readable delivery instead
// of dumping raw JSON.
func deliverableContent(result *RunResult) string {
	if result.Data != nil {
		switch v := result.Data.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case map[string]any:
			// Structured payload: deliver the script's human-facing field if it
			// provided one. A raw object is never JSON-dumped to a notification —
			// the structured data stays in run history, not the user's inbox.
			if s := readableField(v); s != "" {
				return s
			}
		}
	}
	// No deliverable payload. Agent jobs put readable text in Stdout; a script
	// that produced only its raw output-contract JSON line has nothing worth
	// delivering — suppress it so the scheduler skips a JSON-looking notification.
	out := strings.TrimSpace(result.Stdout)
	if isContractJSONLine(out) {
		return ""
	}
	return out
}

// readableField returns the first human-facing string field of a structured
// script payload, so a notification shows prose instead of a raw-JSON dump.
// Scripts opt in by emitting one of these keys in their "data" object.
func readableField(m map[string]any) string {
	for _, k := range []string{"message", "summary", "title", "text"} {
		if s, ok := m[k].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

// isContractJSONLine reports whether s is a single JSON object carrying the
// script output contract's "status" field (i.e. the bare contract line with no
// human payload), as opposed to free-form agent text.
func isContractJSONLine(s string) bool {
	if !strings.HasPrefix(s, "{") {
		return false
	}
	var probe struct {
		Status string `json:"status"`
	}
	return json.Unmarshal([]byte(s), &probe) == nil && probe.Status != ""
}

// deliverResult routes a successful run's output to the job's deliver targets —
// for BOTH script and agent jobs (review C2: script jobs previously delivered
// nothing, the "self-deliver via /api/v1/notify" path was a phantom endpoint).
// Desktop-class targets ("chat"/"notify"/"push" and the empty default) become
// one desktop notification titled with the job name; IM channels route through
// the injected Deliverer, falling back to history-only when none is wired
// (review L2).
func (s *Scheduler) deliverResult(job *Job, result *RunResult) {
	if !s.jobGenerationCurrentFresh(job) {
		slog.Info("[cron] stale generation delivery suppressed",
			"source", "cron", "id", job.ID, "generation", job.Generation)
		return
	}
	content := deliverableContent(result)
	if content == "" {
		return
	}
	// H3 [SILENT]：成功结果含 [SILENT] 标记 → 抑制投递（产物已写历史）。失败永远
	// 投递、不经本函数（executeJob 仅 success 调 deliverResult）。
	if hasSilentMarker(content) {
		slog.Info("[cron] [SILENT] marker — delivery suppressed, result kept in history",
			"source", "cron", "id", job.ID, "name", job.Name)
		s.recordDeliveryOutcomeForGeneration(job, nil) // 抑制非失败 → 清零投递错误
		return
	}
	s.agent.mu.Lock()
	deliverer := s.agent.deliverer
	s.agent.mu.Unlock()

	var deliverErrs []string
	notified := false
	for _, target := range EffectiveDeliver(job) {
		if !s.jobGenerationCurrentFresh(job) {
			return
		}
		switch target {
		case "", "chat", "notify", "push":
			if !notified {
				s.notify(job, NotifyLevelInfo, job.Name, clipForHeal(content, 500))
				notified = true
			}
		default:
			if deliverer == nil {
				slog.Warn("[cron] no deliverer wired for IM target, result kept in history only",
					"source", "cron", "id", job.ID, "name", job.Name, "target", target)
				continue
			}
			if err := deliverer(job, target, content); err != nil {
				deliverErrs = append(deliverErrs, target+": "+err.Error())
				slog.Warn("[cron] IM deliver failed, result kept in history",
					"source", "cron", "id", job.ID, "name", job.Name, "target", target, "err", err.Error())
			}
		}
	}
	// H4：投递结果与执行解耦 —— 全成功清零 LastDeliveryError，否则记录失败目标。
	s.recordDeliveryOutcomeForGeneration(job, deliverErrs)
}

// recordDeliveryOutcomeForGeneration writes delivery bookkeeping only to the
// definition that initiated the delivery. Stable-key upsert intentionally
// reuses the ID, so an ID-only write can clear or overwrite replacement state.
func (s *Scheduler) recordDeliveryOutcomeForGeneration(job *Job, errs []string) {
	if job == nil {
		return
	}
	if !s.jobGenerationCurrentFresh(job) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.currentJobGenerationLocked(job) {
		return
	}
	j, ok := s.jobs[job.ID]
	if !ok {
		return
	}
	if len(errs) == 0 {
		j.LastDeliveryError = ""
		return
	}
	j.LastDeliveryError = strings.Join(errs, "; ")
}

// recordDeliveryOutcome 把本次投递结果写回 jobs map 的规范记录（deliverResult 操作
// 的是 per-run 副本）。仅供显式 ID 管理/兼容调用；执行路径必须用
// recordDeliveryOutcomeForGeneration。errs 为空 = 全部成功 → 清零。
func (s *Scheduler) recordDeliveryOutcome(jobID string, errs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[jobID]
	if !ok {
		return
	}
	if len(errs) == 0 {
		j.LastDeliveryError = ""
		return
	}
	j.LastDeliveryError = strings.Join(errs, "; ")
}

// hasSilentMarker reports whether content carries the [SILENT] delivery-suppression
// marker (case-insensitive). The agent emits it to record a run in history without
// pushing a notification (e.g. a monitor that found nothing noteworthy this tick).
func hasSilentMarker(content string) bool {
	return strings.Contains(strings.ToUpper(content), "[SILENT]")
}

// maybeAlertAgentFailure notifies the user when an agent-mode job has failed
// consecutively past selfHealThreshold. Agent jobs have no compiled script to
// recompile, so they cannot self-heal (maybeSelfHeal returns early for them) —
// but silent permanent failure is worse than a script that keeps retrying, so
// they get a failure-class alert instead, throttled by the same 24h
// anti-bombing budget the heal path uses (review H1).
func (s *Scheduler) maybeAlertAgentFailure(_ context.Context, job *Job, lastResult *RunResult) {
	if job.Spec == nil || job.Spec.Runtime != RuntimeAgent {
		return
	}
	// Own context: the caller's dbCtx may be near expiry, and the failure-count
	// query must not be annihilated alongside it (same reasoning as maybeSelfHeal).
	alertCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !s.jobGenerationCurrentFresh(job) {
		return
	}
	if !s.consecutiveFailures(alertCtx, job.ID, selfHealThreshold) {
		return
	}
	if !s.shouldNotifyHealFailure(job.ID) {
		return
	}
	// consecutiveFailures may consume alertCtx; notification is a separate
	// boundary and needs an independent generation check.
	if !s.jobGenerationCurrentFresh(job) {
		return
	}
	s.notify(job, NotifyLevelWarning, "定时任务持续失败",
		fmt.Sprintf("「%s」连续 %d 次执行失败，请在自动化页面查看原因：%s",
			job.Name, selfHealThreshold, clipForHeal(lastResult.Error, 120)))
}

// buildHealPrompt assembles the self-heal recompile prompt. It must carry the
// FAILING script alongside the error text: with only the error, the compiler
// regenerates another blind guess over the same data source and the heal never
// converges (BUG-20260704 — a heal "succeeded" and the healed script failed
// with the identical "no items found in data structure"). Seeing the exact
// parse paths that already failed lets the LLM pick a different extraction
// strategy, ideally driven by the structural evidence the error carries.
func buildHealPrompt(job *Job, failCtx string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n[自愈上下文] 该任务此前编译的脚本已连续失败 %d 次，最近错误：\n%s\n",
		job.SourcePrompt, selfHealThreshold, failCtx)
	if job.Spec != nil && strings.TrimSpace(job.Spec.Script) != "" {
		fmt.Fprintf(&b, "\n[已失败的脚本] 以下脚本的解析路径已被证明与数据源实际返回结构不符，禁止原样复用其字段路径；请优先依据错误信息中的结构线索（如 keys 列表）定位真实数据路径，改用不同的提取策略：\n%s\n",
			clipForHeal(job.Spec.Script, 4000))
	}
	b.WriteString("\n请根据错误原因生成修正后的脚本。")
	return b.String()
}

// maybeSelfHeal is the self-heal bridge: after a script job fails
// selfHealThreshold times in a row (counting only runs newer than the last
// successful heal), recompile the script once with the failure context.
// Attempts beyond the cooldown-window quota are skipped and logged.
func (s *Scheduler) maybeSelfHeal(_ context.Context, job *Job, lastResult *RunResult) {
	if job.Spec == nil || job.Spec.Runtime == RuntimeAgent || s.compiler == nil {
		return
	}
	// Self-healing runs on its own context: callers pass their short DB-write
	// ctx (10s dbCtx), which an LLM recompile always outlives — and a failure
	// record written with the already-expired ctx would vanish too, leaving
	// zero trace (BUG-20260611 finding #6, double annihilation).
	ctx, healCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer healCancel()

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
	enriched := buildHealPrompt(job, failCtx)

	slog.Info("[cron-heal] triggering self-heal recompile", "source", "cron", "id", job.ID, "name", job.Name)
	compileCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	spec, err := s.compiler.Compile(compileCtx, enriched, CompileHints{UserID: job.UserID})
	now := time.Now()
	if err != nil {
		slog.Warn("[cron-heal] self-heal recompile failed", "source", "cron", "id", job.ID, "err", err.Error())
		persisted, _ := s.persistHistoryForGeneration(ctx, job, "heal_failed", "", "自愈重编译失败: "+err.Error(), 0, now, "", "", 0, nil)
		if !persisted {
			return
		}
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
	res, err := s.db.ExecContext(ctx,
		`UPDATE cron_jobs SET spec_json = ? WHERE id = ? AND meta = ?`,
		string(specJSON), job.ID, serializeJobMeta(job))
	if err != nil {
		slog.Warn("[cron-heal] spec persist failed", "source", "cron", "id", job.ID, "err", err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		// Legacy unit/in-memory callers historically heal jobs with no cron_jobs
		// row. Preserve that no-row behavior only for generation-less jobs; a
		// persisted generation or an existing mismatched row is superseded.
		var exists int
		queryErr := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM cron_jobs WHERE id = ?`, job.ID).Scan(&exists)
		if job.Generation != "" || queryErr != nil || exists != 0 {
			slog.Info("[cron-heal] stale generation heal ignored",
				"source", "cron", "id", job.ID, "generation", job.Generation)
			return
		}
	}
	s.mu.Lock()
	if !s.currentJobGenerationLocked(job) {
		s.mu.Unlock()
		return
	}
	if current, ok := s.jobs[job.ID]; ok {
		current.Spec = spec
	}
	s.mu.Unlock()

	// Re-check before each externally visible side effect. Do not hold the
	// scheduler mutex while invoking notifier callbacks: callbacks are external
	// code and may legitimately call back into scheduler read APIs.
	if !s.currentJobGeneration(job) {
		return
	}
	persisted, _ := s.persistHistoryForGeneration(ctx, job, "healed", "脚本已根据失败上下文自动重编译，下次执行使用新脚本", "", 0, now, "", "", 0, nil)
	if !persisted {
		return
	}
	if !s.currentJobGeneration(job) {
		return
	}
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
