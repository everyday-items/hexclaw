package engine

// BUG-20260616: splitIndexes chose the compression boundary purely by position
// (len-contextKeepRecent). When that boundary fell between an assistant tool_call
// and its tool result, the assistant was summarized away while the orphan tool
// result stayed in the retained segment. Chat APIs reject a tool result with no
// preceding tool_call (HTTP 400), failing the whole turn after compression.

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
)

func TestBug20260616_CompressKeepsToolPairs(t *testing.T) {
	big := strings.Repeat("x", 6000) // push total well past the LLM-summary threshold

	var msgs []llm.Message
	msgs = append(msgs, llm.SystemMessage("sys"))
	for i := 0; i < 15; i++ {
		msgs = append(msgs, llm.UserMessage(big))
		msgs = append(msgs, llm.AssistantMessage(big))
	}
	// Tail arranged so keepStart (= len-8) lands exactly on the tool result whose
	// assistant tool_call sits one slot earlier (and would be summarized away).
	msgs = append(msgs, llm.AssistantToolCallMessage("", []llm.ToolCallRef{{ID: "callX", Name: "f", Arguments: "{}"}}))
	msgs = append(msgs, llm.ToolResultMessage("callX", "tool output"))
	for i := 0; i < 7; i++ {
		msgs = append(msgs, llm.UserMessage("recent"))
	}

	// provider=nil -> llmToolSummary fails -> heuristic fallback, no network call.
	out := compressContextIfNeeded(context.Background(), msgs, nil, "s")

	seen := map[string]bool{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			seen[tc.ID] = true
		}
		if m.ToolCallID != "" && !seen[m.ToolCallID] {
			t.Fatalf("orphan tool result %q in compressed output: its assistant tool_call was summarized away (chat API would 400)", m.ToolCallID)
		}
	}
}
