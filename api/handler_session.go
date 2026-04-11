package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/storage"
)

// --- 会话管理 API ---

func sessionUserIDFromRequest(r *http.Request) string {
	// 优先从 query parameter 读取，其次从请求体 JSON 读取
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if userID != "" {
		return userID
	}
	return "api-user"
}

// sessionUserIDFromRequestOrBody 从 query 或 body 中提取 user_id
func sessionUserIDFromRequestOrBody(r *http.Request, bodyUserID string) string {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": sessions,
		"total":    len(sessions),
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

	sess.Title = req.Title
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

	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "api-user"
	}

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

	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
		"total":   total,
		"query":   query,
	})
}

// --- 对话分支 API ---

// ForkSessionRequest 创建分支请求
type ForkSessionRequest struct {
	MessageID string `json:"message_id"` // 从哪条消息开始分支
	UserID    string `json:"user_id"`    // 用户 ID（可选）
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

	userID := req.UserID
	if userID == "" {
		userID = "api-user"
	}
	if _, err := s.getOwnedSession(r, sessionID, userID); err != nil {
		writeSessionLookupError(w, err)
		return
	}

	newSession, err := s.store.ForkSession(r.Context(), sessionID, req.MessageID, userID)
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
