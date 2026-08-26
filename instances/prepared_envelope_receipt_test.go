package instances

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

type preparedEnvelopeCall struct {
	chatID string
	parts  []adapter.DeliveryPart
}

type preparedEnvelopeCapableAdapter struct {
	deliveryPartCapableAdapter
	envelopeCalls  []preparedEnvelopeCall
	preflightCalls []preparedEnvelopeCall
	preflightErr   error
	ack            adapter.DeliveryAck
	err            error
}

type strictFrozenPathAdapter struct {
	deliveryPartCapableAdapter
	sendReceipts int
	queries      int
}

func (a *strictFrozenPathAdapter) SendWithReceipt(
	_ context.Context,
	chatID string,
	_ *adapter.Reply,
) (adapter.DeliveryAck, error) {
	a.sendReceipts++
	return adapter.DeliveryAck{
		ExternalMessageID: "external:" + chatID,
		Status:            adapter.DeliveryAccepted,
	}, nil
}

func (a *strictFrozenPathAdapter) QueryReceipt(
	_ context.Context,
	externalID string,
) (adapter.DeliveryAck, error) {
	a.queries++
	return adapter.DeliveryAck{
		ExternalMessageID: externalID,
		Status:            adapter.DeliveryDelivered,
	}, nil
}

func (a *preparedEnvelopeCapableAdapter) SendPreparedEnvelopeWithReceipt(
	_ context.Context,
	chatID string,
	envelope adapter.PreparedEnvelope,
) (adapter.DeliveryAck, error) {
	frozen := append([]adapter.DeliveryPart(nil), envelope.Parts...)
	a.envelopeCalls = append(a.envelopeCalls, preparedEnvelopeCall{chatID: chatID, parts: frozen})
	return a.ack, a.err
}

func (a *preparedEnvelopeCapableAdapter) ValidatePreparedEnvelope(envelope adapter.PreparedEnvelope) error {
	frozen := append([]adapter.DeliveryPart(nil), envelope.Parts...)
	a.preflightCalls = append(a.preflightCalls, preparedEnvelopeCall{parts: frozen})
	return a.preflightErr
}

func preparedEnvelopeParts() []adapter.DeliveryPart {
	return []adapter.DeliveryPart{
		{
			Kind:    messagecontent.PartMarkdown,
			MIME:    "text/markdown",
			Ordinal: 0,
			Digest:  "sha256:markdown",
			Text:    "## 作品点评",
		},
		{
			Kind:               messagecontent.PartArtifact,
			MIME:               "image/png",
			Ordinal:            1,
			Digest:             "sha256:image",
			PreparedResourceID: "@media-image",
			Attachment: &adapter.Attachment{
				Type: "image",
				Name: "creative.png",
				Mime: "image/png",
				Data: "aW1hZ2U=",
			},
		},
	}
}

func installPreparedEnvelopeAdapter(mgr *Manager, inst *Instance, adp adapter.Adapter) {
	mgr.mu.Lock()
	defer mgr.mu.Unlock()
	mgr.running[inst.Name] = adp
	mgr.metadata[inst.Name] = inst
}

func TestManagerRoutesPreparedEnvelopeExactlyOnceByStableInstanceID(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	target := &Instance{ID: "pi-envelope-target", Name: "ding-target", Provider: "dingtalk"}
	other := &Instance{ID: "pi-envelope-other", Name: "ding-other", Provider: "dingtalk"}
	wantAck := adapter.DeliveryAck{
		ExternalMessageID: "external-envelope-target",
		Status:            adapter.DeliveryAccepted,
	}
	targetAdapter := &preparedEnvelopeCapableAdapter{ack: wantAck}
	otherAdapter := &preparedEnvelopeCapableAdapter{
		ack: adapter.DeliveryAck{ExternalMessageID: "external-envelope-other", Status: adapter.DeliveryDelivered},
	}
	installPreparedEnvelopeAdapter(mgr, target, targetAdapter)
	installPreparedEnvelopeAdapter(mgr, other, otherAdapter)

	parts := preparedEnvelopeParts()
	gotAck, err := mgr.SendPreparedEnvelopeWithReceipt(
		context.Background(), target.ID, "parent-1", adapter.PreparedEnvelope{Parts: parts},
	)
	if err != nil {
		t.Fatalf("发送 prepared envelope 失败: %v", err)
	}
	if !reflect.DeepEqual(gotAck, wantAck) {
		t.Fatalf("ack 未原样透传: got=%+v want=%+v", gotAck, wantAck)
	}
	if len(targetAdapter.envelopeCalls) != 1 {
		t.Fatalf("stable instance 应精确发送一次 envelope，实际 %d", len(targetAdapter.envelopeCalls))
	}
	if len(otherAdapter.envelopeCalls) != 0 {
		t.Fatalf("stable instance 路由不得发送到同 provider 的其他实例，实际 %d", len(otherAdapter.envelopeCalls))
	}
	call := targetAdapter.envelopeCalls[0]
	if call.chatID != "parent-1" || !reflect.DeepEqual(call.parts, parts) {
		t.Fatalf("envelope 载荷漂移: chat=%q parts=%+v", call.chatID, call.parts)
	}
	if len(targetAdapter.sent) != 0 || len(otherAdapter.sent) != 0 {
		t.Fatalf("envelope 发送不得降级为旧逐 part 发送: target=%d other=%d", len(targetAdapter.sent), len(otherAdapter.sent))
	}
}

func TestManagerPreflightsPreparedEnvelopeByStableInstanceWithoutProviderSend(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	target := &Instance{ID: "pi-envelope-target", Name: "ding-target", Provider: "dingtalk"}
	other := &Instance{ID: "pi-envelope-other", Name: "ding-other", Provider: "dingtalk"}
	targetAdapter := &preparedEnvelopeCapableAdapter{}
	otherAdapter := &preparedEnvelopeCapableAdapter{}
	installPreparedEnvelopeAdapter(mgr, target, targetAdapter)
	installPreparedEnvelopeAdapter(mgr, other, otherAdapter)

	envelope := adapter.PreparedEnvelope{Parts: preparedEnvelopeParts()}
	if err := mgr.PreflightPreparedEnvelope(
		context.Background(), target.ID, "parent-1", envelope,
	); err != nil {
		t.Fatalf("stable instance preflight 失败: %v", err)
	}
	if len(targetAdapter.preflightCalls) != 1 || len(otherAdapter.preflightCalls) != 0 {
		t.Fatalf(
			"preflight stable 路由错误: target=%d other=%d",
			len(targetAdapter.preflightCalls), len(otherAdapter.preflightCalls),
		)
	}
	if len(targetAdapter.envelopeCalls) != 0 || len(otherAdapter.envelopeCalls) != 0 ||
		len(targetAdapter.prepared) != 0 || len(otherAdapter.prepared) != 0 {
		t.Fatalf("preflight 不得上传或发送: target=%+v other=%+v", targetAdapter, otherAdapter)
	}
}

func TestManagerPreparedEnvelopePreflightRejectsWrongStableInstanceWithoutSideEffects(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	capable := &preparedEnvelopeCapableAdapter{}
	installPreparedEnvelopeAdapter(mgr, &Instance{
		ID: "pi-current", Name: "pi-retired", Provider: "dingtalk",
	}, capable)

	err := mgr.PreflightPreparedEnvelope(
		context.Background(), "pi-retired", "parent-1",
		adapter.PreparedEnvelope{Parts: preparedEnvelopeParts()},
	)
	if err == nil {
		t.Fatal("retired stable ID must fail prepared envelope preflight")
	}
	if len(capable.preflightCalls) != 0 || len(capable.envelopeCalls) != 0 || len(capable.prepared) != 0 {
		t.Fatalf("cross-instance preflight reached replacement instance: %+v", capable)
	}
}

func TestManagerPreparedEnvelopePreflightFailsClosedWithoutValidatorCapability(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	inst := &Instance{ID: "pi-part-only", Name: "ding-part-only", Provider: "dingtalk"}
	partOnly := &deliveryPartCapableAdapter{}
	installPreparedEnvelopeAdapter(mgr, inst, partOnly)

	err := mgr.PreflightPreparedEnvelope(
		context.Background(), inst.ID, "parent-1",
		adapter.PreparedEnvelope{Parts: preparedEnvelopeParts()},
	)
	if err == nil {
		t.Fatal("adapter without envelope validator must fail preflight closed")
	}
	if len(partOnly.prepared) != 0 || len(partOnly.sent) != 0 {
		t.Fatalf("missing preflight capability fell back to legacy operations: %+v", partOnly)
	}
}

func TestManagerPreparedEnvelopePassesThroughOutcomeUnknownAckAndError(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	inst := &Instance{ID: "pi-envelope-unknown", Name: "ding-unknown", Provider: "dingtalk"}
	wantErr := errors.New("provider response is unknown")
	wantAck := adapter.DeliveryAck{
		ExternalMessageID: "external-envelope-unknown",
		Status:            adapter.DeliveryOutcomeUnknown,
	}
	capable := &preparedEnvelopeCapableAdapter{ack: wantAck, err: wantErr}
	installPreparedEnvelopeAdapter(mgr, inst, capable)

	gotAck, gotErr := mgr.SendPreparedEnvelopeWithReceipt(
		context.Background(), inst.ID, "parent-unknown", adapter.PreparedEnvelope{Parts: preparedEnvelopeParts()},
	)
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("transport error 未原样透传: got=%v want=%v", gotErr, wantErr)
	}
	if !reflect.DeepEqual(gotAck, wantAck) {
		t.Fatalf("outcome_unknown ack 未原样透传: got=%+v want=%+v", gotAck, wantAck)
	}
	if len(capable.envelopeCalls) != 1 || len(capable.sent) != 0 {
		t.Fatalf("应只有一次 envelope 物理调用且零旧 part 发送: envelope=%d part=%d", len(capable.envelopeCalls), len(capable.sent))
	}
}

func TestManagerPreparedEnvelopeFailsClosedWithoutEnvelopeCapability(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	inst := &Instance{ID: "pi-part-only", Name: "ding-part-only", Provider: "dingtalk"}
	partOnly := &deliveryPartCapableAdapter{}
	installPreparedEnvelopeAdapter(mgr, inst, partOnly)

	ack, err := mgr.SendPreparedEnvelopeWithReceipt(
		context.Background(), inst.ID, "parent-1", adapter.PreparedEnvelope{Parts: preparedEnvelopeParts()},
	)
	if err == nil {
		t.Fatal("只有旧 DeliveryPartAdapter 能力时必须 fail closed")
	}
	if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
		t.Fatalf("fail-closed ack 非法: %+v", ack)
	}
	if len(partOnly.prepared) != 0 || len(partOnly.sent) != 0 {
		t.Fatalf("缺少 envelope 能力时不得回退逐 part: prepared=%d sent=%d", len(partOnly.prepared), len(partOnly.sent))
	}
}

func TestManagerPreparedEnvelopeRejectsInvalidTargetOrInstanceWithoutSideEffects(t *testing.T) {
	tests := []struct {
		name   string
		target string
		chatID string
	}{
		{name: "missing stable instance", target: "pi-missing", chatID: "parent-1"},
		{name: "provider fallback is not a stable instance", target: "dingtalk", chatID: "parent-1"},
		{name: "instance name is not a stable instance", target: "ding-valid", chatID: "parent-1"},
		{name: "empty physical target", target: "pi-valid", chatID: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mgr, cleanup := newTestManager(t)
			defer cleanup()

			inst := &Instance{ID: "pi-valid", Name: "ding-valid", Provider: "dingtalk"}
			capable := &preparedEnvelopeCapableAdapter{
				ack: adapter.DeliveryAck{ExternalMessageID: "must-not-send", Status: adapter.DeliveryAccepted},
			}
			installPreparedEnvelopeAdapter(mgr, inst, capable)

			ack, err := mgr.SendPreparedEnvelopeWithReceipt(
				context.Background(), test.target, test.chatID, adapter.PreparedEnvelope{Parts: preparedEnvelopeParts()},
			)
			if err == nil {
				t.Fatal("非法 target/instance 必须 fail closed")
			}
			if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
				t.Fatalf("非法 target/instance 的 ack 非法: %+v", ack)
			}
			if len(capable.envelopeCalls) != 0 || len(capable.prepared) != 0 || len(capable.sent) != 0 {
				t.Fatalf(
					"非法 target/instance 不得产生副作用: envelope=%d prepared=%d sent=%d",
					len(capable.envelopeCalls), len(capable.prepared), len(capable.sent),
				)
			}
		})
	}
}

func TestManagerFrozenReceiptPathsRejectRetiredStableIDEvenWhenNewInstanceReusesItAsName(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	const retiredStableID = "pi-retired"
	capable := &strictFrozenPathAdapter{}
	installPreparedEnvelopeAdapter(mgr, &Instance{
		ID: "pi-current", Name: retiredStableID, Provider: "dingtalk",
	}, capable)
	part := preparedEnvelopeParts()[1]

	resourceID, prepareErr := mgr.PrepareDeliveryPartResource(
		context.Background(), retiredStableID, part,
	)
	sendAck, sendErr := mgr.SendWithReceipt(
		context.Background(), retiredStableID, "parent-1", &adapter.Reply{Content: "点评"},
	)
	partAck, partErr := mgr.SendPreparedPartWithReceipt(
		context.Background(), retiredStableID, "parent-1", part,
	)
	queryAck, queryErr := mgr.QueryReceipt(
		context.Background(), retiredStableID, "external-retired",
	)

	if prepareErr == nil || sendErr == nil || partErr == nil || queryErr == nil {
		t.Errorf(
			"retired stable ID must fail every frozen path: prepare=%v send=%v part=%v query=%v",
			prepareErr, sendErr, partErr, queryErr,
		)
	}
	if resourceID != "" || sendAck.Status != adapter.DeliveryFailed ||
		partAck.Status != adapter.DeliveryFailed ||
		queryAck.Status != adapter.DeliveryOutcomeUnknown ||
		queryAck.ExternalMessageID != "external-retired" {
		t.Errorf(
			"fail-closed results drifted: resource=%q send=%+v part=%+v query=%+v",
			resourceID, sendAck, partAck, queryAck,
		)
	}
	if len(capable.prepared) != 0 || capable.sendReceipts != 0 || len(capable.sent) != 0 || capable.queries != 0 {
		t.Fatalf(
			"retired stable ID reached replacement instance: prepare=%d send=%d part=%d query=%d",
			len(capable.prepared), capable.sendReceipts, len(capable.sent), capable.queries,
		)
	}
}

func TestManagerResolveRunningInstanceIDCanonicalizesUniqueProviderNameAndStableID(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	dingtalk := &preparedEnvelopeCapableAdapter{}
	slack := &preparedEnvelopeCapableAdapter{}
	installPreparedEnvelopeAdapter(mgr, &Instance{
		ID: "pi-dingtalk-main", Name: "family-dingtalk", Provider: "dingtalk",
	}, dingtalk)
	installPreparedEnvelopeAdapter(mgr, &Instance{
		ID: "pi-slack-main", Name: "family-slack", Provider: "slack",
	}, slack)

	for _, test := range []struct {
		name        string
		platform    string
		instanceRef string
	}{
		{name: "empty legacy binding", platform: "dingtalk"},
		{name: "exact instance name", platform: "dingtalk", instanceRef: "family-dingtalk"},
		{name: "stable instance id", platform: "dingtalk", instanceRef: "pi-dingtalk-main"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := mgr.ResolveRunningInstanceID(test.platform, test.instanceRef)
			if err != nil {
				t.Fatalf("解析运行实例失败: %v", err)
			}
			if got != "pi-dingtalk-main" {
				t.Fatalf("stable instance id=%q want %q", got, "pi-dingtalk-main")
			}
		})
	}
	if len(dingtalk.prepared) != 0 || len(dingtalk.sent) != 0 || len(dingtalk.envelopeCalls) != 0 ||
		len(slack.prepared) != 0 || len(slack.sent) != 0 || len(slack.envelopeCalls) != 0 {
		t.Fatal("实例身份解析必须是零 provider 副作用的只读操作")
	}
}

func TestManagerResolveRunningInstanceIDFailsClosedForAmbiguousUnknownAndCrossPlatform(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()

	first := &preparedEnvelopeCapableAdapter{}
	second := &preparedEnvelopeCapableAdapter{}
	slack := &preparedEnvelopeCapableAdapter{}
	installPreparedEnvelopeAdapter(mgr, &Instance{
		ID: "pi-dingtalk-a", Name: "dingtalk-a", Provider: "dingtalk",
	}, first)
	installPreparedEnvelopeAdapter(mgr, &Instance{
		ID: "pi-dingtalk-b", Name: "dingtalk-b", Provider: "dingtalk",
	}, second)
	installPreparedEnvelopeAdapter(mgr, &Instance{
		ID: "pi-slack", Name: "slack-main", Provider: "slack",
	}, slack)

	for _, test := range []struct {
		name        string
		platform    string
		instanceRef string
	}{
		{name: "ambiguous empty instance", platform: "dingtalk"},
		{name: "unknown instance", platform: "dingtalk", instanceRef: "missing"},
		{name: "cross platform stable id", platform: "dingtalk", instanceRef: "pi-slack"},
		{name: "cross platform instance name", platform: "dingtalk", instanceRef: "slack-main"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, err := mgr.ResolveRunningInstanceID(test.platform, test.instanceRef); err == nil {
				t.Fatalf("非法实例解析必须失败: stable=%q", got)
			}
		})
	}
	for _, capable := range []*preparedEnvelopeCapableAdapter{first, second, slack} {
		if len(capable.prepared) != 0 || len(capable.sent) != 0 || len(capable.envelopeCalls) != 0 {
			t.Fatal("失败的实例身份解析不得触发 provider")
		}
	}
}
