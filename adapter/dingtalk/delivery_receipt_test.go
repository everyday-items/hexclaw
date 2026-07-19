package dingtalk

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type receiptQueryOpenAPI struct {
	*fakeDingtalkOpenAPI
	status string
	err    error
}

func (f *receiptQueryOpenAPI) QueryOTO(_ context.Context, _, _, processQueryKey string) (string, error) {
	if processQueryKey == "" {
		return "", errors.New("missing process query key")
	}
	return f.status, f.err
}

func TestSendWithReceiptReturnsAcceptedExternalMessageID(t *testing.T) {
	a := newTestAdapter()
	fakeAPI := newFakeDingtalkOpenAPI("test-token")
	a.openAPI = fakeAPI

	ack, err := a.SendWithReceipt(context.Background(), "user-1", &adapter.Reply{Content: "辅导要点"})
	if err != nil {
		t.Fatal(err)
	}
	if ack.ExternalMessageID != "pqk-0" || ack.Status != adapter.DeliveryAccepted {
		t.Fatalf("ack=%+v, want accepted pqk-0", ack)
	}
}

func TestSendWithReceiptDoesNotCallHTTPAcceptedDelivered(t *testing.T) {
	a := newTestAdapter()
	fakeAPI := newFakeDingtalkOpenAPI("test-token")
	a.openAPI = fakeAPI

	ack, err := a.SendWithReceipt(context.Background(), "user-1", &adapter.Reply{Content: "辅导要点"})
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status == adapter.DeliveryDelivered {
		t.Fatalf("BatchSendOTO accepted must not masquerade as delivered: %+v", ack)
	}
}

func TestQueryReceiptMapsDingTalkTerminalAndUnknownStatuses(t *testing.T) {
	tests := []struct {
		name        string
		upstream    string
		upstreamErr error
		want        adapter.DeliveryStatus
		wantErr     bool
	}{
		{name: "success", upstream: "SUCCESS", want: adapter.DeliveryDelivered},
		{name: "sdk documented typo", upstream: "SUCESS", want: adapter.DeliveryDelivered},
		{name: "sending", upstream: "SENDING", want: adapter.DeliveryAccepted},
		{name: "failed", upstream: "FAILED", want: adapter.DeliveryFailed},
		{name: "unrecognized", upstream: "SOMETHING_NEW", want: adapter.DeliveryOutcomeUnknown},
		{name: "query error", upstreamErr: context.DeadlineExceeded, want: adapter.DeliveryOutcomeUnknown, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAdapter()
			a.openAPI = &receiptQueryOpenAPI{fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("test-token"), status: tt.upstream, err: tt.upstreamErr}
			got, err := a.QueryReceipt(context.Background(), "pqk-1")
			if (err != nil) != tt.wantErr || got.Status != tt.want {
				t.Fatalf("QueryReceipt status=%s err=%v, want status=%s err=%v", got.Status, err, tt.want, tt.wantErr)
			}
			if got.ExternalMessageID != "pqk-1" {
				t.Fatalf("external id lost: %+v", got)
			}
		})
	}
}
