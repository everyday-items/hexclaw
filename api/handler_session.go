package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	"github.com/hexagon-codes/toolkit/util/idgen"
)

// maxSessionTitleRunes 会话标题上限（rune 计数，CJK 一字一计）。
// 自动标题与用户改名共用该边界（BUG-20260703 P2b）。
const maxSessionTitleRunes = 200

// --- 会话管理 API ---

func sessionUserIDFromRequest(r *http.Request) string {
	// Authentication middleware stamps an unforgeable principal in context.
	// Query/body user_id remain a legacy direct-handler test/embedding fallback
	// only; authenticated HTTP traffic must never be allowed to switch owners.
	if userID := strings.TrimSpace(skill.AuthenticatedUserID(r.Context())); userID != "" {
		return userID
	}
	// 优先从 query parameter 读取，其次从请求体 JSON 读取
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if userID != "" {
		return userID
	}
	return "api-user"
}

// sessionUserIDFromRequestOrBody 从 query 或 body 中提取 user_id
func sessionUserIDFromRequestOrBody(r *http.Request, bodyUserID string) string {
	if userID := strings.TrimSpace(skill.AuthenticatedUserID(r.Context())); userID != "" {
		return userID
	}
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if userID != "" {
		return userID
	}
	bodyUserID = strings.TrimSpace(bodyUserID)
	if bodyUserID != "" {
		return bodyUserID
	}
	return "api-user"
}

func validMessageFeedback(feedback string) bool {
	switch feedback {
	case "", "like", "dislike":
		return true
	default:
		return false
	}
}

func (s *Server) getOwnedSession(r *http.Request, sessionID, userID string) (*storage.Session, error) {
	sess, err := s.store.GetSession(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, storage.ErrNotFound
		}
		return nil, err
	}
	if sess.UserID != userID {
		return nil, storage.ErrNotFound
	}
	return sess, nil
}

func writeSessionLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "会话不存在",
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error": "获取会话失败: " + err.Error(),
	})
}

// handleListSessions 列出用户的会话
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	userID := sessionUserIDFromRequest(r)

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 20
	}

	sessions, err := s.store.ListSessions(r.Context(), userID, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取会话列表失败: " + err.Error(),
		})
		return
	}
	if sessions == nil {
		sessions = []*storage.Session{}
	}

	// Fix 13: len(sessions) 不是真实总数。由于没有 CountSessions 方法，
	// 使用 has_more 标志帮助前端判断是否有下一页。
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": sessions,
		"total":    len(sessions) + offset,
		"has_more": len(sessions) == limit,
	})
}

// handleGetSession 获取单个会话详情
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.getOwnedSession(r, id, sessionUserIDFromRequest(r))
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// handleDeleteSession 删除会话
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.getOwnedSession(r, id, sessionUserIDFromRequest(r)); err != nil {
		writeSessionLookupError(w, err)
		return
	}
	if err := s.store.DeleteSession(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "删除会话失败: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "会话已删除"})
}

// handleListMessages 获取会话的消息历史
func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if _, err := s.getOwnedSession(r, sessionID, sessionUserIDFromRequest(r)); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	messages, err := s.store.ListMessages(r.Context(), sessionID, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取消息历史失败: " + err.Error(),
		})
		return
	}
	if messages == nil {
		messages = []*storage.MessageRecord{}
	}
	hydrateMessageContents(messages)

	// 获取真实总数用于分页
	total := len(messages) + offset // 近似值，实际由 store 提供
	if ct, err := s.store.CountMessages(r.Context(), sessionID); err == nil {
		total = ct
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"messages": messages,
		"total":    total,
	})
}

// handleDeleteMessage 删除单条消息
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	messageID := r.PathValue("id")
	if messageID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "消息 ID 不能为空",
		})
		return
	}

	// 验证消息归属：加载消息 -> 检查会话所有权
	userID := sessionUserIDFromRequest(r)
	msg, err := s.store.GetMessage(r.Context(), messageID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "消息不存在"})
		return
	}
	if _, err := s.getOwnedSession(r, msg.SessionID, userID); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权操作此消息"})
		return
	}

	if err := s.store.DeleteMessage(r.Context(), messageID); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "消息不存在",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "删除消息失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "消息已删除"})
}

type updateMessageFeedbackRequest struct {
	Feedback string `json:"feedback"`
}

// handleUpdateMessageFeedback 更新消息点赞/点踩反馈。
func (s *Server) handleUpdateMessageFeedback(w http.ResponseWriter, r *http.Request) {
	messageID := r.PathValue("id")
	if messageID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "消息 ID 不能为空",
		})
		return
	}

	var req updateMessageFeedbackRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}
	if !validMessageFeedback(req.Feedback) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "更新消息反馈失败: 无效反馈值: " + req.Feedback,
		})
		return
	}

	// 验证消息归属：加载消息 -> 检查会话所有权
	userID := sessionUserIDFromRequest(r)
	msg, err := s.store.GetMessage(r.Context(), messageID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "消息不存在"})
		return
	}
	if _, err := s.getOwnedSession(r, msg.SessionID, userID); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权操作此消息"})
		return
	}

	if err := s.store.UpdateMessageFeedback(r.Context(), messageID, req.Feedback); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, storage.ErrNotFound) {
			status = http.StatusNotFound
		} else if strings.HasPrefix(err.Error(), "无效反馈值:") {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]string{
			"error": "更新消息反馈失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message": "反馈已更新",
	})
}

// --- 创建 / 更新会话 API ---

// createSessionRequest 创建会话请求
type createSessionRequest struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	UserID string `json:"user_id"`
}

// handleCreateSession 创建新会话
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "id 不能为空",
		})
		return
	}

	userID := sessionUserIDFromRequestOrBody(r, req.UserID)
	sess := &storage.Session{
		ID:       req.ID,
		UserID:   userID,
		Platform: "web",
		Title:    req.Title,
		Status:   1,
	}
	if err := s.store.CreateSession(r.Context(), sess); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "创建会话失败: " + err.Error(),
		})
		return
	}

	// 重新读取以获取数据库生成的 created_at
	created, err := s.store.GetSession(r.Context(), req.ID)
	if err != nil {
		// 创建成功但读取失败，返回原始对象
		writeJSON(w, http.StatusCreated, map[string]string{
			"id":         sess.ID,
			"title":      sess.Title,
			"created_at": sess.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"id":         created.ID,
		"title":      created.Title,
		"created_at": created.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// updateSessionRequest 更新会话请求
type updateSessionRequest struct {
	Title string `json:"title"`
}

type suggestSessionTitleRequest struct {
	ExpectedTitle string `json:"expected_title"`
}

type sessionTitleSuggester interface {
	SuggestSessionTitle(context.Context, []*storage.MessageRecord) (string, error)
}

// handleUpdateSession 更新会话（标题）
func (s *Server) handleUpdateSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	userID := sessionUserIDFromRequest(r)

	sess, err := s.getOwnedSession(r, id, userID)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}

	var req updateSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	// BUG-20260703 P2b：标题不再原样入库——空/纯空白会把会话弄成不可辨识，
	// 超长（粘贴整段文章）拖累列表渲染与索引。上限按 rune 计数（CJK 一字一计）。
	title := strings.TrimSpace(req.Title)
	if title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "标题不能为空"})
		return
	}
	if utf8.RuneCountInString(title) > maxSessionTitleRunes {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("标题过长（最多 %d 字符）", maxSessionTitleRunes),
		})
		return
	}

	sess.Title = title
	if err := s.store.UpdateSession(r.Context(), sess); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "更新会话失败: " + err.Error(),
		})
		return
	}

	// 重新读取以获取数据库更新的 updated_at
	updated, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{
			"id":         sess.ID,
			"title":      sess.Title,
			"updated_at": sess.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":         updated.ID,
		"title":      updated.Title,
		"updated_at": updated.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// handleSuggestSessionTitle 基于现有对话生成更自然的标题。
//
// 这是 best-effort 接口：如果标题已经被用户改过，或标题生成失败，
// 返回当前标题且 updated=false，不影响主聊天链路。
func (s *Server) handleSuggestSessionTitle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	userID := sessionUserIDFromRequest(r)

	sess, err := s.getOwnedSession(r, id, userID)
	if err != nil {
		writeSessionLookupError(w, err)
		return
	}

	var req suggestSessionTitleRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "请求格式错误: " + err.Error(),
			})
			return
		}
	}

	if strings.TrimSpace(req.ExpectedTitle) != "" && strings.TrimSpace(sess.Title) != strings.TrimSpace(req.ExpectedTitle) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         sess.ID,
			"title":      sess.Title,
			"updated":    false,
			"updated_at": sess.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
		return
	}

	messages, err := s.store.ListMessages(r.Context(), id, 6, 0)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "加载会话消息失败: " + err.Error(),
		})
		return
	}

	suggester, ok := s.engine.(sessionTitleSuggester)
	if !ok || len(messages) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         sess.ID,
			"title":      sess.Title,
			"updated":    false,
			"updated_at": sess.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
		return
	}

	title, err := suggester.SuggestSessionTitle(r.Context(), messages)
	title = strings.TrimSpace(title)
	if err != nil || title == "" || title == sess.Title {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         sess.ID,
			"title":      sess.Title,
			"updated":    false,
			"updated_at": sess.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
		return
	}

	sess.Title = title
	if err := s.store.UpdateSession(r.Context(), sess); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "更新会话失败: " + err.Error(),
		})
		return
	}

	updated, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":         sess.ID,
			"title":      sess.Title,
			"updated":    true,
			"updated_at": sess.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         updated.ID,
		"title":      updated.Title,
		"updated":    true,
		"updated_at": updated.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// --- 对话搜索 API ---

// handleSearchMessages 全文搜索消息内容
func (s *Server) handleSearchMessages(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "q 参数不能为空",
		})
		return
	}
	// 限制搜索查询长度，防止超长查询给 SQLite 造成压力
	if len([]rune(query)) > 200 {
		query = string([]rune(query)[:200])
	}

	userID := sessionUserIDFromRequest(r)

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 20
	}

	results, total, err := s.store.SearchMessages(r.Context(), userID, query, limit, offset)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "搜索失败: " + err.Error(),
		})
		return
	}
	if results == nil {
		results = []*storage.SearchResult{}
	}
	for _, result := range results {
		if result != nil {
			hydrateMessageContents([]*storage.MessageRecord{result.Message})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
		"total":   total,
		"query":   query,
	})
}

// --- 对话分支 API ---

// ForkSessionRequest 创建分支请求
type ForkSessionRequest struct {
	MessageID      string `json:"message_id"` // 从哪条消息开始分支
	UserID         string `json:"user_id"`    // 用户 ID（可选）
	IncludeMessage *bool  `json:"include_message,omitempty"`
}

// handleForkSession 从指定消息处创建对话分支
func (s *Server) handleForkSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")

	var req ForkSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.MessageID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "message_id 不能为空",
		})
		return
	}

	userID := sessionUserIDFromRequestOrBody(r, req.UserID)
	if _, err := s.getOwnedSession(r, sessionID, userID); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	options := []storage.ForkSessionOptions(nil)
	if req.IncludeMessage != nil {
		options = append(options, storage.ForkSessionOptions{IncludeMessage: *req.IncludeMessage})
	}
	newSession, err := s.store.ForkSession(r.Context(), sessionID, req.MessageID, userID, options...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "创建分支失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"session": newSession,
		"message": "分支已创建",
	})
}

// handleListBranches 列出会话的所有分支
func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if _, err := s.getOwnedSession(r, sessionID, sessionUserIDFromRequest(r)); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	branches, err := s.store.ListSessionBranches(r.Context(), sessionID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取分支列表失败: " + err.Error(),
		})
		return
	}
	if branches == nil {
		branches = []*storage.Session{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"branches": branches,
		"total":    len(branches),
	})
}

// appendMessageRequest 前端构造的完整消息 — 用于图像/视频/语音对话生成模式下
// 绕过 chat handler 直接写入消息历史。
type appendMessageRequest struct {
	ID               string          `json:"id,omitempty"`           // 前端生成的 ID；空则后端生成
	Role             string          `json:"role"`                   // user / assistant
	Content          string          `json:"content"`                // 主文本内容
	ContentType      string          `json:"content_type,omitempty"` // text / multimodal_json
	Metadata         json.RawMessage `json:"metadata,omitempty"`     // attachments / mode / model 等
	ModelName        string          `json:"model_name,omitempty"`
	PromptTokens     int             `json:"prompt_tokens,omitempty"`
	CompletionTokens int             `json:"completion_tokens,omitempty"`
	FinishReason     string          `json:"finish_reason,omitempty"`
	ParentID         string          `json:"parent_id,omitempty"`
	RequestID        string          `json:"request_id,omitempty"`
}

// handleAppendMessage POST /api/v1/sessions/{id}/messages
//
// 把前端构造的消息写入 SQLite。用于生成模式（image_gen / video_gen / voice_chat）
// 直接落库，弥补 WebSocket chat 路径外的消息持久化缺口。
//
// 鉴权：复用 getOwnedSession 校验会话归属。
// 幂等：若客户端 ID 已存在，SaveMessage 会 UPSERT（由 storage 层保证）。
func (s *Server) handleAppendMessage(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage 未启用"})
		return
	}
	sessionID := r.PathValue("id")
	if _, err := s.getOwnedSession(r, sessionID, sessionUserIDFromRequest(r)); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	const maxBody = 8 << 20 // 8MB — 足以容纳 metadata 里的 URL/路径引用，但拒绝塞大 base64
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var req appendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if req.Role != "user" && req.Role != "assistant" && req.Role != "system" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "role 必须是 user/assistant/system"})
		return
	}
	if req.ContentType == "" {
		if len(req.Metadata) > 0 {
			req.ContentType = "multimodal_json"
		} else {
			req.ContentType = "text"
		}
	}

	record := buildMessageRecord(sessionID, &req)
	if err := s.store.SaveMessage(r.Context(), record); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "持久化失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         record.ID,
		"session_id": sessionID,
	})
}

// buildMessageRecord 把前端请求规格化成存储记录，补齐默认字段。
// 单/批量 handler 共用。
func buildMessageRecord(sessionID string, req *appendMessageRequest) *storage.MessageRecord {
	if req.ContentType == "" {
		if len(req.Metadata) > 0 {
			req.ContentType = "multimodal_json"
		} else {
			req.ContentType = "text"
		}
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = "msg-" + idgen.ShortID()
	}
	metadata := string(req.Metadata)
	attachments := ""
	if metadataContainsAttachments(req.Metadata) {
		// Keep the complete metadata envelope in the dedicated attachments
		// column. scanMessage restores this envelope on reads, preserving
		// attachments together with sibling scenario metadata without the
		// generic metadata column's 64 KiB clamp.
		attachments = metadata
	}
	return &storage.MessageRecord{
		ID:               id,
		SessionID:        sessionID,
		ParentID:         req.ParentID,
		Role:             req.Role,
		Content:          req.Content,
		ContentType:      req.ContentType,
		Metadata:         metadata,
		Attachments:      attachments,
		ModelName:        req.ModelName,
		PromptTokens:     req.PromptTokens,
		CompletionTokens: req.CompletionTokens,
		FinishReason:     req.FinishReason,
		RequestID:        req.RequestID,
	}
}

func metadataContainsAttachments(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var envelope struct {
		Attachments json.RawMessage `json:"attachments"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false
	}
	value := bytes.TrimSpace(envelope.Attachments)
	return len(value) > 0 && !bytes.Equal(value, []byte("null")) && !bytes.Equal(value, []byte("[]"))
}

// handleBatchAppendMessages POST /api/v1/sessions/{id}/messages/batch
//
// 批量写入多条消息，全部走同一事务 — 要么全部落库要么全部回滚，
// 解决"user 写入成功后 assistant 写失败"导致的数据不一致。
func (s *Server) handleBatchAppendMessages(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "storage 未启用"})
		return
	}
	sessionID := r.PathValue("id")
	if _, err := s.getOwnedSession(r, sessionID, sessionUserIDFromRequest(r)); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	const maxBody = 16 << 20 // 16MB：批量最多 ~50 条消息（单条 metadata 可 ≤8MB）
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var req struct {
		Messages []appendMessageRequest `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "messages 不能为空"})
		return
	}
	if len(req.Messages) > 50 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "单次批量最多 50 条"})
		return
	}
	// 先校验所有 role（避免事务开启后再回滚）
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role != "user" && m.Role != "assistant" && m.Role != "system" {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("messages[%d].role 必须是 user/assistant/system", i),
			})
			return
		}
	}

	records := make([]*storage.MessageRecord, len(req.Messages))
	ids := make([]string, len(req.Messages))
	for i := range req.Messages {
		records[i] = buildMessageRecord(sessionID, &req.Messages[i])
		ids[i] = records[i].ID
	}

	// 单事务保证原子性
	err := s.store.WithTx(r.Context(), func(tx storage.Store) error {
		for _, rec := range records {
			if err := tx.SaveMessage(r.Context(), rec); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "批量持久化失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"ids":        ids,
		"session_id": sessionID,
	})
}
