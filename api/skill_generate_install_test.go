package api

import "testing"

// 锁定本次 review 修复：content 安装的 name 强校验 + sanitizeGeneratedSkill 去围栏边界。

func TestFrontmatterSkillName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"---\nname: my-skill\n---\n# x", "my-skill"},
		{"---\nname: \"quoted_name\"\n---\n", "quoted_name"},
		{"---\ndescription: d\n---\n", ""},          // 无 name
		{"---\nname: My Skill\n---\n", ""},          // 含空格，非标量标识 → 不匹配
		{"# body\nname: not-frontmatter", ""},       // name 在正文不算（无 frontmatter 段）
	}
	for _, c := range cases {
		if got := frontmatterSkillName(c.in); got != c.want {
			t.Errorf("frontmatterSkillName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidSkillIdentifier(t *testing.T) {
	ok := []string{"my-skill", "a", "weather_report", "abc123"}
	bad := []string{"", "My-Skill", "has space", "../etc", "名字", "a.b"}
	for _, n := range ok {
		if !validSkillIdentifier(n) {
			t.Errorf("validSkillIdentifier(%q) = false, want true", n)
		}
	}
	for _, n := range bad {
		if validSkillIdentifier(n) {
			t.Errorf("validSkillIdentifier(%q) = true, want false", n)
		}
	}
}

func TestSanitizeGeneratedSkill(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"fenced markdown", "```markdown\n---\nname: x\n---\n# y\n```", "---\nname: x\n---\n# y"},
		{"fenced plain", "```\n---\nname: x\n---\n```", "---\nname: x\n---"},
		{"no fence", "---\nname: x\n---\n# y", "---\nname: x\n---\n# y"},
		{"empty", "", ""},
		{"only opening fence no newline", "```", ""},
	}
	for _, c := range cases {
		if got := sanitizeGeneratedSkill(c.in); got != c.want {
			t.Errorf("%s: sanitizeGeneratedSkill(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
