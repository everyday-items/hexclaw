package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"

	"github.com/hexagon-codes/hexclaw/render"
)

// renderRequest POST /api/v1/render 的请求体。
//
// 参考 .claude/doc-generation-architecture.md §3.2。
type renderRequest struct {
	Content string                `json:"content"` // markdown 文本（外部资源已预下载内联）
	Format  render.Format         `json:"format"`
	Title   string                `json:"title,omitempty"`   // 可选：用于 Content-Disposition filename
	Options render.RenderOptions  `json:"options,omitempty"` // 可选：locale 等
}

// handleRender 处理 POST /api/v1/render
//
// 流式下发：从 RenderResult.Path 走 http.ServeContent，OS 内核处理 sendfile/buffered read。
// 非缓存 file 在响应完成后清理。
//
// 鉴权：本端点处于 /api/v1/ 前缀下，自动经 apiAuthMiddleware 校验：
//   - localhost：放行
//   - 非 localhost：必须 Authorization: Bearer <APIToken>
func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) {
	if s.renderSvc == nil {
		writeRenderError(w, &render.RenderError{
			Code:   render.CodeEngineMissing,
			Detail: "render service not configured",
		})
		return
	}

	// 限制请求体最大字节（含 data URL 图片 base64 膨胀）
	r.Body = http.MaxBytesReader(w, r.Body, render.MaxRequestBodyBytes)

	var req renderRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// MaxBytesReader 触发 → 报 INPUT_TOO_LARGE；其他错误 → 400
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeRenderError(w, &render.RenderError{
				Code:   render.CodeInputTooLarge,
				Detail: "request body too large",
			})
			return
		}
		writeRenderError(w, &render.RenderError{
			Code:   render.CodeFormatUnsupported,
			Detail: "invalid request body: " + err.Error(),
		})
		return
	}

	// 渲染（编排：缓存查找 → 渲染 → 后处理 → 入缓存）
	result, err := s.renderSvc.Render(r.Context(), req.Content, req.Format, req.Options)
	if err != nil {
		var renderErr *render.RenderError
		if errors.As(err, &renderErr) {
			writeRenderError(w, renderErr)
			return
		}
		writeRenderError(w, &render.RenderError{
			Code:   render.CodeRenderFailed,
			Format: req.Format,
			Detail: err.Error(),
		})
		return
	}

	// 设置响应头：Content-Type / Length / Content-Disposition
	// 文件名清洗按 RFC 5987 双 header 规范
	title := req.Title
	if title == "" {
		title = "artifact"
	}
	disposition := render.ContentDispositionHeader(title, req.Format.Extension())

	w.Header().Set("Content-Type", result.ContentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Render-Cache-Hit", boolStr(result.CacheHit))

	// 打开文件流
	f, err := os.Open(result.Path)
	if err != nil {
		writeRenderError(w, &render.RenderError{
			Code:   render.CodeRenderFailed,
			Format: req.Format,
			Detail: "open render result: " + err.Error(),
		})
		return
	}
	defer f.Close()

	// http.ServeContent 走 OS sendfile / buffered read，不读到 Go heap 全量
	stat, _ := f.Stat()
	http.ServeContent(w, r, result.Path, stat.ModTime(), f)

	// 非缓存 temp file 在响应完成后清理
	if !result.CacheHit {
		_ = os.Remove(result.Path)
	}
}

func writeRenderError(w http.ResponseWriter, e *render.RenderError) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(e.HTTPStatus())
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": e,
	})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
