package dingtalk

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	dtrobot "github.com/alibabacloud-go/dingtalk/robot_1_0"
	util "github.com/alibabacloud-go/tea-utils/v2/service"
	"github.com/alibabacloud-go/tea/tea"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type dingTalkReceiptSendVariant struct {
	name string
	send func(*testing.T, *DingtalkAdapter) (adapter.DeliveryAck, error)
}

func dingTalkReceiptSendVariants() []dingTalkReceiptSendVariant {
	return []dingTalkReceiptSendVariant{
		{
			name: "reply",
			send: func(_ *testing.T, client *DingtalkAdapter) (adapter.DeliveryAck, error) {
				return client.SendWithReceipt(
					context.Background(),
					"parent-user",
					&adapter.Reply{Content: "## 辅导要点"},
				)
			},
		},
		{
			name: "prepared part",
			send: func(t *testing.T, client *DingtalkAdapter) (adapter.DeliveryAck, error) {
				part := canonicalDingTalkDeliveryPartsForTest(t, "## 本周练习")[0]
				return client.SendPreparedPartWithReceipt(context.Background(), "parent-user", part)
			},
		},
		{
			name: "prepared envelope",
			send: func(t *testing.T, client *DingtalkAdapter) (adapter.DeliveryAck, error) {
				attachment := adapter.Attachment{
					Type: "image",
					Name: "creative-work.png",
					Mime: "image/png",
					Data: base64.StdEncoding.EncodeToString(testPNGBytes(t)),
				}
				parts := canonicalDingTalkDeliveryPartsForTest(t, "## 作品点评", attachment)
				parts[1].PreparedResourceID = "@prepared-image"
				return client.SendPreparedEnvelopeWithReceipt(
					context.Background(),
					"parent-user",
					adapter.PreparedEnvelope{Parts: parts},
				)
			},
		},
	}
}

func TestDingTalkReceiptSendVariantsAmbiguousPostSendErrorsAreOutcomeUnknown(t *testing.T) {
	ambiguousErrors := []struct {
		name string
		err  error
	}{
		{name: "EOF", err: io.EOF},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF},
		{name: "connection reset", err: fmt.Errorf("write response: %w", syscall.ECONNRESET)},
		{
			name: "network timeout",
			err: &url.Error{
				Op:  "Post",
				URL: "https://api.dingtalk.com/v1.0/robot/oToMessages/batchSend",
				Err: context.DeadlineExceeded,
			},
		},
		{name: "canceled", err: context.Canceled},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
		{name: "SDK response decode loss", err: errors.New("invalid response body")},
		{
			name: "SDK error without rejection status",
			err: &tea.SDKError{
				StatusCode: tea.Int(http.StatusOK),
				Message:    tea.String("response decode failed"),
			},
		},
	}

	for _, variant := range dingTalkReceiptSendVariants() {
		for _, test := range ambiguousErrors {
			t.Run(variant.name+"/"+test.name, func(t *testing.T) {
				provider := newFakeDingtalkOpenAPI("bound-instance-token")
				provider.sendErr = test.err
				client := newDirectReceiptTestAdapter(t)
				client.openAPI = provider

				ack, err := variant.send(t, client)

				if !errors.Is(err, test.err) {
					t.Fatalf("error=%v, want provider error %v", err, test.err)
				}
				if ack.Status != adapter.DeliveryOutcomeUnknown || ack.ExternalMessageID != "" {
					t.Fatalf("post-send ack=%+v, want outcome_unknown without external id", ack)
				}
				if calls := provider.SendCalls(); len(calls) != 1 {
					t.Fatalf("provider send calls=%d, want 1", len(calls))
				}
			})
		}
	}
}

type definiteDingTalkProviderRejectionForTest struct {
	cause error
}

func (e *definiteDingTalkProviderRejectionForTest) Error() string {
	return e.cause.Error()
}

func (e *definiteDingTalkProviderRejectionForTest) Unwrap() error {
	return e.cause
}

func (*definiteDingTalkProviderRejectionForTest) definiteDingTalkProviderRejection() {}

func TestDingTalkReceiptSendVariantsDefiniteProviderRejectionsAreFailed(t *testing.T) {
	providerErr := &definiteDingTalkProviderRejectionForTest{cause: errors.New("provider rejected request")}
	for _, variant := range dingTalkReceiptSendVariants() {
		t.Run(variant.name, func(t *testing.T) {
			provider := newFakeDingtalkOpenAPI("bound-instance-token")
			provider.sendErr = providerErr
			client := newDirectReceiptTestAdapter(t)
			client.openAPI = provider

			ack, err := variant.send(t, client)

			if !errors.Is(err, providerErr) {
				t.Fatalf("error=%v, want provider rejection %v", err, providerErr)
			}
			if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
				t.Fatalf("rejected ack=%+v, want failed without external id", ack)
			}
			if calls := provider.SendCalls(); len(calls) != 1 {
				t.Fatalf("provider send calls=%d, want 1", len(calls))
			}
		})
	}
}

func TestDingTalkReceiptSendVariantsPreSendTokenFailuresAreFailed(t *testing.T) {
	for _, variant := range dingTalkReceiptSendVariants() {
		t.Run(variant.name, func(t *testing.T) {
			providerErr := errors.New("token unavailable")
			provider := newFakeDingtalkOpenAPI("bound-instance-token")
			provider.tokenErr = providerErr
			client := newDirectReceiptTestAdapter(t)
			client.openAPI = provider

			ack, err := variant.send(t, client)

			if !errors.Is(err, providerErr) {
				t.Fatalf("error=%v, want token error %v", err, providerErr)
			}
			if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
				t.Fatalf("pre-send ack=%+v, want failed without external id", ack)
			}
			if calls := provider.SendCalls(); len(calls) != 0 {
				t.Fatalf("provider send must not start after token failure: %#v", calls)
			}
		})
	}
}

type blockingDingTalkReceiptOpenAPI struct {
	*fakeDingtalkOpenAPI
	started chan struct{}
	release chan struct{}
}

func (f *blockingDingTalkReceiptOpenAPI) SendOTO(
	_ context.Context,
	_, _, _ string,
	_ dingtalkOutboundMessage,
) (string, error) {
	close(f.started)
	<-f.release
	return "pqk-after-queue-stop", nil
}

func TestSendWithReceiptQueueStopAfterProviderBoundaryIsOutcomeUnknown(t *testing.T) {
	provider := &blockingDingTalkReceiptOpenAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("bound-instance-token"),
		started:             make(chan struct{}),
		release:             make(chan struct{}),
	}
	client := newTestAdapter()
	client.openAPI = provider
	t.Cleanup(client.workerCancel)

	type result struct {
		ack adapter.DeliveryAck
		err error
	}
	sendDone := make(chan result, 1)
	go func() {
		ack, err := client.SendWithReceipt(
			context.Background(),
			"parent-user",
			&adapter.Reply{Content: "## 辅导要点"},
		)
		sendDone <- result{ack: ack, err: err}
	}()

	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider SendOTO did not start")
	}
	stopDone := make(chan error, 1)
	go func() {
		stopDone <- client.queue.Stop(context.Background())
	}()

	var got result
	select {
	case got = <-sendDone:
	case <-time.After(2 * time.Second):
		close(provider.release)
		t.Fatal("receipt send did not return after queue stop")
	}
	close(provider.release)
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("stop queue: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queue stop did not converge after provider release")
	}

	if got.err == nil {
		t.Fatal("queue response loss after provider start must return an error")
	}
	if got.ack.Status != adapter.DeliveryOutcomeUnknown || got.ack.ExternalMessageID != "" {
		t.Fatalf("queue-stop ack=%+v, want outcome_unknown without external id", got.ack)
	}
}

type definiteDingTalkProviderRejectionMarker interface {
	definiteDingTalkProviderRejection()
}

func TestOfficialDingTalkSendOTOClassifiesProviderResponseEvidence(t *testing.T) {
	tests := []struct {
		name         string
		statusCode   int
		body         string
		wantDefinite bool
	}{
		{
			name:         "HTTP 400",
			statusCode:   http.StatusBadRequest,
			body:         `{"code":"BadRequest","message":"request rejected","requestid":"req-400"}`,
			wantDefinite: true,
		},
		{
			name:         "HTTP 401",
			statusCode:   http.StatusUnauthorized,
			body:         `{"code":"Unauthorized","message":"request rejected","requestid":"req-401"}`,
			wantDefinite: true,
		},
		{
			name:         "HTTP 403",
			statusCode:   http.StatusForbidden,
			body:         `{"code":"Forbidden","message":"request rejected","requestid":"req-403"}`,
			wantDefinite: true,
		},
		{
			name:         "HTTP 404",
			statusCode:   http.StatusNotFound,
			body:         `{"code":"NotFound","message":"request rejected","requestid":"req-404"}`,
			wantDefinite: true,
		},
		{
			name:         "HTTP 409",
			statusCode:   http.StatusConflict,
			body:         `{"code":"Conflict","message":"request rejected","requestid":"req-409"}`,
			wantDefinite: true,
		},
		{
			name:         "HTTP 422",
			statusCode:   http.StatusUnprocessableEntity,
			body:         `{"code":"UnprocessableEntity","message":"request rejected","requestid":"req-422"}`,
			wantDefinite: true,
		},
		{
			name:         "HTTP 429",
			statusCode:   http.StatusTooManyRequests,
			body:         `{"code":"TooManyRequests","message":"rate limited","requestid":"req-429"}`,
			wantDefinite: true,
		},
		{
			name:       "HTTP 408",
			statusCode: http.StatusRequestTimeout,
			body:       `{"code":"RequestTimeout","message":"response timed out","requestid":"req-408"}`,
		},
		{
			name:       "HTTP 500",
			statusCode: http.StatusInternalServerError,
			body:       `{"code":"InternalServerError","message":"server failed","requestid":"req-500"}`,
		},
		{
			name:       "HTTP 502",
			statusCode: http.StatusBadGateway,
			body:       `{"code":"BadGateway","message":"gateway failed","requestid":"req-502"}`,
		},
		{
			name:       "HTTP 503",
			statusCode: http.StatusServiceUnavailable,
			body:       `{"code":"ServiceUnavailable","message":"service unavailable","requestid":"req-503"}`,
		},
		{
			name:       "HTTP 504",
			statusCode: http.StatusGatewayTimeout,
			body:       `{"code":"GatewayTimeout","message":"gateway timed out","requestid":"req-504"}`,
		},
		{
			name:       "HTTP 200 malformed response",
			statusCode: http.StatusOK,
			body:       `{not-json`,
		},
		{
			name:         "invalid staff ID",
			statusCode:   http.StatusOK,
			body:         `{"invalidStaffIdList":["parent-user"]}`,
			wantDefinite: true,
		},
		{
			name:         "filtered staff ID",
			statusCode:   http.StatusOK,
			body:         `{"filteredStaffIdList":["parent-user"]}`,
			wantDefinite: true,
		},
		{
			name:         "flow-controlled staff ID",
			statusCode:   http.StatusOK,
			body:         `{"flowControlledStaffIdList":["parent-user"]}`,
			wantDefinite: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/v1.0/robot/oToMessages/batchSend" {
					t.Errorf("request path=%q", request.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)
			endpoint := strings.TrimPrefix(server.URL, "http://")
			robot, err := dtrobot.NewClient(&openapi.Config{
				Protocol: tea.String("http"),
				Endpoint: tea.String(endpoint),
			})
			if err != nil {
				t.Fatalf("initialize official SDK client: %v", err)
			}
			client := &officialDingtalkOpenAPI{
				robot: robot,
				runtime: &util.RuntimeOptions{
					Autoretry: tea.Bool(false),
				},
			}

			_, err = client.SendOTO(
				context.Background(),
				"bound-instance-token",
				"test-robot-code",
				"parent-user",
				dingtalkMarkdownMessage("## 辅导要点"),
			)
			if err == nil {
				t.Fatal("non-accepted provider response must return an error")
			}
			var marker definiteDingTalkProviderRejectionMarker
			gotDefinite := errors.As(err, &marker)
			if gotDefinite != test.wantDefinite {
				t.Fatalf(
					"error type=%T definite=%v, want definite=%v: %v",
					err,
					gotDefinite,
					test.wantDefinite,
					err,
				)
			}
		})
	}
}
