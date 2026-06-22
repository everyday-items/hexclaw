package config

import "testing"

// TestBug20260622_IsMaskedKeyExactFormat 回归锁定 [BUG-MASKED-KEY-FALSEPOS]：
// IsMaskedKey 此前只判 `HasPrefix("****")`，会把「恰以 **** 开头但更长的真实 Key」
// 误判为脱敏占位值；合并配置时该 Key 被当作「用户没改」而丢弃 → 真实 Key 丢失。
//
// MaskAPIKey 的输出只有两种形态：恰好 "****"(len4) 或 "****"+末4位(len8)。
// IsMaskedKey 必须精确匹配该形态，长度 >8 的一律视为真实 Key。
func TestBug20260622_IsMaskedKeyExactFormat(t *testing.T) {
	// 真实 Key（>8 字符）即便以 **** 开头也不应被判脱敏
	if IsMaskedKey("****realLongSecretKeyABCD1234") {
		t.Error("以 **** 开头的长真实 Key 被误判为脱敏值 → 合并时会丢失该 Key")
	}
	// 脱敏值仍须正确识别
	if !IsMaskedKey("****") {
		t.Error(`"****" 应判为脱敏`)
	}
	if !IsMaskedKey("****6789") {
		t.Error(`"****6789"(len8) 应判为脱敏`)
	}
	// round-trip：MaskAPIKey 的输出必然被 IsMaskedKey 认出
	for _, raw := range []string{"sk-abcdefghijklmnop", "short", "x", "ghp_1234567890ABCDEF"} {
		masked := MaskAPIKey(raw)
		if masked != "" && !IsMaskedKey(masked) {
			t.Errorf("MaskAPIKey(%q)=%q 未被 IsMaskedKey 识别", raw, masked)
		}
	}
}
