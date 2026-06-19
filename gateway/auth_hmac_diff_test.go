package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/hexagon-codes/toolkit/crypto/sign"
)

// TestHMACMigration_DifferentialEquivalence 差分验证 [复用-HMAC]：
// toolkit/crypto/sign.VerifyHMACSHA256Hex 与原手搓 hex(hmac-sha256) 字节等价，
// 证明把 gateway/auth 的手搓 HMAC 换成 sign 不改变验签行为（接受/拒绝一致）。
func TestHMACMigration_DifferentialEquivalence(t *testing.T) {
	cases := []struct{ msg, key string }{
		{"1700000000", "secret-a"},
		{"", "k"},
		{"ts-with-中文-and-symbols!@#", "another-secret"},
		{"1699999999", ""},
	}
	for i, c := range cases {
		// 原手搓实现
		mac := hmac.New(sha256.New, []byte(c.key))
		mac.Write([]byte(c.msg))
		handRolled := hex.EncodeToString(mac.Sum(nil))

		// 正确签名：两实现都应接受
		if !sign.VerifyHMACSHA256Hex([]byte(c.msg), []byte(c.key), handRolled) {
			t.Errorf("case %d: sign 应接受手搓产出的签名（字节等价）", i)
		}
		// 篡改签名：应拒绝
		if sign.VerifyHMACSHA256Hex([]byte(c.msg), []byte(c.key), handRolled+"00") {
			t.Errorf("case %d: 篡改签名应被拒绝", i)
		}
		// 直接比对 hex 串相等（彻底确认字节一致）
		if got := sign.HMACSHA256Hex([]byte(c.msg), []byte(c.key)); got != handRolled {
			t.Errorf("case %d: HMACSHA256Hex=%s 应等于手搓 %s", i, got, handRolled)
		}
	}
}
