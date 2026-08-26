package dingtalk

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type preSendTimeoutOpenAPI struct {
	*fakeDingtalkOpenAPI
	uploadCalls int
	uploadRef   string
	uploadErr   error
	afterUpload func()
	waitForDone bool
}

func (f *preSendTimeoutOpenAPI) UploadImage(
	ctx context.Context,
	_ string,
	_ adapter.Attachment,
) (string, error) {
	f.uploadCalls++
	if f.waitForDone {
		<-ctx.Done()
	}
	if f.afterUpload != nil {
		f.afterUpload()
	}
	return f.uploadRef, f.uploadErr
}

func newDirectReceiptTestAdapter(t *testing.T) *DingtalkAdapter {
	t.Helper()
	client := newTestAdapter()
	if err := client.queue.Stop(context.Background()); err != nil {
		t.Fatalf("stop test send queue: %v", err)
	}
	client.queue = nil
	t.Cleanup(client.workerCancel)
	return client
}

func assertDefinitePreSendFailure(
	t *testing.T,
	ack adapter.DeliveryAck,
	err error,
	wantErr error,
	base *fakeDingtalkOpenAPI,
) {
	t.Helper()
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v, want %v", err, wantErr)
	}
	if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
		t.Fatalf("pre-send ack=%+v, want failed without external id", ack)
	}
	if calls := base.SendCalls(); len(calls) != 0 {
		t.Fatalf("provider send must not start after pre-send failure: %#v", calls)
	}
}

func imageReplyForPreSendStage(t *testing.T) *adapter.Reply {
	t.Helper()
	return &adapter.Reply{
		Content: "## 作品与点评",
		Attachments: []adapter.Attachment{{
			Type: "image",
			Name: "creative-work.png",
			Mime: "image/png",
			Data: base64.StdEncoding.EncodeToString(testPNGBytes(t)),
		}},
	}
}

func TestSendWithReceiptPreSendTokenContextFailuresAreFailed(t *testing.T) {
	for _, stageErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(stageErr.Error(), func(t *testing.T) {
			base := newFakeDingtalkOpenAPI("test-token")
			base.tokenErr = stageErr
			client := newDirectReceiptTestAdapter(t)
			client.openAPI = base

			ack, err := client.SendWithReceipt(context.Background(), "user-1", &adapter.Reply{Content: "辅导要点"})

			assertDefinitePreSendFailure(t, ack, err, stageErr, base)
			if calls := base.TokenCalls(); calls != 1 {
				t.Fatalf("token calls=%d, want 1", calls)
			}
		})
	}
}

func TestSendWithReceiptNilReplyIsDefinitePreSendFailure(t *testing.T) {
	base := newFakeDingtalkOpenAPI("test-token")
	client := newDirectReceiptTestAdapter(t)
	client.openAPI = base

	ack, err := client.SendWithReceipt(context.Background(), "user-1", nil)

	if err == nil {
		t.Fatal("nil reply must fail before provider send")
	}
	if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
		t.Fatalf("nil reply ack=%+v, want failed without external id", ack)
	}
	if calls := base.TokenCalls(); calls != 0 {
		t.Fatalf("token calls=%d, want 0", calls)
	}
	if calls := base.SendCalls(); len(calls) != 0 {
		t.Fatalf("provider send must not start for nil reply: %#v", calls)
	}
}

func TestSendWithReceiptPreSendAttachmentDeadlineIsFailed(t *testing.T) {
	base := newFakeDingtalkOpenAPI("test-token")
	openAPI := &preSendTimeoutOpenAPI{
		fakeDingtalkOpenAPI: base,
		uploadErr:           context.DeadlineExceeded,
	}
	client := newDirectReceiptTestAdapter(t)
	client.openAPI = openAPI

	ack, err := client.SendWithReceipt(context.Background(), "user-1", imageReplyForPreSendStage(t))

	assertDefinitePreSendFailure(t, ack, err, context.DeadlineExceeded, base)
	if openAPI.uploadCalls != 1 {
		t.Fatalf("upload calls=%d, want 1", openAPI.uploadCalls)
	}
	if calls := base.TokenCalls(); calls != 1 {
		t.Fatalf("token calls=%d, want 1", calls)
	}
}

func TestSendWithReceiptPreSendAttachmentCancellationIsFailed(t *testing.T) {
	base := newFakeDingtalkOpenAPI("test-token")
	openAPI := &preSendTimeoutOpenAPI{
		fakeDingtalkOpenAPI: base,
		uploadErr:           context.Canceled,
	}
	client := newDirectReceiptTestAdapter(t)
	client.openAPI = openAPI

	ack, err := client.SendWithReceipt(context.Background(), "user-1", imageReplyForPreSendStage(t))

	assertDefinitePreSendFailure(t, ack, err, context.Canceled, base)
	if openAPI.uploadCalls != 1 {
		t.Fatalf("upload calls=%d, want 1", openAPI.uploadCalls)
	}
}

func TestSendWithReceiptPreSendConstructedMessageContextFailuresAreFailed(t *testing.T) {
	for _, stageErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(stageErr.Error(), func(t *testing.T) {
			var stageCtx context.Context
			var cancel context.CancelFunc
			base := newFakeDingtalkOpenAPI("test-token")
			openAPI := &preSendTimeoutOpenAPI{
				fakeDingtalkOpenAPI: base,
				uploadRef:           "@media-accepted-before-send",
			}
			if errors.Is(stageErr, context.Canceled) {
				stageCtx, cancel = context.WithCancel(context.Background())
				openAPI.afterUpload = cancel
			} else {
				stageCtx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
				openAPI.waitForDone = true
			}
			defer cancel()
			client := newDirectReceiptTestAdapter(t)
			client.openAPI = openAPI

			ack, err := client.SendWithReceipt(stageCtx, "user-1", imageReplyForPreSendStage(t))

			assertDefinitePreSendFailure(t, ack, err, stageErr, base)
			if openAPI.uploadCalls != 1 {
				t.Fatalf("upload calls=%d, want 1", openAPI.uploadCalls)
			}
		})
	}
}

func TestSendWithReceiptPreSendMessageConstructionFailureIsFailed(t *testing.T) {
	base := newFakeDingtalkOpenAPI("test-token")
	client := newDirectReceiptTestAdapter(t)
	client.openAPI = base

	ack, err := client.SendWithReceipt(context.Background(), "user-1", &adapter.Reply{
		Content: "## 作品与点评",
		Attachments: []adapter.Attachment{{
			Type: "image",
			Name: "creative-work.png",
			Mime: "image/png",
			URL:  "asset://owner-scoped/internal.png",
		}},
	})

	if err == nil {
		t.Fatal("invalid image reference must fail during message construction")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("construction error=%v, want ordinary pre-send failure", err)
	}
	if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
		t.Fatalf("construction failure ack=%+v, want failed without external id", ack)
	}
	if calls := base.SendCalls(); len(calls) != 0 {
		t.Fatalf("provider send must not start after construction failure: %#v", calls)
	}
}
