package wecom

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
)

// Exercises the WeCom decryption path end to end: an AES-256-CBC + PKCS7
// round-trip through decrypt(), its error guards, and the handleVerify echostr
// handshake (signature → decrypt → plaintext echo).

// 43-char EncodingAESKey whose base64("...=") decodes to a 32-byte AES-256 key.
const wecomTestAESKey = "YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY"

// wecomEncrypt mirrors the WeCom envelope: 16 random bytes + 4-byte BE length +
// message + corpid, PKCS7-padded and AES-256-CBC encrypted with IV = key[:16].
func wecomEncrypt(t *testing.T, key []byte, msg, corpid string) string {
	t.Helper()
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(msg)))

	plain := append([]byte("0123456789abcdef"), lenBuf[:]...)
	plain = append(plain, msg...)
	plain = append(plain, corpid...)

	padLen := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < padLen; i++ {
		plain = append(plain, byte(padLen))
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	ct := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, key[:aes.BlockSize]).CryptBlocks(ct, plain)
	return base64.StdEncoding.EncodeToString(ct)
}

func wecomSign(token, timestamp, nonce, encrypt string) string {
	strs := []string{token, timestamp, nonce, encrypt}
	sort.Strings(strs)
	sum := sha1.Sum([]byte(strings.Join(strs, "")))
	return fmt.Sprintf("%x", sum)
}

func TestDecrypt_RoundTrip(t *testing.T) {
	a := New(config.WecomConfig{AESKey: wecomTestAESKey})
	enc := wecomEncrypt(t, a.aesKey, "hello wecom", "corp-id-1")

	got, err := a.decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt round-trip failed: %v", err)
	}
	if got != "hello wecom" {
		t.Fatalf("decrypt = %q, want %q", got, "hello wecom")
	}
}

func TestDecrypt_InvalidBase64(t *testing.T) {
	a := New(config.WecomConfig{AESKey: wecomTestAESKey})
	if _, err := a.decrypt("!!!not-base64!!!"); err == nil {
		t.Fatal("decrypt of non-base64 must return an error")
	}
}

func TestDecrypt_ShortCiphertext(t *testing.T) {
	a := New(config.WecomConfig{AESKey: wecomTestAESKey})
	short := base64.StdEncoding.EncodeToString([]byte("tiny"))
	if _, err := a.decrypt(short); err == nil {
		t.Fatal("decrypt of sub-block ciphertext must return an error")
	}
}

func TestHandleVerify_ValidSignatureDecryptsEchostr(t *testing.T) {
	a := New(config.WecomConfig{Token: "tk", AESKey: wecomTestAESKey})
	enc := wecomEncrypt(t, a.aesKey, "verify-plain", "corp-id-1")
	// W3-27 修复后 handleVerify 追加重放窗口校验，需使用新鲜 timestamp 才能放行。
	ts := fmt.Sprintf("%d", time.Now().Unix())
	sig := wecomSign("tk", ts, "abc", enc)

	q := url.Values{}
	q.Set("msg_signature", sig)
	q.Set("timestamp", ts)
	q.Set("nonce", "abc")
	q.Set("echostr", enc)
	w := httptest.NewRecorder()

	a.handleVerify(w, httptest.NewRequest(http.MethodGet, "/wh?"+q.Encode(), nil))

	if w.Code != http.StatusOK {
		t.Fatalf("valid verify = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if w.Body.String() != "verify-plain" {
		t.Fatalf("decrypted echostr = %q, want verify-plain", w.Body.String())
	}
}

func TestHandleVerify_InvalidSignatureForbidden(t *testing.T) {
	a := New(config.WecomConfig{Token: "tk", AESKey: wecomTestAESKey})
	q := url.Values{}
	q.Set("msg_signature", "wrong")
	q.Set("timestamp", "100")
	q.Set("nonce", "abc")
	q.Set("echostr", "whatever")
	w := httptest.NewRecorder()

	a.handleVerify(w, httptest.NewRequest(http.MethodGet, "/wh?"+q.Encode(), nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("bad signature verify = %d, want 403", w.Code)
	}
}

func TestHandleCallback_BadXMLReturns400(t *testing.T) {
	a := New(config.WecomConfig{Token: "tk", AESKey: wecomTestAESKey})
	r := httptest.NewRequest(http.MethodPost,
		"/wh?msg_signature=x&timestamp=100&nonce=abc", strings.NewReader("<broken"))
	w := httptest.NewRecorder()

	a.handleCallback(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed XML = %d, want 400", w.Code)
	}
}
