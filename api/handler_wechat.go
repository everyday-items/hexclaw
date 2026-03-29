package api

import (
	"fmt"
	"net/http"
)

// handleWecomGuide 企业微信接入引导
//
// GET /api/v1/channels/wecom/guide
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
