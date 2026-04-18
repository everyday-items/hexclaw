package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/hexagon-codes/hexclaw/imagegen"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// --- 图像生成 API ---

// handleImageGenStatus GET /api/v1/images/status
//
// 返回是否启用了文生图服务，以及已注册的 Provider 和模型列表。
func (s *Server) handleImageGenStatus(w http.ResponseWriter, r *http.Request) {
	enabled := s.imagegenSvc != nil && s.imagegenSvc.HasProvider()
	resp := map[string]any{
		"enabled":   enabled,
		"providers": []string{},
		"models":    []string{},
	}
	if enabled {
		resp["providers"] = s.imagegenSvc.Providers()
		resp["models"] = s.imagegenSvc.Models()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleImageGenGenerate POST /api/v1/images/generate
//
// 请求体：
//
//	{
//	  "provider": "openai-dalle",       // 可选，省略则按 model 路由
//	  "model":    "dall-e-3",           // 模型 ID（必填或与 provider 配合）
//	  "prompt":   "...",                // 提示词（必填）
//	  "size":     "1024x1024",          // 尺寸
//	  "n":        1,                    // 张数
//	  "style":    "vivid",              // DALL-E 3 专用
//	  "quality":  "hd"                  // DALL-E 3 专用
//	}
//
// 响应：imagegen.Result（含 b64_json 或 url）。
func (s *Server) handleImageGenGenerate(w http.ResponseWriter, r *http.Request) {
	if s.imagegenSvc == nil || !s.imagegenSvc.HasProvider() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "图像生成服务未配置（请在配置中启用 imagegen Provider）",
		})
		return
	}

	const maxBody = 64 << 10 // 64KB（prompt 不需要更大）
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var req struct {
		Provider string `json:"provider,omitempty"`
		imagegen.Request
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}
	if req.Prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt 不能为空"})
		return
	}

	result, err := s.imagegenSvc.Generate(r.Context(), req.Provider, req.Request)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成失败: " + err.Error()})
		return
	}

	// 持久化：把 b64 / URL 落盘到 {DataDir}/generated，回填 file_path 字段。
	// 前端优先用 file_path 拼出 /api/v1/files/generated/{path}，避免 base64 撑爆 SQLite，
	// 也避免 Provider URL 过期后旧消息打不开。
	if s.genStore != nil {
		for i := range result.Images {
			img := &result.Images[i]
			if img.B64JSON != "" {
				data, decErr := base64.StdEncoding.DecodeString(img.B64JSON)
				if decErr != nil {
					logger.Warn("[imagegen] decode b64 failed",
						logger.String("model", result.Model), logger.Err(decErr))
					continue
				}
				saved, saveErr := s.genStore.SaveBytes(data, "png")
				if saveErr != nil {
					logger.Warn("[imagegen] save bytes failed",
						logger.String("model", result.Model), logger.Err(saveErr))
					continue
				}
				img.FilePath = saved
				img.B64JSON = "" // 落盘后清空 b64，避免响应体两份大数据
			} else if img.URL != "" {
				saved, saveErr := s.genStore.SaveFromURL(r.Context(), img.URL, "png")
				if saveErr != nil {
					logger.Warn("[imagegen] save from URL failed",
						logger.String("model", result.Model), logger.Err(saveErr))
					continue
				}
				img.FilePath = saved
			}
		}
	}

	writeJSON(w, http.StatusOK, result)
}
