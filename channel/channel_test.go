package channel_test

// ChannelPort 端口行为契约（架构设计-v0.5.0 §6.10 / ADR-K12-011）：
//   - 注册表 name→通道端口，发送路由到正确通道；
//   - 未配置通道 → 诚实错误（ErrNotConfigured），不静默吞、不猜测投递；
//   - 飞书/企微留缝 stub：方法集齐但返回「未实现」诚实错误（ErrNotImplemented）；
//   - 钉钉通道装配顺序缝：sender 未回填前诚实 ErrNotReady，回填后透传发送；
//   - 限绑语义（§3.12）归属通道层：同一私聊目标同一时间只绑一个 TutorAgent。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
)

// fakePort 契约测试替身：记录发送调用。
type fakePort struct {
	name  string
	sends []fakeSend
}

type fakeSend struct {
	to  channel.Target
	msg channel.Message
}

func (f *fakePort) Name() string { return f.name }

func (f *fakePort) SendText(ctx context.Context, to channel.Target, text string) error {
	return f.SendMessage(ctx, to, channel.Message{Text: text})
}

func (f *fakePort) SendMessage(ctx context.Context, to channel.Target, msg channel.Message) error {
	f.sends = append(f.sends, fakeSend{to: to, msg: msg})
	return nil
}

func TestRegistry_RoutesToCorrectChannel(t *testing.T) {
	reg := channel.NewRegistry()
	a := &fakePort{name: "chan-a"}
	b := &fakePort{name: "chan-b"}
	reg.Register(a)
	reg.Register(b)

	got, err := reg.Get("chan-b")
	if err != nil {
		t.Fatalf("已注册通道应可取到, got err=%v", err)
	}
	to := channel.Target{Platform: "chan-b", ChatID: "mom-chat"}
	if err := got.SendText(context.Background(), to, "hi"); err != nil {
		t.Fatal(err)
	}
	if len(a.sends) != 0 || len(b.sends) != 1 {
		t.Fatalf("发送必须路由到 chan-b 而非 chan-a: a=%d b=%d", len(a.sends), len(b.sends))
	}
	if b.sends[0].to.ChatID != "mom-chat" || b.sends[0].msg.Text != "hi" {
		t.Fatalf("目标与内容必须原样透传: %+v", b.sends[0])
	}
}

func TestRegistry_UnconfiguredChannelHonestError(t *testing.T) {
	reg := channel.NewRegistry()
	if _, err := reg.Get("telegram"); !errors.Is(err, channel.ErrNotConfigured) {
		t.Fatalf("未配置通道必须返回 ErrNotConfigured 诚实错误, got %v", err)
	}
}

func TestStubs_MethodSetCompleteButHonestlyUnimplemented(t *testing.T) {
	ctx := context.Background()
	to := channel.Target{ChatID: "c"}
	for _, p := range []channel.Port{channel.NewFeishu(), channel.NewWeCom()} {
		if p.Name() != "feishu" && p.Name() != "wecom" {
			t.Fatalf("stub 通道名异常: %q", p.Name())
		}
		if err := p.SendText(ctx, to, "t"); !errors.Is(err, channel.ErrNotImplemented) {
			t.Errorf("%s SendText 应返回未实现诚实错误, got %v", p.Name(), err)
		}
		if err := p.SendMessage(ctx, to, channel.Message{Text: "t"}); !errors.Is(err, channel.ErrNotImplemented) {
			t.Errorf("%s SendMessage 应返回未实现诚实错误, got %v", p.Name(), err)
		}
	}
}

func TestDingTalk_NotReadyThenDelegates(t *testing.T) {
	ctx := context.Background()
	d := channel.NewDingTalk()
	if d.Name() != "dingtalk" {
		t.Fatalf("通道名应为 dingtalk, got %q", d.Name())
	}
	to := channel.Target{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "mom-chat"}
	// sender 未回填（instanceMgr 尚未建成）：诚实 ErrNotReady，不静默丢。
	if err := d.SendText(ctx, to, "hi"); !errors.Is(err, channel.ErrNotReady) {
		t.Fatalf("sender 未回填应返回 ErrNotReady, got %v", err)
	}
	var got []fakeSend
	d.SetSender(func(ctx context.Context, to channel.Target, msg channel.Message) error {
		got = append(got, fakeSend{to: to, msg: msg})
		return nil
	})
	if err := d.SendText(ctx, to, "hi"); err != nil {
		t.Fatal(err)
	}
	msg := channel.Message{Text: "md", Attachments: []channel.Attachment{{Name: "批改后的作业.png", MIME: "image/png", Data: []byte{1, 2}}}}
	if err := d.SendMessage(ctx, to, msg); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].msg.Text != "hi" || len(got[1].msg.Attachments) != 1 {
		t.Fatalf("回填后发送必须原样透传（文本+图文）: %+v", got)
	}
	if got[1].to != to {
		t.Fatalf("目标必须原样透传: %+v", got[1].to)
	}
}

func TestTarget_SendKeyInstanceFirst(t *testing.T) {
	// 与既有 k12IMDeliverer 规则一致：实例 ID 优先，未记录实例退回平台名。
	if k := (channel.Target{Platform: "dingtalk", InstanceID: "bot-1"}).SendKey(); k != "bot-1" {
		t.Fatalf("有实例 ID 应优先用实例, got %q", k)
	}
	if k := (channel.Target{Platform: "dingtalk"}).SendKey(); k != "dingtalk" {
		t.Fatalf("无实例 ID 应退回平台名, got %q", k)
	}
}

func TestCheckExclusiveBind_OneTutorPerChatTarget(t *testing.T) {
	existing := []channel.Binding{{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "mom-chat", AgentName: "child-a"}}

	// 同实例重复绑定 → 幂等。
	already, err := channel.CheckExclusiveBind(existing, channel.Binding{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "mom-chat", AgentName: "child-a"})
	if err != nil || !already {
		t.Fatalf("同实例重复绑定应幂等, already=%v err=%v", already, err)
	}
	// 同一私聊目标绑第二个孩子 → 拒绝并明示原因（含已绑实例名与解绑引导）。
	_, err = channel.CheckExclusiveBind(existing, channel.Binding{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "mom-chat", AgentName: "child-b"})
	if err == nil {
		t.Fatal("同一私聊目标绑第二个 TutorAgent 必须被拒绝（§3.12 限绑）")
	}
	if !strings.Contains(err.Error(), "child-a") || !strings.Contains(err.Error(), "解绑") {
		t.Errorf("拒绝理由应明示已绑实例并引导先解绑, got %v", err)
	}
	// 不同私聊目标 → 各绑各的。
	already, err = channel.CheckExclusiveBind(existing, channel.Binding{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "dad-chat", AgentName: "child-b"})
	if err != nil || already {
		t.Fatalf("不同私聊目标绑不同实例应放行, already=%v err=%v", already, err)
	}
}
