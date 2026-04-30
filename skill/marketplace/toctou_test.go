package marketplace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hexagon-codes/hexclaw/skill"
)

// helper：写一个 SKILL.md 并返回 MarkdownSkill。
func newTestSkillFile(t *testing.T, content string) *MarkdownSkill {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return &MarkdownSkill{
		Meta:     SkillMeta{Name: "test"},
		FilePath: path,
	}
}

func TestMarkdownSkill_LoadContent_CachesHash(t *testing.T) {
	s := newTestSkillFile(t, "---\nname: test\n---\nbody-v1")
	c1, err := s.LoadContent()
	if err != nil {
		t.Fatal(err)
	}
	if c1 != "body-v1" {
		t.Errorf("got %q", c1)
	}
	if !s.loaded {
		t.Error("loaded should be true")
	}

	// 再次调用使用 cached content
	c2, _ := s.LoadContent()
	if c2 != c1 {
		t.Error("second load should return cached content")
	}
}

func TestMarkdownSkill_VerifyContent_AllowsUnchanged(t *testing.T) {
	s := newTestSkillFile(t, "---\nname: test\n---\nbody")
	if _, err := s.LoadContent(); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyContent(); err != nil {
		t.Errorf("未篡改文件应通过 verify；got %v", err)
	}
}

func TestMarkdownSkill_VerifyContent_DetectsTampering(t *testing.T) {
	s := newTestSkillFile(t, "---\nname: test\n---\noriginal")
	if _, err := s.LoadContent(); err != nil {
		t.Fatal(err)
	}
	// 模拟 LLM 加载 prompt 之前文件被替换
	if err := os.WriteFile(s.FilePath, []byte("---\nname: test\n---\nMALICIOUS"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyContent(); err == nil {
		t.Error("被篡改的文件应被 VerifyContent 拒绝")
	}
}

func TestMarkdownSkill_VerifyContent_BeforeLoadIsNoop(t *testing.T) {
	s := newTestSkillFile(t, "---\nname: test\n---\nbody")
	// 没调过 LoadContent —— Verify 应直接 nil
	if err := s.VerifyContent(); err != nil {
		t.Errorf("首次加载前 Verify 应返回 nil；got %v", err)
	}
}

func TestMarkdownSkill_VerifyContent_FileDeletedReturnsError(t *testing.T) {
	s := newTestSkillFile(t, "---\nname: test\n---\nbody")
	if _, err := s.LoadContent(); err != nil {
		t.Fatal(err)
	}
	os.Remove(s.FilePath)
	if err := s.VerifyContent(); err == nil {
		t.Error("文件被删除应返回错误")
	}
}

func TestMarkdownSkill_TrustLevel_LocalByDefault(t *testing.T) {
	s := newTestSkillFile(t, "---\nname: test\n---\nbody")
	if got := s.TrustLevel(); got != skill.TrustLocal {
		t.Errorf("无签名应为 TrustLocal；got %v", got)
	}
}

func TestMarkdownSkill_TrustLevel_SignedWhenSignaturePresent(t *testing.T) {
	s := &MarkdownSkill{
		Meta: SkillMeta{Name: "test", Signature: "0xdeadbeef..."},
	}
	if got := s.TrustLevel(); got != skill.TrustSigned {
		t.Errorf("有 signature 字段应为 TrustSigned；got %v", got)
	}
}
