package render

import (
	"strings"
	"testing"
)

func TestSanitizeFilename_BasicCases(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		ext      string
		wantUTF8 string // 完整 UTF-8 名（不带扩展校验，看 contains）
	}{
		{"plain", "notes", "md", "notes.md"},
		{"already has ext", "notes.md", "md", "notes.md"},
		{"different ext from title", "notes.txt", "md", "notes.md"},
		{"chinese", "维语翻译", "md", "维语翻译.md"},
		{"chinese with ext", "维语翻译.md", "md", "维语翻译.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, utf8 := SanitizeFilename(tc.title, tc.ext)
			if utf8 != tc.wantUTF8 {
				t.Errorf("got %q, want %q", utf8, tc.wantUTF8)
			}
		})
	}
}

func TestSanitizeFilename_HeaderInjection(t *testing.T) {
	// CR/LF 必须被过滤，否则可注入 HTTP header
	_, utf8 := SanitizeFilename("notes\r\nX-Header: injected", "md")
	if strings.Contains(utf8, "\r") || strings.Contains(utf8, "\n") {
		t.Errorf("CRLF leaked into filename: %q", utf8)
	}
	if strings.Contains(utf8, "X-Header") {
		// 内容字符可保留，CRLF 必须没
		t.Logf("note: header text characters preserved (without CRLF), this is OK: %q", utf8)
	}
}

func TestSanitizeFilename_PathTraversal(t *testing.T) {
	// 路径分隔符必须被剥离
	cases := []string{"../etc/passwd", "..\\windows\\system32", "/abs/path/foo", `C:\Windows\foo`}
	for _, in := range cases {
		_, utf8 := SanitizeFilename(in, "md")
		if strings.ContainsAny(utf8, `/\`) {
			t.Errorf("path separator leaked from %q → %q", in, utf8)
		}
	}
}

func TestSanitizeFilename_WindowsReserved(t *testing.T) {
	_, utf8 := SanitizeFilename(`a<b>c:d"e|f?g*h`, "md")
	for _, c := range `<>:"|?*` {
		if strings.ContainsRune(utf8, c) {
			t.Errorf("Windows reserved char %q leaked: %q", c, utf8)
		}
	}
}

func TestSanitizeFilename_LengthLimit(t *testing.T) {
	long := strings.Repeat("a", 500)
	_, utf8 := SanitizeFilename(long, "md")
	// 200 + ".md" = 203 max
	if len([]rune(utf8)) > 210 {
		t.Errorf("filename too long after sanitize: %d runes", len([]rune(utf8)))
	}
}

func TestSanitizeFilename_EmptyFallback(t *testing.T) {
	cases := []string{"", "   ", "\x00\x01\x02", "....", `<>:"|?*\\`}
	for _, in := range cases {
		ascii, utf8 := SanitizeFilename(in, "md")
		if !strings.HasPrefix(utf8, "artifact-") {
			t.Errorf("empty input %q didn't fallback to artifact-{ts}, got %q / %q", in, ascii, utf8)
		}
		if !strings.HasSuffix(utf8, ".md") {
			t.Errorf("fallback missing extension: %q", utf8)
		}
	}
}

func TestContentDispositionHeader_RFC5987(t *testing.T) {
	// 中文必须 percent-encoded 在 filename*= 部分
	got := ContentDispositionHeader("维语翻译", "md")
	if !strings.Contains(got, `filename="`) {
		t.Errorf("missing ASCII filename=: %s", got)
	}
	if !strings.Contains(got, `filename*=UTF-8''`) {
		t.Errorf("missing RFC 5987 filename*=: %s", got)
	}
	// %E7%BB%B4 是"维"的 UTF-8 percent encoding
	if !strings.Contains(got, "%E7%BB%B4") {
		t.Errorf("Chinese char not percent-encoded: %s", got)
	}
}

func TestContentDispositionHeader_HeaderInjection(t *testing.T) {
	// CR/LF 必须不在最终 header 里
	got := ContentDispositionHeader("a\r\nX-Header: x", "md")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("CRLF leaked into Content-Disposition header: %q", got)
	}
}
