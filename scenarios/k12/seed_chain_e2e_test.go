package k12_test

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
)

// TestSeedChain_EndToEnd 端到端串联验证 seed 链路（hex-test 信条 5 全链路运行时取证）：
//
//	go:embed(BundledSkillsFS) → SeedFromFS 首启写盘 → Init 扫描 → List 注册可见 → IsEnabled 默认启用
//
// 单测各证一环（SeedFromFS 幂等 / embed 内容 / 扫描解析）；本测证明串起来真能跑通——
// 这正是本轮修复的根因链（此前 seed 链路缺失 → 注册表空 → 挂载空指针）。破即红。
func TestSeedChain_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	mp := marketplace.NewMarketplace(dir)

	// 1) 首启 seed（模拟全新安装：~/.hexclaw/skills/ 空）
	n, err := mp.SeedFromFS(k12.BundledSkillsFS(), "skills")
	if err != nil {
		t.Fatalf("首启 seed 失败: %v", err)
	}
	// 冻结#3/INV-014：physics/chemistry 已撤出出厂 seed → 8 挂载 + 4 委派 = 12。
	if n < 12 {
		t.Fatalf("seed 数量应 ≥12（8 挂载 + 4 委派）, got %d", n)
	}

	// 2) Init 扫描刚 seed 的目录 → 注册
	if err := mp.Init(); err != nil {
		t.Fatalf("Init 扫描失败: %v", err)
	}
	list := mp.List()
	got := make(map[string]bool, len(list))
	for _, s := range list {
		got[s.Meta.Name] = true
	}

	// 3) 注册表必须含挂载源 + 委派源，且默认 enabled（否则建档挂载/工具调用命中空）
	for _, name := range []string{
		"grade-constraint", "k12-pedagogy", "math-tutor", "homework-checker",
		"chinese-tutor", "english-tutor", "concept-explainer", "exercise-generator",
		"reading-comprehension", "quiz-generator",
	} {
		if !got[name] {
			t.Errorf("seed 后注册表应含 %q（runtime 挂载/委派命中源）", name)
		}
		if !mp.IsEnabled(name) {
			t.Errorf("%q 应默认 enabled（否则不被召回/挂载）", name)
		}
	}
	if len(list) < 12 {
		t.Errorf("注册 skill 数 %d < 12", len(list))
	}

	// 4) 幂等：二次 seed 不重复写（全已存在）
	n2, err := mp.SeedFromFS(k12.BundledSkillsFS(), "skills")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("二次 seed 应 0（幂等）, got %d", n2)
	}
}
