package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/everyday-items/hexclaw/storage"
)

// --- 会话管理 API ---

// handleListSessions 列出用户的会话
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		userID = "api-user"
	}

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
	sess, err := s.store.GetSession(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "会话不存在",
		})
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

// handleDeleteSession 删除会话
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 {
		limit = 50
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

	writeJSON(w, http.StatusOK, map[string]any{
		"messages": messages,
		"total":    len(messages),
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
