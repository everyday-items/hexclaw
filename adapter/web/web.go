// Package web 提供 Web UI WebSocket 适配器
//
// 通过 WebSocket 实现 Web 前端与 HexClaw 引擎的实时双向通信。
// 支持同步回复、流式输出、以及请求级流恢复。
package web

import (
	"context"
	"encoding/json"
	"errors"
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
	disconnectTimers   sync.Map // requestID → *time.Timer
	disconnectGrace    time.Duration
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

func (s *requestSubscribers) IfEmpty(fn func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.chatIDs) != 0 {
		return false
	}
	fn()
	return true
}

// SetStreamHandler 设置流式消息处理器。
func (a *WebAdapter) SetStreamHandler(h adapter.StreamMessageHandler) {
	a.streamHandler = h
}

// New 创建 Web 适配器。
func New() *WebAdapter {
	return &WebAdapter{
		streams:         streamstate.NewRegistry(2 * time.Minute),
		disconnectGrace: 5 * time.Second,
	}
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
	a.disconnectTimers.Range(func(key, value any) bool {
		if timer, ok := value.(*time.Timer); ok {
			timer.Stop()
		}
		a.disconnectTimers.Delete(key)
		return true
	})
	a.cancelFuncs.Range(func(key, value any) bool {
		if cancel, ok := value.(context.CancelFunc); ok {
			cancel()
		}
		a.cancelFuncs.Delete(key)
		return true
	})
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
	if data == nil {
		return errors.New("permission request data is nil")
	}
	msg := permissionRequestMessage(sessionID, data)
	chatID, ok := a.sessionConns.Load(sessionID)
	if !ok {
		return a.broadcastPermissionRequest(ctx, msg, "", fmt.Errorf("no WebSocket connection for session %s", sessionID))
	}
	chatIDStr, ok := chatID.(string)
	if !ok || chatIDStr == "" {
		return a.broadcastPermissionRequest(ctx, msg, "", fmt.Errorf("invalid WebSocket connection binding for session %s", sessionID))
	}
	conn, ok := a.getConn(chatIDStr)
	if !ok {
		return a.broadcastPermissionRequest(ctx, msg, chatIDStr, fmt.Errorf("WebSocket connection %s disconnected", chatIDStr))
	}
	if err := wsjson.Write(ctx, conn, msg); err != nil {
		return a.broadcastPermissionRequest(ctx, msg, chatIDStr, err)
	}
	return nil
}

func permissionRequestMessage(sessionID string, data *PermissionRequestData) wsMessage {
	return wsMessage{
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
}

func (a *WebAdapter) broadcastPermissionRequest(ctx context.Context, msg wsMessage, excludeChatID string, routeErr error) error {
	sent := 0
	var firstErr error
	a.conns.Range(func(key, value any) bool {
		chatID, _ := key.(string)
		if chatID == excludeChatID {
			return true
		}
		conn, ok := value.(*websocket.Conn)
		if !ok {
			return true
		}
		if err := wsjson.Write(ctx, conn, msg); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return true
		}
		sent++
		return true
	})
	if sent > 0 {
		if routeErr != nil {
			slog.Warn("审批请求使用 WebSocket 广播兜底", "session_id", msg.SessionID, "request_id", msg.RequestID, "err", routeErr)
		}
		return nil
	}
	if firstErr != nil {
		return firstErr
	}
	if routeErr != nil {
		return routeErr
	}
	return fmt.Errorf("no active WebSocket connections for session %s", msg.SessionID)
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
	msg := wsMessage{Type: "reply", Content: reply.Content, Metadata: reply.Metadata, KnowledgeHits: reply.KnowledgeHits, MemoryHits: reply.MemoryHits} // U9
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
	if sessionID != "" {
		a.sessionConns.Store(sessionID, chatID)
		a.sessionRequests.Store(sessionID, requestID)
	}
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
			Type:          "chunk",
			Content:       chunk.Content,
			Reasoning:     chunk.Reasoning,
			Done:          chunk.Done,
			SessionID:     sessionID,
			RequestID:     requestID,
			Metadata:      chunk.Metadata,
			Usage:         chunk.Usage,
			ToolCalls:     chunk.ToolCalls,
			Blocks:        chunk.Blocks,
			KnowledgeHits: chunk.KnowledgeHits, // U9
			MemoryHits:    chunk.MemoryHits,
		}
		if msg.Metadata != nil {
			if msg.SessionID == "" {
				msg.SessionID = msg.Metadata["session_id"]
			}
			if msg.RequestID == "" {
				msg.RequestID = msg.Metadata["request_id"]
			}
		}
		if msg.SessionID != "" {
			a.sessionConns.Store(msg.SessionID, chatID)
			a.sessionRequests.Store(msg.SessionID, requestID)
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
				a.stopDisconnectTimer(requestID)
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
				reqID := incoming.Metadata["request_id"]
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

		msg, err := buildAdapterMessage(chatID, incoming)
		if err != nil {
			_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "error", Content: err.Error()})
			continue
		}
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
				defer func() { a.finishRequest(incoming.RequestID, msg.SessionID) }()
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
				Type:          "reply",
				Content:       reply.Content,
				SessionID:     msg.SessionID,
				RequestID:     incoming.RequestID,
				Metadata:      reply.Metadata,
				Usage:         reply.Usage,
				ToolCalls:     reply.ToolCalls,
				Blocks:        reply.Blocks,
				KnowledgeHits: reply.KnowledgeHits, // U9
				MemoryHits:    reply.MemoryHits,
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
	a.stopDisconnectTimer(requestID)
	value, _ := a.requestConns.LoadOrStore(requestID, newRequestSubscribers())
	value.(*requestSubscribers).Add(chatID)
}

func (a *WebAdapter) removeChatID(chatID string) {
	a.requestConns.Range(func(key, value any) bool {
		requestID, _ := key.(string)
		subs := value.(*requestSubscribers)
		subs.Delete(chatID)
		if subs.Len() == 0 {
			a.scheduleDisconnectCancel(requestID, subs)
		}
		return true
	})
}

func (a *WebAdapter) stopDisconnectTimer(requestID string) {
	if value, ok := a.disconnectTimers.LoadAndDelete(requestID); ok {
		value.(*time.Timer).Stop()
	}
}

func (a *WebAdapter) scheduleDisconnectCancel(requestID string, subs *requestSubscribers) {
	if requestID == "" || subs == nil {
		return
	}
	if _, active := a.cancelFuncs.Load(requestID); !active {
		a.requestConns.Delete(requestID)
		return
	}
	grace := a.disconnectGrace
	if grace < 0 {
		grace = 0
	}
	timer := time.AfterFunc(grace, func() {
		a.disconnectTimers.Delete(requestID)
		current, ok := a.requestConns.Load(requestID)
		if !ok || current != subs {
			return
		}
		canceled := subs.IfEmpty(func() {
			if cancelFn, ok := a.cancelFuncs.LoadAndDelete(requestID); ok {
				cancelFn.(context.CancelFunc)()
			}
			if a.streams != nil {
				a.streams.Cancel(requestID)
			}
		})
		if canceled {
			a.requestConns.Delete(requestID)
			slog.Info("WebSocket 请求因无订阅者取消", "request_id", requestID, "grace", grace)
		}
	})
	if existing, loaded := a.disconnectTimers.LoadOrStore(requestID, timer); loaded {
		timer.Stop()
		_ = existing
	}
}

func (a *WebAdapter) finishRequest(requestID, sessionID string) {
	a.stopDisconnectTimer(requestID)
	a.requestConns.Delete(requestID)
	if sessionID == "" {
		return
	}
	if current, ok := a.sessionRequests.Load(sessionID); ok && current == requestID {
		a.sessionRequests.Delete(sessionID)
	}
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
	Temperature *float64             `json:"temperature,omitempty"`
	MaxTokens   *int                 `json:"max_tokens,omitempty"`
	Done        bool                 `json:"done,omitempty"`
	Metadata    map[string]string    `json:"metadata,omitempty"`
	Usage       *adapter.Usage       `json:"usage,omitempty"`
	ToolCalls   []adapter.ToolCall   `json:"tool_calls,omitempty"`
	Blocks      []adapter.Block      `json:"blocks,omitempty"`
	Attachments []adapter.Attachment `json:"attachments,omitempty"`
	// U9：结构化 RAG/记忆命中（随 done chunk / reply 回传前端渲染命中标签+详情）。
	KnowledgeHits []adapter.KnowledgeHit `json:"knowledge_hits,omitempty"`
	MemoryHits    []adapter.MemoryHit    `json:"memory_hits,omitempty"`
}

// MarshalJSON 自定义序列化（省略空字段）。
func (m wsMessage) MarshalJSON() ([]byte, error) {
	type Alias wsMessage
	return json.Marshal((Alias)(m))
}

func buildAdapterMessage(chatID string, incoming wsMessage) (*adapter.Message, error) {
	userID := incoming.UserID
	if userID == "" {
		userID = "web-user"
	}
	metadata := make(map[string]string, len(incoming.Metadata)+4)
	for k, v := range incoming.Metadata {
		metadata[k] = v
	}
	// GO-3：WebSocket 入站是信任边界——剥除只能由受信内部派发器盖章的保留键，
	// 否则客户端可伪造 source=cron + cron_job_id 盗用他人任务的授权（提权）。
	adapter.StripReservedDispatchMetadata(metadata)
	if err := adapter.ApplyRequestSamplingOverrides(metadata, incoming.Temperature, incoming.MaxTokens); err != nil {
		return nil, err
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
	}, nil
}
