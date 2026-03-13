package plugin

import (
	"context"
	"testing"

	"github.com/everyday-items/hexagon/plugin"
	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/skill"
)

// mockSkillPlugin 模拟 SkillPlugin
type mockSkillPlugin struct {
	plugin.BasePlugin
}

func (m *mockSkillPlugin) Skills() []skill.Skill { return nil }

// mockHookPlugin 模拟 HookPlugin
type mockHookPlugin struct {
	plugin.BasePlugin
	onMsgCalled   bool
	onReplyCalled bool
}

func (m *mockHookPlugin) OnMessage(_ context.Context, msg *adapter.Message) (*adapter.Message, error) {
	m.onMsgCalled = true
	msg.Content = "[hooked] " + msg.Content
	return msg, nil
}

func (m *mockHookPlugin) OnReply(_ context.Context, reply *adapter.Reply) (*adapter.Reply, error) {
	m.onReplyCalled = true
	return reply, nil
}

func TestManagerRegisterAndList(t *testing.T) {
	mgr := NewManager()

	p := &mockSkillPlugin{
		BasePlugin: *plugin.NewBasePlugin(plugin.PluginInfo{
			Name:    "test-skill",
			Version: "1.0.0",
			Type:    TypeSkill,
		}),
	}

	if err := mgr.Register(p); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	infos := mgr.List()
	if len(infos) != 1 {
		t.Fatalf("期望 1 个插件，得到 %d", len(infos))
	}
	if infos[0].Name != "test-skill" {
		t.Errorf("插件名称不匹配: %s", infos[0].Name)
	}
}

func TestManagerHookChain(t *testing.T) {
	mgr := NewManager()

	hook := &mockHookPlugin{
		BasePlugin: *plugin.NewBasePlugin(plugin.PluginInfo{
			Name: "test-hook",
			Type: TypeHook,
		}),
	}

	if err := mgr.Register(hook); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	ctx := context.Background()
	msg := &adapter.Message{Content: "hello"}
	result, err := mgr.RunMessageHooks(ctx, msg)
	if err != nil {
		t.Fatalf("RunMessageHooks 失败: %v", err)
	}
	if result.Content != "[hooked] hello" {
		t.Errorf("期望 '[hooked] hello'，得到 '%s'", result.Content)
	}
	if !hook.onMsgCalled {
		t.Error("OnMessage 未被调用")
	}

	reply := &adapter.Reply{Content: "world"}
	_, err = mgr.RunReplyHooks(ctx, reply)
	if err != nil {
		t.Fatalf("RunReplyHooks 失败: %v", err)
	}
	if !hook.onReplyCalled {
		t.Error("OnReply 未被调用")
	}
}

func TestManagerStartStop(t *testing.T) {
	mgr := NewManager()

	p := &mockSkillPlugin{
		BasePlugin: *plugin.NewBasePlugin(plugin.PluginInfo{
			Name: "lifecycle-test",
			Type: TypeSkill,
		}),
	}

	if err := mgr.Register(p); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	ctx := context.Background()
	if err := mgr.StartAll(ctx, nil); err != nil {
		t.Fatalf("StartAll 失败: %v", err)
	}

	mgr.StopAll(ctx)
}
