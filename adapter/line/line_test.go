package line

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	linewebhook "github.com/line/line-bot-sdk-go/v8/linebot/webhook"
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

func TestLineOfficialSDK_ValidateSignature(t *testing.T) {
	secret := "test-channel-secret"
	body := []byte(`{"events":[]}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	if !linewebhook.ValidateSignature(secret, validSig, body) {
		t.Error("有效签名应验证通过")
	}

	if linewebhook.ValidateSignature(secret, "invalid-signature", body) {
		t.Error("无效签名应验证失败")
	}
}

func TestLineAdapter_UsesOfficialLineSDK(t *testing.T) {
	src, err := os.ReadFile("line.go")
	if err != nil {
		t.Fatalf("读取 line.go 失败: %v", err)
	}
	raw := string(src)
	for _, must := range []string{
		"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api",
		"github.com/line/line-bot-sdk-go/v8/linebot/webhook",
		"linewebhook.ParseRequest",
		"messaging_api.NewMessagingApiAPI",
	} {
		if !strings.Contains(raw, must) {
			t.Fatalf("LINE adapter 必须优先使用官方 SDK，缺少 %q", must)
		}
	}
	for _, banned := range []string{
		"https://api.line.me/v2/bot/message/push",
		"https://api.line.me/v2/bot/message/reply",
		"VerifyHMACSHA256Base64",
		"func (a *LineAdapter) verifySignature",
	} {
		if strings.Contains(raw, banned) {
			t.Fatalf("LINE adapter 仍含手写 API/签名路径 %q，应统一走官方 SDK", banned)
		}
	}
}
