package discord

import (
	"context"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// Exercises the Discord MESSAGE_CREATE parse path: a human message is converted
// to a unified Message (carrying guild/message metadata) and dispatched, while
// malformed gateway payloads are dropped without dispatching.

func TestHandleMessageCreate_DispatchesUserMessage(t *testing.T) {
	a := New(config.DiscordConfig{})
	ch := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		ch <- m
		return nil, nil
	}

	raw := []byte(`{"id":"msg-1","channel_id":"chan-9","guild_id":"guild-7",` +
		`"author":{"id":"user-3","username":"bob","bot":false},"content":"hey discord"}`)
	a.handleMessageCreate(raw)

	select {
	case m := <-ch:
		if m.Content != "hey discord" || m.UserID != "user-3" || m.ChatID != "chan-9" {
			t.Errorf("parsed message = %+v, want gateway fields to round-trip", m)
		}
		if m.UserName != "bob" {
			t.Errorf("username = %q, want bob", m.UserName)
		}
		if m.Metadata["guild_id"] != "guild-7" || m.Metadata["message_id"] != "msg-1" {
			t.Errorf("metadata = %v, want guild_id/message_id populated", m.Metadata)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("user message was not dispatched to the handler")
	}
}

func TestHandleMessageCreate_MalformedPayloadDropped(t *testing.T) {
	a := New(config.DiscordConfig{})
	ch := make(chan *adapter.Message, 1)
	a.handler = func(_ context.Context, m *adapter.Message) (*adapter.Reply, error) {
		ch <- m
		return nil, nil
	}

	a.handleMessageCreate([]byte("{not json"))

	select {
	case m := <-ch:
		t.Fatalf("malformed payload must not dispatch, got %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}
