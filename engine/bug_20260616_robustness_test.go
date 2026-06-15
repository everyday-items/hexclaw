package engine

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/skill"
)

// Regression coverage for the 2026-06-16 robustness fixes:
//   B — empty-after-tool recovery gating (messagesHaveToolResult)
//   C — non-ASCII skill names exposed/routed via a derived slug

// B: the empty-response recovery only fires when a tool result is present, so
// plain empty responses (e.g. system dispatch) aren't double-called.
func TestMessagesHaveToolResult(t *testing.T) {
	cases := []struct {
		name string
		msgs []hexagon.Message
		want bool
	}{
		{"empty", nil, false},
		{"user only", []hexagon.Message{{Role: hexagon.RoleUser, Content: "hi"}}, false},
		{"assistant only", []hexagon.Message{{Role: hexagon.RoleAssistant, Content: "ok"}}, false},
		{"has tool result", []hexagon.Message{
			{Role: hexagon.RoleUser, Content: "browse"},
			{Role: hexagon.RoleAssistant, Content: ""},
			{Role: hexagon.RoleTool, Content: "page text"},
		}, true},
	}
	for _, tc := range cases {
		if got := messagesHaveToolResult(tc.msgs); got != tc.want {
			t.Errorf("%s: messagesHaveToolResult = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// C: a Chinese-named skill is exposed under a valid slug instead of being
// dropped, and the executor routes a slug tool-call back to the real skill.
func TestToolExecutor_ResolvesNonASCIISkillBySlug(t *testing.T) {
	reg := skill.NewRegistry()
	_ = reg.Register(&testSkill{name: "前女友", result: "girlfriend-skill-ran"})
	_ = reg.Register(&testSkill{name: "weather", result: "sunny"})
	exec := NewToolExecutor(reg, nil)
	ctx := context.Background()

	// Direct (already-valid) name still routes.
	if got, err := exec.Execute(ctx, "weather", nil); err != nil || got != "sunny" {
		t.Fatalf("direct-name route = (%q, %v), want (sunny, nil)", got, err)
	}

	// The derived slug routes back to the Chinese-named skill.
	slug := llmToolNameSlug("前女友")
	if slug == "前女友" {
		t.Fatalf("expected a derived slug for a non-ASCII name, got %q", slug)
	}
	got, err := exec.Execute(ctx, slug, nil)
	if err != nil || got != "girlfriend-skill-ran" {
		t.Fatalf("slug route %q = (%q, %v), want (girlfriend-skill-ran, nil)", slug, got, err)
	}

	// HasTool resolves slugs too.
	if !exec.HasTool(slug) {
		t.Errorf("HasTool(%q) should resolve the slug", slug)
	}

	// An unknown name is still rejected.
	if _, err := exec.Execute(ctx, "no-such-tool", nil); err == nil {
		t.Error("unknown tool name must error")
	}
}

// C: the collector exposes the non-ASCII skill under a valid LLM tool name
// (the upstream identifier regex) instead of skipping it.
func TestToolCollector_ExposesNonASCIISkillUnderValidName(t *testing.T) {
	reg := skill.NewRegistry()
	_ = reg.Register(&testSkill{name: "前女友", result: "ok"})
	tools := NewToolCollector(reg, nil, 50).Collect()

	if len(tools) == 0 {
		t.Fatal("non-ASCII skill must be exposed (slugged), not dropped")
	}
	for _, td := range tools {
		if !isValidLLMToolName(td.Function.Name) {
			t.Errorf("exposed tool name %q is not a valid LLM identifier", td.Function.Name)
		}
	}
}
