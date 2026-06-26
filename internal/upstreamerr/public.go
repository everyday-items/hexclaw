package upstreamerr

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type providerErrorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		// Code 用 RawMessage 容纳两种形态：字符串（"Arrearage"）与数字（400）。
		// 上游各家不统一，写死成 string 会让数字 code 触发 Unmarshal 失败、整条净化回退。
		Code json.RawMessage `json:"code"`
	} `json:"error"`
}

// codeString 把 error.code 的原始 JSON token 规整成可读字符串：
// 字符串去引号，数字/其它原样返回，null/缺失返回空串。
func codeString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		if unq, err := strconv.Unquote(s); err == nil {
			return strings.TrimSpace(unq)
		}
	}
	return s
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

	code := codeString(body.Error.Code)
	if code != "" && !strings.Contains(strings.ToLower(message), strings.ToLower(code)) {
		message = fmt.Sprintf("%s (code: %s)", message, code)
	}
	return message, true
}
