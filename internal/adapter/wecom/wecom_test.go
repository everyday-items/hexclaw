package wecom

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
	a := New(config.WecomConfig{
		CorpID: "corp123",
		Secret: "secret",
		Token:  "token",
	})
	if a == nil {
		t.Fatal("适配器不应为 nil")
	}
	if a.Name() != "wecom" {
		t.Errorf("名称不匹配: %q", a.Name())
	}
	if a.Platform() != adapter.PlatformWecom {
		t.Errorf("平台不匹配: %q", a.Platform())
	}
}

// TestStart_EmptyConfig 测试空配置
func TestStart_EmptyConfig(t *testing.T) {
	a := New(config.WecomConfig{})
	err := a.Start(nil, nil)
	if err == nil {
		t.Error("空 CorpID/Secret 应返回错误")
	}
}

// TestStop_NotStarted 测试未启动时停止
func TestStop_NotStarted(t *testing.T) {
	a := New(config.WecomConfig{CorpID: "corp", Secret: "secret"})
	if err := a.Stop(nil); err != nil {
		t.Errorf("未启动时停止应无错: %v", err)
	}
}

// TestCheckSignature 测试签名验证
func TestCheckSignature(t *testing.T) {
	token := "test-token"
	a := New(config.WecomConfig{Token: token})

	timestamp := "1234567890"
	nonce := "nonce123"
	encrypt := "encrypted-data"

	// 计算正确的签名
	strs := []string{token, timestamp, nonce, encrypt}
	sort.Strings(strs)
	combined := strings.Join(strs, "")
	hash := sha1.Sum([]byte(combined))
	validSig := fmt.Sprintf("%x", hash)

	if !a.checkSignature(validSig, timestamp, nonce, encrypt) {
		t.Error("正确签名应通过验证")
	}

	if a.checkSignature("invalid", timestamp, nonce, encrypt) {
		t.Error("错误签名应失败")
	}
}

// TestNew_AESKey 测试 AES Key 解码
func TestNew_AESKey(t *testing.T) {
	// 43 字符的 Base64 编码（标准 EncodingAESKey 长度）
	// 加上 "=" 补位后应解码为 32 字节
	a := New(config.WecomConfig{
		AESKey: "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY",
	})
	if len(a.aesKey) == 0 {
		t.Error("AES Key 应被成功解码")
	}
}

// TestNew_InvalidAESKey 测试无效 AES Key
func TestNew_InvalidAESKey(t *testing.T) {
	a := New(config.WecomConfig{
		AESKey: "!@#$%^&*()", // 无效 Base64
	})
	if len(a.aesKey) != 0 {
		t.Error("无效 AES Key 应解码为空")
	}
}

// TestDecrypt_NoKey 测试未配置 AES Key 时解密
func TestDecrypt_NoKey(t *testing.T) {
	a := New(config.WecomConfig{})
	_, err := a.decrypt("encrypted")
	if err == nil {
		t.Error("未配置 AES Key 时应返回错误")
	}
}
