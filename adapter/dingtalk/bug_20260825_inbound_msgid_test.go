package dingtalk

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dtchatbot "github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

func TestBUG20260825StreamCallbackPreservesProviderMsgIDForRedeliveryDedup(t *testing.T) {
	a := New(config.DingtalkConfig{
		Name: "parent-ding-1", AppKey: "key", AppSecret: "secret", RobotCode: "robot",
	})
	a.openAPI = newFakeDingtalkOpenAPI("token")

	processed := make(chan *adapter.Message, 2)
	seen := make(map[string]struct{})
	var seenMu sync.Mutex
	a.handler = func(_ context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		seenMu.Lock()
		defer seenMu.Unlock()
		key := msg.InstanceID + "\x00" + msg.ID
		if _, duplicate := seen[key]; !duplicate {
			seen[key] = struct{}{}
			processed <- msg
		}
		return nil, nil
	}

	callback := &dtchatbot.BotCallbackDataModel{
		MsgId:            "provider-msg-42",
		ConversationId:   "conversation-42",
		ConversationType: "1",
		SenderStaffId:    "parent-42",
		SenderNick:       "家长",
	}
	callback.Text.Content = "请讲解这道题"

	for attempt := 0; attempt < 2; attempt++ {
		if _, err := a.onChatBotMessage(context.Background(), callback); err != nil {
			t.Fatalf("第 %d 次回调失败: %v", attempt+1, err)
		}
	}

	msg := receiveDingTalkInboundMessage(t, processed)
	if msg.ID != callback.MsgId {
		t.Fatalf("canonical message ID = %q, want provider MsgId %q", msg.ID, callback.MsgId)
	}
	select {
	case duplicate := <-processed:
		t.Fatalf("相同 provider MsgId 重投未去重，第二条 canonical message = %#v", duplicate)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBUG20260825DifferentProviderMsgIDsForSamePictureRemainIndependent(t *testing.T) {
	imageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer imageServer.Close()

	a := New(config.DingtalkConfig{
		Name: "parent-ding-picture", AppKey: "key", AppSecret: "secret", RobotCode: "robot",
	})
	a.openAPI = &fakePictureOpenAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token"),
		downloadURL:         imageServer.URL,
	}
	captured := make(chan *adapter.Message, 2)
	a.handler = func(_ context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		captured <- msg
		return nil, nil
	}

	for _, msgID := range []string{"provider-picture-1", "provider-picture-2"} {
		callback := &dtchatbot.BotCallbackDataModel{
			MsgId:            msgID,
			ConversationId:   "conversation-picture",
			ConversationType: "1",
			SenderStaffId:    "parent-picture",
			Msgtype:          dtMsgTypePicture,
			Content:          map[string]interface{}{"downloadCode": "same-picture-code"},
		}
		if _, err := a.onChatBotMessage(context.Background(), callback); err != nil {
			t.Fatalf("MsgId=%q callback failed: %v", msgID, err)
		}
	}

	got := map[string]bool{}
	for range 2 {
		msg := receiveDingTalkInboundMessage(t, captured)
		got[msg.ID] = true
		if len(msg.Attachments) != 1 {
			t.Fatalf("MsgId=%q attachment count = %d, want 1", msg.ID, len(msg.Attachments))
		}
	}
	for _, want := range []string{"provider-picture-1", "provider-picture-2"} {
		if !got[want] {
			t.Fatalf("同一图片的不同 provider MsgId 必须保持独立，captured IDs = %#v", got)
		}
	}
}

func TestBUG20260825ProviderMsgIDKeepsInstanceAndDirectChatScope(t *testing.T) {
	type inbound struct {
		instance string
		chatID   string
	}
	for _, tc := range []inbound{
		{instance: "ding-family-a", chatID: "parent-a"},
		{instance: "ding-family-b", chatID: "parent-b"},
	} {
		t.Run(tc.instance, func(t *testing.T) {
			a := New(config.DingtalkConfig{
				Name: tc.instance, AppKey: "key", AppSecret: "secret", RobotCode: "robot",
			})
			a.openAPI = newFakeDingtalkOpenAPI("token")
			captured := make(chan *adapter.Message, 1)
			a.handler = func(_ context.Context, msg *adapter.Message) (*adapter.Reply, error) {
				captured <- msg
				return nil, nil
			}

			callback := &dtchatbot.BotCallbackDataModel{
				MsgId:            "provider-msg-shared",
				ConversationId:   "conversation-" + tc.chatID,
				ConversationType: "1",
				SenderStaffId:    tc.chatID,
			}
			callback.Text.Content = "同一个供应商消息标识在不同实例中独立"
			if _, err := a.onChatBotMessage(context.Background(), callback); err != nil {
				t.Fatal(err)
			}

			msg := receiveDingTalkInboundMessage(t, captured)
			if msg.ID != callback.MsgId || msg.InstanceID != tc.instance || msg.ChatID != tc.chatID {
				t.Fatalf("identity scope drifted: ID=%q InstanceID=%q ChatID=%q", msg.ID, msg.InstanceID, msg.ChatID)
			}
		})
	}
}

func TestBUG20260825MissingProviderMsgIDFailsClosed(t *testing.T) {
	for _, msgID := range []string{"", " \t\n "} {
		t.Run("msg_id_"+msgID, func(t *testing.T) {
			a := newTestAdapter()
			called := make(chan struct{}, 1)
			a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
				called <- struct{}{}
				return nil, nil
			}
			callback := &dtchatbot.BotCallbackDataModel{
				MsgId:            msgID,
				ConversationType: "1",
				SenderStaffId:    "parent-missing-id",
			}
			callback.Text.Content = "不能用随机 ID 继续处理"

			if _, err := a.onChatBotMessage(context.Background(), callback); err == nil {
				t.Fatal("missing provider MsgId must fail closed")
			}
			select {
			case <-called:
				t.Fatal("missing provider MsgId reached handler with a generated replacement ID")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestBUG20260825WebhookMissingProviderMsgIDFailsClosed(t *testing.T) {
	for _, body := range []string{
		`{"text":{"content":"不能用空 ID 继续处理"},"senderStaffId":"parent-missing-id"}`,
		`{"msgId":"  ","text":{"content":"不能用空白 ID 继续处理"},"senderStaffId":"parent-missing-id"}`,
	} {
		a := New(config.DingtalkConfig{AppSecret: ""})
		called := make(chan struct{}, 1)
		a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
			called <- struct{}{}
			return nil, nil
		}

		request := httptest.NewRequest(http.MethodPost, "/webhook/dingtalk", strings.NewReader(body))
		recorder := httptest.NewRecorder()
		a.handleWebhook(recorder, request)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("missing provider MsgId status = %d, want %d", recorder.Code, http.StatusBadRequest)
		}
		select {
		case <-called:
			t.Fatal("webhook with missing provider MsgId reached handler")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func TestBUG20260825GroupIgnoreDoesNotDependOnProviderMsgID(t *testing.T) {
	a := newTestAdapter()
	called := make(chan struct{}, 1)
	a.handler = func(_ context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		called <- struct{}{}
		return nil, nil
	}
	callback := &dtchatbot.BotCallbackDataModel{
		ConversationId:   "family-group",
		ConversationType: "2",
		SenderStaffId:    "parent-group",
	}
	callback.Text.Content = "群消息仍按 v0.5 规则忽略"

	ack, err := a.onChatBotMessage(context.Background(), callback)
	if err != nil || ack == nil {
		t.Fatalf("group ignore must remain an immediate successful ack: ack=%q err=%v", ack, err)
	}
	select {
	case <-called:
		t.Fatal("group callback must remain ignored")
	case <-time.After(100 * time.Millisecond):
	}
}

func receiveDingTalkInboundMessage(t *testing.T, messages <-chan *adapter.Message) *adapter.Message {
	t.Helper()
	select {
	case msg := <-messages:
		return msg
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for DingTalk inbound message")
		return nil
	}
}
