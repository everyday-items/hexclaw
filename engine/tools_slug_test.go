package engine

import "testing"

func TestLLMToolNameSlug(t *testing.T) {
	// Valid names pass through unchanged.
	for _, ok := range []string{"code-review-pro", "browser", "a_b-1"} {
		if got := llmToolNameSlug(ok); got != ok {
			t.Errorf("valid name %q must pass through, got %q", ok, got)
		}
	}

	// Invalid names map to a valid, deterministic slug.
	for _, bad := range []string{"前女友", "前leader", "my skill.v2", "工具/能力"} {
		slug := llmToolNameSlug(bad)
		if !isValidLLMToolName(slug) {
			t.Errorf("slug for %q = %q is not a valid LLM tool name", bad, slug)
		}
		if slug == bad {
			t.Errorf("invalid name %q must be rewritten, got identical", bad)
		}
		if llmToolNameSlug(bad) != slug {
			t.Errorf("slug for %q must be deterministic", bad)
		}
	}

	// Partial-ASCII keeps the ASCII stem; fully non-ASCII falls back to a hash.
	if got := llmToolNameSlug("前leader"); got[:7] != "leader-" {
		t.Errorf("partial-ascii slug should keep stem, got %q", got)
	}
	if got := llmToolNameSlug("前女友"); got[:6] != "skill-" {
		t.Errorf("fully non-ascii slug should fall back to skill-<hash>, got %q", got)
	}

	// Distinct invalid names must not collide.
	if llmToolNameSlug("前女友") == llmToolNameSlug("前男友") {
		t.Error("distinct names must produce distinct slugs")
	}
}
