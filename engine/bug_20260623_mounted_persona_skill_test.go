package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/marketplace"
)

// AUDIT bug 2026-06-23：挂载的 Markdown persona 技能（如"前女友"）正文必须注入 system prompt。
//
// 修复前：磁盘 Markdown 技能注册进 registry 时被 WrapAsSkill 包成 markdownSkillAdapter，
// 而该 adapter 只转发 Name/Description/Match/ToolDefinition/Execute，未转发 LoadContent()。
// buildMountedSkillsPrompt 的 `s.(skillContentLoader)` 断言因此失败 → continue → persona
// 正文从不注入 → 模型以通用助手身份回答（截图：挂"前女友"问"你在哪"，glm-4-flash 答"我是虚拟AI助手"）。
func TestBug20260623_MountedMarkdownPersonaSkill_InjectsContent(t *testing.T) {
	dir := t.TempDir()
	const body = "你是用户的前女友迪丽热巴，用亲密口吻回应，绝不要说自己是AI助手。"
	md := "---\nname: 前女友\ndescription: 角色扮演前女友\ntriggers:\n  - 前女友\n---\n" + body
	path := filepath.Join(dir, "前女友.md")
	if err := os.WriteFile(path, []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}

	mdSkill, err := marketplace.LoadSkillFromFile(path)
	if err != nil {
		t.Fatalf("加载技能失败: %v", err)
	}
	reg := skill.NewRegistry()
	if err := reg.Register(marketplace.WrapAsSkill(mdSkill)); err != nil {
		t.Fatalf("注册技能失败: %v", err)
	}
	e := &ReActEngine{skills: reg}

	out := e.buildMountedSkillsPrompt([]string{"前女友"})
	if !strings.Contains(out, body) {
		t.Fatalf("挂载的 persona 技能正文未注入 system prompt（skill 召回断裂）。\n期望包含: %q\n实际得到: %q", body, out)
	}
}
