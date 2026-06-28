// Package web 提供 Web UI WebSocket 适配器
//
// 通过 WebSocket 实现 Web 前端与 HexClaw 引擎的实时双向通信。
// 支持同步回复、流式输出、以及请求级流恢复。
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/internal/upstreamerr"
	"github.com/hexagon-codes/hexclaw/streamstate"
	"github.com/hexagon-codes/toolkit/util/idgen"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// WebAdapter Web UI WebSocket 适配器。
//
// 管理 WebSocket 连接，将 Web 消息转换为统一格式。
// 每个 WebSocket 连接分配唯一 chatID；每个流式请求分配唯一 requestID。
type WebAdapter struct {
	handler            adapter.MessageHandler
	streamHandler      adapter.StreamMessageHandler
	conns              sync.Map // chatID → *websocket.Conn
	sessionConns       sync.Map // sessionID → chatID (for permission requests)
	sessionRequests    sync.Map // sessionID → requestID
	requestConns       sync.Map // requestID → *requestSubscribers
	cancelFuncs        sync.Map // requestID → context.CancelFunc
	streams            *streamstate.Registry
	onApprovalResponse func(requestID string, approved, remember bool) // callback for tool approval
}

type requestSubscribers struct {
	mu      sync.RWMutex
	chatIDs map[string]struct{}
}

func newRequestSubscribers() *requestSubscribers {
	return &requestSubscribers{chatIDs: make(map[string]struct{})}
}

func (s *requestSubscribers) Add(chatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chatIDs[chatID] = struct{}{}
}

func (s *requestSubscribers) Delete(chatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.chatIDs, chatID)
}

func (s *requestSubscribers) Snapshot() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.chatIDs))
	for chatID := range s.chatIDs {
		out = append(out, chatID)
	}
	return out
}

func (s *requestSubscribers) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.chatIDs)
}

// SetStreamHandler 设置流式消息处理器。
func (a *WebAdapter) SetStreamHandler(h adapter.StreamMessageHandler) {
	a.streamHandler = h
}

// New 创建 Web 适配器。
func New() *WebAdapter {
	return &WebAdapter{streams: streamstate.NewRegistry(2 * time.Minute)}
}

func (a *WebAdapter) Name() string               { return "web" }
func (a *WebAdapter) Platform() adapter.Platform { return adapter.PlatformWeb }

// Start 注册消息处理器。
func (a *WebAdapter) Start(_ context.Context, handler adapter.MessageHandler) error {
	a.handler = handler
	return nil
}

// Stop 关闭所有 WebSocket 连接。
func (a *WebAdapter) Stop(_ context.Context) error {
	a.conns.Range(func(key, value any) bool {
		if conn, ok := value.(*websocket.Conn); ok {
			_ = conn.Close(websocket.StatusGoingAway, "服务关闭")
		}
		a.conns.Delete(key)
		return true
	})
	slog.Info("Web 适配器已停止")
	return nil
}

// ListActiveStreams 返回指定用户当前仍在进行中的流式请求。
func (a *WebAdapter) ListActiveStreams(userID string) []streamstate.Snapshot {
	if a.streams == nil {
		return nil
	}
	return a.streams.ListActiveStreams(userID)
}

// GetStreamSnapshot 返回指定请求的最新快照，用于刷新/断线恢复。
func (a *WebAdapter) GetStreamSnapshot(userID, requestID string) (*streamstate.Snapshot, bool) {
	if a.streams == nil {
		return nil, false
	}
	return a.streams.GetStreamSnapshot(userID, requestID)
}

// SetApprovalResponseHandler sets the callback for tool approval responses from the frontend.
func (a *WebAdapter) SetApprovalResponseHandler(fn func(requestID string, approved, remember bool)) {
	a.onApprovalResponse = fn
}

// PermissionRequestData is the data needed to send a tool approval request.
type PermissionRequestData struct {
	ID        string
	ToolName  string
	Arguments map[string]any
	Risk      string
	Reason    string
}

// SendPermissionRequest sends a tool approval request to the frontend via WebSocket.
func (a *WebAdapter) SendPermissionRequest(ctx context.Context, sessionID string, data *PermissionRequestData) error {
	chatID, ok := a.sessionConns.Load(sessionID)
	if !ok {
		return fmt.Errorf("no WebSocket connection for session %s", sessionID)
	}
	conn, ok := a.getConn(chatID.(string))
	if !ok {
		return fmt.Errorf("WebSocket connection %s disconnected", chatID)
	}
	msg := wsMessage{
		Type:      "tool_approval_request",
		SessionID: sessionID,
		RequestID: data.ID,
		Content:   data.Reason,
		Metadata: map[string]string{
			"request_id": data.ID,
			"tool_name":  data.ToolName,
			"risk":       data.Risk,
		},
	}
	return wsjson.Write(ctx, conn, msg)
}

// Broadcast 向所有活跃 WebSocket 连接广播消息。
func (a *WebAdapter) Broadcast(msgType, content string, metadata map[string]string) {
	msg := wsMessage{
		Type:     msgType,
		Content:  content,
		Metadata: metadata,
	}
	a.conns.Range(func(_, value any) bool {
		if conn, ok := value.(*websocket.Conn); ok {
			_ = wsjson.Write(context.Background(), conn, msg)
		}
		return true
	})
}

// Handler 返回 WebSocket HTTP Handler。
func (a *WebAdapter) Handler() http.Handler {
	return http.HandlerFunc(a.handleWS)
}

// Send 发送同步回复到指定连接。
func (a *WebAdapter) Send(ctx context.Context, chatID string, reply *adapter.Reply) error {
	conn, ok := a.getConn(chatID)
	if !ok {
		return nil
	}
	msg := wsMessage{Type: "reply", Content: reply.Content, Metadata: reply.Metadata}
	if reply.Metadata != nil {
		msg.SessionID = reply.Metadata["session_id"]
		msg.RequestID = reply.Metadata["request_id"]
	}
	return wsjson.Write(ctx, conn, msg)
}

// SendStream 流式发送回复（兼容 Adapter 接口）。
func (a *WebAdapter) SendStream(ctx context.Context, chatID string, chunks <-chan *adapter.ReplyChunk) error {
	return a.sendStreamWithIDs(ctx, chatID, "", "", chunks)
}

func (a *WebAdapter) sendStreamWithIDs(ctx context.Context, chatID, sessionID, requestID string, chunks <-chan *adapter.ReplyChunk) error {
	chunkCount := 0
	reasoningCount := 0
	for chunk := range chunks {
		if chunk.Error != nil {
			if requestID != "" && a.streams != nil {
				a.streams.Fail(requestID, chunk.Error)
			}
			errMsg := wsMessage{
				Type: "error",
				// 与启动失败路径(web.go streamHandler err)及 SSE/HTTP 一致：净化上游错误，
				// 不把 provider 原始 JSON / 内部前缀灌进聊天气泡。
				Content:   upstreamerr.PublicMessage(chunk.Error, "error"),
				SessionID: sessionID,
				RequestID: requestID,
			}
			_ = a.sendToTargets(ctx, chatID, requestID, errMsg)
			return chunk.Error
		}

		chunkCount++
		if chunk.Reasoning != "" {
			reasoningCount++
			if reasoningCount == 1 {
				trace.L(ctx).Info("首个 chunk", "type", "reasoning", "chunk_count", chunkCount)
			}
		}
		if requestID != "" && a.streams != nil {
			a.streams.Append(requestID, chunk)
		}

		msg := wsMessage{
			Type:      "chunk",
			Content:   chunk.Content,
			Reasoning: chunk.Reasoning,
			Done:      chunk.Done,
			SessionID: sessionID,
			RequestID: requestID,
			Metadata:  chunk.Metadata,
			Usage:     chunk.Usage,
			ToolCalls: chunk.ToolCalls,
			Blocks:    chunk.Blocks,
		}
		if msg.Metadata != nil {
			if msg.SessionID == "" {
				msg.SessionID = msg.Metadata["session_id"]
			}
			if msg.RequestID == "" {
				msg.RequestID = msg.Metadata["request_id"]
			}
		}
		if err := a.sendToTargets(ctx, chatID, requestID, msg); err != nil {
			return err
		}
	}
	trace.L(ctx).Info("流式输出完成", "chunks", chunkCount, "reasoning_chunks", reasoningCount)
	return nil
}

// handleWS 处理 WebSocket 连接。
func (a *WebAdapter) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		slog.Error("WebSocket 握手失败", "err", err)
		return
	}

	conn.SetReadLimit(20 * 1024 * 1024)

	chatID := "ws-" + idgen.ShortID()
	a.conns.Store(chatID, conn)
	defer func() {
		a.conns.Delete(chatID)
		a.removeChatID(chatID)
		a.sessionConns.Range(func(key, value any) bool {
			if value.(string) == chatID {
				a.sessionConns.Delete(key)
			}
			return true
		})
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	slog.Info("WebSocket 连接建立", "chat_id", chatID)

	for {
		var incoming wsMessage
		if err := wsjson.Read(r.Context(), conn, &incoming); err != nil {
			slog.Info("WebSocket 连接断开", "chat_id", chatID)
			return
		}

		switch incoming.Type {
		case "ping":
			_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "pong"})
			continue
		case "cancel":
			requestID := stringsTrim(incoming.RequestID)
			if requestID == "" && incoming.SessionID != "" {
				if bound, ok := a.sessionRequests.Load(incoming.SessionID); ok {
					requestID = bound.(string)
				}
			}
			slog.Info("WebSocket cancel", "session", incoming.SessionID, "request_id", requestID)
			if requestID != "" {
				if cancelFn, ok := a.cancelFuncs.LoadAndDelete(requestID); ok {
					cancelFn.(context.CancelFunc)()
				}
				if a.streams != nil {
					a.streams.Cancel(requestID)
				}
			}
			continue
		case "resume":
			userID := incoming.UserID
			if userID == "" {
				userID = "web-user"
			}
			snapshot, ok := a.GetStreamSnapshot(userID, incoming.RequestID)
			if !ok {
				_ = wsjson.Write(r.Context(), conn, wsMessage{
					Type:      "error",
					Content:   "stream not found",
					RequestID: incoming.RequestID,
				})
				continue
			}
			if !snapshot.Done {
				a.addSubscriber(snapshot.RequestID, chatID)
				a.sessionConns.Store(snapshot.SessionID, chatID)
				a.sessionRequests.Store(snapshot.SessionID, snapshot.RequestID)
			}
			_ = wsjson.Write(r.Context(), conn, snapshotToMessage(snapshot))
			continue
		case "tool_approval_response":
			if a.onApprovalResponse != nil {
				reqID, _ := incoming.Metadata["request_id"]
				approved := incoming.Content == "approved"
				remember := incoming.Content == "approved_remember"
				a.onApprovalResponse(reqID, approved || remember, remember)
			}
			continue
		}

		if incoming.Type != "message" || !adapter.HasMessageInput(incoming.Content, incoming.Attachments) {
			continue
		}
		if err := adapter.ValidateAttachments(incoming.Attachments); err != nil {
			// 本地输入校验错（确定性、非敏感、对用户有用）原样展示——不走 PublicMessage（AP-070 粒度修正：Validate* 帧白名单豁免）。
			_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "error", Content: err.Error()})
			continue
		}
		if incoming.RequestID == "" {
			incoming.RequestID = "req-" + idgen.ShortID()
		}

		msg := buildAdapterMessage(chatID, incoming)
		msg.InstanceID = a.Name()
		if incoming.SessionID != "" {
			a.sessionConns.Store(incoming.SessionID, chatID)
			a.sessionRequests.Store(incoming.SessionID, incoming.RequestID)
		}

		logger := trace.NewRequest(msg.UserID, msg.SessionID).With("source", "chat", "provider", incoming.Provider, "model", incoming.Model, "request_id", incoming.RequestID)

		go func(incoming wsMessage, msg *adapter.Message) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			if incoming.RequestID != "" {
				a.cancelFuncs.Store(incoming.RequestID, cancel)
				defer a.cancelFuncs.Delete(incoming.RequestID)
			}

			ctx = trace.WithLogger(ctx, logger)
			logger.Info("← 收到消息", "content_len", len([]rune(msg.Content)), "platform", string(msg.Platform), "attachments", len(msg.Attachments))

			if a.streamHandler != nil {
				a.addSubscriber(incoming.RequestID, chatID)
				if a.streams != nil {
					a.streams.Start(msg.UserID, msg.SessionID, incoming.RequestID)
				}
				chunks, err := a.streamHandler(ctx, msg)
				if err != nil {
					if a.streams != nil {
						a.streams.Fail(incoming.RequestID, err)
					}
					trace.L(ctx).Error("流式处理失败", "err", err)
					errMsg := wsMessage{
						Type:      "error",
						Content:   upstreamerr.PublicMessage(err, "error"),
						SessionID: msg.SessionID,
						RequestID: incoming.RequestID,
					}
					_ = a.sendToTargets(ctx, chatID, incoming.RequestID, errMsg)
					return
				}
				if err := a.sendStreamWithIDs(ctx, chatID, msg.SessionID, incoming.RequestID, chunks); err != nil {
					trace.L(ctx).Error("流式发送失败", "err", err)
					cancel()
					for range chunks {
					}
				}
				return
			}

			reply, err := a.handler(ctx, msg)
			if err != nil {
				trace.L(ctx).Error("error", "err", err)
				errMsg := wsMessage{
					Type:      "error",
					Content:   upstreamerr.PublicMessage(err, "error"),
					SessionID: msg.SessionID,
					RequestID: incoming.RequestID,
				}
				_ = a.sendToTargets(ctx, chatID, "", errMsg)
				return
			}
			respMsg := wsMessage{
				Type:      "reply",
				Content:   reply.Content,
				SessionID: msg.SessionID,
				RequestID: incoming.RequestID,
				Metadata:  reply.Metadata,
				Usage:     reply.Usage,
				ToolCalls: reply.ToolCalls,
				Blocks:    reply.Blocks,
			}
			if reply.Metadata == nil {
				respMsg.Metadata = map[string]string{}
			}
			respMsg.Metadata["request_id"] = incoming.RequestID
			if respMsg.Metadata["session_id"] == "" && msg.SessionID != "" {
				respMsg.Metadata["session_id"] = msg.SessionID
			}
			if err := a.sendToTargets(ctx, chatID, "", respMsg); err != nil {
				trace.L(ctx).Error("error", "err", err)
			}
		}(incoming, msg)
	}
}

func (a *WebAdapter) addSubscriber(requestID, chatID string) {
	if requestID == "" || chatID == "" {
		return
	}
	value, _ := a.requestConns.LoadOrStore(requestID, newRequestSubscribers())
	value.(*requestSubscribers).Add(chatID)
}

func (a *WebAdapter) removeChatID(chatID string) {
	a.requestConns.Range(func(key, value any) bool {
		subs := value.(*requestSubscribers)
		subs.Delete(chatID)
		if subs.Len() == 0 {
			a.requestConns.Delete(key)
		}
		return true
	})
}

func (a *WebAdapter) sendToTargets(ctx context.Context, chatID, requestID string, msg wsMessage) error {
	if requestID == "" {
		return a.writeMessage(ctx, chatID, msg)
	}
	value, ok := a.requestConns.Load(requestID)
	if !ok {
		return a.writeMessage(ctx, chatID, msg)
	}
	var firstErr error
	for _, targetID := range value.(*requestSubscribers).Snapshot() {
		if err := a.writeMessage(ctx, targetID, msg); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (a *WebAdapter) writeMessage(ctx context.Context, chatID string, msg wsMessage) error {
	conn, ok := a.getConn(chatID)
	if !ok {
		return nil
	}
	return wsjson.Write(ctx, conn, msg)
}

func snapshotToMessage(snapshot *streamstate.Snapshot) wsMessage {
	if snapshot == nil {
		return wsMessage{Type: "error", Content: "stream not found"}
	}
	return wsMessage{
		Type:      "stream_snapshot",
		Content:   snapshot.Content,
		Reasoning: snapshot.Reasoning,
		SessionID: snapshot.SessionID,
		RequestID: snapshot.RequestID,
		Done:      snapshot.Done,
		Metadata:  snapshot.Metadata,
		Usage:     snapshot.Usage,
		ToolCalls: snapshot.ToolCalls,
		Blocks:    snapshot.Blocks,
	}
}

func stringsTrim(v string) string {
	return v
}

// getConn 获取指定 chatID 的 WebSocket 连接。
func (a *WebAdapter) getConn(chatID string) (*websocket.Conn, bool) {
	v, ok := a.conns.Load(chatID)
	if !ok {
		return nil, false
	}
	return v.(*websocket.Conn), true
}

// wsMessage WebSocket 消息格式。
type wsMessage struct {
	Type        string               `json:"type"` // message / reply / chunk / error / resume / stream_snapshot
	Content     string               `json:"content"`
	Reasoning   string               `json:"reasoning,omitempty"`
	SessionID   string               `json:"session_id,omitempty"`
	RequestID   string               `json:"request_id,omitempty"`
	UserID      string               `json:"user_id,omitempty"`
	Provider    string               `json:"provider,omitempty"`
	Model       string               `json:"model,omitempty"`
	Role        string               `json:"role,omitempty"`
	Done        bool                 `json:"done,omitempty"`
	Metadata    map[string]string    `json:"metadata,omitempty"`
	Usage       *adapter.Usage       `json:"usage,omitempty"`
	ToolCalls   []adapter.ToolCall   `json:"tool_calls,omitempty"`
	Blocks      []adapter.Block      `json:"blocks,omitempty"`
	Attachments []adapter.Attachment `json:"attachments,omitempty"`
}

// MarshalJSON 自定义序列化（省略空字段）。
func (m wsMessage) MarshalJSON() ([]byte, error) {
	type Alias wsMessage
	return json.Marshal((Alias)(m))
}

func buildAdapterMessage(chatID string, incoming wsMessage) *adapter.Message {
	userID := incoming.UserID
	if userID == "" {
		userID = "web-user"
	}
	metadata := make(map[string]string, len(incoming.Metadata)+4)
	for k, v := range incoming.Metadata {
		metadata[k] = v
	}
	if incoming.RequestID != "" {
		metadata["request_id"] = incoming.RequestID
	}
	if incoming.Role != "" {
		metadata["role"] = incoming.Role
	}
	if incoming.Provider != "" {
		metadata["provider"] = incoming.Provider
	}
	if incoming.Model != "" {
		metadata["model"] = incoming.Model
	}

	return &adapter.Message{
		ID:          "web-" + idgen.ShortID(),
		Platform:    adapter.PlatformWeb,
		ChatID:      chatID,
		UserID:      userID,
		UserName:    "Web User",
		SessionID:   incoming.SessionID,
		Content:     incoming.Content,
		Attachments: incoming.Attachments,
		Timestamp:   time.Now(),
		Metadata:    metadata,
	}
}
