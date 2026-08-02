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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/hexagon/observe/trace"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/internal/upstreamerr"
	"github.com/hexagon-codes/hexclaw/messagecontent"
	"github.com/hexagon-codes/hexclaw/skill"
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
	handler                   adapter.MessageHandler
	streamHandler             adapter.StreamMessageHandler
	attachmentResolver        func(context.Context, string, []adapter.Attachment) ([]adapter.Attachment, error)
	conns                     sync.Map // chatID → *websocket.Conn
	connectionOwners          sync.Map // chatID → authenticated ownerID
	sessionConns              sync.Map // sessionID → sessionConnectionBinding
	sessionRequests           sync.Map // sessionID → requestID
	requestOwners             sync.Map // requestID → authenticated ownerID
	requestConns              sync.Map // requestID → *requestSubscribers
	cancelFuncs               sync.Map // requestID → context.CancelFunc
	disconnectTimers          sync.Map // requestID → *time.Timer
	disconnectGrace           time.Duration
	streams                   *streamstate.Registry
	onApprovalResponse        func(requestID string, approved, remember bool) // callback for tool approval
	onApprovalDecision        func(ApprovalResponseData) string
	onDurableApprovalDecision func(ApprovalResponseData) ApprovalDecisionReceipt
	pendingApprovalReplay     func(context.Context, string, string) []*PermissionRequestData
	approvalBindings          sync.Map // requestID -> approvalTransportBinding
	approvalACKMu             sync.Mutex
	approvalACKs              map[string]approvalACKRecord // bounded idempotency key -> terminal ACK
	approvalACKLimit          int
	approvalACKTTL            time.Duration
	approvalACKSeq            uint64
}

type requestSubscribers struct {
	mu      sync.RWMutex
	chatIDs map[string]struct{}
}

type sessionConnectionBinding struct {
	chatID  string
	ownerID string
}

type approvalTransportBinding struct {
	requestID           string
	ownerID             string
	sessionID           string
	chatID              string
	invocationID        string
	argumentsDigest     string
	securityScopeDigest string
	scopeSchemaVersion  int
	expiresAt           time.Time
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

// SetAttachmentResolver installs the Sidecar-owned staging resolver. WebSocket
// frames carry only opaque attachment IDs; metadata and bytes are recovered
// under the authenticated principal before they reach the engine.
func (a *WebAdapter) SetAttachmentResolver(
	resolver func(context.Context, string, []adapter.Attachment) ([]adapter.Attachment, error),
) {
	a.attachmentResolver = resolver
}

// New 创建 Web 适配器。
func New() *WebAdapter {
	return &WebAdapter{
		streams:          streamstate.NewRegistry(2 * time.Minute),
		disconnectGrace:  5 * time.Second,
		approvalACKs:     make(map[string]approvalACKRecord),
		approvalACKLimit: 1024,
		approvalACKTTL:   5 * time.Minute,
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
	for _, state := range []*sync.Map{
		&a.connectionOwners,
		&a.sessionConns,
		&a.sessionRequests,
		&a.requestOwners,
		&a.requestConns,
		&a.approvalBindings,
	} {
		clearSyncMap(state)
	}
	a.approvalACKMu.Lock()
	a.approvalACKs = make(map[string]approvalACKRecord)
	a.approvalACKMu.Unlock()
	slog.Info("Web 适配器已停止")
	return nil
}

func clearSyncMap(state *sync.Map) {
	state.Range(func(key, _ any) bool {
		state.Delete(key)
		return true
	})
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

// ApprovalResponseData carries one immutable approval decision identity.
type ApprovalResponseData struct {
	RequestID           string
	OwnerID             string
	SessionID           string
	DecisionID          string
	InvocationID        string
	Decision            string
	IdempotencyKey      string
	ArgumentsDigest     string
	SecurityScopeDigest string
	ScopeSchemaVersion  int
	Approved            bool
	Remember            bool
	responderChatID     string
}

// SetApprovalDecisionHandler installs the durable coordinator callback used
// before a terminal ACK is emitted.
func (a *WebAdapter) SetApprovalDecisionHandler(fn func(ApprovalResponseData) string) {
	a.onApprovalDecision = fn
}

// ApprovalDecisionReceipt is the transport-safe projection of the backend's
// durable ACK record. WebAdapter validates and serializes it but never decides
// authorization from its own maps or caches.
type ApprovalDecisionReceipt struct {
	RequestID           string
	InvocationID        string
	OwnerID             string
	SessionID           string
	DecisionID          string
	Decision            string
	IdempotencyKey      string
	ArgumentsDigest     string
	SecurityScopeDigest string
	ScopeSchemaVersion  int
	TerminalResult      string
	ACKStatus           string
	Replayed            bool
}

// SetDurableApprovalDecisionHandler installs the sole production approval
// authority. The legacy string callback remains only for non-durable adapters.
func (a *WebAdapter) SetDurableApprovalDecisionHandler(
	fn func(ApprovalResponseData) ApprovalDecisionReceipt,
) {
	a.onDurableApprovalDecision = fn
}

// SetPendingApprovalReplayHandler installs the backend pending projection used
// when an authenticated session moves to a new physical WebSocket.
func (a *WebAdapter) SetPendingApprovalReplayHandler(
	fn func(context.Context, string, string) []*PermissionRequestData,
) {
	a.pendingApprovalReplay = fn
}

// PermissionRequestData is the data needed to send a tool approval request.
type PermissionRequestData struct {
	ID                  string
	OwnerID             string
	InvocationID        string
	ToolName            string
	Arguments           map[string]any
	ArgumentsDigest     string
	SecurityScopeDigest string
	ScopeSchemaVersion  int
	DeadlineAt          time.Time
	Risk                string
	Reason              string
}

// SendPermissionRequest sends a tool approval request to the frontend via WebSocket.
func (a *WebAdapter) SendPermissionRequest(ctx context.Context, sessionID string, data *PermissionRequestData) error {
	if data == nil {
		return errors.New("permission request data is nil")
	}
	msg := permissionRequestMessage(sessionID, data)
	value, ok := a.sessionConns.Load(sessionID)
	if !ok {
		return fmt.Errorf("no WebSocket connection for session %s", sessionID)
	}
	binding, ok := value.(sessionConnectionBinding)
	if !ok || binding.chatID == "" || binding.ownerID == "" {
		return fmt.Errorf("invalid WebSocket connection binding for session %s", sessionID)
	}
	if data.OwnerID == "" || data.OwnerID != binding.ownerID {
		return fmt.Errorf("approval owner does not own session %s", sessionID)
	}
	transportBinding := approvalTransportBinding{
		requestID: data.ID, ownerID: data.OwnerID, sessionID: sessionID, chatID: binding.chatID,
		invocationID: data.InvocationID, argumentsDigest: data.ArgumentsDigest,
		securityScopeDigest: data.SecurityScopeDigest, scopeSchemaVersion: data.ScopeSchemaVersion,
		expiresAt: data.DeadlineAt,
	}
	if !transportBinding.valid() {
		return errors.New("approval request has incomplete transport identity")
	}
	if existing, loaded := a.approvalBindings.LoadOrStore(data.ID, transportBinding); loaded && existing != transportBinding {
		return fmt.Errorf("approval request %s already has a different transport binding", data.ID)
	}
	conn, ok := a.getConn(binding.chatID)
	if !ok {
		a.approvalBindings.Delete(data.ID)
		return fmt.Errorf("WebSocket connection %s disconnected", binding.chatID)
	}
	if err := wsjson.Write(ctx, conn, msg); err != nil {
		a.approvalBindings.Delete(data.ID)
		return err
	}
	return nil
}

func (b approvalTransportBinding) valid() bool {
	return b.requestID != "" && b.ownerID != "" && b.sessionID != "" && b.chatID != "" &&
		b.invocationID != "" && b.argumentsDigest != "" && b.securityScopeDigest != "" &&
		b.scopeSchemaVersion > 0 && !b.expiresAt.IsZero() && time.Now().Before(b.expiresAt)
}

func permissionRequestMessage(sessionID string, data *PermissionRequestData) wsMessage {
	return wsMessage{
		Type:                "tool_approval_request",
		SessionID:           sessionID,
		RequestID:           data.ID,
		OwnerID:             data.OwnerID,
		Content:             data.Reason,
		Arguments:           data.Arguments,
		InvocationID:        data.InvocationID,
		ToolName:            data.ToolName,
		ArgumentsDigest:     data.ArgumentsDigest,
		SecurityScopeDigest: data.SecurityScopeDigest,
		ScopeSchemaVersion:  data.ScopeSchemaVersion,
		DeadlineAt:          data.DeadlineAt.Format(time.RFC3339Nano),
		Metadata: map[string]string{
			"request_id":            data.ID,
			"approval_request_id":   data.ID,
			"invocation_id":         data.InvocationID,
			"tool_name":             data.ToolName,
			"risk":                  data.Risk,
			"arguments_digest":      data.ArgumentsDigest,
			"security_scope_digest": data.SecurityScopeDigest,
			"scope_schema_version":  fmt.Sprintf("%d", data.ScopeSchemaVersion),
			"deadline_at":           data.DeadlineAt.Format(time.RFC3339Nano),
		},
	}
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
	canonical := reply.MessageContent
	if canonical == nil {
		canonical = canonicalReplyContent(reply.Content, reply.Metadata)
	}
	msg := wsMessage{Type: "reply", Content: reply.Content, MessageContent: canonical, RenderManifest: reply.RenderManifest, Metadata: reply.Metadata, KnowledgeHits: reply.KnowledgeHits, MemoryHits: reply.MemoryHits} // U9
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
		if owner, ok := a.connectionOwners.Load(chatID); ok {
			a.bindSession(sessionID, chatID, owner.(string))
		}
		a.sessionRequests.Store(sessionID, requestID)
	}
	chunkCount := 0
	reasoningCount := 0
	var canonical strings.Builder
	for chunk := range chunks {
		if chunk.Error != nil {
			if requestID != "" && a.streams != nil {
				a.streams.Append(requestID, chunk)
				a.streams.Fail(requestID, chunk.Error)
			}
			errMsg := wsMessage{
				Type: "error",
				// 与启动失败路径(web.go streamHandler err)及 SSE/HTTP 一致：净化上游错误，
				// 不把 provider 原始 JSON / 内部前缀灌进聊天气泡。
				Content:             upstreamerr.PublicMessage(chunk.Error, "error"),
				SessionID:           sessionID,
				RequestID:           requestID,
				AssistantMessageID:  chunk.AssistantMessageID,
				BackendMessageID:    chunk.BackendMessageID,
				MessageID:           chunk.MessageID,
				Sequence:            chunk.Sequence,
				ReasoningDisclosure: chunk.ReasoningDisclosure,
				RuntimeEvent:        chunk.RuntimeEvent,
			}
			_ = a.sendToTargets(ctx, chatID, requestID, errMsg)
			return chunk.Error
		}

		chunkCount++
		canonical.WriteString(chunk.Content)
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
			Type:                "chunk",
			Content:             chunk.Content,
			Reasoning:           chunk.Reasoning,
			Done:                chunk.Done,
			SessionID:           sessionID,
			RequestID:           requestID,
			Metadata:            chunk.Metadata,
			Usage:               chunk.Usage,
			ToolCalls:           chunk.ToolCalls,
			Blocks:              chunk.Blocks,
			KnowledgeHits:       chunk.KnowledgeHits, // U9
			MemoryHits:          chunk.MemoryHits,
			AssistantMessageID:  chunk.AssistantMessageID,
			BackendMessageID:    chunk.BackendMessageID,
			MessageID:           chunk.MessageID,
			Sequence:            chunk.Sequence,
			ReasoningDisclosure: chunk.ReasoningDisclosure,
			RuntimeEvent:        chunk.RuntimeEvent,
		}
		if chunk.Done {
			msg.MessageContent = chunk.MessageContent
			if msg.MessageContent == nil {
				msg.MessageContent = canonicalReplyContent(canonical.String(), chunk.Metadata)
			}
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
			if owner, ok := a.connectionOwners.Load(chatID); ok {
				a.bindSession(msg.SessionID, chatID, owner.(string))
			}
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
	ownerID := strings.TrimSpace(skill.AuthenticatedUserID(r.Context()))
	if ownerID == "" {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !isAllowedWebSocketOrigin(r.Header.Get("Origin")) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"localhost", "tauri.localhost", "127.0.0.1"},
	})
	if err != nil {
		slog.Error("WebSocket 握手失败", "err", err)
		return
	}

	conn.SetReadLimit(20 * 1024 * 1024)

	chatID := "ws-" + idgen.ShortID()
	a.conns.Store(chatID, conn)
	a.connectionOwners.Store(chatID, ownerID)
	defer func() {
		a.conns.Delete(chatID)
		a.connectionOwners.Delete(chatID)
		a.deleteApprovalBindingsForChat(chatID)
		a.removeChatID(chatID)
		a.sessionConns.Range(func(key, value any) bool {
			if binding, ok := value.(sessionConnectionBinding); ok && binding.chatID == chatID {
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
			if requestID == "" && incoming.SessionID != "" && a.sessionOwnedBy(incoming.SessionID, ownerID) {
				if bound, ok := a.sessionRequests.Load(incoming.SessionID); ok {
					requestID = bound.(string)
				}
			}
			slog.Info("WebSocket cancel", "session", incoming.SessionID, "request_id", requestID)
			if requestID != "" && a.requestOwnedBy(requestID, ownerID) {
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
			snapshot, ok := a.GetStreamSnapshot(ownerID, incoming.RequestID)
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
				if snapshot.SessionID != "" && !a.bindSession(snapshot.SessionID, chatID, ownerID) {
					_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "error", Content: "session ownership mismatch"})
					continue
				}
				a.sessionRequests.Store(snapshot.SessionID, snapshot.RequestID)
			}
			_ = wsjson.Write(r.Context(), conn, snapshotToMessage(snapshot))
			continue
		case "tool_approval_response", "tool_permission_response":
			reqID := incoming.Metadata["approval_request_id"]
			if reqID == "" {
				reqID = incoming.Metadata["request_id"]
			}
			if reqID == "" {
				reqID = incoming.RequestID
			}
			decision := incoming.Metadata["decision"]
			decisionID := incoming.DecisionID
			if decisionID == "" {
				decisionID = incoming.Metadata["decision_id"]
			}
			idempotencyKey := incoming.Metadata["idempotency_key"]
			scopeSchemaVersion := incoming.ScopeSchemaVersion
			if scopeSchemaVersion == 0 {
				scopeSchemaVersion, _ = strconv.Atoi(incoming.Metadata["scope_schema_version"])
			}
			data := ApprovalResponseData{
				RequestID: reqID, OwnerID: ownerID, DecisionID: decisionID,
				InvocationID: incoming.Metadata["invocation_id"],
				Decision:     decision, IdempotencyKey: idempotencyKey,
				ArgumentsDigest:     incoming.Metadata["arguments_digest"],
				SecurityScopeDigest: incoming.Metadata["security_scope_digest"],
				ScopeSchemaVersion:  scopeSchemaVersion,
				responderChatID:     chatID,
			}
			_ = wsjson.Write(r.Context(), conn, a.approvalACK(data))
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
		if hasStagedAttachmentReference(incoming.Attachments) {
			if a.attachmentResolver == nil {
				_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "error", Content: "attachment staging unavailable"})
				continue
			}
			resolved, err := a.attachmentResolver(r.Context(), ownerID, incoming.Attachments)
			if err != nil {
				_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "error", Content: "attachment is unavailable"})
				continue
			}
			incoming.Attachments = resolved
			if err := adapter.ValidateAttachments(incoming.Attachments); err != nil {
				_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "error", Content: "attachment is invalid"})
				continue
			}
		}
		if incoming.RequestID == "" {
			incoming.RequestID = "req-" + idgen.ShortID()
		}

		msg, err := buildAdapterMessage(chatID, ownerID, incoming)
		if err != nil {
			_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "error", Content: err.Error()})
			continue
		}
		msg.InstanceID = a.Name()
		if incoming.SessionID != "" {
			if !a.bindSession(incoming.SessionID, chatID, ownerID) {
				_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "error", Content: "session ownership mismatch"})
				continue
			}
			a.sessionRequests.Store(incoming.SessionID, incoming.RequestID)
		}
		if existing, loaded := a.requestOwners.LoadOrStore(incoming.RequestID, ownerID); loaded && existing != ownerID {
			_ = wsjson.Write(r.Context(), conn, wsMessage{Type: "error", Content: "request ownership mismatch"})
			continue
		}

		logger := trace.NewRequest(msg.UserID, msg.SessionID).With("source", "chat", "provider", incoming.Provider, "model", incoming.Model, "request_id", incoming.RequestID)

		go func(incoming wsMessage, msg *adapter.Message) {
			ctx, cancel := context.WithTimeout(skill.WithAuthenticatedUser(context.Background(), ownerID), 10*time.Minute)
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
			canonical := reply.MessageContent
			if canonical == nil {
				canonical = canonicalReplyContent(reply.Content, reply.Metadata)
			}
			respMsg := wsMessage{
				Type:           "reply",
				Content:        reply.Content,
				MessageContent: canonical,
				RenderManifest: reply.RenderManifest,
				SessionID:      msg.SessionID,
				RequestID:      incoming.RequestID,
				Metadata:       reply.Metadata,
				Usage:          reply.Usage,
				ToolCalls:      reply.ToolCalls,
				Blocks:         reply.Blocks,
				KnowledgeHits:  reply.KnowledgeHits, // U9
				MemoryHits:     reply.MemoryHits,
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

func hasStagedAttachmentReference(attachments []adapter.Attachment) bool {
	for _, attachment := range attachments {
		if strings.TrimSpace(attachment.ID) != "" {
			return true
		}
	}
	return false
}

func (a *WebAdapter) deleteApprovalBindingsForChat(chatID string) {
	if chatID == "" {
		return
	}
	a.approvalBindings.Range(func(key, value any) bool {
		if binding, ok := value.(approvalTransportBinding); ok && binding.chatID == chatID {
			a.approvalBindings.Delete(key)
		}
		return true
	})
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
	a.requestOwners.Delete(requestID)
	if sessionID == "" {
		return
	}
	if current, ok := a.sessionRequests.Load(sessionID); ok && current == requestID {
		a.sessionRequests.Delete(sessionID)
	}
}

func (a *WebAdapter) bindSession(sessionID, chatID, ownerID string) bool {
	if sessionID == "" || chatID == "" || ownerID == "" {
		return false
	}
	next := sessionConnectionBinding{chatID: chatID, ownerID: ownerID}
	changed := true
	if current, loaded := a.sessionConns.LoadOrStore(sessionID, next); loaded {
		binding, ok := current.(sessionConnectionBinding)
		if !ok || binding.ownerID != ownerID {
			return false
		}
		changed = binding.chatID != chatID
		a.sessionConns.Store(sessionID, next)
	}
	if changed {
		a.replayPendingApprovals(ownerID, sessionID)
	}
	return true
}

func (a *WebAdapter) replayPendingApprovals(ownerID, sessionID string) {
	if a.pendingApprovalReplay == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, request := range a.pendingApprovalReplay(ctx, ownerID, sessionID) {
		if request == nil || request.OwnerID != ownerID {
			continue
		}
		if err := a.SendPermissionRequest(ctx, sessionID, request); err != nil {
			slog.Warn("tool approval reconnect replay failed",
				"request_id", request.ID, "session_id", sessionID, "error", err)
		}
	}
}

func (a *WebAdapter) sessionOwnedBy(sessionID, ownerID string) bool {
	value, ok := a.sessionConns.Load(sessionID)
	if !ok {
		return false
	}
	binding, ok := value.(sessionConnectionBinding)
	return ok && binding.ownerID == ownerID
}

func (a *WebAdapter) requestOwnedBy(requestID, ownerID string) bool {
	value, ok := a.requestOwners.Load(requestID)
	return ok && value == ownerID
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
		Type:                "stream_snapshot",
		Content:             snapshot.Content,
		MessageContent:      canonicalReplyContent(snapshot.Content, snapshot.Metadata),
		Reasoning:           snapshot.Reasoning,
		SessionID:           snapshot.SessionID,
		RequestID:           snapshot.RequestID,
		Done:                snapshot.Done,
		Metadata:            snapshot.Metadata,
		Usage:               snapshot.Usage,
		ToolCalls:           snapshot.ToolCalls,
		Blocks:              snapshot.Blocks,
		AssistantMessageID:  snapshot.AssistantMessageID,
		BackendMessageID:    snapshot.BackendMessageID,
		MessageID:           snapshot.MessageID,
		Sequence:            snapshot.LastSequence,
		RuntimeEvents:       snapshot.RuntimeEvents,
		LastSequence:        snapshot.LastSequence,
		ReasoningDisclosure: snapshot.ReasoningDisclosure,
	}
}

func canonicalReplyContent(markdown string, metadata map[string]string) *messagecontent.MessageContent {
	if strings.TrimSpace(markdown) == "" {
		return nil
	}
	producer := messagecontent.ProducerChat
	if metadata != nil {
		switch messagecontent.ProducerKind(metadata["producer_kind"]) {
		case messagecontent.ProducerChat,
			messagecontent.ProducerQuickChat,
			messagecontent.ProducerK12,
			messagecontent.ProducerSkill,
			messagecontent.ProducerTool,
			messagecontent.ProducerRAG,
			messagecontent.ProducerReport,
			messagecontent.ProducerCron,
			messagecontent.ProducerWebhook,
			messagecontent.ProducerWorkflow:
			producer = messagecontent.ProducerKind(metadata["producer_kind"])
		}
	}
	locale := "und"
	if metadata != nil && strings.TrimSpace(metadata["locale"]) != "" {
		locale = metadata["locale"]
	}
	content, err := messagecontent.New(producer, locale, markdown, nil)
	if err != nil {
		return nil
	}
	return &content
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
	Type                string                         `json:"type"` // message / reply / chunk / error / resume / stream_snapshot
	Content             string                         `json:"content"`
	MessageContent      *messagecontent.MessageContent `json:"message_content,omitempty"`
	RenderManifest      *messagecontent.RenderManifest `json:"render_manifest,omitempty"`
	Reasoning           string                         `json:"reasoning,omitempty"`
	SessionID           string                         `json:"session_id,omitempty"`
	RequestID           string                         `json:"request_id,omitempty"`
	DecisionID          string                         `json:"decision_id,omitempty"`
	Status              string                         `json:"status,omitempty"`
	OwnerID             string                         `json:"owner_id,omitempty"`
	ToolName            string                         `json:"tool_name,omitempty"`
	UserID              string                         `json:"user_id,omitempty"`
	Provider            string                         `json:"provider,omitempty"`
	Model               string                         `json:"model,omitempty"`
	Role                string                         `json:"role,omitempty"`
	Temperature         *float64                       `json:"temperature,omitempty"`
	MaxTokens           *int                           `json:"max_tokens,omitempty"`
	Done                bool                           `json:"done,omitempty"`
	Metadata            map[string]string              `json:"metadata,omitempty"`
	Arguments           map[string]any                 `json:"arguments,omitempty"`
	InvocationID        string                         `json:"invocation_id,omitempty"`
	ArgumentsDigest     string                         `json:"arguments_digest,omitempty"`
	SecurityScopeDigest string                         `json:"security_scope_digest,omitempty"`
	ScopeSchemaVersion  int                            `json:"scope_schema_version,omitempty"`
	DeadlineAt          string                         `json:"deadline_at,omitempty"`
	Usage               *adapter.Usage                 `json:"usage,omitempty"`
	ToolCalls           []adapter.ToolCall             `json:"tool_calls,omitempty"`
	Blocks              []adapter.Block                `json:"blocks,omitempty"`
	Attachments         []adapter.Attachment           `json:"attachments,omitempty"`
	// U9：结构化 RAG/记忆命中（随 done chunk / reply 回传前端渲染命中标签+详情）。
	KnowledgeHits       []adapter.KnowledgeHit          `json:"knowledge_hits,omitempty"`
	MemoryHits          []adapter.MemoryHit             `json:"memory_hits,omitempty"`
	AssistantMessageID  string                          `json:"assistant_message_id,omitempty"`
	BackendMessageID    string                          `json:"backend_message_id,omitempty"`
	MessageID           string                          `json:"message_id,omitempty"`
	Sequence            uint64                          `json:"sequence,omitempty"`
	ReasoningDisclosure adapter.ReasoningDisclosure     `json:"reasoning_disclosure"`
	RuntimeEvent        *adapter.RuntimeEvent           `json:"runtime_event,omitempty"`
	RuntimeEvents       []adapter.SequencedRuntimeEvent `json:"runtime_events,omitempty"`
	LastSequence        uint64                          `json:"last_sequence,omitempty"`
}

type approvalACKRecord struct {
	fingerprint string
	ack         wsMessage
	ownerID     string
	chatID      string
	expiresAt   time.Time
	sequence    uint64
}

func (a *WebAdapter) approvalACK(data ApprovalResponseData) wsMessage {
	approved, remember, valid := approvalDecisionFlags(data.Decision)
	if !valid {
		return rejectedApprovalACK(data, data.IdempotencyKey, "invalid_decision")
	}
	if strings.TrimSpace(data.IdempotencyKey) == "" {
		return rejectedApprovalACK(data, "", "missing_idempotency_key")
	}
	if strings.TrimSpace(data.RequestID) == "" || strings.TrimSpace(data.DecisionID) == "" ||
		strings.TrimSpace(data.OwnerID) == "" || strings.TrimSpace(data.responderChatID) == "" ||
		strings.TrimSpace(data.InvocationID) == "" || strings.TrimSpace(data.ArgumentsDigest) == "" ||
		strings.TrimSpace(data.SecurityScopeDigest) == "" {
		return rejectedApprovalACK(data, data.IdempotencyKey, "identity_mismatch")
	}
	data.Approved = approved
	data.Remember = remember
	key := data.IdempotencyKey
	fingerprint := strings.Join([]string{
		data.RequestID, data.DecisionID, data.InvocationID, data.Decision,
		data.ArgumentsDigest, data.SecurityScopeDigest, strconv.Itoa(data.ScopeSchemaVersion),
	}, "\x00")
	now := time.Now()
	if a.onDurableApprovalDecision == nil {
		a.approvalACKMu.Lock()
		a.pruneApprovalACKsLocked(now)
		if record, cached := a.approvalACKs[key]; cached {
			a.approvalACKMu.Unlock()
			if record.fingerprint == fingerprint {
				if record.ownerID != data.OwnerID || record.chatID != data.responderChatID {
					return rejectedApprovalACK(data, key, "identity_mismatch")
				}
				ack := record.ack
				if ack.Status == "accepted" {
					ack.Status = "already_accepted"
				}
				return ack
			}
			return rejectedApprovalACK(data, key, "idempotency_conflict")
		}
		a.approvalACKMu.Unlock()
	}
	value, ok := a.approvalBindings.Load(data.RequestID)
	if ok {
		binding, bindingOK := value.(approvalTransportBinding)
		if data.ScopeSchemaVersion == 0 && bindingOK {
			data.ScopeSchemaVersion = binding.scopeSchemaVersion
		}
		if !bindingOK || !binding.valid() || now.After(binding.expiresAt) || binding.ownerID != data.OwnerID ||
			binding.chatID != data.responderChatID || binding.invocationID != data.InvocationID ||
			binding.argumentsDigest != data.ArgumentsDigest || binding.securityScopeDigest != data.SecurityScopeDigest ||
			binding.scopeSchemaVersion != data.ScopeSchemaVersion {
			return rejectedApprovalACK(data, key, "identity_mismatch")
		}
		data.SessionID = binding.sessionID
	} else if a.onDurableApprovalDecision == nil {
		return rejectedApprovalACK(data, key, "identity_mismatch")
	}

	if a.onDurableApprovalDecision != nil {
		receipt := a.onDurableApprovalDecision(data)
		if !durableApprovalReceiptMatches(data, receipt) {
			return rejectedApprovalACK(data, key, "identity_mismatch")
		}
		ack := durableApprovalACK(receipt)
		a.cacheApprovalACK(key, fingerprint, data, ack, now)
		if receipt.TerminalResult != "identity_mismatch" {
			a.approvalBindings.Delete(data.RequestID)
		}
		return ack
	}

	a.approvalACKMu.Lock()
	defer a.approvalACKMu.Unlock()
	a.pruneApprovalACKsLocked(now)
	if record, cached := a.approvalACKs[key]; cached {
		if record.fingerprint == fingerprint {
			if record.ownerID != data.OwnerID || record.chatID != data.responderChatID {
				return rejectedApprovalACK(data, key, "identity_mismatch")
			}
			ack := record.ack
			if ack.Status == "accepted" {
				ack.Status = "already_accepted"
			}
			return ack
		}
		return rejectedApprovalACK(data, key, "idempotency_conflict")
	}

	terminalResult := "accepted"
	if a.onApprovalDecision != nil {
		terminalResult = a.onApprovalDecision(data)
	} else if a.onApprovalResponse != nil {
		a.onApprovalResponse(data.RequestID, data.Approved, data.Remember)
	}
	ack := wsMessage{
		Type:       "tool_approval_ack",
		RequestID:  data.RequestID,
		DecisionID: data.DecisionID,
		Status:     approvalACKStatus(terminalResult),
		Metadata: map[string]string{
			"approval_request_id":   data.RequestID,
			"decision_id":           data.DecisionID,
			"invocation_id":         data.InvocationID,
			"decision":              data.Decision,
			"idempotency_key":       key,
			"arguments_digest":      data.ArgumentsDigest,
			"security_scope_digest": data.SecurityScopeDigest,
			"scope_schema_version":  strconv.Itoa(data.ScopeSchemaVersion),
			"terminal_result":       terminalResult,
		},
	}
	a.approvalACKSeq++
	a.approvalACKs[key] = approvalACKRecord{
		fingerprint: fingerprint, ack: ack, ownerID: data.OwnerID, chatID: data.responderChatID,
		expiresAt: now.Add(a.effectiveApprovalACKTTLLocked()), sequence: a.approvalACKSeq,
	}
	a.enforceApprovalACKBoundLocked()
	if terminalResult != "identity_mismatch" {
		a.approvalBindings.Delete(data.RequestID)
	}
	return ack
}

func durableApprovalReceiptMatches(data ApprovalResponseData, receipt ApprovalDecisionReceipt) bool {
	if receipt.RequestID != data.RequestID || receipt.InvocationID != data.InvocationID ||
		receipt.OwnerID != data.OwnerID || strings.TrimSpace(receipt.SessionID) == "" ||
		receipt.Decision != data.Decision || receipt.IdempotencyKey != data.IdempotencyKey ||
		receipt.ArgumentsDigest != data.ArgumentsDigest || receipt.SecurityScopeDigest != data.SecurityScopeDigest ||
		receipt.ScopeSchemaVersion <= 0 ||
		(data.ScopeSchemaVersion > 0 && receipt.ScopeSchemaVersion != data.ScopeSchemaVersion) ||
		strings.TrimSpace(receipt.DecisionID) == "" || strings.TrimSpace(receipt.TerminalResult) == "" {
		return false
	}
	switch receipt.ACKStatus {
	case "accepted", "expired", "rejected":
		return true
	default:
		return false
	}
}

func durableApprovalACK(receipt ApprovalDecisionReceipt) wsMessage {
	return wsMessage{
		Type:       "tool_approval_ack",
		RequestID:  receipt.RequestID,
		DecisionID: receipt.DecisionID,
		Status:     receipt.ACKStatus,
		Metadata: map[string]string{
			"approval_request_id":   receipt.RequestID,
			"decision_id":           receipt.DecisionID,
			"invocation_id":         receipt.InvocationID,
			"decision":              receipt.Decision,
			"idempotency_key":       receipt.IdempotencyKey,
			"arguments_digest":      receipt.ArgumentsDigest,
			"security_scope_digest": receipt.SecurityScopeDigest,
			"scope_schema_version":  strconv.Itoa(receipt.ScopeSchemaVersion),
			"terminal_result":       receipt.TerminalResult,
		},
	}
}

func (a *WebAdapter) cacheApprovalACK(
	key, fingerprint string, data ApprovalResponseData, ack wsMessage, now time.Time,
) {
	a.approvalACKMu.Lock()
	defer a.approvalACKMu.Unlock()
	a.pruneApprovalACKsLocked(now)
	a.approvalACKSeq++
	a.approvalACKs[key] = approvalACKRecord{
		fingerprint: fingerprint, ack: ack, ownerID: data.OwnerID, chatID: data.responderChatID,
		expiresAt: now.Add(a.effectiveApprovalACKTTLLocked()), sequence: a.approvalACKSeq,
	}
	a.enforceApprovalACKBoundLocked()
}

func (a *WebAdapter) effectiveApprovalACKTTLLocked() time.Duration {
	if a.approvalACKTTL <= 0 {
		return 5 * time.Minute
	}
	return a.approvalACKTTL
}

func (a *WebAdapter) effectiveApprovalACKLimitLocked() int {
	if a.approvalACKLimit <= 0 {
		return 1024
	}
	return a.approvalACKLimit
}

func (a *WebAdapter) pruneApprovalACKsLocked(now time.Time) {
	if a.approvalACKs == nil {
		a.approvalACKs = make(map[string]approvalACKRecord)
	}
	for key, record := range a.approvalACKs {
		if !record.expiresAt.IsZero() && !now.Before(record.expiresAt) {
			delete(a.approvalACKs, key)
		}
	}
}

func (a *WebAdapter) enforceApprovalACKBoundLocked() {
	limit := a.effectiveApprovalACKLimitLocked()
	for len(a.approvalACKs) > limit {
		var oldestKey string
		var oldestSeq uint64
		for key, record := range a.approvalACKs {
			if oldestKey == "" || record.sequence < oldestSeq {
				oldestKey, oldestSeq = key, record.sequence
			}
		}
		delete(a.approvalACKs, oldestKey)
	}
}

func approvalDecisionFlags(decision string) (approved, remember, valid bool) {
	switch decision {
	case "approved_once":
		return true, false, true
	case "approved_remember":
		return true, true, true
	case "denied":
		return false, false, true
	default:
		return false, false, false
	}
}

func approvalACKStatus(terminalResult string) string {
	switch terminalResult {
	case "not_pending":
		return "expired"
	case "identity_mismatch", "store_error", "idempotency_conflict":
		return "rejected"
	default:
		return "accepted"
	}
}

func rejectedApprovalACK(data ApprovalResponseData, key, terminalResult string) wsMessage {
	return wsMessage{
		Type:       "tool_approval_ack",
		RequestID:  data.RequestID,
		DecisionID: data.DecisionID,
		Status:     "rejected",
		Metadata: map[string]string{
			"approval_request_id": data.RequestID,
			"decision_id":         data.DecisionID,
			"invocation_id":       data.InvocationID,
			"decision":            data.Decision,
			"idempotency_key":     key,
			"terminal_result":     terminalResult,
		},
	}
}

// MarshalJSON 自定义序列化（省略空字段）。
func (m wsMessage) MarshalJSON() ([]byte, error) {
	type Alias wsMessage
	return json.Marshal((Alias)(m))
}

func buildAdapterMessage(chatID, authenticatedOwnerID string, incoming wsMessage) (*adapter.Message, error) {
	userID := strings.TrimSpace(authenticatedOwnerID)
	if userID == "" {
		return nil, errors.New("authenticated WebSocket owner is required")
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

func isAllowedWebSocketOrigin(origin string) bool {
	origin = strings.TrimSpace(origin)
	if origin == "" {
		// Native WebSocket clients do not send Origin. Authentication remains
		// mandatory, so the browser-CSRF boundary is not weakened.
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	switch strings.ToLower(u.Scheme) {
	case "tauri":
		return host == "localhost" && u.Port() == ""
	case "http":
		if host == "tauri.localhost" {
			return u.Port() == ""
		}
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	default:
		return false
	}
}
