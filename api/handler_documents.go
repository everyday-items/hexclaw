package api

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
)

// DocumentExtractResponse /documents/extract 的返回体。
type DocumentExtractResponse struct {
	Text      string `json:"text"`
	FileName  string `json:"file_name"`
	PageCount int    `json:"page_count,omitempty"`
}

// handleExtractDocument 把上传文档解析为纯文本，返回文本供前端注入对话上下文。无状态、不入库。
//
// 格式路由（见 docextract.go）：
//   - .pdf  → poppler pdftotext（CJK 完整，降级 hexagon）
//   - .doc  → macOS textutil（OLE 二进制）
//   - .docx → extractDocxText（OOXML，知识库同款）
//   - .pptx → hexagon PPTXLoader
//   - .txt/.md/.csv/.json → 直接按 UTF-8 文本
//
// 注：.xlsx/.xls 由前端 SheetJS 解析（唯一能解老 .xls 且更强），不走本端点。
func (s *Server) handleExtractDocument(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 100 << 20 // 100MB
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "解析上传失败: " + err.Error()})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "未找到上传文件"})
		return
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, maxUpload+1))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取文件失败: " + err.Error()})
		return
	}
	if int64(len(data)) > maxUpload {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("文件过大，最大允许 %dMB", maxUpload>>20)})
		return
	}

	ext := strings.ToLower(filepath.Ext(header.Filename))
	var (
		text      string
		pageCount int
	)
	switch ext {
	case ".pdf":
		text, pageCount, err = extractPDFText(r.Context(), data)
	case ".doc":
		text, err = extractDOCText(r.Context(), data)
	case ".docx":
		text, err = extractDocxText(data)
	case ".pptx":
		text, err = extractPPTXText(r.Context(), data)
	case ".txt", ".md", ".csv", ".json":
		text = string(data)
	case ".pages", ".numbers", ".key":
		// 苹果 iWork 私有格式（现代版为压缩 Protobuf，无可靠解析方案）——引导导出 PDF。
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "苹果 " + strings.TrimPrefix(ext, ".") + " 格式暂不支持直接解析，请在 Pages/Numbers/Keynote 里「文件 → 导出为 → PDF」后再上传",
		})
		return
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "不支持的文件格式：" + ext + "（支持 .pdf / .doc / .docx / .pptx / .txt / .md / .csv / .json；表格用 .xlsx/.xls）",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "解析文档失败: " + err.Error()})
		return
	}
	if strings.TrimSpace(text) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "未能从文件中提取到文本（可能是扫描件或纯图片）",
		})
		return
	}

	writeJSON(w, http.StatusOK, DocumentExtractResponse{
		Text:      text,
		FileName:  header.Filename,
		PageCount: pageCount,
	})
}
