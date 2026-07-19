package instances

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type receiptCapableAdapter struct{ stubAdapter }

func (a *receiptCapableAdapter) SendWithReceipt(_ context.Context, chatID string, _ *adapter.Reply) (adapter.DeliveryAck, error) {
	return adapter.DeliveryAck{ExternalMessageID: "external:" + chatID, Status: adapter.DeliveryAccepted}, nil
}

func (a *receiptCapableAdapter) QueryReceipt(_ context.Context, externalID string) (adapter.DeliveryAck, error) {
	return adapter.DeliveryAck{ExternalMessageID: externalID, Status: adapter.DeliveryDelivered}, nil
}

func TestManagerRoutesDeliveryReceiptCapabilityByStableInstanceID(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	inst := &Instance{ID: "pi-receipt", Name: "ding-main", Provider: "dingtalk"}
	mgr.mu.Lock()
	mgr.running[inst.Name] = &receiptCapableAdapter{}
	mgr.metadata[inst.Name] = inst
	mgr.mu.Unlock()

	ack, err := mgr.SendWithReceipt(context.Background(), inst.ID, "user-1", &adapter.Reply{Content: "辅导要点"})
	if err != nil || ack.Status != adapter.DeliveryAccepted || ack.ExternalMessageID != "external:user-1" {
		t.Fatalf("send ack=%+v err=%v", ack, err)
	}
	terminal, err := mgr.QueryReceipt(context.Background(), inst.ID, ack.ExternalMessageID)
	if err != nil || terminal.Status != adapter.DeliveryDelivered {
		t.Fatalf("query ack=%+v err=%v", terminal, err)
	}
}

func TestManagerFailsClosedWhenAdapterHasNoReceiptCapability(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	mgr.mu.Lock()
	mgr.running["slack-main"] = &stubAdapter{}
	mgr.metadata["slack-main"] = &Instance{ID: "pi-slack", Name: "slack-main", Provider: "slack"}
	mgr.mu.Unlock()

	if _, err := mgr.SendWithReceipt(context.Background(), "pi-slack", "user-1", &adapter.Reply{Content: "x"}); err == nil {
		t.Fatal("adapter without receipt capability must not be reported as delivered")
	}
}
