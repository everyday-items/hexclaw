package acp

import (
	"context"
	"testing"
	"time"
)

func TestBusRegisterAndList(t *testing.T) {
	bus := NewBus()

	handler := func(ctx context.Context, msg *Message) (*Message, error) {
		return &Message{Content: "pong"}, nil
	}

	if err := bus.Register("agent-a", handler); err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 重复注册
	if err := bus.Register("agent-a", handler); err == nil {
		t.Error("重复注册应报错")
	}

	agents := bus.ListAgents()
	if len(agents) != 1 || agents[0] != "agent-a" {
		t.Errorf("Agent 列表不正确: %v", agents)
	}

	bus.Unregister("agent-a")
	if len(bus.ListAgents()) != 0 {
		t.Error("取消注册后列表应为空")
	}
}

func TestBusSendNotify(t *testing.T) {
	bus := NewBus()
	received := make(chan string, 1)

	_ = bus.Register("receiver", func(ctx context.Context, msg *Message) (*Message, error) {
		received <- msg.Content
		return nil, nil
	})

	err := bus.Send(context.Background(), &Message{
		Type:    TypeNotify,
		From:    "sender",
		To:      "receiver",
		Content: "hello",
	})
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	select {
	case content := <-received:
		if content != "hello" {
			t.Errorf("内容不匹配: %s", content)
		}
	case <-time.After(time.Second):
		t.Fatal("超时未收到消息")
	}
}

func TestBusRequest(t *testing.T) {
	bus := NewBus()

	_ = bus.Register("echo", func(ctx context.Context, msg *Message) (*Message, error) {
		return &Message{
			Type:    TypeResponse,
			From:    "echo",
			To:      msg.From,
			Content: "echo: " + msg.Content,
		}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := bus.Request(ctx, &Message{
		From:    "caller",
		To:      "echo",
		Content: "ping",
	})
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if resp.Content != "echo: ping" {
		t.Errorf("响应不匹配: %s", resp.Content)
	}
}

func TestBusRequestTimeout(t *testing.T) {
	bus := NewBus()

	_ = bus.Register("slow", func(ctx context.Context, msg *Message) (*Message, error) {
		time.Sleep(5 * time.Second)
		return &Message{Content: "too late"}, nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := bus.Request(ctx, &Message{
		From: "caller",
		To:   "slow",
	})
	if err == nil {
		t.Error("应超时报错")
	}
}

func TestBusSendToUnregistered(t *testing.T) {
	bus := NewBus()
	err := bus.Send(context.Background(), &Message{To: "ghost"})
	if err == nil {
		t.Error("发送给未注册 Agent 应报错")
	}
}

func TestBusBroadcast(t *testing.T) {
	bus := NewBus()
	count := 0
	var mu = make(chan struct{}, 3)

	for _, name := range []string{"a", "b", "c"} {
		bus.Register(name, func(ctx context.Context, msg *Message) (*Message, error) {
			mu <- struct{}{}
			return nil, nil
		})
	}

	err := bus.Broadcast(context.Background(), "a", "hello all")
	if err != nil {
		t.Fatalf("广播失败: %v", err)
	}

	// 等待 b 和 c 收到（a 是发送者，不应收到）
	timeout := time.After(time.Second)
	for count < 2 {
		select {
		case <-mu:
			count++
		case <-timeout:
			t.Fatalf("超时，只收到 %d 条", count)
		}
	}
}
