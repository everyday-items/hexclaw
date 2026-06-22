// Package secret 为本地落盘的敏感数据（平台凭据等）提供透明的「静态加密」。
//
// 设计目标：让上层（instances.Manager）在写库时把明文 JSON 封成密文串、读库时还原成明文，
// 而不必关心密钥管理与算法细节。算法选用 AES-256-GCM（authenticated encryption），
// 既保密又防篡改：任何对密文的改动都会令 Open 解密失败。
//
// 密文串格式：字面前缀 "enc:v1:" + base64( nonce(12 字节) ‖ ciphertext+tag )。
//   - 前缀用于在库里区分「已加密」与「历史明文」，便于平滑迁移与向后兼容。
//   - 每次 Seal 都用 crypto/rand 生成全新 nonce，绝不复用（GCM 的安全前提）。
//
// 主密钥：32 字节随机串，首次使用时由 crypto/rand 生成并落盘到 <dir>/master.key，
// 文件权限 0600（仅属主可读写）。绝不记录明文凭据或主密钥到日志。
package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tkaes "github.com/hexagon-codes/toolkit/crypto/aes"
)

// encPrefix 是密文串的版本化前缀，用于区分已加密值与历史明文值。
const encPrefix = "enc:v1:"

// masterKeyLen 是 AES-256 主密钥长度（字节）。
const masterKeyLen = 32

// masterKeyFile 是主密钥在数据目录下的文件名。
const masterKeyFile = "master.key"

// ErrNotEncrypted 表示传入 Open 的串不带 enc:v1: 前缀（即历史明文），调用方应原样处理。
var ErrNotEncrypted = errors.New("secret: 值不是 enc:v1: 密文")

// Box 持有 AES-256-GCM 主密钥，负责 Seal/Open。零值不可用，须经 NewBox/LoadBox 构造。
// 内层 GCM 加解密委托 toolkit/crypto/aes（EncryptGCM/DecryptGCM，nonce 前置，逐字节兼容历史落盘格式）。
type Box struct {
	key []byte
}

// NewBox 用给定的 32 字节主密钥构造 Box。密钥长度不符立即报错，避免静默降级。
func NewBox(key []byte) (*Box, error) {
	if len(key) != masterKeyLen {
		return nil, fmt.Errorf("secret: 主密钥长度必须为 %d 字节，实际 %d", masterKeyLen, len(key))
	}
	// 复制一份，避免调用方后续改动底层数组影响已构造的 Box。
	k := make([]byte, masterKeyLen)
	copy(k, key)
	return &Box{key: k}, nil
}

// LoadBox 从 <dir>/master.key 加载主密钥；文件不存在时用 crypto/rand 生成并以 0600 落盘。
// dir 不存在会先以 0700 创建（与 ~/.hexclaw 目录权限一致）。
func LoadBox(dir string) (*Box, error) {
	if dir == "" {
		return nil, errors.New("secret: 数据目录不能为空")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secret: 创建数据目录失败: %w", err)
	}
	path := filepath.Join(dir, masterKeyFile)

	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(key) != masterKeyLen {
			return nil, fmt.Errorf("secret: 主密钥文件 %s 长度异常（%d 字节），疑似损坏", path, len(key))
		}
	case errors.Is(err, os.ErrNotExist):
		key = make([]byte, masterKeyLen)
		if _, rerr := rand.Read(key); rerr != nil {
			return nil, fmt.Errorf("secret: 生成主密钥失败: %w", rerr)
		}
		// 先写临时文件再 rename，保证落盘原子；权限 0600 在 OpenFile 时即设定。
		tmp := path + ".tmp"
		if werr := os.WriteFile(tmp, key, 0o600); werr != nil {
			return nil, fmt.Errorf("secret: 写入主密钥失败: %w", werr)
		}
		if rerr := os.Rename(tmp, path); rerr != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("secret: 落盘主密钥失败: %w", rerr)
		}
	default:
		return nil, fmt.Errorf("secret: 读取主密钥失败: %w", err)
	}

	return NewBox(key)
}

// Seal 用全新随机 nonce 加密 plaintext，返回 "enc:v1:base64(nonce‖ciphertext+tag)"。
// 内层 GCM 委托 toolkit/crypto/aes.EncryptGCM：其 blob 布局为 nonce(12B)‖ciphertext+tag，
// 与历史落盘格式逐字节一致。
func (b *Box) Seal(plaintext []byte) (string, error) {
	if b == nil || len(b.key) == 0 {
		return "", errors.New("secret: Box 未初始化")
	}
	blob, err := tkaes.EncryptGCM(plaintext, b.key)
	if err != nil {
		return "", fmt.Errorf("secret: GCM 加密失败: %w", err)
	}
	return encPrefix + base64.StdEncoding.EncodeToString(blob), nil
}

// Open 解密 Seal 产出的串。串不带 enc:v1: 前缀时返回 ErrNotEncrypted（历史明文）。
// 密文被篡改或密钥不匹配时返回非 nil 错误（GCM 认证失败）。
// 内层 GCM 委托 toolkit/crypto/aes.DecryptGCM：按 nonce(12B) 前置切片后 Open，与历史落盘格式一致。
func (b *Box) Open(s string) ([]byte, error) {
	if b == nil || len(b.key) == 0 {
		return nil, errors.New("secret: Box 未初始化")
	}
	if !IsEncrypted(s) {
		return nil, ErrNotEncrypted
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, encPrefix))
	if err != nil {
		return nil, fmt.Errorf("secret: 密文 base64 解码失败: %w", err)
	}
	plaintext, err := tkaes.DecryptGCM(blob, b.key)
	if err != nil {
		// 认证失败/长度不足：篡改或密钥不匹配。不回显密文内容。
		return nil, fmt.Errorf("secret: 解密失败（密文被篡改或密钥不匹配）: %w", err)
	}
	return plaintext, nil
}

// IsEncrypted 报告 s 是否为本包产出的 enc:v1: 密文串。
func IsEncrypted(s string) bool {
	return strings.HasPrefix(s, encPrefix)
}
