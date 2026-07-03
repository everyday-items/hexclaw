package engine

// 功能优先：cron 派发的工具集不再剔除自排程 / 自写 Skill /
// 自装 Skill·MCP。自动任务需要完整工具能力；普通 chat 同样不受影响。

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

func TestCronRecursionGuard_KeepsFunctionalTools(t *testing.T) {
	all := []llm.ToolDefinition{
		llm.NewToolDefinition("browser", "", nil),
		llm.NewToolDefinition("cron_task", "", nil),
		llm.NewToolDefinition("create_skill", "", nil),
		llm.NewToolDefinition("manage_skill", "", nil),
		llm.NewToolDefinition("manage_mcp_server", "", nil),
		llm.NewToolDefinition("knowledge_ingest", "", nil),
		llm.NewToolDefinition("search", "", nil),
	}

	// cron dispatch → all tools kept.
	cronMsg := &adapter.Message{Metadata: map[string]string{"source": cronDispatchSource}}
	got := toolNamesOf(stripCronRecursiveTools(cronMsg, all))
	for _, keep := range []string{"browser", "cron_task", "create_skill", "manage_skill", "manage_mcp_server", "knowledge_ingest", "search"} {
		if !got[keep] {
			t.Fatalf("benign tool %q was wrongly stripped", keep)
		}
	}

	// Case-insensitive legacy path is now no-op too.
	mixed := append([]llm.ToolDefinition{llm.NewToolDefinition("Cron_Task", "", nil)}, all...)
	if !toolNamesOf(stripCronRecursiveTools(cronMsg, mixed))["Cron_Task"] {
		t.Fatalf("functional cron dispatch must keep Cron_Task")
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
