package wechat

import (
	"crypto/sha1"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// Exercises the WeChat MP webhook entry points: the SHA1 signature gate shared
// by handleVerify and handleMessage, the echostr handshake, and the
// XML-parse / non-text guards on the message callback.

func wechatSign(token, timestamp, nonce string) string {
	strs := []string{token, timestamp, nonce}
	sort.Strings(strs)
	sum := sha1.Sum([]byte(strings.Join(strs, "")))
	return fmt.Sprintf("%x", sum)
}

func TestHandleVerify_ValidSignatureEchoesEchostr(t *testing.T) {
	a := New(config.WechatConfig{Token: "tk"})
	sig := wechatSign("tk", "100", "abc")
	url := fmt.Sprintf("/wh?signature=%s&timestamp=100&nonce=abc&echostr=hello-echo", sig)
	w := httptest.NewRecorder()

	a.handleVerify(w, httptest.NewRequest(http.MethodGet, url, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("valid verify = %d, want 200", w.Code)
	}
	if w.Body.String() != "hello-echo" {
		t.Fatalf("echostr = %q, want hello-echo", w.Body.String())
	}
}

func TestHandleVerify_InvalidSignatureForbidden(t *testing.T) {
	a := New(config.WechatConfig{Token: "tk"})
	url := "/wh?signature=wrong&timestamp=100&nonce=abc&echostr=hello"
	w := httptest.NewRecorder()

	a.handleVerify(w, httptest.NewRequest(http.MethodGet, url, nil))

	if w.Code != http.StatusForbidden {
		t.Fatalf("bad signature verify = %d, want 403", w.Code)
	}
}

func TestHandleMessage_RejectsInvalidSignature(t *testing.T) {
	a := New(config.WechatConfig{Token: "tk"})
	r := httptest.NewRequest(http.MethodPost,
		"/wh?signature=wrong&timestamp=100&nonce=abc", strings.NewReader("<xml></xml>"))
	w := httptest.NewRecorder()

	a.handleMessage(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("bad signature message = %d, want 403", w.Code)
	}
}

func TestHandleMessage_BadXMLReturns400(t *testing.T) {
	a := New(config.WechatConfig{Token: "tk"})
	sig := wechatSign("tk", "100", "abc")
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/wh?signature=%s&timestamp=100&nonce=abc", sig), strings.NewReader("<broken"))
	w := httptest.NewRecorder()

	a.handleMessage(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed XML = %d, want 400", w.Code)
	}
}

func TestHandleMessage_NonTextReturnsSuccess(t *testing.T) {
	a := New(config.WechatConfig{Token: "tk"})
	sig := wechatSign("tk", "100", "abc")
	xmlBody := `<xml><ToUserName>gh</ToUserName><FromUserName>oUser</FromUserName>` +
		`<MsgType>image</MsgType></xml>`
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/wh?signature=%s&timestamp=100&nonce=abc", sig), strings.NewReader(xmlBody))
	w := httptest.NewRecorder()

	a.handleMessage(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("non-text message = %d, want 200", w.Code)
	}
	if w.Body.String() != "success" {
		t.Fatalf("non-text body = %q, want success", w.Body.String())
	}
}
