package upstreamerr

import (
	"encoding/json"
	"fmt"
	"strings"
)

type providerErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

// PublicMessage converts provider-facing raw errors into a user-facing message.
// It keeps the actionable reason while stripping raw JSON payloads, request IDs,
// and other transport details that should remain in logs only.
func PublicMessage(err error, fallback string) string {
	if err == nil {
		return strings.TrimSpace(fallback)
	}

	raw := strings.Join(strings.Fields(strings.TrimSpace(err.Error())), " ")
	if raw == "" {
		return strings.TrimSpace(fallback)
	}

	if parsed, ok := parseProviderBody(raw); ok {
		return parsed
	}

	return raw
}

func parseProviderBody(raw string) (string, bool) {
	idx := strings.LastIndex(raw, "body:")
	if idx < 0 {
		return "", false
	}

	payload := strings.TrimSpace(raw[idx+len("body:"):])
	if payload == "" {
		return "", false
	}

	var body providerErrorBody
	if err := json.Unmarshal([]byte(payload), &body); err != nil {
		return "", false
	}

	message := strings.TrimSpace(body.Error.Message)
	if message == "" {
		return "", false
	}

	code := strings.TrimSpace(body.Error.Code)
	if code != "" && !strings.Contains(strings.ToLower(message), strings.ToLower(code)) {
		message = fmt.Sprintf("%s (code: %s)", message, code)
	}
	return message, true
}
