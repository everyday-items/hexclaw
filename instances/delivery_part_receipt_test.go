package instances

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/messagecontent"
)

type deliveryPartCapableAdapter struct {
	receiptCapableAdapter
	prepared []adapter.DeliveryPart
	sent     []adapter.DeliveryPart
}

func (a *deliveryPartCapableAdapter) PrepareDeliveryPartResource(
	_ context.Context,
	part adapter.DeliveryPart,
) (string, error) {
	a.prepared = append(a.prepared, part)
	return "@media-" + part.MIME, nil
}

func (a *deliveryPartCapableAdapter) SendPreparedPartWithReceipt(
	_ context.Context,
	chatID string,
	part adapter.DeliveryPart,
) (adapter.DeliveryAck, error) {
	a.sent = append(a.sent, part)
	return adapter.DeliveryAck{
		ExternalMessageID: "external-part:" + chatID,
		Status:            adapter.DeliveryAccepted,
	}, nil
}

func TestManagerRoutesDeliveryPartCapabilityByStableInstanceID(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	inst := &Instance{ID: "pi-part", Name: "ding-main", Provider: "dingtalk"}
	capable := &deliveryPartCapableAdapter{}
	mgr.mu.Lock()
	mgr.running[inst.Name] = capable
	mgr.metadata[inst.Name] = inst
	mgr.mu.Unlock()

	part := adapter.DeliveryPart{
		Kind:       messagecontent.PartArtifact,
		MIME:       "application/pdf",
		Ordinal:    2,
		Digest:     "sha256:part",
		Attachment: &adapter.Attachment{Type: "file", Name: "practice.pdf", Mime: "application/pdf", Data: "cGRm"},
	}
	resourceID, err := mgr.PrepareDeliveryPartResource(context.Background(), inst.ID, part)
	if err != nil || resourceID != "@media-application/pdf" || len(capable.prepared) != 1 {
		t.Fatalf("prepare resource=%q calls=%d err=%v", resourceID, len(capable.prepared), err)
	}
	part.PreparedResourceID = resourceID
	ack, err := mgr.SendPreparedPartWithReceipt(context.Background(), inst.ID, "parent-1", part)
	if err != nil || ack.Status != adapter.DeliveryAccepted || ack.ExternalMessageID != "external-part:parent-1" || len(capable.sent) != 1 {
		t.Fatalf("send ack=%+v calls=%d err=%v", ack, len(capable.sent), err)
	}
}

func TestManagerDeliveryPartCapabilityFailsClosedWithoutAdapterSupport(t *testing.T) {
	mgr, cleanup := newTestManager(t)
	defer cleanup()
	mgr.mu.Lock()
	mgr.running["receipt-only"] = &receiptCapableAdapter{}
	mgr.metadata["receipt-only"] = &Instance{ID: "pi-receipt-only", Name: "receipt-only", Provider: "dingtalk"}
	mgr.mu.Unlock()

	part := adapter.DeliveryPart{Kind: messagecontent.PartArtifact, MIME: "image/png"}
	if _, err := mgr.PrepareDeliveryPartResource(context.Background(), "pi-receipt-only", part); err == nil {
		t.Fatal("adapter without part preparation capability must fail closed")
	}
	if _, err := mgr.SendPreparedPartWithReceipt(context.Background(), "pi-receipt-only", "parent-1", part); err == nil {
		t.Fatal("adapter without part send capability must fail closed")
	}
}
