package skill

import (
	"context"
	"strings"
	"testing"

	"github.com/everyday-items/hexclaw/internal/adapter"
)

// mockSkill 测试用 Skill 实现
type mockSkill struct {
	name    string
	desc    string
	prefix  string // Match 匹配的前缀
}

func (s *mockSkill) Name() string        { return s.name }
func (s *mockSkill) Description() string  { return s.desc }
func (s *mockSkill) Match(content string) bool {
	return strings.HasPrefix(content, s.prefix)
}
func (s *mockSkill) Execute(_ context.Context, args map[string]any) (*Result, error) {
	return &Result{Content: "executed: " + s.name}, nil
}

func TestRegistry_RegisterAndGet(t *testing.T) {
	reg := NewRegistry()

	s := &mockSkill{name: "weather", desc: "天气查询", prefix: "/天气"}
	if err := reg.Register(s); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	got, ok := reg.Get("weather")
	if !ok {
		t.Fatal("应找到已注册的 Skill")
	}
	if got.Name() != "weather" {
		t.Errorf("Skill 名称不匹配: %s", got.Name())
	}

	// 重复注册应失败
	if err := reg.Register(s); err == nil {
		t.Error("重复注册应返回错误")
	}
}

func TestRegistry_Match(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockSkill{name: "weather", desc: "天气", prefix: "/天气"})
	reg.Register(&mockSkill{name: "translate", desc: "翻译", prefix: "/翻译"})

	// 匹配天气
	msg := &adapter.Message{Content: "/天气 北京"}
	matched, ok := reg.Match(msg)
	if !ok {
		t.Fatal("应匹配到天气 Skill")
	}
	if matched.Name() != "weather" {
		t.Errorf("期望匹配 weather，得到 %s", matched.Name())
	}

	// 匹配翻译
	msg2 := &adapter.Message{Content: "/翻译 hello"}
	matched2, ok := reg.Match(msg2)
	if !ok {
		t.Fatal("应匹配到翻译 Skill")
	}
	if matched2.Name() != "translate" {
		t.Errorf("期望匹配 translate，得到 %s", matched2.Name())
	}

	// 无匹配
	msg3 := &adapter.Message{Content: "你好，今天天气怎么样？"}
	_, ok = reg.Match(msg3)
	if ok {
		t.Error("不应匹配到任何 Skill")
	}
}

func TestRegistry_All(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&mockSkill{name: "a", desc: "A", prefix: "/a"})
	reg.Register(&mockSkill{name: "b", desc: "B", prefix: "/b"})
	reg.Register(&mockSkill{name: "c", desc: "C", prefix: "/c"})

	all := reg.All()
	if len(all) != 3 {
		t.Fatalf("期望 3 个 Skill，得到 %d", len(all))
	}

	// 验证注册顺序
	if all[0].Name() != "a" || all[1].Name() != "b" || all[2].Name() != "c" {
		t.Error("Skill 顺序不正确")
	}
}
