package channel

// K12-INV-015 通道层契约（架构设计-v0.5.0 §7 / §6.10）：Target 只表达一对一私聊目标，
// 群 conversation 永不进入业务流。
//
// 申报（按现实现选择「群目标发送被拒」）：Target 结构体没有 conversation type 字段——
// 现实现里「群会话」唯一能流到通道层的编码形态，是钉钉发送队列的群哨兵 ChatID
// （adapter/dingtalk groupQueueTarget："\x00dingtalk-group:<openConversationId>"，
// 以 NUL 控制字符开头）；真实私聊目标（staffId / 私聊会话 ID）绝不以控制字符开头。
// 故契约落在发送口：Target.EnsureDirect 拒绝控制字符前缀的群编码目标，
// DingTalk.SendMessage（唯一真实通道）发送前强制校验——群目标被拒、send 闭包零调用。

import (
	"context"
	"errors"
	"testing"
)

func TestINV015_EnsureDirectRejectsGroupEncodedTarget(t *testing.T) {
	group := Target{Platform: "dingtalk", ChatID: "\x00dingtalk-group:cidGROUP123"}
	if err := group.EnsureDirect(); !errors.Is(err, ErrGroupTarget) {
		t.Fatalf("群编码目标必须被拒: err=%v want ErrGroupTarget", err)
	}
	direct := Target{Platform: "dingtalk", ChatID: "staff-parent-1"}
	if err := direct.EnsureDirect(); err != nil {
		t.Fatalf("私聊目标不得误伤: %v", err)
	}
}

func TestINV015_DingTalkSendRejectsGroupTargetBeforeSender(t *testing.T) {
	ch := NewDingTalk()
	sends := 0
	ch.SetSender(func(context.Context, Target, Message) error {
		sends++
		return nil
	})

	group := Target{Platform: "dingtalk", InstanceID: "bot-1", ChatID: "\x00dingtalk-group:cidGROUP123"}
	if err := ch.SendMessage(context.Background(), group, Message{Text: "练习卷"}); !errors.Is(err, ErrGroupTarget) {
		t.Fatalf("K12-INV-015 违规：群目标未被通道层拒绝（err=%v）", err)
	}
	if err := ch.SendText(context.Background(), group, "提醒"); !errors.Is(err, ErrGroupTarget) {
		t.Fatalf("SendText 群目标未被拒（err=%v）", err)
	}
	if sends != 0 {
		t.Fatalf("群目标绝不能触达平台发送函数: sends=%d", sends)
	}

	if err := ch.SendText(context.Background(), Target{Platform: "dingtalk", ChatID: "staff-parent-1"}, "hi"); err != nil {
		t.Fatalf("私聊发送不得误伤: %v", err)
	}
	if sends != 1 {
		t.Fatalf("私聊应正常发送一次: sends=%d", sends)
	}
}
