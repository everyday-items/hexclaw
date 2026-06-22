package marketplace

import "testing"

// TestParseFrontmatterIcon 锁定 icon frontmatter 字段解析（桌面端图标契约 2026-06-22）。
func TestParseFrontmatterIcon(t *testing.T) {
	t.Run("emoji icon", func(t *testing.T) {
		meta, _ := parseFrontmatter("---\nname: stocks\nicon: 📊\n---\n# x")
		if meta.Icon != "📊" {
			t.Errorf("icon: got %q, want 📊", meta.Icon)
		}
	})
	t.Run("lucide name icon", func(t *testing.T) {
		meta, _ := parseFrontmatter("---\nname: dev\nicon: Code\n---\n# x")
		if meta.Icon != "Code" {
			t.Errorf("icon: got %q, want Code", meta.Icon)
		}
	})
	t.Run("缺省 icon 为空（前端派生）", func(t *testing.T) {
		meta, _ := parseFrontmatter("---\nname: plain\n---\n# x")
		if meta.Icon != "" {
			t.Errorf("icon: got %q, want empty", meta.Icon)
		}
	})
}
