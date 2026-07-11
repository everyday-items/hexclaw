package matrix

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
)

func TestMatrixErrorBodiesAreBounded(t *testing.T) {
	const maxErrorText = 64<<10 + 1024
	huge := strings.Repeat("x", 256<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(huge))
	}))
	defer srv.Close()

	tests := []struct {
		name string
		call func(*MatrixAdapter) error
	}{
		{name: "send", call: func(a *MatrixAdapter) error {
			return a.sendReplyNow(context.Background(), "!room", &adapter.Reply{Content: "hi"})
		}},
		{name: "sync", call: func(a *MatrixAdapter) error { return a.doSync(context.Background()) }},
		{name: "validate", call: func(a *MatrixAdapter) error { return a.ValidateConfig(context.Background()) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(newTestAdapter(t, srv.URL, "@bot:example.com"))
			if err == nil {
				t.Fatal("non-OK response returned nil error")
			}
			if len(err.Error()) > maxErrorText {
				t.Fatalf("error text length = %d, want <= %d", len(err.Error()), maxErrorText)
			}
		})
	}
}

func TestMatrixSyncRetryIsCancelledByStop(t *testing.T) {
	reached := make(chan struct{}, 1)
	a := New(Config{HomeserverURL: "https://matrix.invalid", AccessToken: "tok", UserID: "@bot:invalid"})
	a.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		select {
		case reached <- struct{}{}:
		default:
		}
		return nil, errors.New("offline")
	})}
	done := make(chan struct{})
	go func() {
		a.syncLoop(context.Background())
		close(done)
	}()
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("sync loop did not reach transport")
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Stop did not cancel Matrix retry backoff")
	}
}

func TestMatrixStopCancelsInflightSyncRequest(t *testing.T) {
	reached := make(chan struct{}, 1)
	a := New(Config{HomeserverURL: "https://matrix.invalid", AccessToken: "tok", UserID: "@bot:invalid"})
	a.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case reached <- struct{}{}:
		default:
		}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(500 * time.Millisecond):
			return nil, errors.New("safety timeout")
		}
	})}
	if err := a.Start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("sync request did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("Stop should cancel in-flight sync, got %v", err)
	}
}

func TestMatrixStopWaitsForActiveMessageHandler(t *testing.T) {
	a := New(Config{UserID: "@bot:example.com"})
	entered := make(chan struct{})
	release := make(chan struct{})
	a.handler = func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		close(entered)
		<-release
		return nil, nil
	}
	a.handleEvent(context.Background(), "!room:example.com", matrixEvent{
		Type: "m.room.message", Sender: "@alice:example.com", EventID: "$1",
		Content: map[string]any{"msgtype": "m.text", "body": "hello"},
	})
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

func TestMatrixStopCancelsActiveHandlerSend(t *testing.T) {
	sendStarted := make(chan struct{}, 1)
	a := New(Config{HomeserverURL: "https://matrix.invalid", AccessToken: "tok", UserID: "@bot:invalid"})
	a.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
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
	})}
	a.handler = func(context.Context, *adapter.Message) (*adapter.Reply, error) {
		return &adapter.Reply{Content: "reply"}, nil
	}
	a.handleEvent(context.Background(), "!room:example.com", matrixEvent{
		Type: "m.room.message", Sender: "@alice:example.com", EventID: "$2",
		Content: map[string]any{"msgtype": "m.text", "body": "hello"},
	})
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
	if elapsed := time.Since(started); elapsed >= 200*time.Millisecond {
		t.Fatalf("Stop took %v, active send was not cancelled", elapsed)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
