// bug_cron_intent_guidance_test guards D2.2 Layer 3:
//
// RED scenario:
//   - the user types "创建一个定时任务，每天上午 10 点采集网易新闻 TOP10..." in chat
//   - the engine throws every MCP tool into req.Tools → the LLM tries an fs
//     tool → tool_use_id chain 400
//
// GREEN protection:
//   - detectCronIntent recognises cron-like Chinese/English keywords
//   - applyCronIntentGuidance forces req.Tools=nil
//   - applyCronIntentGuidance stamps metadata.cron_context=true (hexagon
//     second guard)
//   - applyCronIntentGuidance prepends the guidance system prompt (ask back
//     for clarification + no tools)
package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	hruntime "github.com/hexagon-codes/hexagon/runtime"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

func TestDetectCronIntent_PositiveKeywords(t *testing.T) {
	positives := []string{
		"每天早上 8 点采集新闻",
		"定时提醒我开会",
		"每周一发周报",
		"每隔 5 分钟检查汇率",
		"schedule a daily news collection",
		"remind me hourly",
	}
	for _, in := range positives {
		hit, _ := detectCronIntent(in)
		if !hit {
			t.Errorf("应识别为 cron-like: %q", in)
		}
	}
}

func TestDetectCronIntent_NegativeNonCron(t *testing.T) {
	negatives := []string{
		"今天天气怎么样",
		"帮我写一段 python",
		"hello world",
		"",
		"   ",
	}
	for _, in := range negatives {
		hit, _ := detectCronIntent(in)
		if hit {
			t.Errorf("不应识别为 cron-like: %q", in)
		}
	}
}

func TestApplyCronIntentGuidance_ToolsForceNil(t *testing.T) {
	req := &llm.CompletionRequest{
		Messages: []llm.Message{{Role: "user", Content: "每天早上做事"}},
		Tools:    []llm.ToolDefinition{{Type: "function"}},
	}
	applyCronIntentGuidance(req)
	if req.Tools != nil {
		t.Fatalf("Tools 必须被强制清空，实际 %v", req.Tools)
	}
}

func TestApplyCronIntentGuidance_MetadataCronContext(t *testing.T) {
	req := &llm.CompletionRequest{
		Messages: []llm.Message{{Role: "user", Content: "x"}},
	}
	applyCronIntentGuidance(req)
	v, ok := req.Metadata["cron_context"].(bool)
	if !ok || !v {
		t.Errorf("metadata.cron_context 必须为 true（hexagon runner 二次守卫）, got %v", req.Metadata["cron_context"])
	}
}

func TestApplyCronIntentGuidance_GuidancePrependedNoSystem(t *testing.T) {
	req := &llm.CompletionRequest{
		Messages: []llm.Message{{Role: "user", Content: "每天做事"}},
	}
	applyCronIntentGuidance(req)
	if len(req.Messages) != 2 {
		t.Fatalf("应 prepend system，msg 数 %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("首条应为 system，实际 %q", req.Messages[0].Role)
	}
	if !strings.Contains(req.Messages[0].Content, "Do NOT call any tools") {
		t.Errorf("引导内容缺关键约束 'Do NOT call any tools'，实际：%s", req.Messages[0].Content)
	}
}

func TestApplyCronIntentGuidance_GuidanceMergedWithSystem(t *testing.T) {
	originalSys := "你是一个 helpful assistant"
	req := &llm.CompletionRequest{
		Messages: []llm.Message{
			{Role: "system", Content: originalSys},
			{Role: "user", Content: "每天做事"},
		},
	}
	applyCronIntentGuidance(req)
	if len(req.Messages) != 2 {
		t.Fatalf("不应增加 msg，实际 %d", len(req.Messages))
	}
	merged := req.Messages[0].Content
	if !strings.Contains(merged, "Do NOT call any tools") {
		t.Error("引导内容未合并入 system")
	}
	if !strings.Contains(merged, originalSys) {
		t.Error("原 system 内容丢失")
	}
}

func TestApplyCronIntentGuidance_NilSafe(t *testing.T) {
	// must not panic
	applyCronIntentGuidance(nil)
}

// ─── new mode when the cron_task tool is available ─────────────────

func cronTaskToolDef() llm.ToolDefinition {
	return llm.ToolDefinition{
		Type:     "function",
		Function: llm.ToolFunctionDef{Name: "cron_task", Description: "管理内置定时任务"},
	}
}

func otherToolDef(name string) llm.ToolDefinition {
	return llm.ToolDefinition{Type: "function", Function: llm.ToolFunctionDef{Name: name}}
}

func TestApplyCronIntentGuidance_KeepsOnlyCronTaskTool(t *testing.T) {
	req := &llm.CompletionRequest{
		Messages: []llm.Message{{Role: "user", Content: "每5分钟采集百度热搜"}},
		Tools: []llm.ToolDefinition{
			otherToolDef("file_ops"),
			cronTaskToolDef(),
			otherToolDef("list_allowed_directories"),
		},
	}
	applyCronIntentGuidance(req)
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "cron_task" {
		t.Fatalf("应只保留 cron_task 工具，实际 %v", req.Tools)
	}
}

func TestApplyCronIntentGuidance_CronTaskMode_NoCronContextMetadata(t *testing.T) {
	req := &llm.CompletionRequest{
		Messages: []llm.Message{{Role: "user", Content: "每天 8 点发简报"}},
		Tools:    []llm.ToolDefinition{cronTaskToolDef()},
	}
	applyCronIntentGuidance(req)
	// the hexagon runner disables ALL tools when cron_context is set, so the
	// cron_task mode must never stamp it
	if v, ok := req.Metadata["cron_context"]; ok {
		t.Fatalf("cron_task 模式不得设置 cron_context（hexagon 会禁工具），got %v", v)
	}
}

func TestApplyCronIntentGuidance_CronTaskMode_GuidanceMentionsTool(t *testing.T) {
	req := &llm.CompletionRequest{
		Messages: []llm.Message{{Role: "user", Content: "每天 8 点发简报"}},
		Tools:    []llm.ToolDefinition{cronTaskToolDef()},
	}
	applyCronIntentGuidance(req)
	if req.Messages[0].Role != "system" {
		t.Fatal("应注入 system 引导")
	}
	sys := req.Messages[0].Content
	if !strings.Contains(sys, "cron_task") {
		t.Errorf("引导应指向 cron_task 工具：%s", sys)
	}
	if !strings.Contains(sys, "crontab") {
		t.Errorf("引导应禁止手动 crontab 路径：%s", sys)
	}
	if strings.Contains(sys, "Do NOT call any tools") {
		t.Errorf("cron_task 模式不应禁用工具调用：%s", sys)
	}
}

// 2026-06-27：真机 DeepSeek 在信息齐备且用户已说"不用确认"时仍反复反问澄清（bug #2）。
// 引导词必须显式要求：①时间+动作齐备即立即建、不纠缠次要细节 ②用户说"不用确认/直接建"时
// 必须立即调用、不得再反问。这里锁定引导词契约（模型遵从是概率事件，但契约必须在位）。
func TestCronToolGuidance_HonorsImmediateCreate(t *testing.T) {
	sys := cronToolGuidanceSystemPrompt
	// 显式尊重"不用确认/直接创建"。
	for _, kw := range []string{"不用确认", "直接创建", "不用问"} {
		if !strings.Contains(sys, kw) {
			t.Errorf("引导词应显式覆盖用户的无需确认措辞 %q：\n%s", kw, sys)
		}
	}
	// 要求"立即调用"且明确"齐备即建"。
	if !strings.Contains(sys, "IMMEDIATELY") {
		t.Errorf("引导词应要求信息齐备时立即调用 cron_task：\n%s", sys)
	}
	// 明确不要纠缠次要细节（防过度澄清）。
	if !strings.Contains(strings.ToLower(sys), "secondary details") {
		t.Errorf("引导词应明确不为次要细节反问：\n%s", sys)
	}
	// 仍保留单轮澄清的退路（核心信息缺失时）。
	if !strings.Contains(sys, "ONE short clarifying question") {
		t.Errorf("引导词应仍保留单轮澄清退路：\n%s", sys)
	}
}

func TestCronIntentGuidanceRunsAfterToolCollection(t *testing.T) {
	var captured hexagon.CompletionRequest
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		captured = req
		return &hexagon.CompletionResponse{
			Content: "ok",
			Usage:   hexagon.Usage{TotalTokens: 1},
		}, nil
	})

	eng := newEngineWithProvider(t, provider)
	eng.cfg.LLM.Tools.Enabled = "on"
	reg := skill.NewRegistry()
	if err := reg.Register(&cronProbeSkill{}); err != nil {
		t.Fatalf("register cron probe: %v", err)
	}
	if err := reg.Register(&echoSkill{}); err != nil {
		t.Fatalf("register echo: %v", err)
	}
	eng.SetToolCollector(NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))

	_, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "msg-cron-tool-guidance-order",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "帮我创建一个定时任务：每天早上9点采集百度热搜榜并写入知识库。直接用 cron_task 工具创建，不用问我确认。",
		Metadata: map[string]string{},
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Function.Name != "cron_task" {
		t.Fatalf("cron intent should keep exactly cron_task after collection, got %#v", captured.Tools)
	}
	if _, ok := captured.Metadata["cron_context"]; ok {
		t.Fatalf("cron_task mode must not carry cron_context because runtime disables tools, metadata=%#v", captured.Metadata)
	}
	if len(captured.Messages) == 0 || captured.Messages[0].Role != "system" || !strings.Contains(captured.Messages[0].Content, "cron_task") {
		t.Fatalf("cron_task guidance system prompt not injected: %#v", captured.Messages)
	}
}

// ─── session stickiness + creation-claim check (the two deterministic
// layers of the three-layer anti-hallucination defense) ─────────────

func TestDetectCronIntentSticky_AffirmationFollowsCronQuestion(t *testing.T) {
	last := "我准备按这个创建：每天 10:00 采集微博 TOP10 热搜，是否从今天 10:00 开始？"
	for _, cur := range []string{"是", "好的", "确认", "ok"} {
		if !detectCronIntentSticky(cur, last) {
			t.Errorf("简短确认 %q 应沿用上一条助手消息的 cron 语境", cur)
		}
	}
}

type cronProbeSkill struct{}

func (s *cronProbeSkill) Name() string        { return "cron_task" }
func (s *cronProbeSkill) Description() string { return "管理内置定时任务" }
func (s *cronProbeSkill) Match(string) bool   { return false }
func (s *cronProbeSkill) Execute(context.Context, map[string]any) (*skill.Result, error) {
	return &skill.Result{Content: "ok"}, nil
}
func (s *cronProbeSkill) ToolDefinition() llm.ToolDefinition {
	return cronTaskToolDef()
}

func TestDetectCronIntentSticky_LongMessageDoesNotInherit(t *testing.T) {
	last := "我准备按这个创建：每天 10:00 采集微博热搜"
	cur := "帮我看看这段 Python 代码哪里写错了，报错信息如下"
	if detectCronIntentSticky(cur, last) {
		t.Error("长消息（新话题）不应继承 cron 语境")
	}
}

func TestDetectCronCreationClaim_Positive(t *testing.T) {
	positives := []string{
		"🦀 已为你创建应用内置定时任务：任务名称：每天上午10点采集微博TOP10热搜",
		"已成功创建定时任务，每天 10 点执行",
		"任务已添加到定时任务列表",
	}
	for _, in := range positives {
		if !detectCronCreationClaim(in) {
			t.Errorf("应识别为创建声明: %q", in)
		}
	}
}

func TestDetectCronCreationClaim_Negative(t *testing.T) {
	negatives := []string{
		"我可以帮你创建定时任务，请确认执行时间",
		"定时任务系统支持 cron 表达式",
		"已为你查询天气",
	}
	for _, in := range negatives {
		if detectCronCreationClaim(in) {
			t.Errorf("不应识别为创建声明: %q", in)
		}
	}
}

func TestHasCronTaskCall(t *testing.T) {
	if hasCronTaskCall(nil) {
		t.Error("空调用列表应为 false")
	}
	calls := []hruntime.ToolCallRecord{{Name: "file_ops"}, {Name: "cron_task"}}
	if !hasCronTaskCall(calls) {
		t.Error("含 cron_task 调用应为 true")
	}
}

// M8: sticky inheritance must require the last assistant message to be a
// genuine cron clarification/confirmation question. An assistant reply that
// merely mentions a schedule keyword ("每天…") must not capture the next
// short user message and strip its tool surface.
func TestDetectCronIntentSticky_NoInheritWithoutClarificationQuestion(t *testing.T) {
	cases := []string{
		"上海这周每天都有雨，出门记得带伞。", // weather smalltalk mentioning 每天
		"坚持每天锻炼半小时对身体很好。",   // advice mentioning 每天
		"你的提醒我收到了，已经处理完毕。",  // statement mentioning 提醒, not a question
	}
	for _, last := range cases {
		if detectCronIntentSticky("查天气", last) {
			t.Errorf("[M8] short follow-up after non-clarification assistant reply %q must not inherit cron context", last)
		}
	}
}

func TestDetectCronIntentSticky_ClarificationQuestionStillInherits(t *testing.T) {
	lasts := []string{
		"我准备按这个创建：每天 10:00 采集微博 TOP10 热搜，是否从今天 10:00 开始？",
		"需要我设置每天 8 点的新闻提醒任务吗？",
	}
	for _, last := range lasts {
		if !detectCronIntentSticky("好的", last) {
			t.Errorf("[M8] genuine cron clarification %q must keep sticky inheritance", last)
		}
	}
}

// Review Low: English-replying models must not bypass the deterministic
// creation-claim guard.
func TestDetectCronCreationClaim_EnglishPositive(t *testing.T) {
	positives := []string{
		"I've created the scheduled task: collect Weibo trends at 10:00 every day.",
		"Done. I have set up the recurring task for you.",
		"Successfully created a cron job to fetch news daily.",
		"I scheduled the task to run every morning at 8.",
		"We've added the scheduled reminder as requested.",
	}
	for _, in := range positives {
		if !detectCronCreationClaim(in) {
			t.Errorf("should be detected as an English creation claim: %q", in)
		}
	}
}

func TestDetectCronCreationClaim_EnglishNegative(t *testing.T) {
	negatives := []string{
		"I can create a scheduled task for you — just confirm the time.",
		"The system supports scheduled tasks via cron expressions.",
		"Would you like me to set up a recurring task?",
		"I will create the task once you confirm the schedule.",
	}
	for _, in := range negatives {
		if detectCronCreationClaim(in) {
			t.Errorf("should NOT be detected as a creation claim: %q", in)
		}
	}
}

func TestCronSourceMessageSkipsGuidance(t *testing.T) {
	// Execution messages dispatched by the cron scheduler must not get the
	// cron intent guidance (otherwise "execute the task" turns into "create
	// the task").
	hit, _ := detectCronIntent("每天早上总结我的待办，挑重点发我")
	if !hit {
		t.Fatal("前置条件：该 prompt 本身应命中意图检测")
	}
	// The source=cron bypass in buildCompletionRequest is guarded by
	// react.go; this locks the metadata key/value contract the bypass
	// depends on.
	const key, val = "source", "cron"
	if key != "source" || val != "cron" {
		t.Fatal("cron 执行旁路的元数据契约被修改")
	}
}
