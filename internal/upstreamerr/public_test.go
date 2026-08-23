package upstreamerr

import (
	"errors"
	"testing"
)

func TestPublicMessage_StripsRawProviderBody(t *testing.T) {
	err := errors.New(`openai api error: 400 Bad Request, body: {"error":{"message":"Access denied, please make sure your account is in good standing.","type":"Arrearage","code":"Arrearage"},"id":"chatcmpl-123","request_id":"req-123"}`)

	got := PublicMessage(err, "处理消息失败")

	want := "Access denied, please make sure your account is in good standing. (code: Arrearage)"
	if got != want {
		t.Fatalf("期望 %q，实际 %q", want, got)
	}
}

// 真实事故复现（2026-06-25）：OpenRouter→Nvidia 免费 VL 模型返回的 body 里
// error.code 是数字（"code":400）而非字符串。旧 providerErrorBody.Code string
// 导致 json.Unmarshal 失败、净化回退、整坨原始 JSON 灌进聊天气泡。
func TestPublicMessage_ToleratesNumericCode(t *testing.T) {
	err := errors.New(`runtime stream 失败: llm complete: openai api error: 400 Bad Request, body: {"error":{"message":"Provider returned error","code":400,"metadata":{"raw":"{\"error\":{\"message\":\"Unterminated string\",\"type\":\"BadRequestError\",\"param\":null,\"code\":400}}","provider_name":"Nvidia","is_byok":false}},"user_id":"user_3CT0RSYU4fXLCL1v89Eo4dA0Zy8"}`)

	got := PublicMessage(err, "处理消息失败")

	want := "Provider returned error (code: 400)"
	if got != want {
		t.Fatalf("期望净化为 %q，实际 %q", want, got)
	}
}

func TestPublicMessage_StripsMalformedProviderBody(t *testing.T) {
	got := PublicMessage(
		errors.New(`openai api error: 429 Too Many Requests, body: {"error":"private-upstream-payload"`),
		"Provider connection test failed",
	)
	if got != "openai api error: 429 Too Many Requests" {
		t.Fatalf("expected public prefix without raw body")
	}
}

func TestPublicMessage_PreservesNonProviderErrors(t *testing.T) {
	err := errors.New("context deadline exceeded")

	got := PublicMessage(err, "处理消息失败")

	if got != "context deadline exceeded" {
		t.Fatalf("期望保留普通错误，实际 %q", got)
	}
}
