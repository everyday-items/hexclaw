package adapter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeWireAssignsTrustedIdentityMonotonicSequenceAndSafeTerminal(t *testing.T) {
	wire := NewRuntimeWire("msg-assistant-007", ReasoningDisclosure{})

	first := wire.Decorate(&ReplyChunk{Content: "hello"})
	if first.AssistantMessageID != "msg-assistant-007" ||
		first.BackendMessageID != first.AssistantMessageID ||
		first.MessageID != first.AssistantMessageID {
		t.Fatalf("message identity aliases diverged: %+v", first)
	}
	if first.Sequence != 1 {
		t.Fatalf("first sequence = %d, want 1", first.Sequence)
	}
	if first.ReasoningDisclosure.Visibility != ReasoningNotExposed {
		t.Fatalf("unknown disclosure = %+v, want not_exposed", first.ReasoningDisclosure)
	}

	done := wire.Decorate(&ReplyChunk{Done: true})
	if done.Sequence != 2 {
		t.Fatalf("terminal sequence = %d, want 2", done.Sequence)
	}
	if done.RuntimeEvent == nil ||
		done.RuntimeEvent.Kind != RuntimeEventTerminal ||
		done.RuntimeEvent.TerminalStatus != RuntimeTerminalCompleted {
		t.Fatalf("terminal event = %+v", done.RuntimeEvent)
	}
}

func TestRuntimeEventV1JSONExactSetNeverCarriesSensitivePayloads(t *testing.T) {
	event, ok := NewToolRuntimeEvent(
		RuntimeEventToolCompleted,
		"call-1",
		"web_search",
		map[string]struct{}{"web_search": {}},
	)
	if !ok {
		t.Fatal("real allowlisted tool event was rejected")
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	wantKeys := map[string]bool{
		"version": true, "event_id": true, "kind": true,
		"tool_call_id": true, "tool_name": true,
	}
	for key := range got {
		if !wantKeys[key] {
			t.Fatalf("unexpected runtime event field %q in %s", key, raw)
		}
	}
	if len(got) != len(wantKeys) {
		t.Fatalf("runtime event fields = %v, want exact-set %v", got, wantKeys)
	}
	for _, forbidden := range []string{"arguments", "result", "error", "prompt", "state", "message"} {
		if _, exists := got[forbidden]; exists {
			t.Fatalf("sensitive field %q leaked in %s", forbidden, raw)
		}
	}

	if _, ok := NewToolRuntimeEvent(RuntimeEventToolStarted, "fake", "search", nil); ok {
		t.Fatal("unproven search event must not be invented")
	}
}

func TestNormalizeReasoningDisclosureFailsClosed(t *testing.T) {
	route := FrozenReasoningRoute{Provider: "trusted-provider", Model: "trusted-model"}
	tests := []ReasoningDisclosure{
		{},
		{Visibility: ReasoningVisible, Source: "unknown", Dialect: "reasoning-v1", Provider: route.Provider, Model: route.Model},
		{Visibility: ReasoningVisible, Source: "provider_adapter", Dialect: "", Provider: route.Provider, Model: route.Model},
		{Visibility: ReasoningVisible, Source: "provider_adapter", Dialect: "reasoning-v1", Provider: "spoofed", Model: route.Model},
	}
	for _, input := range tests {
		got := NormalizeReasoningDisclosure(input, route, map[string]struct{}{
			"provider_adapter/reasoning-v1": {},
		})
		if got.Visibility != ReasoningNotExposed {
			t.Fatalf("input %+v normalized to %+v, want fail-closed", input, got)
		}
	}

	valid := ReasoningDisclosure{
		Visibility: ReasoningVisible,
		Source:     "provider_adapter",
		Dialect:    "reasoning-v1",
		Provider:   route.Provider,
		Model:      route.Model,
	}
	if got := NormalizeReasoningDisclosure(valid, route, map[string]struct{}{"provider_adapter/reasoning-v1": {}}); !reflect.DeepEqual(got, valid) {
		t.Fatalf("valid disclosure = %+v, want %+v", got, valid)
	}
}

func TestRuntimeWireDropsNotExposedReasoningFromReplyChunkAndSSEJSON(t *testing.T) {
	wire := NewRuntimeWire("msg-private", ReasoningDisclosure{Visibility: ReasoningNotExposed})
	chunk := wire.Decorate(&ReplyChunk{
		Reasoning: "private chain of thought",
		ReasoningDisclosure: ReasoningDisclosure{
			Visibility: ReasoningNotExposed,
		},
	})
	if chunk.Reasoning != "" {
		t.Fatalf("not_exposed ReplyChunk leaked reasoning: %q", chunk.Reasoning)
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" || containsJSONText(raw, "private chain of thought") {
		t.Fatalf("SSE JSON leaked not_exposed reasoning: %s", raw)
	}

	public := NewRuntimeWire("msg-public", ReasoningDisclosure{
		Visibility: ReasoningNotExposed,
		Provider:   "ollama",
		Model:      "trusted-model",
	})
	visible := public.Decorate(&ReplyChunk{
		Reasoning: "public provider summary",
		ReasoningDisclosure: ReasoningDisclosure{
			Visibility: ReasoningVisible,
			Source:     "ollama",
			Dialect:    "message.thinking",
			Provider:   "ollama",
			Model:      "trusted-model",
		},
	})
	if visible.Reasoning != "public provider summary" {
		t.Fatalf("trusted visible reasoning was removed: %+v", visible)
	}
}

func containsJSONText(raw []byte, text string) bool {
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return false
	}
	return strings.Contains(string(raw), text)
}
