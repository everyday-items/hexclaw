package dingtalk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type fakeGroupSendCall struct {
	ConversationID string
	Message        dingtalkOutboundMessage
}

type fakeConversationOpenAPI struct {
	*fakeDingtalkOpenAPI
	mu           sync.Mutex
	groupSends   []fakeGroupSendCall
	groupRecalls [][]string
	uploads      []adapter.Attachment
}

func newFakeConversationOpenAPI() *fakeConversationOpenAPI {
	return &fakeConversationOpenAPI{fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token")}
}

func (f *fakeConversationOpenAPI) SendGroup(_ context.Context, _, _ string, conversationID string, msg dingtalkOutboundMessage) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := "group-key-" + string(rune('0'+len(f.groupSends)))
	f.groupSends = append(f.groupSends, fakeGroupSendCall{ConversationID: conversationID, Message: msg})
	return key, nil
}

func (f *fakeConversationOpenAPI) RecallGroup(_ context.Context, _, _, _ string, keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupRecalls = append(f.groupRecalls, append([]string(nil), keys...))
	return nil
}

func (f *fakeConversationOpenAPI) UploadImage(_ context.Context, _ string, att adapter.Attachment) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads = append(f.uploads, att)
	return "@media-corrected-homework", nil
}

func TestDingTalkV05GroupPhotoDoesNotCallOutboundProvider(t *testing.T) {
	a := newTestAdapter()
	fake := newFakeConversationOpenAPI()
	a.openAPI = fake
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{
			Content: "## 作业批改完成\n\n正确 8 题，需订正 1 题。",
			Attachments: []adapter.Attachment{{
				Type: "image", Name: "graded-homework.png", Mime: "image/png",
				Data: base64.StdEncoding.EncodeToString([]byte("png-bytes")),
			}},
		}, nil
	}

	event := dtEvent{ConversationId: "cid-family-group", ConversationType: "2", SenderStaffId: "parent-user", MsgType: dtMsgTypePicture}
	event.Text.Content = "请批改"
	a.handleMessage(event)

	if got := fake.SendCalls(); len(got) != 0 {
		t.Fatalf("v0.5 group event must not call SendOTO: %#v", got)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.groupSends) != 0 {
		t.Fatalf("v0.5 group event must not call SendGroup: %#v", fake.groupSends)
	}
	if len(fake.uploads) != 0 {
		t.Fatalf("v0.5 group event must not call UploadImage: %#v", fake.uploads)
	}
	if len(fake.groupRecalls) != 0 {
		t.Fatalf("v0.5 group event must not call RecallGroup: %#v", fake.groupRecalls)
	}
}

func TestBUG20260714_PhotoTaskUsesLongBudgetAndUsefulETA(t *testing.T) {
	a := newTestAdapter()
	if got := a.messageHandlerTimeoutFor(dtEvent{MsgType: dtMsgTypePicture}); got < 10*time.Minute {
		t.Fatalf("photo grading budget = %s, want >= 10m", got)
	}
	text := thinkingFeedbackForEvent(dtEvent{MsgType: dtMsgTypePicture})
	for _, want := range []string{"2–5 分钟", "5–10 分钟", "当前对话", "如有批改图"} {
		if !strings.Contains(text, want) {
			t.Fatalf("photo ETA feedback %q missing %q", text, want)
		}
	}
}

func TestBUG20260714_PhotoETASendsBeforePictureDownload(t *testing.T) {
	a := newTestAdapter()
	fake := &orderedPictureOpenAPI{fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token")}
	a.openAPI = fake
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "done"}, nil
	}
	event := dtEvent{ConversationType: "1", SenderStaffId: "parent", MsgType: dtMsgTypePicture}
	event.Content.DownloadCode = "download-code"
	a.handleMessage(event)
	if !fake.etaSeenBeforeDownload {
		t.Fatal("photo ETA must be sent before calling DownloadMessageFile")
	}
}

type orderedPictureOpenAPI struct {
	*fakeDingtalkOpenAPI
	etaSeenBeforeDownload bool
}

func (f *orderedPictureOpenAPI) DownloadMessageFile(_ context.Context, _, _, _ string) (string, error) {
	calls := f.SendCalls()
	f.etaSeenBeforeDownload = len(calls) > 0 && strings.Contains(calls[0].Text, "2–5 分钟")
	return "", context.DeadlineExceeded
}

func TestBUG20260714_GroupWithoutConversationIDNeverLeaksToPrivateChat(t *testing.T) {
	a := newTestAdapter()
	fake := newFakeConversationOpenAPI()
	a.openAPI = fake
	handlerCalls := 0
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		handlerCalls++
		return &adapter.Reply{Content: "private data"}, nil
	}
	event := dtEvent{ConversationType: "2", SenderStaffId: "parent", MsgType: dtMsgTypePicture}
	event.Text.Content = "请批改"
	a.handleMessage(event)
	if handlerCalls != 0 || len(fake.SendCalls()) != 0 {
		t.Fatalf("group event without conversationId must not fall back to OTO: handler=%d sends=%#v", handlerCalls, fake.SendCalls())
	}
}

func TestUploadDingtalkImage_UsesMultipartMediaEndpoint(t *testing.T) {
	var gotToken, gotQueryType, gotFormType, gotName, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.URL.Query().Get("access_token")
		gotQueryType = r.URL.Query().Get("type")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		gotFormType = r.FormValue("type")
		file, header, err := r.FormFile("media")
		if err != nil {
			t.Errorf("media part: %v", err)
			http.Error(w, "missing media", http.StatusBadRequest)
			return
		}
		defer file.Close()
		gotName = header.Filename
		raw, _ := io.ReadAll(file)
		gotBody = string(raw)
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "errmsg": "ok", "media_id": "@media-123", "type": "image"})
	}))
	defer srv.Close()

	realPNG := testPNGBytes(t)
	mediaID, err := uploadDingtalkImage(context.Background(), srv.Client(), srv.URL, "secret-token", adapter.Attachment{
		Type: "image", Name: "graded.png", Mime: "image/png", Data: base64.StdEncoding.EncodeToString(realPNG),
	})
	if err != nil {
		t.Fatal(err)
	}
	if mediaID != "@media-123" || gotToken != "secret-token" || gotQueryType != "image" || gotFormType != "image" || gotName != "graded.png" || !bytes.Equal([]byte(gotBody), realPNG) {
		t.Fatalf("unexpected multipart upload: media=%q token=%q query_type=%q form_type=%q name=%q body_bytes=%d", mediaID, gotToken, gotQueryType, gotFormType, gotName, len(gotBody))
	}
}

func TestUploadDingtalkImage_RejectsDisguisedOrOversizedPayload(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "invalid base64", data: "%%%"},
		{name: "not an image", data: base64.StdEncoding.EncodeToString([]byte("not really an image"))},
		{name: "over 20MB", data: base64.StdEncoding.EncodeToString(make([]byte, dingtalkMaxOutboundImageBytes+1))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := uploadDingtalkImage(context.Background(), http.DefaultClient, "https://example.invalid/media", "token", adapter.Attachment{
				Type: "image", Name: "fake.png", Mime: "image/png", Data: tt.data,
			})
			if err == nil {
				t.Fatal("unsafe image payload should be rejected before network call")
			}
		})
	}
}

func testPNGBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func messageText(msg dingtalkOutboundMessage) string {
	var payload struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal([]byte(msg.MsgParam), &payload)
	return payload.Text
}
