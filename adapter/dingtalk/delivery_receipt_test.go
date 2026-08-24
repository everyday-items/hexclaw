package dingtalk

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type receiptQueryOpenAPI struct {
	*fakeDingtalkOpenAPI
	status string
	err    error
}

type receiptMediaOpenAPI struct {
	*fakeDingtalkOpenAPI
	uploadRef string
	uploadErr error
	uploads   []adapter.Attachment
}

func (f *receiptMediaOpenAPI) UploadImage(
	_ context.Context,
	_ string,
	attachment adapter.Attachment,
) (string, error) {
	f.uploads = append(f.uploads, attachment)
	return f.uploadRef, f.uploadErr
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

func TestSendWithReceiptImageUploadFailureIsFailClosed(t *testing.T) {
	imageReply := func() *adapter.Reply {
		return &adapter.Reply{
			Content: "## 作品与点评",
			Attachments: []adapter.Attachment{{
				Type: "image", Name: "creative-work.png", Mime: "image/png",
				Data: base64.StdEncoding.EncodeToString([]byte("creative-work-png")),
			}},
		}
	}
	tests := []struct {
		name        string
		openAPI     dingtalkOpenAPI
		base        *fakeDingtalkOpenAPI
		wantUploads int
	}{
		{
			name: "media upload capability is unavailable",
			base: newFakeDingtalkOpenAPI("test-token"),
		},
		{
			name: "media upload returns an error",
			base: newFakeDingtalkOpenAPI("test-token"),
		},
		{
			name: "media upload returns an invalid reference",
			base: newFakeDingtalkOpenAPI("test-token"),
		},
	}
	tests[0].openAPI = tests[0].base
	tests[1].openAPI = &receiptMediaOpenAPI{
		fakeDingtalkOpenAPI: tests[1].base,
		uploadErr:           errors.New("upload rejected"),
	}
	tests[1].wantUploads = 1
	tests[2].openAPI = &receiptMediaOpenAPI{
		fakeDingtalkOpenAPI: tests[2].base,
		uploadRef:           "not-a-dingtalk-media-reference",
	}
	tests[2].wantUploads = 1

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newTestAdapter()
			a.openAPI = tt.openAPI

			ack, err := a.SendWithReceipt(context.Background(), "user-1", imageReply())
			if err == nil {
				t.Fatalf("image delivery must fail before provider send: ack=%+v", ack)
			}
			if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
				t.Fatalf("failed image delivery ack=%+v, want failed without external id", ack)
			}
			if calls := tt.base.SendCalls(); len(calls) != 0 {
				t.Fatalf("attachment failure must not degrade to text-only send: %#v", calls)
			}
			if media, ok := tt.openAPI.(*receiptMediaOpenAPI); ok {
				if len(media.uploads) != tt.wantUploads {
					t.Fatalf("upload calls=%d want %d", len(media.uploads), tt.wantUploads)
				}
			}
		})
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
