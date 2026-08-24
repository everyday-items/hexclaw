package dingtalk

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestSendWithReceiptPostSendContextFailuresAreOutcomeUnknown(t *testing.T) {
	for _, stageErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(stageErr.Error(), func(t *testing.T) {
			openAPI := newFakeDingtalkOpenAPI("test-token")
			openAPI.sendErr = stageErr
			client := newTestAdapter()
			client.openAPI = openAPI
			t.Cleanup(func() {
				if err := client.Stop(context.Background()); err != nil {
					t.Errorf("stop adapter: %v", err)
				}
			})

			ack, err := client.SendWithReceipt(context.Background(), "user-1", &adapter.Reply{Content: "辅导要点"})

			if !errors.Is(err, stageErr) {
				t.Fatalf("error=%v, want %v", err, stageErr)
			}
			if ack.Status != adapter.DeliveryOutcomeUnknown || ack.ExternalMessageID != "" {
				t.Fatalf("post-send ack=%+v, want outcome_unknown without external id", ack)
			}
			if calls := openAPI.SendCalls(); len(calls) != 1 {
				t.Fatalf("provider send calls=%d, want 1", len(calls))
			}
		})
	}
}
