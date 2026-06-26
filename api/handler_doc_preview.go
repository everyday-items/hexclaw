package api

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

// 原文件预览：前端无法在 WKWebView 的 iframe 里渲染 PDF，也没有 fs 插件写临时文件；
// 故由 sidecar 暂存原文件并以 http://localhost 提供（shell open 仅放行 http://localhost:*），
// 前端点击文件卡片时上传 → 拿 token → 打开 /documents/preview/{token} 渲染原版。
// 内存环形缓存，仅保留最近 N 份，重启即清，无需落盘清理。

type previewEntry struct {
	data        []byte
	contentType string
	name        string
}

const previewMaxEntries = 16

var (
	previewMu    sync.Mutex
	previewStore = map[string]previewEntry{}
	previewOrder []string
)

func putPreview(name, contentType string, data []byte) string {
	previewMu.Lock()
	defer previewMu.Unlock()
	var b [12]byte
	_, _ = rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	previewStore[token] = previewEntry{data: data, contentType: contentType, name: name}
	previewOrder = append(previewOrder, token)
	for len(previewOrder) > previewMaxEntries {
		old := previewOrder[0]
		previewOrder = previewOrder[1:]
		delete(previewStore, old)
	}
	return token
}

// handleDocumentPreviewUpload 暂存原文件，返回 token。
func (s *Server) handleDocumentPreviewUpload(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 100 << 20
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

	ct := header.Header.Get("Content-Type")
	if ct == "" || ct == "application/octet-stream" {
		if byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(header.Filename))); byExt != "" {
			ct = byExt
		} else {
			ct = "application/octet-stream"
		}
	}
	token := putPreview(header.Filename, ct, data)
	writeJSON(w, http.StatusOK, map[string]string{"token": token})
}

// handleDocumentPreviewGet 以 inline 方式提供暂存的原文件（PDF 等浏览器/webview 直接渲染）。
func (s *Server) handleDocumentPreviewGet(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	previewMu.Lock()
	entry, ok := previewStore[token]
	previewMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	disposition := "inline"
	if r.URL.Query().Get("dl") == "1" {
		disposition = "attachment" // 下载按钮：让浏览器另存到下载目录
	}
	w.Header().Set("Content-Type", entry.contentType)
	w.Header().Set("Content-Disposition", disposition+"; filename=\""+sanitizeHeaderValue(entry.name)+"\"")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(entry.data)
}

// sanitizeHeaderValue 去掉可能破坏 header 的字符。
func sanitizeHeaderValue(s string) string {
	return strings.NewReplacer("\"", "", "\r", "", "\n", "").Replace(s)
}
