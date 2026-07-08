package k12

import (
	"io/fs"
	"testing"
)

// TestBundledSkillsFS_ContainsMountedAndDelegated 出厂 seed 内容回归锁：
// manifest 挂载的 10 个 skill + 学科 tutor 委派的 4 个通用 skill 必须都被 go:embed 打进二进制，
// 否则首启 seed 后 runtime 挂载/工具调用命中空——正是本轮修复的 seed 链路根因，钉死防回归。
func TestBundledSkillsFS_ContainsMountedAndDelegated(t *testing.T) {
	entries, err := fs.ReadDir(BundledSkillsFS(), "skills")
	if err != nil {
		t.Fatalf("读取内嵌 seed 目录失败（scenarios/k12/skills/ 是否已 sync？）: %v", err)
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name()] = true
	}

	must := []string{
		// manifest 挂载（正文注入 system prompt）
		"grade-constraint.md", "k12-pedagogy.md", "math-tutor.md", "homework-checker.md",
		"chinese-tutor.md", "english-tutor.md", "physics-tutor.md", "chemistry-tutor.md",
		"concept-explainer.md", "exercise-generator.md",
		// 学科 tutor 正文委派（工具调用，需在注册表）
		"reading-comprehension.md", "classical-chinese.md", "english-vocab-coach.md", "quiz-generator.md",
	}
	for _, m := range must {
		if !got[m] {
			t.Errorf("出厂 seed 缺关键 skill: %s（跑 scripts/sync-hub-embed.sh 重新 seed）", m)
		}
	}
	if len(entries) < len(must) {
		t.Errorf("seed 数量 %d 少于必需 %d", len(entries), len(must))
	}
}
