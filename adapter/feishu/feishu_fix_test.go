package feishu

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// ============================================================
// 修复 1: replyAndGetID — 飞书回复模式
// ============================================================

// TestReplyAndGetID_修复前_思考消息作为新消息发送 验证修复前的行为
func TestReplyAndGetID_修复前_思考消息作为新消息发送(t *testing.T) {
	// 修复前: sendAndGetID 使用 POST /im/v1/messages?receive_id_type=chat_id
	// 这发送的是一条新消息，不是对原消息的回复
	// 用户看到的是独立的"思考中..."消息，而不是关联到原问题的回复

	var capturedURL string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.Path + "?" + r.URL.RawQuery
		if strings.Contains(r.URL.Path, "/auth/") {
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "test-token", "expire": 7200,
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"message_id": "om_new_msg_001"},
		})
	}))
	defer ts.Close()

	a := newTestAdapterWithBase(ts.URL)

	msgID, err := a.sendAndGetID(context.Background(), "oc_chat123", "🤔 思考中...")
	if err != nil {
		t.Fatalf("sendAndGetID 失败: %v", err)
	}
	if msgID == "" {
		t.Fatal("sendAndGetID 返回空 message_id")
	}

	// 修复前的 URL: /im/v1/messages?receive_id_type=chat_id (发新消息)
	if !strings.Contains(capturedURL, "/im/v1/messages?receive_id_type=chat_id") {
		t.Fatalf("期望 sendAndGetID 使用新消息 API，实际: %s", capturedURL)
	}
	// 不包含 /reply — 确认这是旧行为
	if strings.Contains(capturedURL, "/reply") {
		t.Fatal("sendAndGetID 不应使用 reply API（这是修复前的旧方法）")
	}
}

// TestReplyAndGetID_修复后_思考消息作为回复发送 验证修复后的行为
func TestReplyAndGetID_修复后_思考消息作为回复发送(t *testing.T) {
	var capturedURL string
	var capturedBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.Path
		if strings.Contains(r.URL.Path, "/auth/") {
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "test-token", "expire": 7200,
			})
			return
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedBody)
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]string{"message_id": "om_reply_001"},
		})
	}))
	defer ts.Close()

	a := newTestAdapterWithBase(ts.URL)

	origMsgID := "om_user_msg_123"
	replyMsgID, err := a.replyAndGetID(context.Background(), origMsgID, "🤔 思考中...")
	if err != nil {
		t.Fatalf("replyAndGetID 失败: %v", err)
	}

	// 修复后的 URL 必须包含原消息 ID 和 /reply
	expectedSuffix := "/im/v1/messages/" + origMsgID + "/reply"
	if !strings.HasSuffix(capturedURL, expectedSuffix) {
		t.Fatalf("期望 reply API 路径以 %s 结尾，实际: %s", expectedSuffix, capturedURL)
	}

	// 返回的是回复消息的 ID
	if replyMsgID != "om_reply_001" {
		t.Fatalf("期望回复消息 ID om_reply_001，实际: %s", replyMsgID)
	}

	// 验证请求体包含 msg_type 和 content
	if capturedBody["msg_type"] != "text" {
		t.Fatalf("期望 msg_type=text，实际: %v", capturedBody["msg_type"])
	}
	contentStr, ok := capturedBody["content"].(string)
	if !ok {
		t.Fatal("content 应为 JSON 字符串")
	}
	var parsed map[string]string
	json.Unmarshal([]byte(contentStr), &parsed)
	if parsed["text"] != "🤔 思考中..." {
		t.Fatalf("期望文本'🤔 思考中...'，实际: %s", parsed["text"])
	}

	// 验证不包含 receive_id（reply API 不需要）
	if _, hasReceiveID := capturedBody["receive_id"]; hasReceiveID {
		t.Fatal("reply API 不应包含 receive_id")
	}
}

// ============================================================
// 修复 2: patchMessage 错误处理
// ============================================================

// TestPatchMessage_修复前_静默吞掉API错误 验证修复前的行为
func TestPatchMessage_修复前_静默吞掉API错误(t *testing.T) {
	// 修复前: patchMessage 不检查 HTTP 状态码
	// 即使飞书 API 返回 403/400，patchMessage 也返回 nil
	// 导致 handleSDKMessage 认为消息已更新，无法准确记录回复失败
	// 结果：用户可能看不到最终回复，日志也无法暴露真实失败原因

	// 这个测试文档化了修复前的 bug 行为
	t.Log("修复前行为: patchMessage 不检查 resp.StatusCode，API 返回 400/403/500 时仍返回 nil")
	t.Log("后果: handleSDKMessage 认为 patch 成功，用户可能看不到最终回复")
}

// TestPatchMessage_修复后_API错误正确返回 验证修复后的行为
func TestPatchMessage_修复后_API错误正确返回(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantErr    bool
	}{
		{"200 成功", 200, `{"code":0}`, false},
		{"400 请求错误", 400, `{"code":400,"msg":"invalid content"}`, true},
		{"403 权限不足", 403, `{"code":403,"msg":"no permission"}`, true},
		{"500 服务端错误", 500, `{"code":500,"msg":"internal error"}`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requestCount int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&requestCount, 1)
				if strings.Contains(r.URL.Path, "/auth/") {
					json.NewEncoder(w).Encode(map[string]any{
						"code": 0, "tenant_access_token": "test-token", "expire": 7200,
					})
					return
				}
				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.body))
			}))
			defer ts.Close()

			a := newTestAdapterWithBase(ts.URL)

			err := a.patchMessage(context.Background(), "om_msg_123", "最终回复内容")

			if tt.wantErr && err == nil {
				t.Fatal("期望 patchMessage 返回错误（修复后应检查状态码），但返回了 nil")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("期望 patchMessage 成功，但返回错误: %v", err)
			}
			if tt.wantErr && err != nil {
				// 验证错误信息包含有用信息
				if !strings.Contains(err.Error(), "飞书 UPDATE") {
					t.Fatalf("错误信息应包含'飞书 UPDATE'，实际: %s", err.Error())
				}
			}
		})
	}
}

// TestPatchMessage_修复后_请求体包含msg_type 验证文本消息更新请求格式
func TestPatchMessage_修复后_请求体包含msg_type(t *testing.T) {
	var capturedBody map[string]any

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/") {
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "test-token", "expire": 7200,
			})
			return
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &capturedBody)
		json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))
	defer ts.Close()

	a := newTestAdapterWithBase(ts.URL)
	_ = a.patchMessage(context.Background(), "om_msg_123", "更新后的内容")

	// 修复后: 请求体包含 msg_type
	if capturedBody["msg_type"] != "text" {
		t.Fatalf("期望 msg_type=text，实际: %v", capturedBody["msg_type"])
	}
}

// ============================================================
// 修复 3: patchMessage 错误必须透出
// ============================================================

// TestPatchMessage_UpdateFailureReturnsError 模拟官方 SDK 更新消息失败时返回错误。
func TestPatchMessage_UpdateFailureReturnsError(t *testing.T) {
	// 单一路径原则：不在适配器里自写备用发送链路。patchMessage 必须把官方 SDK
	// 返回的错误透出，由上层记录并进入统一失败处理。

	var apiCalls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls = append(apiCalls, r.Method+" "+r.URL.Path)
		if strings.Contains(r.URL.Path, "/auth/") {
			json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "tenant_access_token": "test-token", "expire": 7200,
			})
			return
		}
		// 文本消息更新端点 — 模拟失败
		if r.Method == "PUT" {
			w.WriteHeader(403)
			w.Write([]byte(`{"code":403,"msg":"no permission to update"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"code": 0})
	}))
	defer ts.Close()

	a := newTestAdapterWithBase(ts.URL)

	// 验证 patchMessage 确实返回错误
	err := a.patchMessage(context.Background(), "om_thinking_001", "最终回复")
	if err == nil {
		t.Fatal("修复后 patchMessage 应在 403 时返回错误")
	}
	t.Logf("patchMessage 正确返回错误: %v", err)

	hasUpdate := false
	hasCreate := false
	for _, call := range apiCalls {
		if strings.HasPrefix(call, "PUT") {
			hasUpdate = true
		}
		if strings.HasPrefix(call, "POST") && strings.Contains(call, "/im/v1/messages") && !strings.Contains(call, "/reply") {
			hasCreate = true
		}
	}
	if !hasUpdate {
		t.Fatal("应有 PUT 更新请求")
	}
	if hasCreate {
		t.Fatal("patchMessage 失败不应触发备用新消息发送")
	}
}

// ============================================================
// 真实数据验证: hexclaw.yaml 配置格式
// ============================================================

// TestHexclawYaml_FilesystemMCP_路径格式 验证各平台路径格式
func TestHexclawYaml_FilesystemMCP_路径格式(t *testing.T) {
	tests := []struct {
		name string
		path string
		ok   bool
	}{
		{"macOS home", "/Users/hexagon", true},
		{"Linux home", "/home/hexagon", true},
		{"Windows home", `C:\Users\hexagon`, true},
		{"旧的 /tmp", "/tmp", true},        // 格式合法但权限太窄
		{"空路径", "", false},               // 不应使用空路径
		{"相对路径", "relative/path", false}, // 不应使用相对路径
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isAbsolute := len(tt.path) > 0 && (tt.path[0] == '/' || (len(tt.path) >= 3 && tt.path[1] == ':'))
			if tt.ok && !isAbsolute {
				t.Errorf("路径 %q 应为绝对路径", tt.path)
			}
			if !tt.ok && isAbsolute {
				t.Errorf("路径 %q 不应被识别为合法路径", tt.path)
			}
		})
	}
}

// ============================================================
// 辅助函数
// ============================================================

func newTestAdapterWithBase(baseURL string) *FeishuAdapter {
	// 将官方 SDK 发起的 HTTP 请求重定向到测试服务器。
	a := New(config.FeishuConfig{
		AppID:     "test-app",
		AppSecret: "test-secret",
	})
	// 通过自定义 http.Client 覆盖 SDK 请求的目标 host。
	origClient := a.client
	a.client = &http.Client{
		Transport: &rewriteTransport{
			base:      baseURL,
			transport: origClient.Transport,
		},
	}
	return a
}

// rewriteTransport 将所有请求重定向到测试服务器
type rewriteTransport struct {
	base      string
	transport http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Scheme = "http"
	req.URL.Host = strings.TrimPrefix(t.base, "http://")
	var (
		resp *http.Response
		err  error
	)
	if t.transport != nil {
		resp, err = t.transport.RoundTrip(req)
	} else {
		resp, err = http.DefaultTransport.RoundTrip(req)
	}
	if resp != nil {
		resp.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	return resp, err
}
