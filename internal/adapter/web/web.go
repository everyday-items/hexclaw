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

	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/toolkit/util/idgen"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// WebAdapter Web UI WebSocket 适配器
//
// 管理 WebSocket 连接，将 Web 消息转换为统一格式。
// 每个 WebSocket 连接分配唯一 chatID。
type WebAdapter struct {
	handler adapter.MessageHandler
	conns   sync.Map // chatID → *websocket.Conn
}

// New 创建 Web 适配器
func New() *WebAdapter {
	return &WebAdapter{}
}

func (a *WebAdapter) Name() string              { return "web" }
func (a *WebAdapter) Platform() adapter.Platform { return adapter.PlatformWeb }

// Start 注册消息处理器
//
// Web 适配器不自己启动 HTTP 服务器，而是通过 Handler() 返回 http.Handler
// 供主 API 服务器挂载到 /ws 路径。
func (a *WebAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	log.Println("Web 适配器已就绪")
	return nil
}

// Stop 关闭所有 WebSocket 连接
func (a *WebAdapter) Stop(_ context.Context) error {
	a.conns.Range(func(key, value any) bool {
		if conn, ok := value.(*websocket.Conn); ok {
			conn.Close(websocket.StatusGoingAway, "服务关闭")
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
		Type:    "reply",
		Content: reply.Content,
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
			Type:    "chunk",
			Content: chunk.Content,
			Done:    chunk.Done,
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
		// 允许跨域（开发模式）
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("WebSocket 握手失败: %v", err)
		return
	}

	chatID := "ws-" + idgen.ShortID()
	a.conns.Store(chatID, conn)
	defer func() {
		a.conns.Delete(chatID)
		conn.Close(websocket.StatusNormalClosure, "")
	}()

	log.Printf("WebSocket 连接建立: %s", chatID)

	// 读取消息循环
	for {
		var incoming wsMessage
		if err := wsjson.Read(r.Context(), conn, &incoming); err != nil {
			// 连接关闭
			log.Printf("WebSocket 连接断开: %s", chatID)
			return
		}

		if incoming.Type != "message" || incoming.Content == "" {
			continue
		}

		// 构建统一消息
		msg := &adapter.Message{
			ID:        "web-" + idgen.ShortID(),
			Platform:  adapter.PlatformWeb,
			ChatID:    chatID,
			UserID:    "web-user", // Web 用户暂用默认 ID
			UserName:  "Web User",
			SessionID: incoming.SessionID,
			Content:   incoming.Content,
			Timestamp: time.Now(),
		}

		// 异步处理消息
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			reply, err := a.handler(ctx, msg)
			if err != nil {
				log.Printf("Web: 处理消息失败: %v", err)
				errMsg := wsMessage{Type: "error", Content: "处理消息时出现错误，请稍后重试。"}
				_ = wsjson.Write(ctx, conn, errMsg)
				return
			}

			respMsg := wsMessage{
				Type:      "reply",
				Content:   reply.Content,
				SessionID: msg.SessionID,
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
	Type      string `json:"type"`                 // message / reply / chunk / error
	Content   string `json:"content"`              // 消息内容
	SessionID string `json:"session_id,omitempty"`  // 会话 ID
	Done      bool   `json:"done,omitempty"`        // 流式输出是否结束
}

// MarshalJSON 自定义序列化（省略空字段）
func (m wsMessage) MarshalJSON() ([]byte, error) {
	type Alias wsMessage
	return json.Marshal((Alias)(m))
}
