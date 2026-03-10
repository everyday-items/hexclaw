package wechat

import (
	"crypto/sha1"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/everyday-items/hexclaw/internal/adapter"
	"github.com/everyday-items/hexclaw/internal/config"
)

// TestNew 测试创建适配器
func TestNew(t *testing.T) {
	a := New(config.WechatConfig{
		AppID:     "wx123",
		AppSecret: "secret",
		Token:     "token",
	})
	if a == nil {
		t.Fatal("适配器不应为 nil")
	}
	if a.Name() != "wechat" {
		t.Errorf("名称不匹配: %q", a.Name())
	}
	if a.Platform() != adapter.PlatformWechat {
		t.Errorf("平台不匹配: %q", a.Platform())
	}
}

// TestStart_EmptyConfig 测试空配置
func TestStart_EmptyConfig(t *testing.T) {
	a := New(config.WechatConfig{})
	err := a.Start(nil, nil)
	if err == nil {
		t.Error("空 AppID/AppSecret 应返回错误")
	}
}

// TestStop_NotStarted 测试未启动时停止
func TestStop_NotStarted(t *testing.T) {
	a := New(config.WechatConfig{AppID: "wx", AppSecret: "secret"})
	if err := a.Stop(nil); err != nil {
		t.Errorf("未启动时停止应无错: %v", err)
	}
}

// TestCheckSignature 测试签名验证
func TestCheckSignature(t *testing.T) {
	token := "my-token"
	a := New(config.WechatConfig{Token: token})

	timestamp := "1234567890"
	nonce := "abc123"

	// 计算正确的签名
	strs := []string{token, timestamp, nonce}
	sort.Strings(strs)
	combined := strings.Join(strs, "")
	hash := sha1.Sum([]byte(combined))
	validSig := fmt.Sprintf("%x", hash)

	if !a.checkSignature(validSig, timestamp, nonce) {
		t.Error("正确签名应通过验证")
	}

	if a.checkSignature("wrong-sig", timestamp, nonce) {
		t.Error("错误签名应失败")
	}
}

// TestCheckSignature_Empty 测试空签名
func TestCheckSignature_Empty(t *testing.T) {
	a := New(config.WechatConfig{Token: "token"})

	// 空签名应失败
	if a.checkSignature("", "ts", "nonce") {
		t.Error("空签名应失败")
	}
}
