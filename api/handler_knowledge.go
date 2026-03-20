package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hexagon-codes/hexclaw/knowledge"
)

// --- 知识库 API ---

// AddDocumentRequest 添加文档请求
type AddDocumentRequest struct {
	Title   string `json:"title"`   // 文档标题
	Content string `json:"content"` // 文档内容
	Source  string `json:"source"`  // 来源（可选）
}

type knowledgeDocumentResponse struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Source       string   `json:"source,omitempty"`
	ChunkCount   int      `json:"chunk_count"`
	CreatedAt    any      `json:"created_at"`
	UpdatedAt    any      `json:"updated_at,omitempty"`
	Status       string   `json:"status"`
	ErrorMessage string   `json:"error_message,omitempty"`
	SourceType   string   `json:"source_type,omitempty"`
	Warnings     []string `json:"warnings,omitempty"`
}

// handleAddDocument 添加文档到知识库
func (s *Server) handleAddDocument(w http.ResponseWriter, r *http.Request) {
	var req AddDocumentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
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

	writeJSON(w, http.StatusOK, knowledgeDocResponse(doc))
}

// handleUploadDocument 上传文件到知识库（支持 TXT/MD/CSV/JSON/DOCX）
func (s *Server) handleUploadDocument(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 10 << 20 // 10MB
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "解析上传失败: " + err.Error(),
		})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "未找到上传文件",
		})
		return
	}
	defer file.Close()

	ext := strings.ToLower(filepath.Ext(header.Filename))
	allowed := map[string]bool{".txt": true, ".md": true, ".csv": true, ".json": true, ".docx": true}
	if !allowed[ext] {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "不支持的文件格式，请上传 .txt / .md / .csv / .json / .docx（PDF 暂不支持）",
		})
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "读取文件失败: " + err.Error(),
		})
		return
	}

	title := strings.TrimSuffix(header.Filename, ext)
	var content string
	switch ext {
	case ".txt", ".md", ".csv":
		content = string(data)
	case ".json":
		content = string(data)
	case ".docx":
		content, err = extractDocxText(data)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "解析 DOCX 失败: " + err.Error(),
			})
			return
		}
	default:
		content = string(data)
	}

	if strings.TrimSpace(content) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "文件内容为空",
		})
		return
	}

	doc, err := s.kb.AddDocument(r.Context(), title, content, "upload:"+header.Filename)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "添加文档失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, knowledgeDocResponse(doc))
}

// extractDocxText 从 DOCX 中提取纯文本（DOCX 为 ZIP，内含 word/document.xml）
func extractDocxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var docXML *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			docXML = f
			break
		}
	}
	if docXML == nil {
		return "", nil
	}
	rc, err := docXML.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return extractTextFromXML(raw), nil
}

// extractTextFromXML 从 OOXML word/document.xml 中提取 <w:t> 文本
var wTRe = regexp.MustCompile(`<w:t[^>]*>([^<]*)</w:t>`)

func extractTextFromXML(data []byte) string {
	matches := wTRe.FindAllSubmatch(data, -1)
	var parts []string
	for _, m := range matches {
		if len(m) > 1 {
			parts = append(parts, string(m[1]))
		}
	}
	return strings.Join(parts, " ")
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

// handleReindexDocument 重新构建文档索引。
func (s *Server) handleReindexDocument(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	if docID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "文档 ID 不能为空"})
		return
	}

	doc, err := s.kb.ReindexDocument(r.Context(), docID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"status":  "failed",
			"message": "重建索引失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":      doc.Status,
		"message":     "文档已重新索引",
		"id":          doc.ID,
		"chunk_count": doc.ChunkCount,
		"updated_at":  doc.UpdatedAt,
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
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

	results, err := s.kb.Search(r.Context(), req.Query, topK)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "搜索失败: " + err.Error(),
		})
		return
	}

	context, err := s.kb.Query(r.Context(), req.Query, topK)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "搜索失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"result":  context,
		"context": context,
		"results": results,
		"total":   len(results),
	})
}

func knowledgeDocResponse(doc *knowledge.Document) knowledgeDocumentResponse {
	return knowledgeDocumentResponse{
		ID:           doc.ID,
		Title:        doc.Title,
		Source:       doc.Source,
		ChunkCount:   doc.ChunkCount,
		CreatedAt:    doc.CreatedAt,
		UpdatedAt:    doc.UpdatedAt,
		Status:       doc.Status,
		ErrorMessage: doc.ErrorMessage,
		SourceType:   doc.SourceType,
		Warnings:     []string{},
	}
}
