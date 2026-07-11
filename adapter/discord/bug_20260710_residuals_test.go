package discord

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

const discordErrorBodyTestLimit = 64 << 10

type countingReadCloser struct {
	reader io.Reader
	mu     sync.Mutex
	read   int
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.mu.Lock()
	r.read += n
	r.mu.Unlock()
	return n, err
}

func (*countingReadCloser) Close() error { return nil }

func (r *countingReadCloser) bytesRead() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read
}

func TestBUG20260710_DiscordErrorBodiesAreBounded(t *testing.T) {
	largeBody := strings.Repeat("x", discordErrorBodyTestLimit*3)

	tests := []struct {
		name string
		call func(context.Context, *DiscordAdapter) error
	}{
		{
			name: "create message",
			call: func(ctx context.Context, a *DiscordAdapter) error {
				_, err := a.createMessage(ctx, "channel", "message")
				return err
			},
		},
		{
			name: "edit message",
			call: func(ctx context.Context, a *DiscordAdapter) error {
				return a.editMessage(ctx, "channel", "message", "edited")
			},
		},
		{
			name: "validate config",
			call: func(ctx context.Context, a *DiscordAdapter) error {
				return a.ValidateConfig(ctx)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := &countingReadCloser{reader: strings.NewReader(largeBody)}
			a := newTestAdapter()
			a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Status:     "500 Internal Server Error",
					Body:       body,
					Header:     make(http.Header),
					Request:    req,
				}, nil
			})

			err := tt.call(context.Background(), a)
			if err == nil {
				t.Fatal("non-2xx response must return an error")
			}
			if got := body.bytesRead(); got > discordErrorBodyTestLimit {
				t.Fatalf("response body bytes read = %d; want <= %d", got, discordErrorBodyTestLimit)
			}
			if got := strings.Count(err.Error(), "x"); got > discordErrorBodyTestLimit {
				t.Fatalf("attacker-controlled error bytes = %d; want <= %d", got, discordErrorBodyTestLimit)
			}
		})
	}
}

func TestBUG20260710_DiscordStopCancelsAndWaitsForHandler(t *testing.T) {
	a := newTestAdapter()
	started := make(chan struct{})
	exited := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

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
	a.handleMessageCreate([]byte(`{"id":"1","channel_id":"c","author":{"id":"u","bot":false},"content":"x"}`))

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop returned an error: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(100 * time.Millisecond):
		t.Error("Stop returned before canceling and joining the handler")
		releaseOnce.Do(func() { close(release) })
	}
}

func TestBUG20260710_DiscordStopHonorsContextWhileJoiningHandler(t *testing.T) {
	a := newTestAdapter()
	started := make(chan struct{})
	release := make(chan struct{})
	a.handler = func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		close(started)
		<-release
		return nil, nil
	}
	a.handleMessageCreate([]byte(`{"id":"1","channel_id":"c","author":{"id":"u","bot":false},"content":"x"}`))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	err := a.Stop(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		close(release)
		t.Fatalf("Stop error = %v; want context deadline exceeded", err)
	}
	close(release)
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop did not converge after handler exit: %v", err)
	}
}

func TestBUG20260710_DiscordStartRejectsNilHandler(t *testing.T) {
	a := newTestAdapter()
	err := a.Start(context.Background(), nil)
	if err == nil {
		_ = a.Stop(context.Background())
		t.Fatal("Start accepted a nil handler")
	}
	if !strings.Contains(err.Error(), "handler") {
		t.Fatalf("Start error = %q; want an explicit handler validation error", err)
	}
}

func TestBUG20260710_DiscordStopCancelsGatewayDial(t *testing.T) {
	oldResolver := net.DefaultResolver
	dialStarted := make(chan struct{}, 1)
	dialCanceled := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			select {
			case dialStarted <- struct{}{}:
			default:
			}
			select {
			case <-ctx.Done():
				select {
				case dialCanceled <- struct{}{}:
				default:
				}
				return nil, ctx.Err()
			case <-release:
				return nil, errors.New("test DNS released")
			}
		},
	}
	t.Cleanup(func() {
		net.DefaultResolver = oldResolver
		releaseOnce.Do(func() { close(release) })
	})

	a := newTestAdapter()
	handler := func(context.Context, *adapter.Message) (*adapter.Reply, error) { return nil, nil }
	if err := a.Start(context.Background(), handler); err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	select {
	case <-dialStarted:
	case <-time.After(2 * time.Second):
		t.Skip("gateway hostname resolution did not use the Go resolver")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Stop(ctx); err != nil {
		releaseOnce.Do(func() { close(release) })
		t.Fatalf("Stop returned an error: %v", err)
	}
	select {
	case <-dialCanceled:
	case <-time.After(100 * time.Millisecond):
		releaseOnce.Do(func() { close(release) })
		t.Error("Stop did not cancel the in-flight Gateway dial")
	}
}

func TestBUG20260711_DiscordStopCancelsReplyReturnedAfterHandlerCancellation(t *testing.T) {
	a := newTestAdapter()
	a.queue = nil // isolate the adapter's reply context from SendQueue shutdown behavior
	handlerStarted := make(chan struct{})
	releaseSend := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseSend) })

	a.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-releaseSend:
			return nil, errors.New("test send released")
		}
	})}
	a.handler = func(ctx context.Context, _ *adapter.Message) (*adapter.Reply, error) {
		close(handlerStarted)
		<-ctx.Done()
		// A handler may finish useful cleanup and still return a reply when its
		// context is canceled. The adapter must not detach that outbound send.
		return &adapter.Reply{Content: "late reply"}, nil
	}
	a.handleMessageCreate([]byte(`{"id":"1","channel_id":"c","author":{"id":"u","bot":false},"content":"x"}`))
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stopDone := make(chan error, 1)
	go func() { stopDone <- a.Stop(ctx) }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		releaseOnce.Do(func() { close(releaseSend) })
		<-stopDone
		t.Fatal("Stop did not cancel the reply send derived after handler cancellation")
	}
}
