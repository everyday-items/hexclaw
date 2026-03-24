package sqliteutil

import "testing"

func TestEscapeLike(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"100%", `100\%`},
		{"a_b", `a\_b`},
		{`a\b`, `a\\b`},
		{`%_\`, `\%\_\\`},
		{"", ""},
		{"普通中文", "普通中文"},
	}
	for _, tt := range tests {
		got := EscapeLike(tt.input)
		if got != tt.want {
			t.Errorf("EscapeLike(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
