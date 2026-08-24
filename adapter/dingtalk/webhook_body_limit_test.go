package dingtalk

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

func TestHandleWebhookBodyLimit(t *testing.T) {
	const maxWebhookBodyBytes = 1 << 20

	tests := []struct {
		name       string
		bodySize   int
		wantStatus int
	}{
		{
			name:       "exactly one MiB is accepted",
			bodySize:   maxWebhookBodyBytes,
			wantStatus: http.StatusOK,
		},
		{
			name:       "one byte over one MiB is rejected",
			bodySize:   maxWebhookBodyBytes + 1,
			wantStatus: http.StatusRequestEntityTooLarge,
		},
	}

	a := New(config.DingtalkConfig{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := validDingtalkWebhookBodyOfSize(t, tt.bodySize)
			req := httptest.NewRequest(http.MethodPost, "/webhook/dingtalk", bytes.NewReader(body))
			rec := httptest.NewRecorder()

			a.handleWebhook(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("body size %d: status = %d, want %d", tt.bodySize, rec.Code, tt.wantStatus)
			}
		})
	}
}

func validDingtalkWebhookBodyOfSize(t *testing.T, size int) []byte {
	t.Helper()

	prefix := []byte(`{"msgId":"provider-msg-body-limit-1","padding":"`)
	suffix := []byte(`"}`)
	paddingSize := size - len(prefix) - len(suffix)
	if paddingSize < 0 {
		t.Fatalf("requested body size %d is smaller than JSON framing", size)
	}

	body := make([]byte, 0, size)
	body = append(body, prefix...)
	body = append(body, bytes.Repeat([]byte("a"), paddingSize)...)
	body = append(body, suffix...)
	if len(body) != size {
		t.Fatalf("body size = %d, want %d", len(body), size)
	}
	return body
}
