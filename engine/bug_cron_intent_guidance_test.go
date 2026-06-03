// bug_cron_intent_guidance_test 守卫 D2.2 Layer 3：
//
// RED 场景：
//   - 用户在 chat 输入"创建一个定时任务，每天上午 10 点采集网易新闻 TOP10..."
//   - engine 把 MCP tools 都丢进 req.Tools → LLM 试图调 fs 工具 → tool_use_id 链路 400
//
// GREEN 守护：
//   - detectCronIntent 识别 cron-like 中英关键词
//   - applyCronIntentGuidance 强制 req.Tools=nil
//   - applyCronIntentGuidance 注入 metadata.cron_context=true（hexagon 守卫双保险）
//   - applyCronIntentGuidance prepend 引导 system prompt（指示 LLM 反问 + 禁工具）
package engine

import (
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
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
	if !strings.Contains(req.Messages[0].Content, "不要调用任何工具") {
		t.Errorf("引导内容缺关键约束 '不要调用任何工具'，实际：%s", req.Messages[0].Content)
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
	if !strings.Contains(merged, "不要调用任何工具") {
		t.Error("引导内容未合并入 system")
	}
	if !strings.Contains(merged, originalSys) {
		t.Error("原 system 内容丢失")
	}
}

func TestApplyCronIntentGuidance_NilSafe(t *testing.T) {
	// 不应 panic
	applyCronIntentGuidance(nil)
}
