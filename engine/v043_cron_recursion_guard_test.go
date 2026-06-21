package engine

// cron Agent 递归防护测试：cron 派发的工具集必须剔除
// 自排程 / 自写 Skill / 自装 Skill·MCP 这类自升级工具；普通 chat 不受影响。

import (
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func toolNamesOf(tools []llm.ToolDefinition) map[string]bool {
	m := make(map[string]bool, len(tools))
	for _, t := range tools {
		m[t.Function.Name] = true
	}
	return m
}

func TestCronRecursionGuard_StripsSelfEscalationTools(t *testing.T) {
	all := []llm.ToolDefinition{
		llm.NewToolDefinition("browser", "", nil),
		llm.NewToolDefinition("cron_task", "", nil),
		llm.NewToolDefinition("create_skill", "", nil),
		llm.NewToolDefinition("manage_skill", "", nil),
		llm.NewToolDefinition("manage_mcp_server", "", nil),
		llm.NewToolDefinition("knowledge_ingest", "", nil),
		llm.NewToolDefinition("search", "", nil),
	}

	// cron dispatch → denylist stripped, benign tools kept.
	cronMsg := &adapter.Message{Metadata: map[string]string{"source": cronDispatchSource}}
	got := toolNamesOf(stripCronRecursiveTools(cronMsg, all))
	for _, denied := range cronRecursiveToolDenylist {
		if got[denied] {
			t.Fatalf("cron dispatch must NOT expose %q (recursion/escalation vector)", denied)
		}
	}
	for _, keep := range []string{"browser", "knowledge_ingest", "search"} {
		if !got[keep] {
			t.Fatalf("benign tool %q was wrongly stripped", keep)
		}
	}

	// Case-insensitive: an upper-cased dup must still be denied.
	mixed := append([]llm.ToolDefinition{llm.NewToolDefinition("Cron_Task", "", nil)}, all...)
	if toolNamesOf(stripCronRecursiveTools(cronMsg, mixed))["Cron_Task"] {
		t.Fatalf("denylist must be case-insensitive")
	}

	// Interactive chat (no cron source) keeps every tool — guard is cron-scoped.
	chatMsg := &adapter.Message{Metadata: map[string]string{}}
	if len(stripCronRecursiveTools(chatMsg, all)) != len(all) {
		t.Fatalf("interactive chat must keep all %d tools", len(all))
	}

	// Other system dispatches (heartbeat/webhook) are NOT cron → unaffected.
	hbMsg := &adapter.Message{Metadata: map[string]string{"source": heartbeatDispatchSource}}
	if len(stripCronRecursiveTools(hbMsg, all)) != len(all) {
		t.Fatalf("non-cron system dispatch must keep all tools")
	}
}
