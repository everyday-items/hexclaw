package engine

import (
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestBuildLLMCacheInputPartitionsThinkingEffort(t *testing.T) {
	low := &adapter.Message{
		Content: "same prompt",
		Metadata: map[string]string{
			"thinking":        "on",
			"thinking_effort": "low",
		},
	}
	maximum := &adapter.Message{
		Content: "same prompt",
		Metadata: map[string]string{
			"thinking":        "on",
			"thinking_effort": "max",
		},
	}
	if buildLLMCacheInput(low) == buildLLMCacheInput(maximum) {
		t.Fatal("different thinking_effort values must not share an LLM cache key")
	}
}

func TestCopyProviderMetadataPreservesThinkingEffort(t *testing.T) {
	request := hexagon.CompletionRequest{}
	copyProviderMetadata(&request, map[string]string{
		"thinking":        "on",
		"thinking_effort": "xhigh",
	})
	if got := request.Metadata["thinking_effort"]; got != "xhigh" {
		t.Fatalf("provider metadata thinking_effort=%#v, want xhigh", got)
	}
}
