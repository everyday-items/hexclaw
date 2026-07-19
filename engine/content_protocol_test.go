package engine

import (
	"testing"

	"github.com/hexagon-codes/ai-core/template"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

func TestRuntimeToolResultBlockCarriesCanonicalContent(t *testing.T) {
	blocks := runtimeBlocksToAdapter(template.Blocks{
		template.TextBlock("先分析 **题意**。"),
		template.ToolUseBlock("call-1", "weather", `{}`),
		template.ToolResultBlock("call-1", "温度 **25°C**", false, "success"),
	})
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v", blocks)
	}
	if content := blocks[0].MessageContent; content == nil || content.ProducerKind != messagecontent.ProducerChat || content.Markdown != blocks[0].Text {
		t.Fatalf("text block omitted canonical chat content: %#v", blocks[0])
	}
	content := blocks[2].MessageContent
	if content == nil {
		t.Fatal("tool result block omitted MessageContent")
	}
	if content.ProducerKind != messagecontent.ProducerTool || content.Markdown != blocks[2].Output {
		t.Fatalf("tool content = %#v block=%#v", content, blocks[2])
	}
	if err := content.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestCanonicalProducerContentRejectsEmptySuccess(t *testing.T) {
	if content := canonicalProducerContent(messagecontent.ProducerSkill, "  ", "zh-CN"); content != nil {
		t.Fatalf("empty content must stay fail-visible, got %#v", content)
	}
}
