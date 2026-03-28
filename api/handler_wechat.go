package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleWechatQRStream 微信扫码登录 SSE 流
//
// POST /api/v1/channels/wechat/qr-stream
// SSE 事件类型:
//   - status: 进度状态 (generating / waiting / scanning)
//   - qr:     二维码内容 (base64 encoded image data)
//   - result: 登录成功 (nickname + avatar)
//   - error:  登录失败 (message)
//
// 对标 OpenClaw 的微信扫码 SSE 流。
func (s *Server) handleWechatQRStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Send status event
	sendSSE(w, flusher, "status", map[string]string{"status": "generating", "message": "正在生成二维码..."})

	// TODO: Integrate with openwechat library for actual QR generation
	// For now, send a placeholder that tells the user this feature is pending
	time.Sleep(500 * time.Millisecond)
	sendSSE(w, flusher, "status", map[string]string{"status": "pending", "message": "微信扫码功能开发中，请先通过配置文件接入"})

	// Timeout after informing the user
	sendSSE(w, flusher, "error", map[string]string{"message": "微信扫码接入需要 openwechat 库支持，当前版本请通过 hexclaw.yaml 配置微信适配器"})
}

// handleWecomGuide 企业微信接入引导
//
// GET /api/v1/channels/wecom/guide
// 返回企业微信接入的步骤说明和需要填写的配置字段。
func (s *Server) handleWecomGuide(w http.ResponseWriter, r *http.Request) {
	guide := map[string]any{
		"steps": []map[string]string{
			{"step": "1", "title": "创建企业微信自建应用", "description": "在企业微信管理后台 → 应用管理 → 创建应用"},
			{"step": "2", "title": "获取凭证", "description": "记录 CorpID、AgentID、Secret"},
			{"step": "3", "title": "配置回调 URL", "description": "设置消息接收地址为 HexClaw 的 webhook 端点"},
			{"step": "4", "title": "测试连接", "description": "发送测试消息验证连接"},
		},
		"required_fields": []map[string]string{
			{"field": "corp_id", "label": "企业 ID (CorpID)", "placeholder": "ww1234567890"},
			{"field": "agent_id", "label": "应用 ID (AgentID)", "placeholder": "1000001"},
			{"field": "secret", "label": "应用 Secret", "placeholder": "Secret Key"},
			{"field": "callback_token", "label": "回调 Token", "placeholder": "Token for callback verification"},
			{"field": "callback_aes_key", "label": "回调 EncodingAESKey", "placeholder": "43-char AES key"},
		},
		"callback_url": fmt.Sprintf("http://localhost:%d/api/v1/webhook/wecom", s.port()),
	}
	writeJSON(w, http.StatusOK, guide)
}

func (s *Server) port() int {
	if s.cfg != nil && s.cfg.Server.Port > 0 {
		return s.cfg.Server.Port
	}
	return 16060
}

func sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data any) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}
