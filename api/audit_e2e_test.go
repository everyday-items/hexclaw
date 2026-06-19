package api

// audit_e2e_test.go —— HTTP API 端到端闭环测试。
//
// 目标：用 httptest.NewServer 起一个真实 HTTP 服务，对主要端点打真实 HTTP 请求，
// 验证完整调用链：请求解析 → 中间件（CORS/Auth）→ handler 处理 → 响应/错误码闭环。
//
// 与单测（直接调用 srv.handleXxx）的区别：
//   - 这里走完整 mux 路由 + apiAuthMiddleware + corsMiddleware 包装层，
//     能发现"路由没注册 / 中间件吞错 / 跨层错误码丢失"这类闭环断裂。
//   - 所有外部依赖（LLM 引擎 / 安全网关）都用 mock，确保确定性：
//     不打真实网络、不依赖墙钟、不依赖文件系统副作用。
//
// 复用 server_test.go 的 mockEngine / mockGateway，复用 sse_characterization_test.go 的 sseFrames。

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/gateway"
)

// newE2EServer 构造一个用 mock 引擎驱动的 httptest 真实 HTTP 服务。
//
// 返回 *httptest.Server（调用方负责 Close）和底层 mockEngine（便于断言引擎是否被调用）。
// gw 可为 nil（不挂安全网关）。
func newE2EServer(t *testing.T, reply *adapter.Reply, gw gateway.Gateway) (*httptest.Server, *mockEngine) {
	t.Helper()
	eng := &mockEngine{reply: reply}
	srv := NewServer(config.DefaultConfig(), eng, gw, nil)
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	return ts, eng
}

// doJSON 对 httptest 服务发起一个真实 HTTP 请求，返回响应（调用方负责 Body.Close）。
func doJSON(t *testing.T, ts *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	return resp
}

// decodeMap 把响应体解析为通用 map（用于断言 JSON wire format 字段是否齐全）。
func decodeMap(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 1：/api/v1/chat 成功路径（请求解析 → 引擎 → 响应）
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_Chat_HappyPath(t *testing.T) {
	ts, eng := newE2EServer(t, &adapter.Reply{
		Content:  "你好！有什么可以帮你的？",
		Metadata: map[string]string{"provider": "test"},
	}, nil)

	resp := doJSON(t, ts, http.MethodPost, "/api/v1/chat", `{"message":"你好","user_id":"u1"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("状态码 = %d, 期望 200, body=%s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q, 期望 application/json", ct)
	}
	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析 ChatResponse 失败: %v", err)
	}
	if out.Reply != "你好！有什么可以帮你的？" {
		t.Fatalf("reply 不匹配: %q", out.Reply)
	}
	if eng.calls != 1 {
		t.Fatalf("引擎调用次数 = %d, 期望 1", eng.calls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 2：/api/v1/chat 校验失败（空消息）→ 400，且引擎不被调用
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_Chat_EmptyMessage_400_NoEngineCall(t *testing.T) {
	ts, eng := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	resp := doJSON(t, ts, http.MethodPost, "/api/v1/chat", `{"message":""}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
	}
	if eng.calls != 0 {
		t.Fatalf("校验失败不应进入引擎，实际调用 %d 次", eng.calls)
	}
}

func TestE2E_Chat_InvalidJSON_400(t *testing.T) {
	ts, eng := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	resp := doJSON(t, ts, http.MethodPost, "/api/v1/chat", `{not json`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, 期望 400", resp.StatusCode)
	}
	if eng.calls != 0 {
		t.Fatalf("无效 JSON 不应进入引擎，实际调用 %d 次", eng.calls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 3：安全网关拒绝 → 403，错误码跨层透传（GatewayError.Code 不丢）
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_Chat_GatewayReject_403_PropagatesCode(t *testing.T) {
	gw := &mockGateway{err: &gateway.GatewayError{
		Layer:   "rate_limit",
		Code:    "minute_exceeded",
		Message: "请求过于频繁",
	}}
	ts, eng := newE2EServer(t, &adapter.Reply{Content: "ok"}, gw)

	resp := doJSON(t, ts, http.MethodPost, "/api/v1/chat", `{"message":"你好","user_id":"u1"}`)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态码 = %d, 期望 403", resp.StatusCode)
	}
	out := decodeMap(t, resp)
	// 网关错误码必须穿过 handler 闭环原样透传给前端
	if out["code"] != "minute_exceeded" {
		t.Fatalf("code 跨层丢失: 实际 %v, 期望 minute_exceeded", out["code"])
	}
	if out["layer"] != "rate_limit" {
		t.Fatalf("layer 跨层丢失: 实际 %v", out["layer"])
	}
	if eng.calls != 0 {
		t.Fatalf("网关拒绝后引擎不应被调用，实际 %d 次", eng.calls)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 4：SSE 流式聊天端点（Accept: text/event-stream）
// 验证：状态 200 + Content-Type text/event-stream + data 帧 + [DONE] 收尾
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_Chat_SSE_StreamsDataFramesThenDone(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "流式回复"}, nil)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"你好","user_id":"u1"}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("SSE 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, 期望 text/event-stream", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 SSE body 失败: %v", err)
	}
	frames := sseFrames(string(body))
	if len(frames) < 2 {
		t.Fatalf("SSE 帧数 = %d, 期望至少 content 帧 + [DONE], body=%q", len(frames), body)
	}
	last := frames[len(frames)-1]
	if last != "[DONE]" {
		t.Fatalf("SSE 末帧 = %q, 期望 [DONE]", last)
	}
	// 第一帧应是合法 chunk JSON，且能解析出内容
	var chunk adapter.ReplyChunk
	if err := json.Unmarshal([]byte(frames[0]), &chunk); err != nil {
		t.Fatalf("首帧非合法 chunk JSON: %v (%q)", err, frames[0])
	}
	if chunk.Content != "流式回复" {
		t.Fatalf("SSE 内容不匹配: %q", chunk.Content)
	}
}

// scanSSE 辅助：用 bufio 逐行读 SSE，验证流式协议在真实 HTTP 传输下不被缓冲破坏。
func TestE2E_Chat_SSE_LineFramingOverRealHTTP(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "x"}, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/api/v1/chat", strings.NewReader(`{"message":"hi","user_id":"u1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("SSE 请求失败: %v", err)
	}
	defer resp.Body.Close()

	var sawData, sawDone bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			sawData = true
			if strings.TrimPrefix(line, "data: ") == "[DONE]" {
				sawDone = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("扫描 SSE 出错: %v", err)
	}
	if !sawData {
		t.Fatal("未收到任何 data: 帧")
	}
	if !sawDone {
		t.Fatal("未收到 [DONE] 收尾帧")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 5：只读端点 GET 闭环（version / stats / models / roles / config）
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_GetEndpoints_OK(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	cases := []struct {
		name string
		path string
		keys []string // 响应必须包含的顶层 key
	}{
		{"health", "/health", []string{"status"}},
		{"version", "/api/v1/version", []string{"version", "engine"}},
		{"stats", "/api/v1/stats", []string{"goroutines", "uptime_seconds"}},
		{"models", "/api/v1/models", []string{"models", "total"}},
		{"roles", "/api/v1/roles", []string{"roles"}},
		{"full_config", "/api/v1/config", []string{"server", "llm", "security"}},
		{"llm_config", "/api/v1/config/llm", []string{"default", "providers"}},
		{"images_status", "/api/v1/images/status", []string{"enabled", "providers"}},
		{"videos_status", "/api/v1/videos/status", []string{"enabled", "providers"}},
		{"voicechat_status", "/api/v1/voicechat/status", []string{"enabled", "providers"}},
		// 注意：/api/v1/ollama/status 会真实探测 localhost:11434，故意排除以保持
		// 测试无网络依赖 / 无墙钟 timeout 抖动。
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doJSON(t, ts, http.MethodGet, tc.path, "")
			if resp.StatusCode != http.StatusOK {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("%s 状态码 = %d, 期望 200, body=%s", tc.path, resp.StatusCode, b)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				resp.Body.Close()
				t.Fatalf("%s Content-Type = %q, 期望 JSON", tc.path, ct)
			}
			out := decodeMap(t, resp)
			for _, k := range tc.keys {
				if _, ok := out[k]; !ok {
					t.Errorf("%s 响应缺顶层字段 %q: %v", tc.path, k, out)
				}
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 6：未配置模块的 fallback 端点（mcp/agents/memory/knowledge）走空列表闭环
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_DisabledModules_FallbackEmptyList(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	cases := []struct {
		path    string
		listKey string
	}{
		{"/api/v1/knowledge/documents", "documents"},
		{"/api/v1/mcp/servers", "servers"},
		{"/api/v1/mcp/tools", "tools"},
		{"/api/v1/agents", "agents"},
		{"/api/v1/agents/rules", "rules"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp := doJSON(t, ts, http.MethodGet, tc.path, "")
			if resp.StatusCode != http.StatusOK {
				resp.Body.Close()
				t.Fatalf("%s 状态码 = %d, 期望 200", tc.path, resp.StatusCode)
			}
			out := decodeMap(t, resp)
			v, ok := out[tc.listKey]
			if !ok {
				t.Fatalf("%s 缺列表字段 %q: %v", tc.path, tc.listKey, out)
			}
			// 未启用模块应返回空数组（而非 null），避免前端 .map(null) 崩溃
			arr, ok := v.([]any)
			if !ok {
				t.Fatalf("%s 字段 %q 非数组: %T", tc.path, tc.listKey, v)
			}
			if len(arr) != 0 {
				t.Fatalf("%s 字段 %q 期望空数组, 实际 %v", tc.path, tc.listKey, arr)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 7：未注册路由 → 404（mux 默认行为，验证 fallback 不误吞）
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_UnknownRoute_404(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	resp := doJSON(t, ts, http.MethodGet, "/api/v1/this/does/not/exist", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("未知路由状态码 = %d, 期望 404", resp.StatusCode)
	}
}

// 方法不匹配（GET-only 端点用 POST）→ 405
func TestE2E_MethodNotAllowed_405(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	resp := doJSON(t, ts, http.MethodPost, "/api/v1/version", `{}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("方法不匹配状态码 = %d, 期望 405", resp.StatusCode)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 8：CORS 预检（OPTIONS）+ 桌面 Origin 闭环
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_CORS_DesktopOrigin_Preflight(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodOptions, ts.URL+"/api/v1/chat", nil)
	req.Header.Set("Origin", "tauri://localhost")
	req.Header.Set("Access-Control-Request-Method", "POST")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("OPTIONS 预检失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("预检状态码 = %d, 期望 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "tauri://localhost" {
		t.Fatalf("Access-Control-Allow-Origin = %q, 期望 tauri://localhost", got)
	}
}

// 非白名单 Origin 不应回 ACAO 头（防止任意站点跨域读管理接口）
func TestE2E_CORS_UntrustedOrigin_NoACAO(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodOptions, ts.URL+"/api/v1/chat", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("OPTIONS 预检失败: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("不可信 Origin 不应回 ACAO 头, 实际 %q", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 9：鉴权中间件——非 loopback 的写操作（无 Token 配置）→ 403
// （用 srv.routes() + 伪造非 loopback RemoteAddr，验证 auth 中间件闭环；
//  httptest.NewServer 的连接固定来自 127.0.0.1 会触发 loopback 放行，无法测此路径）
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_Auth_NonLoopbackWrite_NoToken_403(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)

	// PUT /api/v1/config/llm 属于受保护写操作（非 chat / 非 webhook）
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.10:54321" // 非 loopback（TEST-NET-3）
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("未配置 Token 的非本机写操作状态码 = %d, 期望 403, body=%s", w.Code, w.Body.String())
	}
}

// 配置了 Token 但请求未携带 → 401
func TestE2E_Auth_NonLoopbackWrite_WithToken_MissingHeader_401(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "secret-token-123"
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/config/llm", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.10:54321"
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺 Token 状态码 = %d, 期望 401, body=%s", w.Code, w.Body.String())
	}
}

// 配置了 Token 且请求携带正确 Bearer → 放行（不被中间件拦截，进入 handler）。
//
// 目标端点 POST /api/v1/config/llm/test 是写操作（受鉴权保护），但 handler 对空
// body 走纯校验早返回 400，不触碰任何持久化（避免测试副作用写入 ~/.hexclaw）。
// 因此"拿到 handler 的 400 而非中间件的 401/403"恰好证明鉴权已放行。
func TestE2E_Auth_NonLoopbackWrite_WithToken_ValidHeader_PassesAuth(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "secret-token-123"
	srv := NewServer(cfg, &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/llm/test", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret-token-123")
	req.RemoteAddr = "203.0.113.10:54321"
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Fatalf("携带有效 Token 不应被鉴权拦截, 实际状态码 = %d, body=%s", w.Code, w.Body.String())
	}
	// 进一步确认确实落到了 handler（400 校验失败），而非被中间件提前放行到别处
	if w.Code != http.StatusBadRequest {
		t.Fatalf("鉴权放行后应进入 handler 并因空 body 返 400, 实际 = %d, body=%s", w.Code, w.Body.String())
	}
}

// chat 端点豁免鉴权：非 loopback 也能打 chat（POST 但 path == /api/v1/chat）
func TestE2E_Auth_ChatExemptFromAuth_NonLoopback(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(`{"message":"hi","user_id":"u1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "203.0.113.10:54321"
	w := httptest.NewRecorder()

	srv.routes().ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
		t.Fatalf("chat 端点应豁免鉴权, 实际状态码 = %d, body=%s", w.Code, w.Body.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 10：引擎错误跨层传播（引擎返错 → 500，错误文案不被吞）
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_Chat_EngineError_500_PropagatesMessage(t *testing.T) {
	eng := &mockEngine{err: context.DeadlineExceeded}
	srv := NewServer(config.DefaultConfig(), eng, nil, nil)
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)

	resp := doJSON(t, ts, http.MethodPost, "/api/v1/chat", `{"message":"hi","user_id":"u1"}`)

	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("引擎错误状态码 = %d, 期望 500", resp.StatusCode)
	}
	out := decodeMap(t, resp)
	// 错误信息应跨层透传到响应体 error 字段（不被吞成空串）
	if msg, _ := out["error"].(string); msg == "" {
		t.Fatalf("引擎错误未透传到响应 error 字段: %v", out)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 闭环 11：错误响应结构一致性——APIError 标准端点返 code/message/error 三字段
// （cron unified 端点：scheduler 未注入 → 该路由不注册 → 404；
//  改测 config/llm/test 的 400 错误体结构）
// ─────────────────────────────────────────────────────────────────────────────

func TestE2E_ErrorBody_LLMTest_BadRequest_Shape(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	// 空 provider 配置 → 400（具体错误体由 handleTestLLMConfig 决定）
	resp := doJSON(t, ts, http.MethodPost, "/api/v1/config/llm/test", `{}`)

	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("状态码 = %d, 期望 400, body=%s", resp.StatusCode, b)
	}
	out := decodeMap(t, resp)
	// 错误体至少要有一个人类可读的错误描述字段（error 或 message）
	_, hasErr := out["error"]
	_, hasMsg := out["message"]
	if !hasErr && !hasMsg {
		t.Fatalf("400 错误体缺少 error/message 字段: %v", out)
	}
}

// imagegen 未配置 → POST /generate 返 503（服务不可用闭环），不是 500/200
func TestE2E_ImageGen_NotConfigured_503(t *testing.T) {
	ts, _ := newE2EServer(t, &adapter.Reply{Content: "ok"}, nil)

	resp := doJSON(t, ts, http.MethodPost, "/api/v1/images/generate", `{"prompt":"a cat"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("未配置图像服务状态码 = %d, 期望 503", resp.StatusCode)
	}
	out := decodeMap(t, resp)
	if msg, _ := out["error"].(string); msg == "" {
		t.Fatalf("503 错误体缺少 error 文案: %v", out)
	}
}
