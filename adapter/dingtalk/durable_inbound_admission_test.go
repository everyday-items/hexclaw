package dingtalk

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dtchatbot "github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"

	"github.com/hexagon-codes/hexclaw/adapter"
)

type inboundPhotoAdmissionProbe struct {
	mu       sync.Mutex
	handled  bool
	err      error
	entered  chan struct{}
	release  <-chan struct{}
	once     sync.Once
	messages []*adapter.Message
}

func (p *inboundPhotoAdmissionProbe) AdmitInboundPhoto(
	ctx context.Context, message *adapter.Message,
) (bool, error) {
	p.mu.Lock()
	p.messages = append(p.messages, message)
	p.mu.Unlock()
	if p.entered != nil {
		p.once.Do(func() { close(p.entered) })
	}
	if p.release != nil {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-p.release:
		}
	}
	return p.handled, p.err
}

func (p *inboundPhotoAdmissionProbe) captured() []*adapter.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*adapter.Message(nil), p.messages...)
}

func TestDingTalkInboundPhotoAdmissionPortCanBeInstalled(t *testing.T) {
	a := newTestAdapter()
	a.SetInboundPhotoAdmissionPort(&inboundPhotoAdmissionProbe{})
}

func TestDingTalkStreamACKWaitsForDurablePhotoAdmissionAndSkipsLegacyWorker(t *testing.T) {
	a, pictureAPI, cleanup := newDurableAdmissionPictureAdapter(t)
	defer cleanup()

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	port := &inboundPhotoAdmissionProbe{
		handled: true,
		entered: entered,
		release: release,
	}
	a.SetInboundPhotoAdmissionPort(port)
	handlerCalls := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, message *adapter.Message) (*adapter.Reply, error) {
		handlerCalls <- message
		return nil, nil
	}

	type callbackResult struct {
		ack []byte
		err error
	}
	returned := make(chan callbackResult, 1)
	go func() {
		ack, err := a.onChatBotMessage(context.Background(), durableAdmissionPictureCallback("provider-photo-admit-1"))
		returned <- callbackResult{ack: ack, err: err}
	}()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("durable admission was not reached")
	}
	select {
	case result := <-returned:
		t.Fatalf("callback ACK returned before durable admission completed: ack=%q err=%v", result.ack, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	assertNoLegacyDingTalkWorkerCall(t, handlerCalls)
	assertDurableAdmissionMessage(t, port.captured(), "provider-photo-admit-1")
	if got := pictureDownloadCallCount(pictureAPI); got != 1 {
		t.Fatalf("picture download calls before admission = %d, want 1", got)
	}

	releaseOnce.Do(func() { close(release) })
	select {
	case result := <-returned:
		if result.err != nil || result.ack == nil {
			t.Fatalf("durable admission success must ACK: ack=%q err=%v", result.ack, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not ACK after durable admission completed")
	}
	assertNoLegacyDingTalkWorkerCall(t, handlerCalls)
}

func TestDingTalkStreamAdmissionFailureIsRetryableAndStartsNoBusinessWorker(t *testing.T) {
	a, _, cleanup := newDurableAdmissionPictureAdapter(t)
	defer cleanup()

	wantErr := errors.New("durable store unavailable")
	port := &inboundPhotoAdmissionProbe{handled: false, err: wantErr}
	a.SetInboundPhotoAdmissionPort(port)
	handlerCalls := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, message *adapter.Message) (*adapter.Reply, error) {
		handlerCalls <- message
		return nil, nil
	}

	ack, err := a.onChatBotMessage(
		context.Background(), durableAdmissionPictureCallback("provider-photo-admit-failed"),
	)
	if !errors.Is(err, wantErr) || ack != nil {
		t.Fatalf("admission failure must be retryable without ACK: ack=%q err=%v", ack, err)
	}
	assertNoLegacyDingTalkWorkerCall(t, handlerCalls)
	assertDurableAdmissionMessage(t, port.captured(), "provider-photo-admit-failed")
}

func TestDingTalkUnhandledPhotoReusesPredownloadedAttachmentInLegacyWorker(t *testing.T) {
	a, pictureAPI, cleanup := newDurableAdmissionPictureAdapter(t)
	defer cleanup()

	port := &inboundPhotoAdmissionProbe{handled: false}
	a.SetInboundPhotoAdmissionPort(port)
	handlerCalls := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, message *adapter.Message) (*adapter.Reply, error) {
		handlerCalls <- message
		return nil, nil
	}

	ack, err := a.onChatBotMessage(
		context.Background(), durableAdmissionPictureCallback("provider-photo-unhandled"),
	)
	if err != nil || ack == nil {
		t.Fatalf("unhandled non-K12 photo must preserve legacy ACK: ack=%q err=%v", ack, err)
	}
	assertDurableAdmissionMessage(t, port.captured(), "provider-photo-unhandled")
	select {
	case message := <-handlerCalls:
		assertDurableAdmissionMessage(t, []*adapter.Message{message}, "provider-photo-unhandled")
	case <-time.After(3 * time.Second):
		t.Fatal("unhandled photo did not reach the legacy worker")
	}
	if got := pictureDownloadCallCount(pictureAPI); got != 1 {
		t.Fatalf("unhandled photo downloaded %d times, want exactly once", got)
	}
}

func TestDingTalkWebhookAdmissionFailureReturnsServiceUnavailableWithoutWorker(t *testing.T) {
	a, _, cleanup := newDurableAdmissionPictureAdapter(t)
	defer cleanup()
	a.cfg.AppSecret = ""
	a.SetInboundPhotoAdmissionPort(&inboundPhotoAdmissionProbe{
		err: errors.New("durable store unavailable"),
	})
	handlerCalls := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, message *adapter.Message) (*adapter.Reply, error) {
		handlerCalls <- message
		return nil, nil
	}

	body := `{"msgId":"provider-photo-webhook-failed","msgtype":"picture","content":{"downloadCode":"download-code"},"senderStaffId":"parent-1","senderNick":"家长","conversationId":"conversation-1","conversationType":"1"}`
	request := httptest.NewRequest(http.MethodPost, "/webhook/dingtalk", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	a.handleWebhook(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("admission failure status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	assertNoLegacyDingTalkWorkerCall(t, handlerCalls)
}

func TestDingTalkDuplicateProviderMessageIDReentersOnlyDurableAdmission(t *testing.T) {
	a, _, cleanup := newDurableAdmissionPictureAdapter(t)
	defer cleanup()
	port := &inboundPhotoAdmissionProbe{handled: true}
	a.SetInboundPhotoAdmissionPort(port)
	handlerCalls := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, message *adapter.Message) (*adapter.Reply, error) {
		handlerCalls <- message
		return nil, nil
	}

	for attempt := 0; attempt < 2; attempt++ {
		ack, err := a.onChatBotMessage(
			context.Background(), durableAdmissionPictureCallback("provider-photo-duplicate"),
		)
		if err != nil || ack == nil {
			t.Fatalf("duplicate attempt %d was not ACKed after admission: ack=%q err=%v", attempt+1, ack, err)
		}
	}
	messages := port.captured()
	if len(messages) != 2 || messages[0].ID != "provider-photo-duplicate" ||
		messages[1].ID != "provider-photo-duplicate" {
		t.Fatalf("provider identity did not converge at durable port: %#v", messages)
	}
	assertNoLegacyDingTalkWorkerCall(t, handlerCalls)
}

func TestDingTalkPredownloadFailureReturnsNoACKAndNeverCallsAdmissionOrWorker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	a := newTestAdapter()
	a.openAPI = &fakePictureOpenAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token"),
		downloadURL:         server.URL,
	}
	port := &inboundPhotoAdmissionProbe{handled: true}
	a.SetInboundPhotoAdmissionPort(port)
	handlerCalls := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, message *adapter.Message) (*adapter.Reply, error) {
		handlerCalls <- message
		return nil, nil
	}

	ack, err := a.onChatBotMessage(
		context.Background(), durableAdmissionPictureCallback("provider-photo-download-failed"),
	)
	if err == nil || ack != nil {
		t.Fatalf("predownload failure must be retryable without ACK: ack=%q err=%v", ack, err)
	}
	if messages := port.captured(); len(messages) != 0 {
		t.Fatalf("failed download reached durable admission: %#v", messages)
	}
	assertNoLegacyDingTalkWorkerCall(t, handlerCalls)
}

func newDurableAdmissionPictureAdapter(
	t *testing.T,
) (*DingtalkAdapter, *fakePictureOpenAPI, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	a := newTestAdapter()
	api := &fakePictureOpenAPI{
		fakeDingtalkOpenAPI: newFakeDingtalkOpenAPI("token"),
		downloadURL:         server.URL,
	}
	a.openAPI = api
	return a, api, server.Close
}

func durableAdmissionPictureCallback(messageID string) *dtchatbot.BotCallbackDataModel {
	return &dtchatbot.BotCallbackDataModel{
		MsgId:            messageID,
		ConversationId:   "conversation-1",
		ConversationType: "1",
		SenderStaffId:    "parent-1",
		SenderNick:       "家长",
		Msgtype:          dtMsgTypePicture,
		Content:          map[string]interface{}{"downloadCode": "download-code"},
	}
}

func assertDurableAdmissionMessage(
	t *testing.T, messages []*adapter.Message, wantMessageID string,
) {
	t.Helper()
	if len(messages) != 1 {
		t.Fatalf("admitted message count = %d, want 1", len(messages))
	}
	message := messages[0]
	if message.ID != wantMessageID || message.Platform != adapter.PlatformDingtalk ||
		message.InstanceID != "dingtalk" || message.ChatID != "parent-1" {
		t.Fatalf("canonical inbound identity drifted: %#v", message)
	}
	if len(message.Attachments) != 1 || message.Attachments[0].Type != "image" ||
		message.Attachments[0].Data != base64.StdEncoding.EncodeToString(pngBytes) {
		t.Fatalf("durable admission did not receive downloaded image bytes: %#v", message.Attachments)
	}
}

func pictureDownloadCallCount(api *fakePictureOpenAPI) int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return len(api.downloadCalls)
}

func assertNoLegacyDingTalkWorkerCall(t *testing.T, calls <-chan *adapter.Message) {
	t.Helper()
	select {
	case message := <-calls:
		t.Fatalf("durably handled or failed admission reached legacy worker: %#v", message)
	case <-time.After(100 * time.Millisecond):
	}
}
