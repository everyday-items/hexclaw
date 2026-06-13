// cron_intent implements the D2.2 Layer 3 guidance prompt:
//
// When a user types a cron-like but incomplete description in chat ("每天做点
// 东西") that neither the Layer 1 fast path nor the Layer 2 LLM JSON parse
// caught, this layer is the backstop:
//   - detect the intent (keyword scan, zero cost)
//   - inject a "ask-back for clarification" guidance system prompt
//   - force req.Tools=nil (eliminates the tool_use_id chain 400 bug at the
//     protocol level)
//
// This is belt-and-suspenders: the frontend classifyCronIntent + the cron
// parse endpoint already cover 99% of the paths; this layer catches the
// remaining 1% (manual chat / third-party IM forwarding).
package engine

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/hexagon-codes/ai-core/llm"
	hruntime "github.com/hexagon-codes/hexagon/runtime"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/trace"
)

// cronDispatchSource is the metadata marker for messages dispatched by the
// cron scheduler (as opposed to typed by a user).
const cronDispatchSource = "cron"

// NewCronDispatchMessage builds the message a cron-dispatched agent job sends
// through the engine. It is the single place that stamps source=cron — the
// marker that matchSkillFastPath (skip keyword fast path) and
// buildCompletionRequest (skip cron intent guidance) both rely on. Callers
// (cmd/hexclaw AgentRunner) must use this instead of hand-building the
// message, so the contract stays regression-locked by engine tests.
// cronOutcomeContract instructs the agent to self-report the task outcome so
// the scheduler can distinguish "goal accomplished" from "replied but failed"
// (BUG-20260613: replies like "无法访问页面" were recorded as success).
//
// The marker is pinned to the literal ASCII token TASK_STATUS regardless of
// reply language — a Chinese-replying model (glm-4-flash) otherwise localizes
// it to "任务状态：失败" and the parser misses it. parseAgentOutcome also
// accepts that localized form as a fallback.
const cronOutcomeContract = "\n\n---\n[Scheduler contract] On the very last line of your reply, output the literal ASCII marker (do NOT translate the word TASK_STATUS, even if the rest of your reply is in another language): `TASK_STATUS: done` if the task goal was fully accomplished, or `TASK_STATUS: failed - <short reason>` if it was not (e.g. a tool was blocked, a page was unreachable, or data could not be saved). Output nothing after that line."

func NewCronDispatchMessage(userID, chatID, jobID, prompt string) *adapter.Message {
	if chatID == "" {
		// Stable per-job chat scope: every run of a job reuses one session
		// instead of spawning a new one per tick (BUG-20260613: each run
		// polluted the desktop chat sidebar with a fresh conversation).
		chatID = "cron:" + jobID
	}
	return &adapter.Message{
		// Dedicated platform so session listing can exclude scheduler runs
		// from user-facing chat lists.
		Platform: adapter.PlatformCron,
		UserID:   userID,
		ChatID:   chatID,
		Content:  prompt + cronOutcomeContract,
		Metadata: map[string]string{"source": cronDispatchSource, "cron_job_id": jobID},
	}
}

// cronIntentKeywords are cron-like trigger words (common Chinese and English
// phrasings). Any single hit marks the text as cron-like. False positives are
// cheap (one extra clarification prompt); false negatives are expensive
// (they used to trigger the tool_use_id chain 400).
var cronIntentKeywords = []string{
	"定时", "每天", "每周", "每月", "每年", "每小时", "每分钟", "每秒",
	"提醒", "周期", "周期性", "schedule", "cron", "remind",
	"daily", "hourly", "weekly", "monthly",
	"每隔", "每过",
}

// detectCronIntent returns (isCronLike, lookupMissing).
//
// lookupMissing=true means the intent is clear but a key field (time or
// action) is missing — that case takes the "ask back for clarification"
// route instead of the LLM tool-calling path.
func detectCronIntent(text string) (bool, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false, false
	}
	hit := false
	for _, kw := range cronIntentKeywords {
		if strings.Contains(lower, strings.ToLower(kw)) {
			hit = true
			break
		}
	}
	if !hit {
		return false, false
	}
	// Simplified heuristic: "feels like cron" but fewer than 16 runes usually
	// means half the information is missing.
	missing := len([]rune(lower)) < 16
	return true, missing
}

// cronGuidanceSystemPrompt steers the LLM to ask back for clarification and
// never call tools (the fallback when cron_task is unavailable). The Chinese
// snippets are intentional examples for the Chinese-facing UX.
const cronGuidanceSystemPrompt = `The user appears to be describing a scheduled task, but the information may be incomplete.

Your job:
1. Do NOT call any tools — reply directly in natural language, in the user's language
2. Politely ask the user to fill in the gaps: when should it run? what should it do?
3. Give one or two examples to help, e.g. "每天 8 点采集新闻头条" (collect news headlines at 8:00 every day)
4. Keep the reply short (≤ 80 characters)

Never:
- call any tool (mcp / filesystem / search / ...)
- pretend a task has been created
- fabricate execution results`

// cronToolGuidanceSystemPrompt steers the LLM to use the built-in cron_task
// tool. The Chinese snippets are intentional examples for the Chinese-facing UX.
const cronToolGuidanceSystemPrompt = `The user wants to create or manage a scheduled task. This app has a built-in scheduler operated exclusively through the cron_task tool.

Rules:
1. Information complete (when to run + what to do) → call the cron_task tool directly; the task is compiled and scheduled by the app
2. Time or action missing → first ask a short clarifying question in natural language, with one example (e.g. "每天 8 点采集新闻头条")
3. Listing / pausing / resuming / deleting tasks also goes through the cron_task tool
4. Never write script files and never tell the user to edit crontab manually — those never enter the app's task system
5. Never claim a task was created before calling cron_task and receiving a success result — the tool result is the only proof of creation`

// cronTaskToolName is the tool name of the built-in scheduled-task skill
// (skill/builtin.CronTaskSkill).
const cronTaskToolName = "cron_task"

// cronGuidanceActiveKey is an engine-internal metadata marker stamped on the
// incoming message when cron intent guidance was injected for this turn.
// The creation-claim guard keys off it so that replies in unrelated turns
// (e.g. a truthful restatement of a prior-turn creation) are never flagged.
const cronGuidanceActiveKey = "cron_guidance_active"

// applyCronIntentGuidance injects the cron guidance at the LLM request layer.
//
// Two modes:
//   - cron_task tool available: keep only that tool + inject usage guidance.
//     Does NOT set the cron_context metadata (the hexagon runner would
//     disable ALL tools because of it, including cron_task itself).
//   - unavailable (cron disabled): keep the original defense — tools=nil +
//     cron_context + ask-back guidance, eliminating the tool_use_id chain
//     400 at the protocol level.
//
// Called after buildCompletionRequest and before provider.Complete.
// The req is modified in place.
func applyCronIntentGuidance(req *llm.CompletionRequest) {
	if req == nil {
		return
	}

	guidance := cronGuidanceSystemPrompt
	var cronTool []llm.ToolDefinition
	for _, t := range req.Tools {
		if t.Function.Name == cronTaskToolName {
			cronTool = []llm.ToolDefinition{t}
			break
		}
	}

	toolsIn := len(req.Tools)
	if cronTool != nil {
		// Narrow the tool surface: keep only cron_task and strip the rest
		// (filesystem/mcp/search). This both stops the LLM from implementing
		// the schedule with the wrong tool and keeps the tool_use_id chain safe.
		req.Tools = cronTool
		guidance = cronToolGuidanceSystemPrompt
		logCronGuidanceApplied("tool", toolsIn, 1)
	} else {
		// Key point 1: tools=nil eliminates the tool_use_id chain 400 at the
		// protocol level.
		req.Tools = nil
		// Key point 2: stamp cron_context so the hexagon runner acts as a
		// second guard.
		if req.Metadata == nil {
			req.Metadata = make(map[string]any)
		}
		req.Metadata["cron_context"] = true
		logCronGuidanceApplied("fallback", toolsIn, 0)
	}

	// Prepend/merge the guidance into the system message (short lines so it
	// is not drowned out).
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		req.Messages[0].Content = guidance + "\n\n" + req.Messages[0].Content
	} else {
		req.Messages = append([]llm.Message{{Role: "system", Content: guidance}}, req.Messages...)
	}
}

// markCronGuidanceActive stamps the engine-internal marker consumed by
// guardCronCreationClaim. See cronGuidanceActiveKey.
func markCronGuidanceActive(msg *adapter.Message) {
	if msg == nil {
		return
	}
	if msg.Metadata == nil {
		msg.Metadata = make(map[string]string)
	}
	msg.Metadata[cronGuidanceActiveKey] = "true"
}

// cronClarificationNouns are creation/task nouns that, combined with a
// question marker and a schedule keyword, identify a genuine cron
// clarification/confirmation question from the assistant.
var cronClarificationNouns = []string{
	"任务", "创建", "设置", "新建", "添加", "提醒", "执行", "开始",
	"task", "schedule", "remind", "cron",
}

// isCronClarificationQuestion reports whether an assistant message is a
// genuine cron clarification/confirmation question (the phrasing the cron
// guidance prompts instruct the model to produce: a question that mentions
// both the schedule and the task/creation), as opposed to a reply that
// merely mentions a schedule keyword ("每天…" smalltalk).
//
// Note: history messages carry no metadata, so a metadata marker set when
// guidance was applied cannot survive the round-trip — phrase matching on
// the guidance's own output style is the most robust available signal.
func isCronClarificationQuestion(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return false
	}
	if !strings.ContainsAny(t, "?？") && !strings.Contains(t, "是否") {
		return false
	}
	if hit, _ := detectCronIntent(t); !hit {
		return false
	}
	for _, noun := range cronClarificationNouns {
		if strings.Contains(t, noun) {
			return true
		}
	}
	return false
}

// detectCronIntentSticky is the session-sticky variant of intent detection.
//
// A hit on the current message wins outright. When the current message is a
// short confirmation (≤ 8 runes, e.g. "是"/"好的"/"确认"), the cron context of
// the previous assistant turn is inherited — but only when that turn was a
// genuine cron clarification/confirmation question (M8). Typical flow: the
// assistant asks "是否从今天 10:00 开始？", the user answers "是"; this round
// must keep the cron guidance, otherwise the tool surface reverts to the
// full set and a weak model will claim the task was created without calling
// any tool. Conversely, "查天气" after an assistant reply that merely
// mentions "每天" must NOT lose its tool surface.
func detectCronIntentSticky(current, lastAssistant string) bool {
	if hit, _ := detectCronIntent(current); hit {
		return true
	}
	if len([]rune(strings.TrimSpace(current))) > 8 {
		return false
	}
	return isCronClarificationQuestion(lastAssistant)
}

// cronCreationClaimRes match common "claims a scheduled task was created"
// phrasings. Deliberately narrowed to creation-verb + task-noun combinations
// (and, in English, a completed-action subject/auxiliary) so ordinary
// discussion or offers ("I can create…", "Would you like me to set up…")
// are not caught.
var cronCreationClaimRes = []*regexp.Regexp{
	// Chinese: 已/成功 + creation verb + scheduled-task noun.
	regexp.MustCompile(`(已|成功)(为你|为您)?[^\n。，]{0,6}(创建|添加|建立|新建)[^\n。]{0,30}(定时任务|计划任务|cron)`),
	// English: completed creation by the assistant ("I've created the
	// scheduled task", "I scheduled the task", "we've added the reminder").
	regexp.MustCompile(`(?i)\b(?:i|we)(?:'ve|\s+have|\s+just)?\s+(?:successfully\s+)?(?:created|added|set\s+up|registered|scheduled)\s+(?:the\s+|a\s+|an\s+|your\s+)?(?:new\s+)?(?:scheduled\s+|recurring\s+|cron\s+|timed\s+)?(?:task|job|reminder)\b`),
	// English: bare "successfully created a cron job" (no subject).
	regexp.MustCompile(`(?i)\bsuccessfully\s+(?:created|added|set\s+up|registered|scheduled)\s+(?:the\s+|a\s+|an\s+|your\s+)?(?:new\s+)?(?:scheduled\s+|recurring\s+|cron\s+|timed\s+)?(?:task|job|reminder)\b`),
	// English passive: "the scheduled task has been created / is now in place".
	regexp.MustCompile(`(?i)\b(?:scheduled|recurring|cron|timed)\s+(?:task|job|reminder)\s+(?:has\s+been|was|is\s+now)\s+(?:created|added|set\s+up|scheduled|registered|in\s+place)\b`),
}

// detectCronCreationClaim reports whether the reply text claims a scheduled
// task was created.
func detectCronCreationClaim(content string) bool {
	for _, re := range cronCreationClaimRes {
		if re.MatchString(content) {
			return true
		}
	}
	return false
}

// hasCronTaskCall reports whether cron_task appears in this round's runtime
// tool-call records.
func hasCronTaskCall(calls []hruntime.ToolCallRecord) bool {
	for _, c := range calls {
		if c.Name == cronTaskToolName {
			return true
		}
	}
	return false
}

// hasCronTaskAdapterCall is hasCronTaskCall for the adapter-level tool-call
// shape used by finalizeReply and the legacy stream paths.
func hasCronTaskAdapterCall(calls []adapter.ToolCall) bool {
	for _, c := range calls {
		if c.Name == cronTaskToolName {
			return true
		}
	}
	return false
}

// cronClaimGuardNotice is the user-visible correction appended to a flagged
// reply (user-facing text stays Chinese).
const cronClaimGuardNotice = "\n\n⚠️ 系统校验：上述\"已创建\"并未真正执行——本轮没有调用定时任务工具。请回复「创建」，我会通过内置 cron_task 工具实际创建并出现在「自动化」页面。"

// guardCronCreationClaim is the deterministic anti-hallucination backstop
// shared by every reply-finalize path (non-stream, runtime stream, legacy
// stream): when cron intent guidance was active for THIS turn and the reply
// claims a scheduled task was created without any cron_task call, it returns
// the user-visible correction notice to append and logs the interception.
//
// The guidance-active gate also prevents the cross-turn false positive where
// a model truthfully RESTATES a prior-turn creation in an unrelated turn.
// Tradeoff: a hallucinated claim in a turn with no detectable cron intent is
// not flagged — accepted, because flagging without intent context produced
// false positives on truthful restatements.
func guardCronCreationClaim(ctx context.Context, msg *adapter.Message, content string, cronTaskCalled bool, sessionID, modelName string, toolCallCount int) (string, bool) {
	if msg == nil || msg.Metadata[cronGuidanceActiveKey] != "true" {
		return "", false
	}
	if cronTaskCalled || !detectCronCreationClaim(content) {
		return "", false
	}
	trace.L(ctx).Warn("[cron-guard] scheduled-task creation claim without a cron_task tool call",
		"session", sessionID, "model", modelName, "tool_calls", toolCallCount)
	return cronClaimGuardNotice, true
}

// logCronGuidanceApplied records the guidance mode and tool-surface
// narrowing, used to diagnose "the agent did not call cron_task" issues
// (mode=tool means the tool was present; mode=fallback means it was not
// recalled).
func logCronGuidanceApplied(mode string, toolsIn, toolsOut int) {
	slog.Info("[cron-intent] guidance applied",
		"mode", mode, "tools_in", toolsIn, "tools_out", toolsOut)
}
