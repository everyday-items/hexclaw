package main

// IM 出口 LaTeX→Unicode 兜底接线契约（BUG-20260712-U 同源风险）：
// solve 链提示词硬禁 LaTeX（engine/solve.go）是软约束，真机取证过模型会违反；
// 钉钉不渲染 LaTeX，漏出去就是乱码。三条 K12→IM 投递出口必须在投递前过
// channel.LaTeXToUnicode 确定性兜底：
//   1. k12IMDeliverer.DeliverText（「发送到手机」：点评/练习卡）；
//   2. k12PhotoReply（钉钉拍照批改回复 Markdown）；
//   3. newCronIMDeliver（cron IM 投递，含通道端口与平台直发回退两条分支）。
// 边界申报：桌面 HTTP 响应不转（桌面前端可自行渲染，保持原文）——本文件只断言 IM 出口。

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/cron"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

const (
	latexEgressSample = `本题 $\frac{3}{4} \times 8 = 6$，注意 5^2 = 25`
	latexEgressWant   = "本题 3/4 × 8 = 6，注意 5² = 25"
)

func TestDeliverText_ConvertsLaTeXBeforeIMSend(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	ding := &recordChannel{name: "dingtalk"}
	reg.Register(ding)
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()

	if _, err := d.DeliverText(context.Background(), "child-a", latexEgressSample); err != nil {
		t.Fatal(err)
	}
	if len(ding.sent) != 1 {
		t.Fatalf("应发送 1 条, got %d", len(ding.sent))
	}
	if ding.sent[0].text != latexEgressWant {
		t.Fatalf("「发送到手机」出口必须投递 LaTeX→Unicode 转换后的文本:\n got  %q\n want %q",
			ding.sent[0].text, latexEgressWant)
	}
}

func TestCronIMDeliver_ConvertsLaTeX_ChannelAndDirectBranches(t *testing.T) {
	reg := channel.NewRegistry()
	ding := &recordChannel{name: "dingtalk"}
	reg.Register(ding)

	var direct []recordedSend
	deliver := newCronIMDeliver(context.Background(), reg, func(ctx context.Context, target, chatID string, msg channel.Message) error {
		direct = append(direct, recordedSend{to: channel.Target{Platform: target, ChatID: chatID}, text: msg.Text})
		return nil
	})
	job := &cron.Job{ID: "job-1", ChatID: "mom-chat"}

	// 分支 1：注册通道（ChannelPort）投递。
	if err := deliver(job, "dingtalk", latexEgressSample); err != nil {
		t.Fatal(err)
	}
	if len(ding.sent) != 1 || ding.sent[0].text != latexEgressWant {
		t.Fatalf("cron 通道分支必须投递转换后文本: %+v", ding.sent)
	}
	// 分支 2：未注册目标回退平台通用直发。
	if err := deliver(job, "tg-instance-1", latexEgressSample); err != nil {
		t.Fatal(err)
	}
	if len(direct) != 1 || direct[0].text != latexEgressWant {
		t.Fatalf("cron 直发回退分支必须投递转换后文本: %+v", direct)
	}
}

func TestK12PhotoReply_ConvertsLaTeXMarkdown(t *testing.T) {
	reply := k12PhotoReply(k12usecase.PhotoGradeResult{Markdown: latexEgressSample})
	if reply == nil {
		t.Fatal("reply 不应为 nil")
	}
	if reply.Content != latexEgressWant {
		t.Fatalf("拍照批改 IM 回复必须投递转换后 Markdown:\n got  %q\n want %q",
			reply.Content, latexEgressWant)
	}
}

// 无 LaTeX 时零改动——既有「内容原样透传」契约（k12_channel_test.go）不被破坏。
func TestIMEgress_NoLaTeXPassthroughUnchanged(t *testing.T) {
	reply := k12PhotoReply(k12usecase.PhotoGradeResult{Markdown: "全对，继续保持！"})
	if reply.Content != "全对，继续保持！" {
		t.Fatalf("无 LaTeX 文本必须原样透传: %q", reply.Content)
	}
}
