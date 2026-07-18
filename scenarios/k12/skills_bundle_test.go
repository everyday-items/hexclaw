package k12

import (
	"io/fs"
	"strings"
	"testing"
)

// TestBundledSkillsFS_ContainsMountedAndDelegated 出厂 seed 内容回归锁：
// manifest 挂载的 skill + 学科 tutor 委派的通用 skill + hub v0.0.7 新增的 4 个小学 skill
// （science-tutor / information-technology-tutor / writing-feedback / art-feedback）
// 必须都被 go:embed 打进二进制，否则首启 seed 后 runtime 挂载/工具调用命中空
// ——正是此前修复的 seed 链路根因，钉死防回归。
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
		"chinese-tutor.md", "english-tutor.md",
		"concept-explainer.md", "exercise-generator.md",
		// 学科 tutor 正文委派（工具调用，需在注册表）
		"reading-comprehension.md", "classical-chinese.md", "english-vocab-coach.md", "quiz-generator.md",
		// hub v0.0.7 新增小学学科 tutor（homework-checker 学科路由目标）
		"science-tutor.md", "information-technology-tutor.md",
		// hub v0.0.7 新增作品反馈（作品点评提示词基座，engineadapter/work_feedback.go 消费）
		"writing-feedback.md", "art-feedback.md",
	}
	for _, m := range must {
		if !got[m] {
			t.Errorf("出厂 seed 缺关键 skill: %s（跑 scripts/sync-hub-embed.sh 重新 seed）", m)
		}
	}
	if len(entries) != len(must) {
		t.Errorf("seed 数量 %d ≠ 期望 %d（新增/裁剪 seed 需同步本清单与 sync-hub-embed.sh 的 EXCLUDE/KEEP）", len(entries), len(must))
	}
}

// TestBundledSkillsFS_FrozenExcludesPhysicsChemistry v0.5.0 冻结守卫
// （架构设计-v0.5.0《明确不做》#3：不做物理、化学和音乐；K12-INV-014：当前 Manifest
// 不暴露初中、高中、物理、化学或音乐）：出厂 seed 内嵌的文件名不得含 physics/chemistry，
// 防止历史 skill 经 hub sync 回流进二进制。
func TestBundledSkillsFS_FrozenExcludesPhysicsChemistry(t *testing.T) {
	entries, err := fs.ReadDir(BundledSkillsFS(), "skills")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		for _, banned := range []string{"physics", "chemistry"} {
			if strings.Contains(name, banned) {
				t.Errorf("冻结#3/INV-014：出厂 seed 不得含 %s skill, got %q", banned, e.Name())
			}
		}
	}
}
