// Package acp 提供 HexClaw Agent 间通信能力
//
// 基于 Hexagon 的 A2A (Agent-to-Agent) 协议实现，
// 为 HexClaw 内部 Agent 提供轻量级消息总线。
//
// 同时桥接 Hexagon A2A 的标准 HTTP 接口，
// 使 HexClaw Agent 可与外部 A2A 兼容 Agent 通信。
//
// 内部通信使用 Bus（进程内 channel），
// 外部通信使用 Hexagon A2A Client/Server。
package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// MessageType 消息类型
type MessageType string

const (
	TypeRequest  MessageType = "request"
	TypeResponse MessageType = "response"
	TypeNotify   MessageType = "notify"
	TypeError    MessageType = "error"
)

// Message Agent 间通信消息
type Message struct {
	ID        string            `json:"id"`
	Type      MessageType       `json:"type"`
	From      string            `json:"from"`
	To        string            `json:"to"`
	ReplyTo   string            `json:"reply_to,omitempty"`
	Content   string            `json:"content"`
	Metadata  map[string]string `json:"metadata,omitempty"`
	Timestamp time.Time         `json:"timestamp"`
}

// Handler 消息处理函数
type Handler func(ctx context.Context, msg *Message) (*Message, error)

// Bus Agent 消息总线（进程内通信）
type Bus struct {
	mu       sync.RWMutex
	handlers map[string]Handler       // agent name → handler
	pending  map[string]chan *Message  // msg ID → response channel
}

// NewBus 创建消息总线
func NewBus() *Bus {
	return &Bus{
		handlers: make(map[string]Handler),
		pending:  make(map[string]chan *Message),
	}
}

// Register 注册 Agent 的消息处理器
func (b *Bus) Register(agentName string, handler Handler) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.handlers[agentName]; exists {
		return fmt.Errorf("agent %s 已注册", agentName)
	}
	b.handlers[agentName] = handler
	return nil
}

// Unregister 取消注册
func (b *Bus) Unregister(agentName string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.handlers, agentName)
}

// Send 异步发送消息（fire-and-forget）
func (b *Bus) Send(ctx context.Context, msg *Message) error {
	if msg.ID == "" {
		msg.ID = genID()
	}
	msg.Timestamp = time.Now()

	b.mu.RLock()
	handler, ok := b.handlers[msg.To]
	b.mu.RUnlock()

	if !ok {
		return fmt.Errorf("目标 agent %s 未注册", msg.To)
	}

	go func() {
		// 检查 context 是否已取消
		if ctx.Err() != nil {
			return
		}
		// 兜底超时：即使调用方 context 无超时，也不会永远阻塞
		execCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()

		resp, err := handler(execCtx, msg)
		if err != nil {
			return
		}
		// 如果有 pending request 在等待响应
		if resp != nil && msg.Type == TypeRequest {
			b.mu.RLock()
			ch, ok := b.pending[msg.ID]
			b.mu.RUnlock()
			if ok {
				select {
				case ch <- resp:
				case <-execCtx.Done():
				}
			}
		}
	}()

	return nil
}

// Request 同步请求-响应（带超时）
func (b *Bus) Request(ctx context.Context, msg *Message) (*Message, error) {
	if msg.ID == "" {
		msg.ID = genID()
	}
	msg.Type = TypeRequest
	msg.Timestamp = time.Now()

	// 创建响应 channel
	respCh := make(chan *Message, 1)
	b.mu.Lock()
	b.pending[msg.ID] = respCh
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.pending, msg.ID)
		b.mu.Unlock()
	}()

	// 发送请求
	b.mu.RLock()
	handler, ok := b.handlers[msg.To]
	b.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("目标 agent %s 未注册", msg.To)
	}

	go func() {
		resp, err := handler(ctx, msg)
		if err != nil {
			respCh <- &Message{
				Type:    TypeError,
				From:    msg.To,
				To:      msg.From,
				ReplyTo: msg.ID,
				Content: err.Error(),
			}
			return
		}
		if resp != nil {
			resp.ReplyTo = msg.ID
			respCh <- resp
		}
	}()

	select {
	case resp := <-respCh:
		if resp.Type == TypeError {
			return nil, fmt.Errorf("agent %s 返回错误: %s", msg.To, resp.Content)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Broadcast 广播消息给所有已注册 Agent（除了发送者）
func (b *Bus) Broadcast(ctx context.Context, from, content string) error {
	b.mu.RLock()
	names := make([]string, 0, len(b.handlers))
	for name := range b.handlers {
		if name != from {
			names = append(names, name)
		}
	}
	b.mu.RUnlock()

	for _, name := range names {
		msg := &Message{
			Type:    TypeNotify,
			From:    from,
			To:      name,
			Content: content,
		}
		if err := b.Send(ctx, msg); err != nil {
			return err
		}
	}
	return nil
}

// ListAgents 列出所有已注册的 Agent
func (b *Bus) ListAgents() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	names := make([]string, 0, len(b.handlers))
	for name := range b.handlers {
		names = append(names, name)
	}
	return names
}

func genID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
