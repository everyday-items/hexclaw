package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"

	"github.com/hexagon-codes/ai-core/media/voicechat"
	"github.com/hexagon-codes/toolkit/util/logger"
)

// --- 语音对话 API ---

// handleVoiceChatStatus GET /api/v1/voicechat/status
func (s *Server) handleVoiceChatStatus(w http.ResponseWriter, r *http.Request) {
	enabled := s.voiceChatSvc != nil && s.voiceChatSvc.HasProvider()
	resp := map[string]any{
		"enabled":   enabled,
		"providers": []string{},
		"models":    []string{},
	}
	if enabled {
		resp["providers"] = s.voiceChatSvc.Providers()
		resp["models"] = s.voiceChatSvc.Models()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleVoiceChat POST /api/v1/voicechat/chat
//
// 请求体：
//
//	{
//	  "provider": "openai-gpt4o-audio",  // 可选
//	  "model":    "gpt-4o-audio-preview",
//	  "audio":    "<base64>",            // 用户语音
//	  "text":     "...",                  // 文本兜底
//	  "voice":    "alloy",                // 输出音色
//	  "format":   "wav"
//	}
//
// 响应：voicechat.Result（含 audio_file_path 持久化路径 + transcript）
func (s *Server) handleVoiceChat(w http.ResponseWriter, r *http.Request) {
	if s.voiceChatSvc == nil || !s.voiceChatSvc.HasProvider() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "语音对话服务未配置（需要 OpenAI gpt-4o-audio-preview API Key）",
		})
		return
	}

	const maxBody = 16 << 20 // 16MB（用户音频 b64 上限）
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	var req struct {
		Provider string `json:"provider,omitempty"`
		voicechat.Request
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求格式错误: " + err.Error()})
		return
	}

	result, err := s.voiceChatSvc.Chat(r.Context(), req.Provider, req.Request)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "对话失败: " + err.Error()})
		return
	}

	// 持久化模型音频回应（同 image/video gen）
	type voiceChatResponse struct {
		*voicechat.Result
		AudioFilePath string `json:"audio_file_path,omitempty"`
	}
	resp := voiceChatResponse{Result: result}
	if s.genStore != nil && result.ResponseB64 != "" {
		data, dErr := base64.StdEncoding.DecodeString(result.ResponseB64)
		if dErr != nil {
			logger.Warn("[voicechat] decode b64 failed",
				logger.String("model", result.Model), logger.Err(dErr))
		} else {
			ext := result.Format
			if ext == "" {
				ext = "wav"
			}
			saved, sErr := s.genStore.SaveBytes(data, ext)
			if sErr != nil {
				logger.Warn("[voicechat] save bytes failed",
					logger.String("model", result.Model), logger.Err(sErr))
			} else {
				resp.AudioFilePath = saved
				// 落盘后清空 b64，前端用 file_path
				result.ResponseB64 = ""
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
