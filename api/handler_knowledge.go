package api

import (
	"encoding/json"
	"net/http"
)

// --- 知识库 API ---

// AddDocumentRequest 添加文档请求
type AddDocumentRequest struct {
	Title   string `json:"title"`   // 文档标题
	Content string `json:"content"` // 文档内容
	Source  string `json:"source"`  // 来源（可选）
}

// handleAddDocument 添加文档到知识库
func (s *Server) handleAddDocument(w http.ResponseWriter, r *http.Request) {
	var req AddDocumentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Title == "" || req.Content == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "title 和 content 不能为空",
		})
		return
	}

	doc, err := s.kb.AddDocument(r.Context(), req.Title, req.Content, req.Source)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "添加文档失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":          doc.ID,
		"title":       doc.Title,
		"chunk_count": doc.ChunkCount,
		"created_at":  doc.CreatedAt,
	})
}

// handleListDocuments 列出知识库文档
func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	docs, err := s.kb.ListDocuments(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取文档列表失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"documents": docs,
		"total":     len(docs),
	})
}

// handleDeleteDocument 删除知识库文档
func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	if docID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "文档 ID 不能为空",
		})
		return
	}

	if err := s.kb.DeleteDocument(r.Context(), docID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "删除文档失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message": "文档已删除",
	})
}

// SearchKnowledgeRequest 知识库搜索请求
type SearchKnowledgeRequest struct {
	Query string `json:"query"` // 搜索查询
	TopK  int    `json:"top_k"` // 返回条数（默认 3）
}

// handleSearchKnowledge 搜索知识库
func (s *Server) handleSearchKnowledge(w http.ResponseWriter, r *http.Request) {
	var req SearchKnowledgeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "请求格式错误: " + err.Error(),
		})
		return
	}

	if req.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "query 不能为空",
		})
		return
	}

	topK := req.TopK
	if topK <= 0 {
		topK = 3
	}

	result, err := s.kb.Query(r.Context(), req.Query, topK)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "搜索失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result": result,
	})
}
