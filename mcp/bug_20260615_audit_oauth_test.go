package mcp

// BUG-20260615 (audit 🔴 CSRF): the OAuth callback's state check was
// `got != expected`, and the expected state was generated with the error
// dropped (`state, _ := generateRandomString(16)`). On a crypto/rand failure
// the expected state is "", and `"" == ""` lets a callback carrying NO state
// param pass the CSRF guard. The check must reject an empty expected state.

import "testing"

func TestBug20260615_OAuthStateValid(t *testing.T) {
	if oauthStateValid("", "") {
		t.Error("empty expected state must NEVER validate — a rand failure must not open a CSRF hole")
	}
	if oauthStateValid("expected", "") {
		t.Error("a callback with no state must not validate against a real expected state")
	}
	if !oauthStateValid("abc123XYZ", "abc123XYZ") {
		t.Error("a matching non-empty state must validate")
	}
	if oauthStateValid("abc123XYZ", "tampered") {
		t.Error("a mismatched state must not validate")
	}
}

func TestBug20260615_TruncateForErr(t *testing.T) {
	if got := truncateForErr("short", 256); got != "short" {
		t.Errorf("under-cap string must pass through, got %q", got)
	}
	long := make([]rune, 500)
	for i := range long {
		long[i] = 'x'
	}
	got := truncateForErr(string(long), 256)
	if len([]rune(got)) > 256+len([]rune("…(truncated)")) {
		t.Errorf("over-cap string must be truncated, got len %d", len([]rune(got)))
	}
}
