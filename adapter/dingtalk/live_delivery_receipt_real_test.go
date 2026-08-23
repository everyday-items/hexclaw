package dingtalk

// 钉钉真实投递回执门默认跳过。只有发送开关、用途确认、精确实例和精确用户
// 全部显式给出时，才会发送一条消息；发送后只轮询该消息的回执，绝不重发。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestLiveDeliveryReceipt_RealSendAndQuery(t *testing.T) {
	if strings.TrimSpace(os.Getenv("DINGTALK_LIVE_DELIVERY_RECEIPT")) != "1" {
		t.Skip("real DingTalk delivery receipt test is disabled")
	}

	cfg, userID := loadLiveDingtalkConfig(t)
	adp := New(cfg)
	startedAt := time.Now()

	sendCtx, cancelSend := context.WithTimeout(context.Background(), 20*time.Second)
	ack, err := adp.SendWithReceipt(sendCtx, userID, &adapter.Reply{
		Content: "HexClaw delivery receipt verification.",
	})
	cancelSend()
	if err != nil || ack.Status != adapter.DeliveryAccepted || strings.TrimSpace(ack.ExternalMessageID) == "" {
		logLiveDeliveryReceiptResult(t, ack.Status, startedAt)
		t.FailNow()
	}

	deadline := startedAt.Add(90 * time.Second)
	finalStatus := adapter.DeliveryAccepted
	for time.Now().Before(deadline) {
		queryCtx, cancelQuery := context.WithTimeout(context.Background(), 10*time.Second)
		queried, queryErr := adp.QueryReceipt(queryCtx, ack.ExternalMessageID)
		cancelQuery()
		if queryErr == nil {
			finalStatus = queried.Status
			switch queried.Status {
			case adapter.DeliveryDelivered:
				logLiveDeliveryReceiptResult(t, queried.Status, startedAt)
				return
			case adapter.DeliveryFailed:
				logLiveDeliveryReceiptResult(t, queried.Status, startedAt)
				t.FailNow()
			}
		} else {
			finalStatus = adapter.DeliveryOutcomeUnknown
		}
		time.Sleep(2 * time.Second)
	}

	logLiveDeliveryReceiptResult(t, finalStatus, startedAt)
	t.FailNow()
}

func logLiveDeliveryReceiptResult(t *testing.T, status adapter.DeliveryStatus, startedAt time.Time) {
	t.Helper()
	t.Logf("status=%s elapsed=%s", status, time.Since(startedAt).Round(time.Millisecond))
}
