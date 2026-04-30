package main

import (
	"strings"
	"testing"
)

// Bug-2026-05-01 G2 闭环：cfg.Skills.Dir 含字面 "~/" 时，computeSkillDraftDir
// 必须展开为用户家目录，否则 os.MkdirAll 会在文件系统根上尝试创建 "~" 目录被拒
// （macOS APFS root 只读 → "mkdir ~: read-only file system"）。
//
// 复现路径：~/.hexclaw/hexclaw.yaml 默认 `skills.dir: "~/.hexclaw/skills/"`，
// Go 不展 shell `~`，需在代码层显式处理。
func TestBUG20260501_ComputeSkillDraftDir_ExpandsTilde(t *testing.T) {
	const home = "/Users/hexagon"

	cases := []struct {
		name     string
		skills   string
		want     string
		mustNotContain string
	}{
		{
			name:           "tilde slash prefix is expanded",
			skills:         "~/.hexclaw/skills/",
			want:           "/Users/hexagon/.hexclaw/skills/.drafts",
			mustNotContain: "~",
		},
		{
			name:           "bare tilde is expanded to home",
			skills:         "~",
			want:           "/Users/hexagon/.drafts",
			mustNotContain: "~",
		},
		{
			name:           "absolute path is left as-is",
			skills:         "/opt/hexclaw/skills",
			want:           "/opt/hexclaw/skills/.drafts",
			mustNotContain: "~",
		},
		{
			name:           "empty skills dir falls back to ~/.hexclaw/skills-drafts",
			skills:         "",
			want:           "/Users/hexagon/.hexclaw/skills-drafts",
			mustNotContain: "~",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeSkillDraftDir(tc.skills, home)
			if got != tc.want {
				t.Errorf("computeSkillDraftDir(%q, %q) = %q, want %q", tc.skills, home, got, tc.want)
			}
			if strings.Contains(got, tc.mustNotContain) {
				t.Errorf("result %q contains forbidden substring %q (bug not fixed: ~ would be passed to MkdirAll as literal)", got, tc.mustNotContain)
			}
		})
	}
}
