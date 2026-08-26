package dingtalk

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestDingTalkPreparedMarkdownRejectsInternalReferenceBeforeProviderBoundary(t *testing.T) {
	references := []struct {
		name  string
		value string
	}{
		{name: "asset URI", value: "asset://child/creative-work.png"},
		{name: "file URI", value: "file:///private/tmp/creative-work.png"},
		{name: "POSIX path", value: "/private/tmp/creative-work.png"},
		{name: "Windows path", value: `C:\Users\private\creative-work.png`},
		{name: "protected asset URL", value: "http://127.0.0.1:16060/api/k12/assets/internal.png"},
		{name: "blob URL", value: "blob:https://desktop.invalid/internal-image"},
		{name: "data URL", value: "data:image/png;base64,aW50ZXJuYWw="},
	}

	for _, reference := range references {
		t.Run(reference.name, func(t *testing.T) {
			api := newFakeDingtalkOpenAPI("bound-instance-token")
			client := newTestAdapter()
			client.queue = nil
			client.openAPI = api
			part := canonicalDingTalkDeliveryPartsForTest(
				t,
				"## 作品点评\n\n原图："+reference.value,
			)[0]

			ack, err := client.SendPreparedPartWithReceipt(context.Background(), "parent-user", part)
			if err == nil {
				t.Fatalf("internal reference reached visible prepared Markdown: ack=%+v", ack)
			}
			if ack.Status != adapter.DeliveryFailed || ack.ExternalMessageID != "" {
				t.Fatalf("ack=%+v, want failed without external message ID", ack)
			}
			if api.TokenCalls() != 0 || len(api.SendCalls()) != 0 {
				t.Fatalf(
					"internal reference caused provider side effects: token=%d send=%d",
					api.TokenCalls(), len(api.SendCalls()),
				)
			}
		})
	}
}

func TestDingTalkReplyAttachmentFieldsCannotBecomeVisibleMedia(t *testing.T) {
	validImage := base64.StdEncoding.EncodeToString(testPNGBytes(t))
	tests := []struct {
		name       string
		attachment adapter.Attachment
	}{
		{
			name: "uncontrolled URL",
			attachment: adapter.Attachment{
				Type: "image", Name: "creative-work.png", Mime: "image/png",
				Data: validImage, URL: "https://internal.invalid/creative-work.png",
			},
		},
		{
			name: "local path filename",
			attachment: adapter.Attachment{
				Type: "image", Name: "/private/tmp/creative-work.png", Mime: "image/png",
				Data: validImage,
			},
		},
		{
			name: "unsupported attachment",
			attachment: adapter.Attachment{
				Type: "file", Name: "private-notes.txt", Mime: "text/plain",
				Data: base64.StdEncoding.EncodeToString([]byte("private notes")),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newFakeDingtalkOpenAPI("bound-instance-token")
			client := newTestAdapter()
			client.queue = nil
			client.openAPI = api

			err := client.Send(context.Background(), "parent-user", &adapter.Reply{
				Content:     "## 作品点评",
				Attachments: []adapter.Attachment{test.attachment},
			})
			if err == nil {
				t.Fatal("attachment bypass must fail before visible delivery")
			}
			if api.TokenCalls() != 0 || len(api.SendCalls()) != 0 {
				t.Fatalf(
					"attachment bypass caused provider side effects: token=%d send=%d",
					api.TokenCalls(), len(api.SendCalls()),
				)
			}
		})
	}
}
