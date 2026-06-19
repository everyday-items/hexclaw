package slack

// audit_wave3_apiok_test.go —— Slack 出站 Web API ok:false 处理 + 401 错误信息审计
//
// 覆盖重点：
//   - W3-19: postThreadMessage / updateMessage / fetchBotID 必须校验 Slack API
//            响应中的 ok 字段，ok=false 时返回/记录错误，而非把失败当成功。
//   - W3-20: 签名校验失败时返回的 401 响应体应是有意义的错误信息，而非写死 "name"。
//
// 测试通过把 slackAPIBase 临时指向 httptest mock 服务器，断言对
// {"ok":false,"error":"..."} 响应的处理，无需真实网络。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
)

// withMockSlackAPI 临时把 slackAPIBase 指向 mock 服务器，返回还原函数。
func withMockSlackAPI(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	orig := slackAPIBase
	slackAPIBase = srv.URL
	t.Cleanup(func() {
		slackAPIBase = orig
		srv.Close()
	})
	return srv
}

// okFalseHandler 始终返回 ok:false 的 Slack API 响应（HTTP 200，但业务失败）。
func okFalseHandler(errCode string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":false,"error":"` + errCode + `"}`))
	}
}

// 回归: W3-19 —— postThreadMessage 必须在 Slack API ok=false 时返回 error。
//
// Slack Web API 即使在业务失败（如 channel_not_found / not_in_channel）时
// 仍返回 HTTP 200，仅通过 body 中的 "ok":false 表达失败。源码原先只看
// resp.Body.Close()，不解析 ok，导致发送失败被当成成功 —— 功能未闭环。
func TestPostThreadMessage_OKFalseReturnsError(t *testing.T) {
	withMockSlackAPI(t, okFalseHandler("channel_not_found"))
	a := New(config.SlackConfig{Token: "t"})

	err := a.postThreadMessage(context.Background(), "C1", "1700.1", "hi")
	if err == nil {
		t.Fatal("Slack API ok=false 时 postThreadMessage 应返回 error，却返回 nil（失败被当成功）")
	}
	if !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("error 应包含 Slack 错误码 channel_not_found，得到: %v", err)
	}
}

// 回归: W3-19 —— updateMessage 必须在 Slack API ok=false 时返回 error。
func TestUpdateMessage_OKFalseReturnsError(t *testing.T) {
	withMockSlackAPI(t, okFalseHandler("message_not_found"))
	a := New(config.SlackConfig{Token: "t"})

	err := a.updateMessage(context.Background(), "C1", "1700.1", "updated")
	if err == nil {
		t.Fatal("Slack API ok=false 时 updateMessage 应返回 error，却返回 nil（失败被当成功）")
	}
	if !strings.Contains(err.Error(), "message_not_found") {
		t.Errorf("error 应包含 Slack 错误码 message_not_found，得到: %v", err)
	}
}

// 回归: W3-19 —— fetchBotID 在 Slack API ok=false 时不得污染 botID。
//
// 原源码不解析 ok，且把 user_id（ok=false 时为空）直接赋给 a.botID。
// 修复后：ok=false 时记录错误并保持 botID 为空，不把失败响应当成功。
func TestFetchBotID_OKFalseDoesNotSetBotID(t *testing.T) {
	withMockSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// ok=false 且无 user_id，模拟 invalid_auth
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	})
	a := New(config.SlackConfig{Token: "bad-token"})

	a.fetchBotID()
	if a.botID != "" {
		t.Errorf("ok=false 时 botID 应保持为空，却被设置为 %q", a.botID)
	}
}

// TestFetchBotID_OKTrueSetsBotID 正向：ok=true 时正确解析 user_id。
func TestFetchBotID_OKTrueSetsBotID(t *testing.T) {
	withMockSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"user_id":"UBOTXYZ"}`))
	})
	a := New(config.SlackConfig{Token: "t"})

	a.fetchBotID()
	if a.botID != "UBOTXYZ" {
		t.Errorf("ok=true 时 botID 应为 UBOTXYZ，得到 %q", a.botID)
	}
}

// TestPostThreadMessage_OKTrueSucceeds 正向：ok=true 时不报错。
func TestPostThreadMessage_OKTrueSucceeds(t *testing.T) {
	withMockSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1700.9"}`))
	})
	a := New(config.SlackConfig{Token: "t"})
	if err := a.postThreadMessage(context.Background(), "C1", "1700.1", "hi"); err != nil {
		t.Errorf("ok=true 时 postThreadMessage 不应报错，得到 %v", err)
	}
}

// TestUpdateMessage_OKTrueSucceeds 正向：ok=true 时不报错。
func TestUpdateMessage_OKTrueSucceeds(t *testing.T) {
	withMockSlackAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"ts":"1700.9"}`))
	})
	a := New(config.SlackConfig{Token: "t"})
	if err := a.updateMessage(context.Background(), "C1", "1700.1", "x"); err != nil {
		t.Errorf("ok=true 时 updateMessage 不应报错，得到 %v", err)
	}
}

// 回归: W3-20 —— 签名校验失败的 401 响应体应是有意义的错误信息，而非写死 "name"。
//
// 原源码 http.Error(w, "name", 401)，响应体字面量 "name" 无任何诊断价值。
func TestHandleEvents_InvalidSignatureBodyIsMeaningful(t *testing.T) {
	a := New(config.SlackConfig{SigningSecret: "real-secret"})
	w := httptest.NewRecorder()

	// 构造一个签名错误的请求（用错误 secret 签名）。
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(`{"type":"event_callback"}`)
	r := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(body)))
	r.Header.Set("X-Slack-Request-Timestamp", ts)
	r.Header.Set("X-Slack-Signature", signV0("attacker-secret", ts, body))

	a.handleEvents(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("签名错误应返回 401，得到 %d", w.Code)
	}
	respBody := strings.TrimSpace(w.Body.String())
	if respBody == "name" {
		t.Fatalf("401 响应体写死为无意义字符串 %q，应返回有意义的错误信息", respBody)
	}
	if respBody == "" {
		t.Fatal("401 响应体不应为空，应说明签名校验失败")
	}
	// 应包含可诊断的关键词（签名 / signature）。
	low := strings.ToLower(respBody)
	if !strings.Contains(low, "signature") && !strings.Contains(respBody, "签名") {
		t.Errorf("401 响应体应说明签名校验失败，得到: %q", respBody)
	}
}
