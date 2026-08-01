package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// knowledgeUploadProcessTimeout 限定上传文档「解析(pdftotext/VLM)+向量嵌入」处理阶段的
// 最长耗时。扫描件/超大 PDF 的提取或嵌入可能长时间无产出，无界等待会让前端字节传完后
// 永远停在「100%」等不到响应（BUG-20260712 #8「卡 100% 不动」根因）。var 便于测试压小
// 值验证超时分支。
var knowledgeUploadProcessTimeout = 5 * time.Minute

// uploadProcessTimedOut 判定上传处理是否命中有界超时（区别于普通解析/入库失败）。
func uploadProcessTimedOut(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

// writeUploadTimeout 回超时响应：504 + 可操作提示，不再让用户对着「100%」干等。
func writeUploadTimeout(w http.ResponseWriter) {
	writeJSON(w, http.StatusGatewayTimeout, map[string]string{
		"error": "文档处理超时；扫描件/超大文件请配置视觉模型 后重试，或改用文本 / 可复制文字的 PDF",
	})
}

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
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data") {
		s.handleCreateKnowledgeDocument(w, r)
		return
	}
	var req AddDocumentRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 200<<20)).Decode(&req); err != nil {
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

// handleCreateKnowledgeDocument is the canonical asynchronous upload path.
// ParseMultipartForm is configured with only 1 MiB memory, so large sources
// stream through an OS temp file before the application service fsyncs them
// into its content-addressed store. Parsing/OCR/chunking never runs here.
func (s *Server) handleCreateKnowledgeDocument(w http.ResponseWriter, r *http.Request) {
	service, ok := s.semanticIndex.(KnowledgeDocumentIngestAPI)
	if !ok {
		writeDocumentIngestError(w, knowledge.ErrDocumentIngestUnavailable)
		return
	}
	const multipartOverhead = 2 << 20
	r.Body = http.MaxBytesReader(w, r.Body, knowledge.MaxKnowledgeDocumentBytes+multipartOverhead)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error": "文件过大，最大允许 200MB",
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "解析上传失败: " + err.Error()})
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未找到上传文件"})
		return
	}
	defer file.Close()
	if header.Size > knowledge.MaxKnowledgeDocumentBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "文件过大，最大允许 200MB"})
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required"})
		return
	}
	corpusID := strings.TrimSpace(r.FormValue("corpus_id"))
	if corpusID == "" {
		corpusID = strings.TrimSpace(r.URL.Query().Get("corpus_id"))
	}
	if corpusID == "" {
		corpusID = knowledgeDefaultCorpusID
	}
	if !requireSupportedKnowledgeCorpus(w, corpusID) {
		return
	}
	mediaType := strings.TrimSpace(header.Header.Get("Content-Type"))
	if mediaType == "" || mediaType == "application/octet-stream" {
		if inferred := mime.TypeByExtension(strings.ToLower(filepath.Ext(header.Filename))); inferred != "" {
			mediaType = inferred
		}
	}
	result, err := service.CreateDocument(r.Context(), knowledgePrincipalID(r), corpusID,
		knowledge.CreateDocumentInput{
			IdempotencyKey: idempotencyKey,
			Filename:       header.Filename,
			MediaType:      mediaType,
			SizeBytes:      header.Size,
			Body:           file,
			AgentID:        strings.TrimSpace(r.FormValue("agent_id")),
			LearnerID:      strings.TrimSpace(r.FormValue("learner_id")),
			Subject:        strings.TrimSpace(r.FormValue("subject")),
			Grade:          strings.TrimSpace(r.FormValue("grade")),
		})
	if err != nil {
		writeDocumentIngestError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func writeDocumentIngestError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, knowledge.ErrDocumentTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error(), "code": "knowledge_document_too_large"})
	case errors.Is(err, knowledge.ErrInvalidDocumentUpload):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error(), "code": "knowledge_document_invalid"})
	case errors.Is(err, knowledge.ErrIdempotencyConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error(), "code": "knowledge_idempotency_conflict"})
	case errors.Is(err, knowledge.ErrDocumentIngestUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "knowledge document ingest unavailable", "code": "knowledge_ingest_unavailable"})
	default:
		logger.Error("[knowledge] asynchronous document acceptance failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "knowledge document acceptance failed", "code": "knowledge_ingest_internal"})
	}
}

// handleUploadDocument 上传文件到知识库。
func (s *Server) handleUploadDocument(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 200 << 20 // 200MB
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
	allowed := map[string]bool{".txt": true, ".md": true, ".csv": true, ".json": true, ".docx": true, ".pdf": true, ".doc": true, ".pptx": true,
		".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true}
	if !allowed[ext] {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "不支持的文件格式，请上传 .txt / .md / .csv / .json / .doc / .docx / .pptx / .pdf / 图片(.png/.jpg/.jpeg/.webp/.gif)",
		})
		return
	}

	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "读取文件失败: " + err.Error(),
		})
		return
	}
	if int64(len(data)) > maxUpload {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("文件过大，最大允许 %dMB", maxUpload>>20),
		})
		return
	}

	// 图片：走多模态入库（视觉模型转写为中文描述 → 文本 RAG 入库，source_type=image）。
	// 未配置 captioner 时 AddImageDocument 优雅报错（不吞垃圾）。
	if mime := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif"}[ext]; mime != "" {
		title := strings.TrimSuffix(header.Filename, ext)
		doc, ierr := s.kb.AddImageDocument(r.Context(), title, data, mime, "upload:"+header.Filename)
		if ierr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "图片入库失败: " + ierr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, knowledgeDocResponse(doc))
		return
	}

	title := strings.TrimSuffix(header.Filename, ext)

	// BUG-20260712 #8：解析(pdftotext/VLM)+向量嵌入阶段套有界超时，扫描件/超大文件不再
	// 无限挂起（前端字节传完却永远等不到响应=「卡 100% 不动」根因）。超时给可操作提示。
	ctx, cancel := context.WithTimeout(r.Context(), knowledgeUploadProcessTimeout)
	defer cancel()

	extracted, err := extractDocumentForKnowledge(ctx, ext, data, s.kb)
	if err != nil {
		if uploadProcessTimedOut(ctx) {
			writeUploadTimeout(w)
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "解析文件失败: " + err.Error(),
		})
		return
	}

	if strings.TrimSpace(extracted.Text) == "" {
		msg := "文件内容为空"
		if ext == ".pdf" || ext == ".docx" {
			msg = "未能从文件中提取到可入库内容；扫描页或内嵌图片需要配置视觉模型 / VLM 后重试"
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    msg,
			"warnings": extracted.Warnings,
		})
		return
	}

	doc, err := s.kb.AddDocument(ctx, title, extracted.Text, "upload:"+header.Filename)
	if err != nil {
		if uploadProcessTimedOut(ctx) {
			writeUploadTimeout(w)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "添加文档失败: " + err.Error(),
		})
		return
	}

	resp := knowledgeDocResponse(doc)
	resp.Warnings = extracted.Warnings
	writeJSON(w, http.StatusOK, resp)
}

var errDOCXXMLTooLarge = errors.New("DOCX word/document.xml exceeds extraction limit")

func readDOCXXMLLimited(reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%w: invalid limit", errDOCXXMLTooLarge)
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("%w: maximum is %d bytes", errDOCXXMLTooLarge, maxBytes)
	}
	return raw, nil
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
	// 防 zip bomb: 限制解压后读取量（header 中的 UncompressedSize64 可伪造，不可信赖）
	const maxDocXMLSize = 100 << 20 // 100MB
	rc, err := docXML.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	raw, err := readDOCXXMLLimited(rc, maxDocXMLSize)
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

// handleListDocuments 列出知识库文档。
//
// 支持查询参数 ?source=&limit=&offset=（皆可选）：缺省（无 limit）时返回全部，
// 与旧契约一致；带 limit 时返回该页 + 全集总数 + 按 source 的计数 facet，避免
// 定时任务快照累积到数千条后把列表页拖垮。
func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	res, err := s.kb.ListDocumentsPaged(r.Context(), knowledge.DocListQuery{
		Source: strings.TrimSpace(q.Get("source")),
		Limit:  atoiDefault(q.Get("limit"), 0),
		Offset: atoiDefault(q.Get("offset"), 0),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "获取文档列表失败: " + err.Error(),
		})
		return
	}
	if projectionService, ok := s.semanticIndex.(KnowledgeDocumentVectorProjectionAPI); ok {
		projections, projectionErr := projectionService.ListDocumentVectorProjections(
			r.Context(), knowledgePrincipalID(r), knowledgeDefaultCorpusID,
		)
		if projectionErr != nil && !errors.Is(projectionErr, knowledge.ErrSemanticIndexNotFound) {
			writeSemanticIndexError(w, projectionErr)
			return
		}
		for _, document := range res.Documents {
			if projection, found := projections[document.ID]; found {
				applyKnowledgeDocumentVectorProjection(document, projection)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"documents": res.Documents, // ListDocumentsPaged guarantees non-nil
		"total":     res.Total,
		"limit":     res.Limit,
		"offset":    res.Offset,
		"sources":   res.Sources,
	})
}

// atoiDefault parses s as a non-negative int, returning def on empty/invalid
// input (so a malformed ?limit= degrades to "no paging" rather than erroring).
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

// handleGetDocument 获取单个知识库文档详情（含正文）
func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	docID := r.PathValue("id")
	if docID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "文档 ID 不能为空",
		})
		return
	}

	if projectionService, ok := s.semanticIndex.(KnowledgeDocumentProjectionAPI); ok {
		projection, projectionErr := projectionService.GetIngestDocumentProjectionForCorpus(
			r.Context(), knowledgePrincipalID(r), knowledgeDefaultCorpusID, docID,
		)
		if projectionErr == nil {
			if projection.CorpusID != knowledgeDefaultCorpusID {
				writeSemanticIndexError(w, knowledge.ErrSemanticIndexNotFound)
				return
			}
			payload := s.knowledgeDocumentDetail(r, projection)
			if vectorService, vectorOK := s.semanticIndex.(KnowledgeDocumentVectorProjectionAPI); vectorOK {
				vectors, vectorErr := vectorService.ListDocumentVectorProjections(
					r.Context(), knowledgePrincipalID(r), knowledgeDefaultCorpusID,
				)
				if vectorErr != nil && !errors.Is(vectorErr, knowledge.ErrSemanticIndexNotFound) {
					writeSemanticIndexError(w, vectorErr)
					return
				}
				if vector, found := vectors[docID]; found {
					applyKnowledgeDocumentVectorPayload(payload, vector)
				}
			}
			writeJSON(w, http.StatusOK, payload)
			return
		}
		if !errors.Is(projectionErr, knowledge.ErrSemanticIndexNotFound) {
			writeSemanticIndexError(w, projectionErr)
			return
		}
	}

	doc, err := s.kb.GetDocument(r.Context(), docID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, knowledge.ErrSemanticIndexNotFound) || strings.Contains(err.Error(), "不存在") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{
			"error": "获取文档失败: " + err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, doc)
}

func applyKnowledgeDocumentVectorProjection(
	document *knowledge.Document,
	projection knowledge.DocumentVectorProjection,
) {
	document.VectorIndexState = projection.VectorIndexState
	document.VectorJobID = projection.JobID
	document.VectorJobState = projection.JobState
	document.VectorJobStage = projection.Stage
	document.VectorChunksDone = projection.ChunksDone
	document.VectorChunksTotal = projection.ChunksTotal
	document.VectorError = projection.LastError
	document.VectorOutcomeUnknown = projection.OutcomeUnknown
}

func applyKnowledgeDocumentVectorPayload(
	payload map[string]any,
	projection knowledge.DocumentVectorProjection,
) {
	payload["vector_index_state"] = projection.VectorIndexState
	payload["vector_job_id"] = projection.JobID
	payload["vector_job_state"] = projection.JobState
	payload["vector_job_stage"] = projection.Stage
	payload["vector_chunks_done"] = projection.ChunksDone
	payload["vector_chunks_total"] = projection.ChunksTotal
	payload["vector_error"] = projection.LastError
	payload["vector_outcome_unknown"] = projection.OutcomeUnknown
}

func (s *Server) knowledgeDocumentDetail(
	r *http.Request,
	projection knowledge.KnowledgeDocumentProjection,
) map[string]any {
	payload := map[string]any{}
	if legacy, err := s.kb.GetDocument(r.Context(), projection.DocumentID); err == nil {
		if raw, marshalErr := json.Marshal(legacy); marshalErr == nil {
			_ = json.Unmarshal(raw, &payload)
		}
	}
	status := string(projection.TextIndexState)
	if projection.TextIndexState == knowledge.TextIndexReady {
		status = "indexed"
	}
	if current, ok := payload["status"].(string); !ok || strings.TrimSpace(current) == "" {
		payload["status"] = status
	}
	payload["id"] = projection.DocumentID
	payload["document_id"] = projection.DocumentID
	payload["document_generation"] = projection.DocumentGeneration
	payload["owner_id"] = projection.OwnerID
	payload["corpus_id"] = projection.CorpusID
	payload["filename"] = projection.Filename
	payload["media_type"] = projection.MediaType
	payload["size_bytes"] = projection.SizeBytes
	payload["sha256"] = projection.SHA256
	payload["source_digest"] = projection.SHA256
	payload["agent_id"] = projection.AgentID
	payload["learner_id"] = projection.LearnerID
	payload["subject"] = projection.Subject
	payload["grade"] = projection.Grade
	payload["text_index_state"] = projection.TextIndexState
	payload["warnings"] = projection.Warnings
	payload["source_spans"] = projection.SourceSpans
	if projection.PageCount != nil {
		payload["page_count"] = *projection.PageCount
		payload["pages_total"] = *projection.PageCount
		if projection.TextIndexState == knowledge.TextIndexReady {
			payload["pages_done"] = *projection.PageCount
		} else {
			payload["pages_done"] = int64(0)
		}
	}
	if chunks, ok := payload["chunk_count"]; ok {
		payload["chunks_total"] = chunks
		if projection.TextIndexState == knowledge.TextIndexReady {
			payload["chunks_done"] = chunks
		} else {
			payload["chunks_done"] = 0
		}
	}
	return payload
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
		status := http.StatusInternalServerError
		if errors.Is(err, knowledge.ErrSemanticIndexNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{
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
		status := http.StatusInternalServerError
		if errors.Is(err, knowledge.ErrSemanticIndexNotFound) || strings.Contains(err.Error(), "不存在") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{
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

	// ── 元数据过滤（可选）：维度间 AND、维度内 OR、留空=不过滤 ──
	Sources       []string `json:"sources,omitempty"`        // 按文档来源精确匹配
	SourceTypes   []string `json:"source_types,omitempty"`   // manual / upload / url / file / agent
	CreatedAfter  string   `json:"created_after,omitempty"`  // RFC3339 或 YYYY-MM-DD，文档创建时间下界
	CreatedBefore string   `json:"created_before,omitempty"` // RFC3339 或 YYYY-MM-DD，文档创建时间上界
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

	if strings.TrimSpace(req.Query) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "query 不能为空",
		})
		return
	}
	if !knowledge.SearchQueryWithinBudget(req.Query) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("query 长度不能超过 %d 个字符", knowledge.MaxSearchQueryRunes),
		})
		return
	}

	topK := req.TopK
	if topK <= 0 {
		topK = knowledge.DefaultSearchTopK
	}
	if topK > knowledge.MaxSearchTopK {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("top_k 不能超过 %d", knowledge.MaxSearchTopK),
		})
		return
	}

	createdAfter, err := knowledge.ParseFilterDate(req.CreatedAfter)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "created_after " + err.Error()})
		return
	}
	createdBefore, err := knowledge.ParseFilterDate(req.CreatedBefore)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "created_before " + err.Error()})
		return
	}
	filter := knowledge.Filter{
		Sources:       req.Sources,
		SourceTypes:   req.SourceTypes,
		CreatedAfter:  createdAfter,
		CreatedBefore: createdBefore,
	}

	results, receipts, err := s.kb.SearchWithFilterReceipt(
		r.Context(), req.Query, topK, filter,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "搜索失败: " + err.Error(),
		})
		return
	}

	// Fix 16: 只返回 "results"（复数），移除冗余的 "result" 字段
	writeJSON(w, http.StatusOK, map[string]any{
		"results":        results,
		"total":          len(results),
		"query_receipts": receipts,
	})
}

// handleKnowledgeRetrievalMetrics exposes aggregate, query-content-free local
// diagnostics for FTS/LIKE fallback and vector lane latency. It is intentionally
// read-only and inherits the existing local management API protection.
func (s *Server) handleKnowledgeRetrievalMetrics(w http.ResponseWriter, _ *http.Request) {
	if s.kb == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "知识库未启用"})
		return
	}
	writeJSON(w, http.StatusOK, s.kb.RetrievalMetricsSnapshot())
}

// ── 检索质量参数（检索参数面板）─────────────────────────────────────────────
//
// GET 读当前生效配置；PUT 即时热替换 Manager 运行时参数 + 落 yaml 持久化。
// 即时生效字段：rerank/query_expand/contextual 开关、min_score、candidate_k。
// rerank_model 换模型走「重启 sidecar 生效」（专用重排器在启动时注入，运行时不重建）。

// KnowledgeConfigPayload 检索质量参数的读写载体（GET 响应 / PUT 请求同形）。
type KnowledgeConfigPayload struct {
	Rerank      bool    `json:"rerank"`       // 重排总开关
	RerankModel string  `json:"rerank_model"` // 专用 cross-encoder 重排模型（空=LLM 重排 / 自动）
	QueryExpand bool    `json:"query_expand"` // HyDE + multi-query 查询扩展
	Contextual  bool    `json:"contextual"`   // 入库 Contextual Retrieval
	MinScore    float64 `json:"min_score"`    // 向量相关度地板 [0,1]，0=关
	CandidateK  int     `json:"candidate_k"`  // 宽召回候选池大小（>0）
}

// handleGetKnowledgeConfig 读当前生效的检索质量参数。
//
// 运行时字段取自 Manager 的活配置（GetHybridConfig，即实际生效值）；rerank_model 取自
// 持久化配置（最近一次保存值，反映“待重启生效”的目标模型）——cfgWriter 缺省时回落到启动配置。
func (s *Server) handleGetKnowledgeConfig(w http.ResponseWriter, r *http.Request) {
	if s.kb == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "知识库未启用"})
		return
	}
	hc := s.kb.GetHybridConfig()
	rerankModel := s.cfg.Knowledge.RerankModel
	if s.cfgWriter != nil {
		if k, err := s.cfgWriter.ReadKnowledge(); err == nil {
			rerankModel = k.RerankModel
		}
	}
	writeJSON(w, http.StatusOK, KnowledgeConfigPayload{
		Rerank:      hc.RerankEnabled,
		RerankModel: rerankModel,
		QueryExpand: hc.ExpandEnabled,
		Contextual:  hc.ContextualEnabled,
		MinScore:    hc.MinScore,
		CandidateK:  hc.CandidateK,
	})
}

// handlePutKnowledgeConfig 全量替换检索质量参数：校验 → 落 yaml → 即时热替换 Manager。
//
// 校验或持久化失败时不改动运行时配置，避免「本次 500、运行态已变、重启后回退」的配置漂移。
// 响应回带新生效配置 + rerank_model_restart_required（rerank_model 变更需重启才生效）。
func (s *Server) handlePutKnowledgeConfig(w http.ResponseWriter, r *http.Request) {
	if s.kb == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "知识库未启用"})
		return
	}
	var req KnowledgeConfigPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if req.MinScore < 0 || req.MinScore > 1 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "min_score 必须在 [0,1] 区间"})
		return
	}
	if req.CandidateK <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "candidate_k 必须大于 0"})
		return
	}
	if req.CandidateK > knowledge.MaxCandidateK {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("candidate_k 不能超过 %d", knowledge.MaxCandidateK),
		})
		return
	}
	req.RerankModel = strings.TrimSpace(req.RerankModel)
	// Keep the runtime snapshot, persisted YAML and other whole-config writers
	// in one serializable transaction. cfgMu is the existing process-wide guard
	// for read-copy-save-apply configuration updates.
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()

	// 先构造候选运行时配置（保留嵌入前缀等非面板项），但在 YAML 原子写成功前绝不发布。
	hc := s.kb.GetHybridConfig()
	hc.RerankEnabled = req.Rerank
	hc.ExpandEnabled = req.QueryExpand
	hc.ContextualEnabled = req.Contextual
	hc.MinScore = req.MinScore
	hc.CandidateK = req.CandidateK
	// 落 yaml 持久化；rerank_model 是否变化决定“需重启”提示。
	restartRequired := false
	if s.cfgWriter != nil {
		if prev, err := s.cfgWriter.ReadKnowledge(); err == nil {
			restartRequired = strings.TrimSpace(prev.RerankModel) != req.RerankModel
		}
		if err := s.cfgWriter.UpdateKnowledgeRetrieval(config.KnowledgeRetrievalSettings{
			Rerank:      req.Rerank,
			RerankModel: req.RerankModel,
			QueryExpand: req.QueryExpand,
			Contextual:  req.Contextual,
			MinScore:    req.MinScore,
			CandidateK:  req.CandidateK,
		}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "保存配置失败: " + err.Error()})
			return
		}
	}
	// YAML 已成功原子落盘后才发布运行态，二者从此点起保持同一版本语义。
	s.kb.SetHybridConfig(hc)

	writeJSON(w, http.StatusOK, map[string]any{
		"rerank":                        req.Rerank,
		"rerank_model":                  req.RerankModel,
		"query_expand":                  req.QueryExpand,
		"contextual":                    req.Contextual,
		"min_score":                     req.MinScore,
		"candidate_k":                   req.CandidateK,
		"rerank_model_restart_required": restartRequired,
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
