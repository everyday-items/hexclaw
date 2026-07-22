package main

// ChannelPort 收敛后的 composition root 契约（架构设计-v0.5.0 §6.10 / §3.12）：
//   - k12IMDeliverer 按绑定规则的 platform 路由到注册表里的正确通道（fake channel 证据）；
//   - 未绑定 → 诚实拒绝（家长向文案，HTTP 层映射 409）；
//   - 通道未就绪 / 未配置 / 留缝 stub 未实现 → 各自诚实降级文案，绝不虚标已发送；
//   - 限绑语义保持（复用 im_bind_exclusive_test.go 既有契约，binder 改走 channel.CheckExclusiveBind）；
//   - cron IM 投递走通道：已注册通道经 ChannelPort 发送；未注册目标/留缝 stub 回退
//     平台通用直发（cron 是平台通用面，不因 K12 留缝停摆）。

import (
	"context"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/channel"
	"github.com/hexagon-codes/hexclaw/cron"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
)

// recordChannel 契约替身：记录经通道发出的消息。
type recordChannel struct {
	name       string
	sent       []recordedSend
	fail       error
	sendAck    channel.DeliveryAck
	queryAck   channel.DeliveryAck
	queryCalls int
}

type recordedSend struct {
	to   channel.Target
	text string
}

func (c *recordChannel) Name() string { return c.name }

func (c *recordChannel) SendText(ctx context.Context, to channel.Target, text string) error {
	if c.fail != nil {
		return c.fail
	}
	c.sent = append(c.sent, recordedSend{to: to, text: text})
	return nil
}

func (c *recordChannel) SendMessage(ctx context.Context, to channel.Target, msg channel.Message) error {
	return c.SendText(ctx, to, msg.Text)
}

func (c *recordChannel) SendMessageWithReceipt(ctx context.Context, to channel.Target, msg channel.Message) (channel.DeliveryAck, error) {
	if err := c.SendMessage(ctx, to, msg); err != nil {
		return channel.DeliveryAck{Status: channel.DeliveryFailed, Target: to}, err
	}
	ack := c.sendAck
	if ack.Status == "" {
		ack = channel.DeliveryAck{ExternalMessageID: "process-query-key", Status: channel.DeliveryAccepted}
	}
	ack.Target = to
	return ack, nil
}

func (c *recordChannel) QueryReceipt(_ context.Context, to channel.Target, externalMessageID string) (channel.DeliveryAck, error) {
	c.queryCalls++
	ack := c.queryAck
	if ack.Status == "" {
		ack = channel.DeliveryAck{Status: channel.DeliveryDelivered}
	}
	if ack.ExternalMessageID == "" {
		ack.ExternalMessageID = externalMessageID
	}
	ack.Target = to
	return ack, nil
}

func newDelivererFixture(t *testing.T) (*k12IMDeliverer, *agentrouter.Dispatcher, *channel.Registry) {
	t.Helper()
	dispatcher := agentrouter.New()
	for _, name := range []string{"child-a", "child-b"} {
		if err := dispatcher.Register(agentrouter.AgentConfig{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	reg := channel.NewRegistry()
	d := &k12IMDeliverer{router: dispatcher, channels: reg}
	return d, dispatcher, reg
}

func bindRule(t *testing.T, dispatcher *agentrouter.Dispatcher, platform, instanceID, chatID, agent string) {
	t.Helper()
	if err := dispatcher.AddRule(agentrouter.Rule{Platform: platform, InstanceID: instanceID, ChatID: chatID, AgentName: agent, Priority: 50}); err != nil {
		t.Fatal(err)
	}
}

func TestK12IMDeliverer_RoutesToBoundChannel(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	ding := &recordChannel{name: "dingtalk"}
	other := &recordChannel{name: "feishu"}
	reg.Register(ding)
	reg.Register(other)
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()

	target, err := d.DeliverText(context.Background(), "child-a", "点评要点")
	if err != nil {
		t.Fatal(err)
	}
	if target != "dingtalk" {
		t.Fatalf("返回投递目标应为绑定平台, got %q", target)
	}
	if len(other.sent) != 0 || len(ding.sent) != 1 {
		t.Fatalf("发送必须路由到绑定通道: dingtalk=%d feishu=%d", len(ding.sent), len(other.sent))
	}
	got := ding.sent[0]
	if got.to.ChatID != "mom-chat" || got.to.InstanceID != "bot-1" || got.to.Platform != "dingtalk" || got.text != "点评要点" {
		t.Fatalf("目标与内容必须原样透传: %+v", got)
	}
}

func TestK12IMDeliverer_UnboundHonestRefusal(t *testing.T) {
	d, _, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk"})
	d.MarkReady()
	if _, err := d.DeliverText(context.Background(), "child-a", "x"); err == nil || !strings.Contains(err.Error(), "还没绑定") {
		t.Fatalf("未绑定必须诚实拒绝（家长向文案）, got %v", err)
	}
}

func TestK12IMDeliverer_NotReadyHonest(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk"})
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	// 未 MarkReady（instanceMgr 尚未建成）：保持既有「还没就绪」文案。
	if _, err := d.DeliverText(context.Background(), "child-a", "x"); err == nil || !strings.Contains(err.Error(), "还没就绪") {
		t.Fatalf("通道未就绪必须诚实报错, got %v", err)
	}
}

func TestK12IMDeliverer_StubChannelHonestDegrade(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(channel.NewFeishu())
	bindRule(t, dispatcher, "feishu", "fs-1", "mom-chat", "child-a")
	d.MarkReady()
	_, err := d.DeliverText(context.Background(), "child-a", "x")
	if err == nil || !strings.Contains(err.Error(), "还没有开通") {
		t.Fatalf("留缝 stub 通道必须诚实「未开通」降级, got %v", err)
	}
}

func TestK12IMDeliverer_UnconfiguredChannelHonestDegrade(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk"})
	bindRule(t, dispatcher, "telegram", "tg-1", "mom-chat", "child-a")
	d.MarkReady()
	_, err := d.DeliverText(context.Background(), "child-a", "x")
	if err == nil || !strings.Contains(err.Error(), "还没有接入") {
		t.Fatalf("未配置通道必须诚实「未接入」降级, got %v", err)
	}
}

func TestK12IMDeliverer_SendFailureKeepsParentFacingCopy(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	reg.Register(&recordChannel{name: "dingtalk", fail: context.DeadlineExceeded})
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()
	_, err := d.DeliverText(context.Background(), "child-a", "x")
	if err == nil || !strings.Contains(err.Error(), "发送没有成功（dingtalk）") {
		t.Fatalf("发送失败文案必须保持既有家长向措辞, got %v", err)
	}
}

func TestAdapterDeliveryAckMapsAcceptedWithoutClaimingDelivered(t *testing.T) {
	target := channel.Target{Platform: "dingtalk", InstanceID: "pi-1", ChatID: "user-1"}
	got := channelAckFromAdapter(adapter.DeliveryAck{
		ExternalMessageID: "pqk-1",
		Status:            adapter.DeliveryAccepted,
	}, target)
	if got.Status != channel.DeliveryAccepted || got.Status == channel.DeliveryDelivered || got.ExternalMessageID != "pqk-1" || got.Target != target {
		t.Fatalf("mapped ack=%+v", got)
	}
}

func TestK12IMDelivererFreezesReceiptPayloadBeforeProviderSend(t *testing.T) {
	d, dispatcher, reg := newDelivererFixture(t)
	ding := &recordChannel{name: "dingtalk", sendAck: channel.DeliveryAck{
		ExternalMessageID: "pqk-24", Status: channel.DeliveryAccepted,
	}}
	reg.Register(ding)
	bindRule(t, dispatcher, "dingtalk", "bot-1", "mom-chat", "child-a")
	d.MarkReady()

	prepared, err := d.PrepareText(context.Background(), "child-a", "计算 $x^2$，长度 $12 \\, \\mathrm{cm}$ 的点评")
	if err != nil {
		t.Fatal(err)
	}
	if len(ding.sent) != 0 {
		t.Fatal("PrepareText must be side-effect free until receipt persistence")
	}
	if !strings.HasPrefix(prepared.BindingID, "agent-rule:") || prepared.Target.ChatID != "mom-chat" ||
		prepared.Target.InstanceID != "bot-1" || prepared.PayloadJSON == "" || prepared.RenderJSON == "" {
		t.Fatalf("prepared delivery evidence incomplete: %+v", prepared)
	}
	receipt := k12.DeliveryReceipt{
		DeliveryID: "delivery-24", AgentName: "child-a", BindingID: prepared.BindingID,
		Target: prepared.Target, PayloadJSON: prepared.PayloadJSON, RenderJSON: prepared.RenderJSON,
		PayloadDigest: deliveryPayloadDigest(prepared.PayloadJSON), Status: k12.DeliverySending,
	}
	ack, err := d.SendPrepared(context.Background(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != k12.DeliverySending || ack.ExternalMessageID != "pqk-24" {
		t.Fatalf("provider acceptance must map to domain sending: %+v", ack)
	}
	if len(ding.sent) != 1 || !strings.Contains(ding.sent[0].text, "x²") ||
		!strings.Contains(ding.sent[0].text, "12 cm") ||
		strings.Contains(ding.sent[0].text, "\\,") ||
		strings.Contains(ding.sent[0].text, "\\mathrm") {
		t.Fatalf("send must reuse frozen readable projection exactly once: %+v", ding.sent)
	}
	tampered := receipt
	tampered.BindingID = "agent-rule:rebound"
	badAck, badErr := d.SendPrepared(context.Background(), tampered)
	if badErr == nil || badAck.Status != k12.DeliveryFailed || len(ding.sent) != 1 {
		t.Fatalf("stale/rebound receipt must fail before send: ack=%+v sends=%d err=%v", badAck, len(ding.sent), badErr)
	}

	ding.queryAck = channel.DeliveryAck{Status: channel.DeliveryDelivered}
	queried, err := d.QueryPrepared(context.Background(), k12.DeliveryReceipt{
		Target: prepared.Target, ExternalMessageID: "pqk-24",
	})
	if err != nil || queried.Status != k12.DeliveryDelivered || queried.ExternalMessageID != "pqk-24" || ding.queryCalls != 1 {
		t.Fatalf("query must preserve provider evidence: ack=%+v calls=%d err=%v", queried, ding.queryCalls, err)
	}
}

func TestCronIMDeliver_GoesThroughChannel(t *testing.T) {
	reg := channel.NewRegistry()
	ding := &recordChannel{name: "dingtalk"}
	reg.Register(ding)
	reg.Register(channel.NewFeishu())

	var direct []recordedSend
	deliver := newCronIMDeliver(context.Background(), reg, func(ctx context.Context, target, chatID string, msg channel.Message) error {
		direct = append(direct, recordedSend{to: channel.Target{Platform: target, ChatID: chatID}, text: msg.Text})
		return nil
	})

	job := &cron.Job{ID: "job-1", ChatID: "mom-chat"}
	// 已注册通道：投递必须走 ChannelPort，不再直连。
	if err := deliver(job, "dingtalk", "本周练习卷"); err != nil {
		t.Fatal(err)
	}
	if len(ding.sent) != 1 || len(direct) != 0 {
		t.Fatalf("cron 投递应走通道: channel=%d direct=%d", len(ding.sent), len(direct))
	}
	if ding.sent[0].to.ChatID != "mom-chat" || ding.sent[0].text != "本周练习卷" {
		t.Fatalf("cron 投递目标/内容必须原样透传: %+v", ding.sent[0])
	}
	// 未注册目标（其他平台/实例 ID）：回退平台通用直发，行为不变。
	if err := deliver(job, "tg-instance-1", "提醒"); err != nil {
		t.Fatal(err)
	}
	if len(direct) != 1 || direct[0].to.Platform != "tg-instance-1" {
		t.Fatalf("未注册目标应回退通用直发: %+v", direct)
	}
	// 留缝 stub（未实现）：cron 是平台通用面，回退直发不停摆。
	if err := deliver(job, "feishu", "提醒"); err != nil {
		t.Fatal(err)
	}
	if len(direct) != 2 {
		t.Fatalf("stub 未实现应回退通用直发: %+v", direct)
	}
	// 无 chat_id：保持既有报错。
	if err := deliver(&cron.Job{ID: "job-2"}, "dingtalk", "x"); err == nil || !strings.Contains(err.Error(), "no chat_id") {
		t.Fatalf("无 chat_id 应报错, got %v", err)
	}
}
