package secret

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSealOpenRoundTrip 验证 Seal→Open 还原出原始明文，且密文带 enc:v1: 前缀。
func TestSealOpenRoundTrip(t *testing.T) {
	box := newTestBox(t)
	plain := []byte(`{"app_id":"cli_xxx","app_secret":"s3cr3t"}`)

	sealed, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}
	if !IsEncrypted(sealed) {
		t.Fatalf("密文应带 enc:v1: 前缀，得到 %q", sealed)
	}
	// 密文里不应肉眼可见明文片段。
	if bytes.Contains([]byte(sealed), []byte("s3cr3t")) {
		t.Fatal("密文中不应出现明文凭据片段")
	}

	opened, err := box.Open(sealed)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if !bytes.Equal(opened, plain) {
		t.Fatalf("还原明文不一致：got %q want %q", opened, plain)
	}
}

// TestSealUniqueNonce 验证同明文两次 Seal 产出不同密文（nonce 随机）。
func TestSealUniqueNonce(t *testing.T) {
	box := newTestBox(t)
	plain := []byte("same plaintext")
	a, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal a 失败: %v", err)
	}
	b, err := box.Seal(plain)
	if err != nil {
		t.Fatalf("Seal b 失败: %v", err)
	}
	if a == b {
		t.Fatal("两次 Seal 同明文应产出不同密文（nonce 必须每次随机）")
	}
}

// TestOpenTamperedFails 验证篡改密文后 Open 报错（GCM 认证）。
func TestOpenTamperedFails(t *testing.T) {
	box := newTestBox(t)
	sealed, err := box.Seal([]byte("integrity matters"))
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}
	// 翻转密文末尾一个字符（仍是合法 base64 字符集内的改动概率高，但即便不是也应报错）。
	tampered := sealed[:len(sealed)-1]
	if sealed[len(sealed)-1] == 'A' {
		tampered += "B"
	} else {
		tampered += "A"
	}
	if _, err := box.Open(tampered); err == nil {
		t.Fatal("篡改后的密文 Open 应失败")
	}
}

// TestOpenWrongKeyFails 验证用另一把密钥无法解开。
func TestOpenWrongKeyFails(t *testing.T) {
	box := newTestBox(t)
	sealed, err := box.Seal([]byte("secret data"))
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}

	other := newTestBox(t) // 不同 dir → 不同随机密钥
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("用错误密钥 Open 应失败")
	}
}

// TestOpenPlaintextReturnsErrNotEncrypted 验证历史明文（无前缀）走 ErrNotEncrypted 分支。
func TestOpenPlaintextReturnsErrNotEncrypted(t *testing.T) {
	box := newTestBox(t)
	if _, err := box.Open(`{"legacy":"plaintext"}`); !errors.Is(err, ErrNotEncrypted) {
		t.Fatalf("明文 Open 应返回 ErrNotEncrypted，得到 %v", err)
	}
}

// TestLoadBoxPersistsAndReloads 验证主密钥落盘后重载得到同一把密钥（同一密文能跨实例解开），
// 且密钥文件权限为 0600。
func TestLoadBoxPersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	box1, err := LoadBox(dir)
	if err != nil {
		t.Fatalf("首次 LoadBox 失败: %v", err)
	}
	sealed, err := box1.Seal([]byte("persist me"))
	if err != nil {
		t.Fatalf("Seal 失败: %v", err)
	}

	// 校验密钥文件存在且权限 0600。
	keyPath := filepath.Join(dir, masterKeyFile)
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("主密钥文件应存在: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("主密钥文件权限应为 0600，实际 %o", perm)
	}

	// 重载同一目录，应能解开 box1 的密文（即密钥一致）。
	box2, err := LoadBox(dir)
	if err != nil {
		t.Fatalf("二次 LoadBox 失败: %v", err)
	}
	opened, err := box2.Open(sealed)
	if err != nil {
		t.Fatalf("重载后 Open 失败（密钥应一致）: %v", err)
	}
	if string(opened) != "persist me" {
		t.Fatalf("还原明文不一致: %q", opened)
	}
}

// TestNewBoxRejectsBadKeyLen 验证非 32 字节密钥被拒绝。
func TestNewBoxRejectsBadKeyLen(t *testing.T) {
	if _, err := NewBox([]byte("short")); err == nil {
		t.Fatal("短密钥应被拒绝")
	}
}

// newTestBox 用一个临时目录的随机密钥构造 Box。
func newTestBox(t *testing.T) *Box {
	t.Helper()
	box, err := LoadBox(t.TempDir())
	if err != nil {
		t.Fatalf("LoadBox 失败: %v", err)
	}
	return box
}
