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

func TestPublicMessage_PreservesNonProviderErrors(t *testing.T) {
	err := errors.New("context deadline exceeded")

	got := PublicMessage(err, "处理消息失败")

	if got != "context deadline exceeded" {
		t.Fatalf("期望保留普通错误，实际 %q", got)
	}
}
