package security

import (
	"fmt"
	"regexp"
	"strings"
)

// SkillScanner scans Skill content for dangerous patterns.
//
// Prevents malicious Skills from:
//   - Shell command injection (os.system, subprocess, exec)
//   - Network exfiltration (curl, wget, nc)
//   - File system traversal (../../../etc/passwd, ~/.ssh)
//   - Encoded payloads (base64 decode + exec patterns)
type SkillScanner struct {
	patterns []*dangerPattern
}

type dangerPattern struct {
	regex    *regexp.Regexp
	category string
	message  string
}

// NewSkillScanner creates a scanner with default dangerous patterns.
func NewSkillScanner() *SkillScanner {
	return &SkillScanner{
		patterns: defaultPatterns(),
	}
}

// Scan checks content for dangerous patterns. Returns nil if safe.
func (s *SkillScanner) Scan(content string) error {
	lower := strings.ToLower(content)
	var violations []string

	for _, p := range s.patterns {
		if p.regex.MatchString(lower) {
			violations = append(violations, fmt.Sprintf("[%s] %s", p.category, p.message))
		}
	}

	if len(violations) > 0 {
		return fmt.Errorf("skill security scan failed (%d violation(s)):\n  - %s",
			len(violations), strings.Join(violations, "\n  - "))
	}
	return nil
}

func defaultPatterns() []*dangerPattern {
	return []*dangerPattern{
		// Shell command injection
		{regexp.MustCompile(`os\.system\s*\(`), "shell-injection", "os.system() call detected"},
		{regexp.MustCompile(`subprocess\.\w+\s*\(`), "shell-injection", "subprocess call detected"},
		{regexp.MustCompile(`\beval\s*\(`), "code-injection", "eval() call detected"},
		{regexp.MustCompile(`\bexec\s*\(`), "code-injection", "exec() call detected"},
		{regexp.MustCompile(`child_process`), "shell-injection", "child_process module detected"},
		{regexp.MustCompile(`\bspawn\s*\(`), "shell-injection", "spawn() call detected"},

		// Network exfiltration
		{regexp.MustCompile(`\bcurl\s+`), "exfiltration", "curl command detected"},
		{regexp.MustCompile(`\bwget\s+`), "exfiltration", "wget command detected"},
		{regexp.MustCompile(`\bnc\s+-`), "exfiltration", "netcat command detected"},
		{regexp.MustCompile(`\btelnet\s+`), "exfiltration", "telnet command detected"},

		// File system traversal
		{regexp.MustCompile(`\.\./\.\./`), "path-traversal", "directory traversal pattern detected"},
		{regexp.MustCompile(`/etc/passwd`), "path-traversal", "sensitive file path /etc/passwd"},
		{regexp.MustCompile(`~/\.ssh`), "path-traversal", "SSH key directory access"},
		{regexp.MustCompile(`~/\.aws`), "path-traversal", "AWS credentials directory access"},
		{regexp.MustCompile(`~/\.env`), "path-traversal", ".env file access"},

		// Encoded payload patterns
		{regexp.MustCompile(`base64.*decode.*exec`), "encoded-payload", "base64 decode + exec pattern"},
		{regexp.MustCompile(`atob\s*\(.*\)\s*\)`), "encoded-payload", "atob decode pattern"},
	}
}
