package builtin

import (
	"context"
	"strings"
	"testing"
)

func TestCodeSkill_Match(t *testing.T) {
	s := NewCodeSkill()

	tests := []struct {
		input string
		want  bool
	}{
		{"/run go\nfmt.Println(\"hi\")", true},
		{"/code python\nprint('hi')", true},
		{"/Run Go\ncode", true},
		{"run something", false},
		{"hello", false},
	}

	for _, tt := range tests {
		if got := s.Match(tt.input); got != tt.want {
			t.Errorf("Match(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestCodeSkill_ParseCodeInput(t *testing.T) {
	tests := []struct {
		input    string
		wantLang string
		wantCode string
	}{
		{
			"/run go\nfmt.Println(\"hello\")",
			"go",
			"fmt.Println(\"hello\")",
		},
		{
			"/code python\n```python\nprint('hello')\n```",
			"python",
			"print('hello')",
		},
		{
			"/run js\nconsole.log('hi')",
			"js",
			"console.log('hi')",
		},
		{
			"/run go",
			"go",
			"",
		},
	}

	for _, tt := range tests {
		lang, code := parseCodeInput(tt.input)
		if lang != tt.wantLang {
			t.Errorf("parseCodeInput(%q) lang = %q, want %q", tt.input, lang, tt.wantLang)
		}
		if code != tt.wantCode {
			t.Errorf("parseCodeInput(%q) code = %q, want %q", tt.input, code, tt.wantCode)
		}
	}
}

func TestCodeSkill_Execute_NoCode(t *testing.T) {
	s := NewCodeSkill()
	result, err := s.Execute(context.Background(), map[string]any{"query": "/run go"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "未检测到代码") {
		t.Errorf("expected error hint, got: %s", result.Content)
	}
}

func TestCodeSkill_Execute_UnsupportedLang(t *testing.T) {
	s := NewCodeSkill()
	result, err := s.Execute(context.Background(), map[string]any{"query": "/run rust\nfn main() {}"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result.Content, "不支持的语言") {
		t.Errorf("expected unsupported lang hint, got: %s", result.Content)
	}
}

func TestTruncateOutput(t *testing.T) {
	short := "hello"
	if truncateOutput(short, 100) != short {
		t.Error("short string should not be truncated")
	}

	long := strings.Repeat("a", 200)
	result := truncateOutput(long, 100)
	if len(result) > 130 { // 100 + suffix
		t.Errorf("truncated output too long: %d", len(result))
	}
	if !strings.Contains(result, "截断") {
		t.Error("should contain truncation hint")
	}
}

func TestTruncateOutput_ZeroMaxLen(t *testing.T) {
	if got := truncateOutput("hello", 0); got != "" {
		t.Fatalf("truncateOutput(_, 0) = %q, want empty", got)
	}
}

func TestTruncateOutput_NegativeMaxLen(t *testing.T) {
	if got := truncateOutput("hello", -1); got != "" {
		t.Fatalf("truncateOutput(_, -1) = %q, want empty", got)
	}
}
