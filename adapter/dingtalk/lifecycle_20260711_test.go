package dingtalk

import (
	"context"
	"sync"
	"testing"
	"time"

	dtchatbot "github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestDingtalkStopCancelsAndWaitsForMessageHandler(t *testing.T) {
	a := newTestAdapter()
	a.openAPI = newFakeDingtalkOpenAPI("token")
	started := make(chan struct{})
	exited := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	a.handler = func(ctx context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		close(started)
		defer close(exited)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return nil, nil
		}
	}

	data := &dtchatbot.BotCallbackDataModel{
		MsgId:         "provider-msg-lifecycle-1",
		SenderStaffId: "staff-1",
		SenderNick:    "user",
	}
	data.Text.Content = "hello"
	if _, err := a.onChatBotMessage(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Stop(ctx); err != nil {
		releaseOnce.Do(func() { close(release) })
		t.Fatalf("Stop returned error: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(50 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("Stop returned before canceling and joining the message handler")
	}
}
