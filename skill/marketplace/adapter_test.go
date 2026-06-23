package marketplace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AUDIT bug 2026-06-23 契约回归：WrapAsSkill 的产物必须暴露 LoadContent()，
// 否则引擎 buildMountedSkillsPrompt 通过 `interface{ LoadContent() (string, error) }`
// 断言时拿不到正文，挂载的 Markdown persona 技能正文永远注入不进 system prompt。
//
// 这是"举一反三"的检测：任何未来给 Skill 加的 wrapper，只要 registry 里存的是它、
// 而引擎需要从它读正文，就必须保持转发 LoadContent —— 本断言守住这条窄接口契约。
func TestWrapAsSkill_ExposesLoadContentContract(t *testing.T) {
	dir := t.TempDir()
	const body = "你是用户的前女友，亲密口吻回应，绝不说自己是AI助手。"
	md := "---\nname: persona-contract\ndescription: d\ntriggers:\n  - x\n---\n" + body
	path := filepath.Join(dir, "persona.md")
	if err := os.WriteFile(path, []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
	mdSkill, err := LoadSkillFromFile(path)
	if err != nil {
		t.Fatalf("加载技能失败: %v", err)
	}

	wrapped := WrapAsSkill(mdSkill)
	loader, ok := wrapped.(interface{ LoadContent() (string, error) })
	if !ok {
		t.Fatal("WrapAsSkill 产物未暴露 LoadContent()：引擎将无法注入 persona 正文（skill 召回断裂）")
	}
	got, err := loader.LoadContent()
	if err != nil {
		t.Fatalf("LoadContent 失败: %v", err)
	}
	if !strings.Contains(got, body) {
		t.Fatalf("LoadContent 未返回技能正文。\n期望包含: %q\n实际: %q", body, got)
	}
}
