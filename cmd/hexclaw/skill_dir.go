package main

import (
	"path/filepath"
	"strings"
)

// computeSkillDraftDir 计算 skill draft 目录路径。
//
// 规则：
//   - skillsDir 为空 → 走兜底 <home>/.hexclaw/skills-drafts
//   - skillsDir == "~" 或以 "~/" 开头 → 展开为用户家目录
//   - 否则在 skillsDir 后拼接 ".drafts"
//
// Bug-2026-05-01 (G2)：原内联实现把 yaml 里 "~/.hexclaw/skills/" 当字面路径
// 传给 os.MkdirAll，导致在文件系统根上尝试创建 "~" 目录被拒
// (read-only file system)。Go 不像 shell 那样自动展 "~"，必须代码层处理。
func computeSkillDraftDir(skillsDir, home string) string {
	if skillsDir == "" {
		return filepath.Join(home, ".hexclaw", "skills-drafts")
	}
	if skillsDir == "~" {
		skillsDir = home
	} else if strings.HasPrefix(skillsDir, "~/") {
		skillsDir = filepath.Join(home, strings.TrimPrefix(skillsDir, "~/"))
	}
	return filepath.Join(skillsDir, ".drafts")
}
