package engine

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

func TestToolCollector_CollectFromSkills(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&testSkill{name: "search", result: "ok"})
	reg.Register(&testSkill{name: "weather", result: "ok"})

	tc := NewToolCollector(reg, nil, 40)
	tools := tc.Collect()

	if len(tools) != 2 {
		t.Errorf("got %d tools, want 2", len(tools))
	}
	// Sorted alphabetically
	if tools[0].Function.Name != "search" {
		t.Errorf("first tool should be 'search', got %q", tools[0].Function.Name)
	}
	if tools[1].Function.Name != "weather" {
		t.Errorf("second tool should be 'weather', got %q", tools[1].Function.Name)
	}
}

func TestToolCollector_MaxToolsCap(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&testSkill{name: "a"})
	reg.Register(&testSkill{name: "b"})
	reg.Register(&testSkill{name: "c"})

	tc := NewToolCollector(reg, nil, 2)
	tools := tc.Collect()

	if len(tools) != 2 {
		t.Errorf("got %d tools, want 2 (capped)", len(tools))
	}
}

func TestToolCollector_Dedup(t *testing.T) {
	reg := skill.NewRegistry()
	reg.Register(&testSkill{name: "search", result: "first"})
	reg.Register(&testSkill{name: "search", result: "second"})

	tc := NewToolCollector(reg, nil, 40)
	tools := tc.Collect()

	count := 0
	for _, td := range tools {
		if td.Function.Name == "search" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected 1 'search' tool after dedup, got %d", count)
	}
}

func TestToolCollector_NilSources(t *testing.T) {
	tc := NewToolCollector(nil, nil, 40)
	tools := tc.Collect()

	if len(tools) != 0 {
		t.Errorf("expected 0 tools with nil sources, got %d", len(tools))
	}
}
