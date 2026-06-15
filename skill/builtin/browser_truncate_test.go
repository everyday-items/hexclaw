package builtin

import (
	"strings"
	"testing"
)

// Regression coverage for the 2026-06-16 fetch text bound: large/noisy page
// text is capped (rune-safe) before reaching the LLM.
func TestTruncateRunes(t *testing.T) {
	// Under the limit passes through unchanged.
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Errorf("short string must pass through, got %q", got)
	}
	// At the limit passes through.
	if got := truncateRunes("abcde", 5); got != "abcde" {
		t.Errorf("at-limit string must pass through, got %q", got)
	}
	// Over the limit is cut and marked.
	got := truncateRunes("abcdefghij", 4)
	if !strings.HasPrefix(got, "abcd") {
		t.Errorf("expected prefix abcd, got %q", got)
	}
	if !strings.Contains(got, "截断") {
		t.Errorf("truncation must carry a marker, got %q", got)
	}
	// Rune-safe on CJK: cutting must not split a multi-byte character.
	cjk := strings.Repeat("热", 100)
	cut := truncateRunes(cjk, 10)
	if !strings.HasPrefix(cut, strings.Repeat("热", 10)) {
		t.Errorf("CJK truncation must keep 10 whole runes, got prefix %q", cut[:30])
	}
	if !strings.ContainsRune(cut, '热') || strings.Contains(cut, "�") {
		t.Errorf("CJK truncation produced an invalid rune: %q", cut)
	}
}
