package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestTelegramErrorBodyIsBounded(t *testing.T) {
	a := newTestAdapter()
	a.client = &http.Client{Transport: &mockTransport{handler: func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", 256<<10))),
		}, nil
	}}}
	err := a.sendMessageNow(context.Background(), "chat", nil)
	if err != nil {
		t.Fatalf("nil reply should remain a no-op: %v", err)
	}
	err = a.sendMessage(context.Background(), "chat", "hello")
	if err == nil {
		t.Fatal("non-OK response returned nil error")
	}
	if len(err.Error()) > 64<<10+1024 {
		t.Fatalf("error text length = %d, want bounded", len(err.Error()))
	}
}

func TestTelegramPollRetryIsCancelledByStop(t *testing.T) {
	reached := make(chan struct{}, 1)
	a := newTestAdapter()
	a.client = &http.Client{Transport: &mockTransport{handler: func(*http.Request) (*http.Response, error) {
		select {
		case reached <- struct{}{}:
		default:
		}
		return nil, errors.New("offline")
	}}}
	done := make(chan struct{})
	go func() {
		a.pollLoop()
		close(done)
	}()
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("poll loop did not reach transport")
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Stop did not cancel Telegram retry backoff")
	}
}

func TestTelegramStopWaitsForActiveMessageHandler(t *testing.T) {
	a := newTestAdapter()
	var calls int
	a.client = &http.Client{Transport: &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":2,"from":{"id":3},"chat":{"id":4,"type":"private"},"text":"hello"}}]}`)),
			}, nil
		}
		<-req.Context().Done()
		return nil, req.Context().Err()
	}}}
	entered := make(chan struct{})
	release := make(chan struct{})
	if err := a.Start(context.Background(), func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		close(entered)
		<-release
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := a.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop with active handler = %v, want deadline exceeded", err)
	}
	close(release)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("Stop after handler release: %v", err)
	}
}

func TestTelegramStopCancelsActiveHandlerSend(t *testing.T) {
	sendStarted := make(chan struct{}, 1)
	a := newTestAdapter()
	a.client = &http.Client{Transport: &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		select {
		case sendStarted <- struct{}{}:
		default:
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(500 * time.Millisecond):
			return nil, errors.New("safety timeout")
		}
	}}}
	a.handler = func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "reply"}, nil
	}
	done := make(chan struct{})
	go func() {
		a.handleMessage(&tgMessage{MessageID: 1, From: tgUser{ID: 2}, Chat: tgChat{ID: 3}, Text: "hello"})
		close(done)
	}()
	select {
	case <-sendStarted:
	case <-time.After(time.Second):
		t.Fatal("handler send did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop should cancel active send, got %v", err)
	}
	select {
	case <-done:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("handler send remained blocked after Stop")
	}
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("Stop took %v, active send was not cancelled", elapsed)
	}
}

func TestTelegramStopCancelsTrackedHandlerSend(t *testing.T) {
	sendStarted := make(chan struct{}, 1)
	a := newTestAdapter()
	var pollCalls int
	a.client = &http.Client{Transport: &mockTransport{handler: func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/getUpdates") {
			pollCalls++
			if pollCalls == 1 {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":2,"from":{"id":3},"chat":{"id":4,"type":"private"},"text":"hello"}}]}`)),
				}, nil
			}
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		select {
		case sendStarted <- struct{}{}:
		default:
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(500 * time.Millisecond):
			return nil, errors.New("safety timeout")
		}
	}}}
	if err := a.Start(context.Background(), func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "reply"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sendStarted:
	case <-time.After(time.Second):
		t.Fatal("tracked handler send did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop should cancel tracked handler send, got %v", err)
	}
}
