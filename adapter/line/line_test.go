package line

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestLineAdapter_NameAndPlatform(t *testing.T) {
	a := New(Config{ChannelToken: "test", ChannelSecret: "secret"})
	if a.Name() != "line" {
		t.Errorf("期望 line, 得到 %s", a.Name())
	}
	if a.Platform() != PlatformLINE {
		t.Errorf("期望 line, 得到 %s", a.Platform())
	}
}

func TestLineAdapter_DefaultConfig(t *testing.T) {
	a := New(Config{})
	if a.config.WebhookPort != 6064 {
		t.Errorf("期望默认端口 6064, 得到 %d", a.config.WebhookPort)
	}
}

func TestLineAdapter_VerifySignature(t *testing.T) {
	secret := "test-channel-secret"
	a := New(Config{ChannelSecret: secret})

	body := []byte(`{"events":[]}`)

	// 计算正确签名
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !a.verifySignature(body, validSig) {
		t.Error("有效签名应验证通过")
	}

	if a.verifySignature(body, "invalid-signature") {
		t.Error("无效签名应验证失败")
	}
}
