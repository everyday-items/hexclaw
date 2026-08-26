package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hexagon-codes/hexclaw/channel"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/scenarios/k12"
	k12usecase "github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

type exactInstanceGuardChannel struct {
	prepares        int
	preflights      int
	partSends       int
	envelopeSends   int
	queries         int
	prepareTarget   channel.Target
	preflightTarget channel.Target
	envelopeTarget  channel.Target
	queryTarget     channel.Target
}

func (*exactInstanceGuardChannel) Name() string { return "dingtalk" }

func (*exactInstanceGuardChannel) SendText(context.Context, channel.Target, string) error {
	return nil
}

func (*exactInstanceGuardChannel) SendMessage(context.Context, channel.Target, channel.Message) error {
	return nil
}

func (*exactInstanceGuardChannel) SendMessageWithReceipt(
	context.Context,
	channel.Target,
	channel.Message,
) (channel.DeliveryAck, error) {
	return channel.DeliveryAck{Status: channel.DeliveryAccepted}, nil
}

func (c *exactInstanceGuardChannel) QueryReceipt(
	_ context.Context,
	to channel.Target,
	externalMessageID string,
) (channel.DeliveryAck, error) {
	c.queries++
	c.queryTarget = to
	return channel.DeliveryAck{
		ExternalMessageID: externalMessageID,
		Status:            channel.DeliveryDelivered,
		Target:            to,
	}, nil
}

func (c *exactInstanceGuardChannel) PrepareDeliveryPartResource(
	_ context.Context,
	to channel.Target,
	_ channel.DeliveryPart,
) (string, error) {
	c.prepares++
	c.prepareTarget = to
	return "@prepared-image", nil
}

func (c *exactInstanceGuardChannel) SendPreparedPartWithReceipt(
	_ context.Context,
	to channel.Target,
	_ channel.DeliveryPart,
) (channel.DeliveryAck, error) {
	c.partSends++
	return channel.DeliveryAck{
		ExternalMessageID: "part-external-id",
		Status:            channel.DeliveryAccepted,
		Target:            to,
	}, nil
}

func (c *exactInstanceGuardChannel) SendPreparedEnvelopeWithReceipt(
	_ context.Context,
	to channel.Target,
	_ channel.PreparedEnvelope,
) (channel.DeliveryAck, error) {
	c.envelopeSends++
	c.envelopeTarget = to
	return channel.DeliveryAck{
		ExternalMessageID: "envelope-external-id",
		Status:            channel.DeliveryAccepted,
		Target:            to,
	}, nil
}

func (c *exactInstanceGuardChannel) PreflightPreparedEnvelope(
	_ context.Context,
	to channel.Target,
	_ channel.PreparedEnvelope,
) error {
	c.preflights++
	c.preflightTarget = to
	return nil
}

func (c *exactInstanceGuardChannel) assertZeroProviderSideEffects(t *testing.T) {
	t.Helper()
	if c.prepares != 0 || c.preflights != 0 || c.partSends != 0 || c.envelopeSends != 0 || c.queries != 0 {
		t.Fatalf(
			"provider side effects: prepare=%d preflight=%d part_send=%d envelope_send=%d query=%d",
			c.prepares, c.preflights, c.partSends, c.envelopeSends, c.queries,
		)
	}
}

func TestK12IMDelivererCanonicalizesLegacyEmptyAndNamedBindingsBeforeFreeze(t *testing.T) {
	d, dispatcher, registry := newDelivererFixture(t)
	provider := &exactInstanceGuardChannel{}
	registry.Register(provider)
	for _, rule := range []agentrouter.Rule{
		{ID: 21, Platform: "dingtalk", ChatID: "parent-1", AgentName: "child-a"},
		{ID: 22, Platform: "dingtalk", InstanceID: "family-dingtalk", ChatID: "parent-1", AgentName: "child-a"},
	} {
		if err := dispatcher.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	d.SetInstanceResolver(func(platform, instanceRef string) (string, error) {
		if platform != "dingtalk" || (instanceRef != "" && instanceRef != "family-dingtalk") {
			return "", fmt.Errorf("unexpected instance reference %q/%q", platform, instanceRef)
		}
		return "pi-dingtalk-main", nil
	})
	d.MarkReady()

	targets, err := d.ResolveTextTargets(context.Background(), "child-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 {
		t.Fatalf("同一物理实例的 legacy/name 绑定必须去重，targets=%+v", targets)
	}
	if targets[0].Target.InstanceID != "pi-dingtalk-main" ||
		targets[0].BindingID != "agent-rule:21" {
		t.Fatalf("冻结目标未 canonicalize 为 stable ID: %+v", targets[0])
	}
	prepared, err := d.PrepareTextForTargets(context.Background(), "## 作品点评", targets)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 1 || prepared[0].Target.InstanceID != "pi-dingtalk-main" {
		t.Fatalf("冻结载荷实例身份漂移: %+v", prepared)
	}
	provider.assertZeroProviderSideEffects(t)
}

func TestK12IMDelivererSkipsGroupSentinelBeforeInstanceResolution(t *testing.T) {
	d, dispatcher, _ := newDelivererFixture(t)
	for _, rule := range []agentrouter.Rule{
		{ID: 23, Platform: "dingtalk", ChatID: "\x00dingtalk-group:g1", AgentName: "child-a"},
		{ID: 24, Platform: "dingtalk", ChatID: "parent-1", AgentName: "child-a"},
	} {
		if err := dispatcher.AddRule(rule); err != nil {
			t.Fatal(err)
		}
	}
	resolverCalls := 0
	d.SetInstanceResolver(func(platform, instanceRef string) (string, error) {
		resolverCalls++
		return "pi-dingtalk-main", nil
	})
	d.MarkReady()

	targets, err := d.ResolveTextTargets(context.Background(), "child-a")
	if err != nil {
		t.Fatal(err)
	}
	if resolverCalls != 1 || len(targets) != 1 || targets[0].Target.ChatID != "parent-1" {
		t.Fatalf("群聊哨兵必须在实例解析前跳过: resolver_calls=%d targets=%+v", resolverCalls, targets)
	}
}

func TestK12IMDelivererInstanceResolutionFailureStopsBeforeProvider(t *testing.T) {
	for _, test := range []struct {
		name        string
		instanceRef string
		err         error
	}{
		{name: "ambiguous empty instance", err: errors.New("multiple running instances")},
		{name: "unknown instance", instanceRef: "missing", err: errors.New("instance not running")},
		{name: "cross platform instance", instanceRef: "pi-slack", err: errors.New("instance belongs to another platform")},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, dispatcher, registry := newDelivererFixture(t)
			provider := &exactInstanceGuardChannel{}
			registry.Register(provider)
			if err := dispatcher.AddRule(agentrouter.Rule{
				ID: 31, Platform: "dingtalk", InstanceID: test.instanceRef,
				ChatID: "parent-1", AgentName: "child-a",
			}); err != nil {
				t.Fatal(err)
			}
			d.SetInstanceResolver(func(platform, instanceRef string) (string, error) {
				if platform != "dingtalk" || instanceRef != test.instanceRef {
					t.Fatalf("resolver input=%q/%q", platform, instanceRef)
				}
				return "", test.err
			})
			d.MarkReady()

			if targets, err := d.ResolveTextTargets(context.Background(), "child-a"); err == nil || len(targets) != 0 {
				t.Fatalf("实例解析失败必须阻止目标冻结: targets=%+v err=%v", targets, err)
			}
			provider.assertZeroProviderSideEffects(t)
		})
	}
}

func TestK12IMDelivererResolvedStableInstanceSurvivesPrepareSendAndQuery(t *testing.T) {
	for _, test := range []struct {
		name        string
		instanceRef string
	}{
		{name: "legacy empty binding"},
		{name: "named binding", instanceRef: "family-dingtalk"},
	} {
		t.Run(test.name, func(t *testing.T) {
			d, dispatcher, registry := newDelivererFixture(t)
			provider := &exactInstanceGuardChannel{}
			registry.Register(provider)
			rule := agentrouter.Rule{
				ID: 35, Platform: "dingtalk", InstanceID: test.instanceRef,
				ChatID: "parent-1", AgentName: "child-a",
			}
			if err := dispatcher.AddRule(rule); err != nil {
				t.Fatal(err)
			}
			d.SetInstanceResolver(func(platform, instanceRef string) (string, error) {
				if platform != "dingtalk" {
					return "", fmt.Errorf("unexpected platform %q", platform)
				}
				switch instanceRef {
				case "", "family-dingtalk", "pi-dingtalk-main":
					return "pi-dingtalk-main", nil
				default:
					return "", fmt.Errorf("unexpected instance reference %q", instanceRef)
				}
			})
			d.MarkReady()

			targets, err := d.ResolveTextTargets(context.Background(), "child-a")
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := d.PrepareMessageForTargets(
				context.Background(),
				k12usecase.DeliveryMessage{
					Content: "## 可见证据\n\n- 颜色清楚。",
					Attachments: []k12usecase.DeliveryAttachment{{
						Name: "creative.png", MIME: "image/png", Data: []byte("image-bytes"),
					}},
				},
				targets,
			)
			if err != nil {
				t.Fatal(err)
			}
			receipts := make([]k12.DeliveryReceipt, 0, len(prepared))
			for i, item := range prepared {
				receipts = append(receipts, k12.DeliveryReceipt{
					DeliveryID: fmt.Sprintf("stable-instance-part-%d", i+1),
					BatchID:    "stable-instance-batch", BatchOrdinal: i + 1,
					PartKind: item.PartKind, PartMIME: item.PartMIME,
					PartOrdinal: item.PartOrdinal, PartDigest: item.PartDigest,
					AgentName: "child-a", ObjectKind: "creative_work", ObjectID: "creative-1",
					BindingID: item.BindingID, Target: item.Target,
					PayloadDigest: deliveryPayloadDigest(item.PayloadJSON),
					PayloadJSON:   item.PayloadJSON, RenderJSON: item.RenderJSON,
					Status: k12.DeliveryPending,
				})
			}
			if len(receipts) != 2 || receipts[0].Target.InstanceID != "pi-dingtalk-main" ||
				receipts[1].Target.InstanceID != "pi-dingtalk-main" {
				t.Fatalf("component receipts 未冻结 stable instance: %+v", receipts)
			}

			resourceID, err := d.PrepareDeliveryPartResource(context.Background(), receipts[1])
			if err != nil {
				t.Fatal(err)
			}
			receipts[1].PreparedResourceID = resourceID
			for i := range receipts {
				receipts[i].Status = k12.DeliverySending
				receipts[i].Attempt = 1
			}
			ack, err := d.SendPreparedEnvelope(context.Background(), receipts)
			if err != nil || ack.Status != k12.DeliverySending {
				t.Fatalf("stable envelope send: ack=%+v err=%v", ack, err)
			}
			for i := range receipts {
				receipts[i].ExternalMessageID = ack.ExternalMessageID
			}
			// 查询只依赖已经冻结的 stable target；绑定随后移除也不得改投其他实例。
			dispatcher.RemoveRules("child-a")
			queryAck, err := d.QueryPreparedEnvelope(context.Background(), receipts)
			if err != nil || queryAck.Status != k12.DeliveryDelivered {
				t.Fatalf("stable envelope query: ack=%+v err=%v", queryAck, err)
			}
			wantTarget := (channel.Target{
				Platform: "dingtalk", InstanceID: "pi-dingtalk-main", ChatID: "parent-1",
			})
			if provider.prepares != 1 || provider.envelopeSends != 1 || provider.queries != 1 ||
				provider.partSends != 0 || provider.prepareTarget != wantTarget ||
				provider.envelopeTarget != wantTarget || provider.queryTarget != wantTarget {
				t.Fatalf("stable target 未贯穿 prepare/send/query: provider=%+v want=%+v", provider, wantTarget)
			}
		})
	}
}

func emptyInstanceCreativeReceipts(
	t *testing.T,
) (*k12IMDeliverer, *exactInstanceGuardChannel, []k12.DeliveryReceipt) {
	t.Helper()
	d, dispatcher, registry := newDelivererFixture(t)
	provider := &exactInstanceGuardChannel{}
	registry.Register(provider)
	rule := agentrouter.Rule{
		ID: 41, Platform: "dingtalk", ChatID: "parent-1", AgentName: "child-a",
	}
	if err := dispatcher.AddRule(rule); err != nil {
		t.Fatal(err)
	}
	prepared, err := d.PrepareMessageForTargets(
		context.Background(),
		k12usecase.DeliveryMessage{
			Content: "## 可见证据\n\n- 颜色清楚。",
			Attachments: []k12usecase.DeliveryAttachment{{
				Name: "creative.png", MIME: "image/png", Data: []byte("image-bytes"),
			}},
		},
		[]k12usecase.ResolvedDeliveryTarget{{
			BindingID: stableBindingID(rule),
			Target: k12.DeliveryTarget{
				Platform: "dingtalk", ChatID: "parent-1",
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 2 {
		t.Fatalf("fixture parts=%d want 2", len(prepared))
	}
	receipts := make([]k12.DeliveryReceipt, 0, len(prepared))
	for i, item := range prepared {
		receipts = append(receipts, k12.DeliveryReceipt{
			DeliveryID: fmt.Sprintf("empty-instance-part-%d", i+1),
			BatchID:    "empty-instance-batch", BatchOrdinal: i + 1,
			PartKind: item.PartKind, PartMIME: item.PartMIME,
			PartOrdinal: item.PartOrdinal, PartDigest: item.PartDigest,
			AgentName: "child-a", ObjectKind: "creative_work", ObjectID: "creative-1",
			BindingID: item.BindingID, Target: item.Target,
			PayloadDigest: deliveryPayloadDigest(item.PayloadJSON),
			PayloadJSON:   item.PayloadJSON, RenderJSON: item.RenderJSON,
		})
	}
	return d, provider, receipts
}

func TestK12IMDelivererRejectsEmptyFrozenInstanceBeforeMediaPreparation(t *testing.T) {
	d, provider, receipts := emptyInstanceCreativeReceipts(t)
	if resourceID, err := d.PrepareDeliveryPartResource(context.Background(), receipts[1]); err == nil || resourceID != "" {
		t.Fatalf("空实例回执必须在媒体准备前失败: resource=%q err=%v", resourceID, err)
	}
	provider.assertZeroProviderSideEffects(t)
}

func TestK12IMDelivererRejectsEmptyFrozenInstanceBeforeEnvelopeSend(t *testing.T) {
	d, provider, receipts := emptyInstanceCreativeReceipts(t)
	for i := range receipts {
		receipts[i].Status = k12.DeliverySending
		receipts[i].Attempt = 1
	}
	receipts[1].PreparedResourceID = "@prepared-image"
	if ack, err := d.SendPreparedEnvelope(context.Background(), receipts); err == nil || ack.Status != k12.DeliveryFailed {
		t.Fatalf("空实例回执必须在 envelope 发送前失败: ack=%+v err=%v", ack, err)
	}
	provider.assertZeroProviderSideEffects(t)
}

func TestK12IMDelivererRejectsEmptyFrozenInstanceBeforeReceiptQuery(t *testing.T) {
	d, provider, receipts := emptyInstanceCreativeReceipts(t)
	for i := range receipts {
		receipts[i].Status = k12.DeliverySending
		receipts[i].Attempt = 1
		receipts[i].ExternalMessageID = "external-envelope"
	}
	receipts[1].PreparedResourceID = "@prepared-image"
	if ack, err := d.QueryPreparedEnvelope(context.Background(), receipts); err == nil || ack.Status != k12.DeliveryOutcomeUnknown {
		t.Fatalf("空实例回执必须在查询前失败: ack=%+v err=%v", ack, err)
	}
	provider.assertZeroProviderSideEffects(t)
}
