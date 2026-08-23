package api

import (
	"net/http"

	"github.com/hexagon-codes/hexclaw/internal/upstreamerr"
	"github.com/hexagon-codes/hexclaw/llmrouter"
)

// llmErrorPayload 生成聊天传输共用的错误体，保持机器码与用户可读信息同步。
func llmErrorPayload(err error) map[string]any {
	payload := map[string]any{
		"error": upstreamerr.PublicMessage(err, "error"),
		"done":  true,
	}
	if classification, ok := llmrouter.ClassifyLLMError(err); ok {
		payload["code"] = string(classification.Code)
		payload["retryable"] = classification.Retryable
	}
	return payload
}

func writeChatLLMError(w http.ResponseWriter, status int, err error) {
	payload := llmErrorPayload(err)
	delete(payload, "done")
	writeJSON(w, status, payload)
}
