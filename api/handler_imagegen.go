package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	imagegen "github.com/hexagon-codes/ai-core/media/image"
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

	requestRef := req.IdempotencyKey
	if requestRef == "" {
		requestRef = r.Header.Get("Idempotency-Key")
	}
	if requestRef == "" {
		requestRef = r.Header.Get("X-Request-ID")
	}
	requestRef = mediaLogRef(requestRef)
	providerName := req.Provider
	modelName := req.Model
	providerStarted := time.Now()
	logger.InfoContext(r.Context(), "[media] stage",
		"media_kind", "image", "request_ref", requestRef,
		"provider", providerName, "model", modelName,
		"stage", "provider_wait", "status", "started",
		"elapsed_ms", int64(0), "result_count", 0)
	providerHeartbeatDone := make(chan struct{})
	var providerHeartbeatWG sync.WaitGroup
	providerHeartbeatWG.Add(1)
	go func() {
		defer providerHeartbeatWG.Done()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				logger.InfoContext(r.Context(), "[media] stage",
					"media_kind", "image", "request_ref", requestRef,
					"provider", providerName, "model", modelName,
					"stage", "provider_wait", "status", "heartbeat",
					"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", 0)
			case <-providerHeartbeatDone:
				return
			case <-r.Context().Done():
				return
			}
		}
	}()
	result, err := s.imagegenSvc.Generate(r.Context(), req.Provider, req.Request)
	close(providerHeartbeatDone)
	providerHeartbeatWG.Wait()
	if err != nil {
		waitStatus := "failed"
		errorType := "provider_error"
		if r.Context().Err() != nil {
			waitStatus = "cancelled"
			errorType = "context_cancelled"
		}
		logger.WarnContext(r.Context(), "[media] stage",
			"media_kind", "image", "request_ref", requestRef,
			"provider", providerName, "model", modelName,
			"stage", "provider_wait", "status", waitStatus,
			"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", 0,
			"error_type", errorType)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "生成失败: " + err.Error()})
		return
	}
	resultCount := 0
	if result != nil {
		if result.Provider != "" {
			providerName = result.Provider
		}
		if result.Model != "" {
			modelName = result.Model
		}
		if requestRef == "" && result.RequestID != "" {
			requestRef = mediaLogRef(result.RequestID)
		}
		resultCount = len(result.Images)
	}
	logger.InfoContext(r.Context(), "[media] stage",
		"media_kind", "image", "request_ref", requestRef,
		"provider", providerName, "model", modelName,
		"stage", "provider_wait", "status", "completed",
		"elapsed_ms", time.Since(providerStarted).Milliseconds(), "result_count", resultCount)

	// 持久化：把 b64 / URL 落盘到 {DataDir}/generated，回填 file_path 字段。
	// 前端优先用 file_path 拼出 /api/v1/files/generated/{path}，避免 base64 撑爆 SQLite，
	// 也避免 Provider URL 过期后旧消息打不开。
	if s.genStore != nil {
		persistStarted := time.Now()
		logger.InfoContext(r.Context(), "[media] stage",
			"media_kind", "image", "request_ref", requestRef,
			"provider", providerName, "model", modelName,
			"stage", "persist", "status", "started",
			"elapsed_ms", int64(0), "result_count", 0)
		persistHeartbeatDone := make(chan struct{})
		var persistHeartbeatWG sync.WaitGroup
		persistHeartbeatWG.Add(1)
		go func() {
			defer persistHeartbeatWG.Done()
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					logger.InfoContext(r.Context(), "[media] stage",
						"media_kind", "image", "request_ref", requestRef,
						"provider", providerName, "model", modelName,
						"stage", "persist", "status", "heartbeat",
						"elapsed_ms", time.Since(persistStarted).Milliseconds(), "result_count", 0)
				case <-persistHeartbeatDone:
					return
				case <-r.Context().Done():
					return
				}
			}
		}()
		persistedCount := 0
		persistFailed := false
		persistErrorType := ""
		for i := range result.Images {
			img := &result.Images[i]
			if img.B64JSON != "" {
				data, decErr := base64.StdEncoding.DecodeString(img.B64JSON)
				if decErr != nil {
					persistFailed = true
					if persistErrorType == "" {
						persistErrorType = "decode_error"
					}
					continue
				}
				saved, saveErr := s.genStore.SaveBytes(data, "png")
				if saveErr != nil {
					persistFailed = true
					if persistErrorType == "" {
						persistErrorType = "store_error"
					}
					continue
				}
				img.FilePath = saved
				img.B64JSON = "" // 落盘后清空 b64，避免响应体两份大数据
				persistedCount++
			} else if img.URL != "" {
				saved, saveErr := s.genStore.SaveFromURL(r.Context(), img.URL, "png")
				if saveErr != nil {
					persistFailed = true
					if persistErrorType == "" {
						persistErrorType = "store_error"
					}
					continue
				}
				img.FilePath = saved
				persistedCount++
			}
		}
		close(persistHeartbeatDone)
		persistHeartbeatWG.Wait()
		persistStatus := "completed"
		if persistFailed {
			persistStatus = "failed"
		}
		elapsedMS := time.Since(persistStarted).Milliseconds()
		if persistFailed {
			logger.WarnContext(r.Context(), "[media] stage",
				"media_kind", "image", "request_ref", requestRef,
				"provider", providerName, "model", modelName,
				"stage", "persist", "status", persistStatus,
				"elapsed_ms", elapsedMS, "result_count", persistedCount,
				"error_type", persistErrorType)
		} else {
			logger.InfoContext(r.Context(), "[media] stage",
				"media_kind", "image", "request_ref", requestRef,
				"provider", providerName, "model", modelName,
				"stage", "persist", "status", persistStatus,
				"elapsed_ms", elapsedMS, "result_count", persistedCount)
		}
	}

	writeJSON(w, http.StatusOK, result)
}

func mediaLogRef(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}
