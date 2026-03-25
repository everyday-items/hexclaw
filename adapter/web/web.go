// Package web 提供 Web UI WebSocket 适配器
//
// 通过 WebSocket 实现 Web 前端与 HexClaw 引擎的实时双向通信。
// 支持同步回复和流式输出（打字机效果）。
//
// 消息协议（JSON）：
//
//	客户端 → 服务端: {"type":"message","content":"你好","session_id":"可选"}
//	服务端 → 客户端: {"type":"reply","content":"你好！","session_id":"sess-xxx"}
//	服务端 → 客户端: {"type":"chunk","content":"你","done":false}
//	服务端 → 客户端: {"type":"error","content":"错误信息"}
package web

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/internal/upstreamerr"
	"github.com/hexagon-codes/toolkit/util/idgen"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// WebAdapter Web UI WebSocket 适配器
//
// 管理 WebSocket 连接，将 Web 消息转换为统一格式。
// 每个 WebSocket 连接分配唯一 chatID。
type WebAdapter struct {
	handler       adapter.MessageHandler
	streamHandler adapter.StreamMessageHandler
	conns         sync.Map // chatID → *websocket.Conn
}

// SetStreamHandler 设置流式消息处理器
//
// 设置后 WebSocket 消息将使用流式处理，逐 chunk 推送给客户端（打字机效果）。
// 未设置时降级为同步处理。
func (a *WebAdapter) SetStreamHandler(h adapter.StreamMessageHandler) {
	a.streamHandler = h
}

// New 创建 Web 适配器
func New() *WebAdapter {
	return &WebAdapter{}
}

func (a *WebAdapter) Name() string               { return "web" }
func (a *WebAdapter) Platform() adapter.Platform { return adapter.PlatformWeb }

// Start 注册消息处理器
//
// Web 适配器不自己启动 HTTP 服务器，而是通过 Handler() 返回 http.Handler
// 供主 API 服务器挂载到 /ws 路径。
func (a *WebAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	// 启动日志由 main 统一输出
	return nil
}

// Stop 关闭所有 WebSocket 连接
func (a *WebAdapter) Stop(_ context.Context) error {
	a.conns.Range(func(key, value any) bool {
		if conn, ok := value.(*websocket.Conn); ok {
			_ = conn.Close(websocket.StatusGoingAway, "服务关闭")
		}
		a.conns.Delete(key)
		return true
	})
	log.Println("Web 适配器已停止")
	return nil
}

// Handler 返回 WebSocket HTTP Handler
//
// 挂载到主 API 服务器的 /ws 路径：
//
//	mux.Handle("/ws", webAdapter.Handler())
func (a *WebAdapter) Handler() http.Handler {
	return http.HandlerFunc(a.handleWS)
}

// Send 发送同步回复到指定连接
func (a *WebAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	conn, ok := a.getConn(chatID)
	if !ok {
		return nil // 连接已断开，静默忽略
	}

	msg := wsMessage{
		Type:     "reply",
		Content:  reply.Content,
		Metadata: reply.Metadata,
	}
	return wsjson.Write(ctx, conn, msg)
}

// SendStream 流式发送回复（打字机效果）
func (a *WebAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	conn, ok := a.getConn(chatID)
	if !ok {
		return nil
	}

	for chunk := range chunks {
		if chunk.Error != nil {
			errMsg := wsMessage{Type: "error", Content: chunk.Error.Error()}
			_ = wsjson.Write(ctx, conn, errMsg)
			return chunk.Error
		}

		msg := wsMessage{
			Type:      "chunk",
			Content:   chunk.Content,
			Done:      chunk.Done,
			Metadata:  chunk.Metadata,
			Usage:     chunk.Usage,
			ToolCalls: chunk.ToolCalls,
		}
		if err := wsjson.Write(ctx, conn, msg); err != nil {
			return err
		}
	}
	return nil
}

// handleWS 处理 WebSocket 连接
func (a *WebAdapter) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{
			"localhost:*",
			"127.0.0.1:*",
			"tauri://localhost",
			"https://tauri.localhost",
		},
	})
	if err != nil {
		log.Printf("WebSocket 握手失败: %v", err)
		return
	}

	// 限制客户端消息大小为 20MB，支持图片附件
	conn.SetReadLimit(20 * 1024 * 1024)

	chatID := "ws-" + idgen.ShortID()
	a.conns.Store(chatID, conn)
	defer func() {
		a.conns.Delete(chatID)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	log.Printf("WebSocket 连接建立: %s", chatID)

	// 读取消息循环
	for {
		var incoming wsMessage
		if err := wsjson.Read(r.Context(), conn, &incoming); err != nil {
			log.Printf("WebSocket 连接断开: %s", chatID)
			return
		}

		// 处理心跳 ping
		if incoming.Type == "ping" {
			_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "pong"})
			continue
		}

		if incoming.Type != "message" || !adapter.HasMessageInput(incoming.Content, incoming.Attachments) {
			continue
		}
		if err := adapter.ValidateAttachments(incoming.Attachments); err != nil {
			_ = wsjson.Write(r.Context(), conn, wsMessage{
				Type:    "error",
				Content: err.Error(),
			})
			continue
		}

		// 构建统一消息
		msg := &adapter.Message{
			ID:          "web-" + idgen.ShortID(),
			Platform:    adapter.PlatformWeb,
			InstanceID:  a.Name(),
			ChatID:      chatID,
			UserID:      "web-user",
			UserName:    "Web User",
			SessionID:   incoming.SessionID,
			Content:     incoming.Content,
			Attachments: incoming.Attachments,
			Timestamp:   time.Now(),
			Metadata:    make(map[string]string),
		}
		if incoming.Role != "" {
			msg.Metadata["role"] = incoming.Role
		}
		if incoming.Provider != "" {
			msg.Metadata["provider"] = incoming.Provider
		}
		if incoming.Model != "" {
			msg.Metadata["model"] = incoming.Model
		}

		// 异步处理消息
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			// 优先使用流式处理
			if a.streamHandler != nil {
				chunks, err := a.streamHandler(ctx, msg)
				if err != nil {
					log.Printf("Web: 流式处理失败: %v", err)
					errMsg := wsMessage{
						Type:      "error",
						Content:   upstreamerr.PublicMessage(err, "处理消息失败"),
						SessionID: msg.SessionID,
					}
					_ = wsjson.Write(ctx, conn, errMsg)
					return
				}
				if err := a.SendStream(ctx, chatID, chunks); err != nil {
					log.Printf("Web: 流式发送失败: %v", err)
					// 客户端断开 → 取消 context → 通知 pipeStream 停止消费 LLM
					cancel()
					// drain 剩余 chunks 防止 pipeStream 阻塞
					for range chunks {
					}
				}
				return
			}

			// 降级为同步处理
			reply, err := a.handler(ctx, msg)
			if err != nil {
				log.Printf("Web: 处理消息失败: %v", err)
				errMsg := wsMessage{
					Type:      "error",
					Content:   upstreamerr.PublicMessage(err, "处理消息失败"),
					SessionID: msg.SessionID,
				}
				_ = wsjson.Write(ctx, conn, errMsg)
				return
			}

			respMsg := wsMessage{
				Type:      "reply",
				Content:   reply.Content,
				SessionID: msg.SessionID,
				Metadata:  reply.Metadata,
			}
			if err := wsjson.Write(ctx, conn, respMsg); err != nil {
				log.Printf("Web: 发送回复失败: %v", err)
			}
		}()
	}
}

// getConn 获取指定 chatID 的 WebSocket 连接
func (a *WebAdapter) getConn(chatID string) (*websocket.Conn, bool) {
	v, ok := a.conns.Load(chatID)
	if !ok {
		return nil, false
	}
	return v.(*websocket.Conn), true
}

// wsMessage WebSocket 消息格式
type wsMessage struct {
	Type        string               `json:"type"`                  // message / reply / chunk / error
	Content     string               `json:"content"`               // 消息内容
	SessionID   string               `json:"session_id,omitempty"`  // 会话 ID
	Provider    string               `json:"provider,omitempty"`    // 显式指定的 Provider
	Model       string               `json:"model,omitempty"`       // 显式指定的模型
	Role        string               `json:"role,omitempty"`        // Agent 角色
	Done        bool                 `json:"done,omitempty"`        // 流式输出是否结束
	Metadata    map[string]string    `json:"metadata,omitempty"`    // 附加元数据
	Usage       *adapter.Usage       `json:"usage,omitempty"`       // Token 使用统计（仅在 done=true 时）
	ToolCalls   []adapter.ToolCall   `json:"tool_calls,omitempty"`  // 工具调用记录（仅在 done=true 时）
	Attachments []adapter.Attachment `json:"attachments,omitempty"` // 图片附件列表
}

// MarshalJSON 自定义序列化（省略空字段）
func (m wsMessage) MarshalJSON() ([]byte, error) {
	type Alias wsMessage
	return json.Marshal((Alias)(m))
}
