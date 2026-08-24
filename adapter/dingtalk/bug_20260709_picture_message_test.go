package dingtalk

// BUG-20260709：钉钉发送图片（msgtype=picture）没有任何回复。
//
// 根因：Stream 入口 onChatBotMessage 只拷贝 data.Text.Content，picture 消息正文为空 →
// `if strings.TrimSpace(event.Text.Content) != ""` 直接丢弃，handler 永远收不到；
// Webhook 入口 handleWebhook 同样只认 event.Text.Content。dtEvent 甚至没有解析
// content.downloadCode 的字段，图片下载链路完全缺失。
//
// 期望行为（本测试断言的正确行为，未修复时 FAIL 即证明 bug 存在）：
//   picture 消息 → 用 downloadCode 经 openAPI 换 downloadUrl → 下载图片字节 →
//   以 adapter.Attachment{Type:"image", Data: base64} 进入 handler 消息，
//   由引擎按多模态/vision 路由（无 vision 模型时引擎有友好拒绝文案）——绝不静默丢弃。

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dtchatbot "github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// fakePictureOpenAPI 在既有 fake 之上补图片下载：downloadCode → 预置 downloadURL。
type fakePictureOpenAPI struct {
	*fakeDingtalkOpenAPI
	mu            sync.Mutex
	downloadURL   string
	downloadCalls []string
}

func (f *fakePictureOpenAPI) DownloadMessageFile(_ context.Context, _, _, downloadCode string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloadCalls = append(f.downloadCalls, downloadCode)
	return f.downloadURL, nil
}

// pngBytes 是最小 PNG 文件头（http.DetectContentType 足以识别为 image/png）。
var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0, 0, 0, 0}

// captureHandler 返回一个把收到的消息投递到 channel 的 handler（回空 reply 简化断言面）。
func capturePictureHandler(ch chan *adapter.Message) adapter.MessageHandler {
	return func(_ context.Context, msg *adapter.Message) (*adapter.Reply, error) {
		ch <- msg
		return &adapter.Reply{Content: "ok"}, nil
	}
}

func assertPictureMessage(t *testing.T, ch chan *adapter.Message, wantData []byte) {
	t.Helper()
	select {
	case msg := <-ch:
		if len(msg.Attachments) != 1 {
			t.Fatalf("图片消息应携带 1 个附件进入 handler，实际 %d 个（附件丢失=图片被丢弃）", len(msg.Attachments))
		}
		att := msg.Attachments[0]
		if !adapter.IsImageAttachment(att) {
			t.Fatalf("附件应为 image 类型，实际 Type=%q Mime=%q", att.Type, att.Mime)
		}
		if att.Data != base64.StdEncoding.EncodeToString(wantData) {
			t.Fatalf("附件 Data 应为下载图片字节的 base64，实际 %q", att.Data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("BUG 复现：picture 消息被静默丢弃——3s 内 handler 未被调用（期望收到 1 条带 image 附件的消息）")
	}
}

// TestBUG20260709_StreamPictureMessage_ReachesHandlerWithImageAttachment
// Stream 入口：msgtype=picture + content.downloadCode → handler 必须收到带 image 附件的消息。
func TestBUG20260709_StreamPictureMessage_ReachesHandlerWithImageAttachment(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer imgSrv.Close()

	a := newTestAdapter()
	fake := &fakePictureOpenAPI{fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("tok"), downloadURL: imgSrv.URL}
	a.openAPI = fake

	ch := make(chan *adapter.Message, 1)
	a.handler = capturePictureHandler(ch)

	_, err := a.onChatBotMessage(context.Background(), &dtchatbot.BotCallbackDataModel{
		MsgId:            "provider-picture-stream-1",
		ConversationId:   "cid-1",
		ConversationType: "1",
		SenderStaffId:    "staff-1",
		SenderNick:       "测试家长",
		Msgtype:          "picture",
		Content:          map[string]interface{}{"downloadCode": "dl-code-1"},
	})
	if err != nil {
		t.Fatalf("onChatBotMessage 不应报错: %v", err)
	}

	assertPictureMessage(t, ch, pngBytes)
}

// TestBUG20260709_WebhookPictureMessage_ReachesHandlerWithImageAttachment
// Webhook 入口：同样的 picture 载荷经 HTTP 回调 → handler 必须收到带 image 附件的消息。
func TestBUG20260709_WebhookPictureMessage_ReachesHandlerWithImageAttachment(t *testing.T) {
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer imgSrv.Close()

	a := newTestAdapter()
	a.cfg.AppSecret = "" // 跳过签名校验（本测试只关心消息路由）
	fake := &fakePictureOpenAPI{fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("tok"), downloadURL: imgSrv.URL}
	a.openAPI = fake

	ch := make(chan *adapter.Message, 1)
	a.handler = capturePictureHandler(ch)

	body := `{"msgId":"provider-picture-webhook-1","msgtype":"picture","content":{"downloadCode":"dl-code-2"},"senderStaffId":"staff-2","senderNick":"测试家长","conversationId":"cid-2","conversationType":"1"}`
	req := httptest.NewRequest(http.MethodPost, "/webhook/dingtalk", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook 应 200，实际 %d", rec.Code)
	}

	assertPictureMessage(t, ch, pngBytes)
}
