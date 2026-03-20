package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// ════════════════════════════════════════════════
// Round 3: 安全、中间件、边界、规范
// ════════════════════════════════════════════════

// ── 1. API Token 需要精确匹配 ──

func TestApiAuth_TokenComparison_ExactMatchRequired(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "secret-token"
	s := &Server{cfg: cfg, logCollector: NewLogCollector(10)}

	handler := s.apiAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	tests := []struct {
		name   string
		auth   string
		expect int
	}{
		{name: "exact", auth: "Bearer secret-token", expect: http.StatusOK},
		{name: "wrong", auth: "Bearer wrong", expect: http.StatusUnauthorized},
		{name: "shorter", auth: "Bearer secret", expect: http.StatusUnauthorized},
		{name: "longer", auth: "Bearer secret-token-extra", expect: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil)
			req.RemoteAddr = "192.168.1.100:12345"
			req.Header.Set("Authorization", tt.auth)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.expect {
				t.Fatalf("status=%d, want %d", w.Code, tt.expect)
			}
		})
	}
}

// ── 2. CORS 中间件边界测试 ──

func TestCorsMiddleware_AllowedOrigins(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(inner)

	tests := []struct {
		origin  string
		allowed bool
	}{
		{"http://localhost:3000", true},
		{"http://localhost:8080", true},
		{"http://localhost:1", true},
		{"http://localhost:65535", true},
		{"tauri://localhost", true},
		{"http://tauri.localhost", true},

		// 应被拒绝
		{"http://localhost:", false},       // 空端口
		{"http://localhost:abc", false},    // 非数字端口
		{"http://localhost:123456", false}, // 超过 5 位
		{"http://localhost:0evil", false},  // 混入字母
		{"http://evil.com", false},
		{"http://localhost", false},           // 无端口
		{"https://localhost:3000", false},     // https (非 http)
		{"http://localhost:3000/path", false}, // 带路径
	}

	for _, tt := range tests {
		req := httptest.NewRequest("GET", "/api/v1/logs", nil)
		req.Header.Set("Origin", tt.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		got := w.Header().Get("Access-Control-Allow-Origin")
		if tt.allowed && got != tt.origin {
			t.Errorf("origin %q should be allowed, got ACAO=%q", tt.origin, got)
		}
		if !tt.allowed && got != "" {
			t.Errorf("origin %q should be rejected, got ACAO=%q", tt.origin, got)
		}
	}
}

func TestCorsMiddleware_Preflight(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("inner handler should NOT be called for OPTIONS preflight")
	})
	handler := corsMiddleware(inner)

	req := httptest.NewRequest("OPTIONS", "/api/v1/chat", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
}

func TestCorsMiddleware_NoOrigin(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(inner)

	req := httptest.NewRequest("GET", "/api/v1/logs", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("no origin should get no ACAO, got %q", got)
	}
}

// ── 3. Auth 中间件路径绕过测试 ──

func TestApiAuth_PathBypass(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "secret-token"
	s := &Server{cfg: cfg, logCollector: NewLogCollector(10)}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := s.apiAuthMiddleware(inner)

	tests := []struct {
		method string
		path   string
		token  string
		expect int
	}{
		{"GET", "/api/v1/logs", "", http.StatusUnauthorized}, // 日志 API 需认证
		{"GET", "/api/v1/sessions", "", http.StatusOK},
		{"POST", "/api/v1/chat", "", http.StatusOK},
		{"POST", "/api/v1/webhooks/github", "", http.StatusOK},
		{"POST", "/api/v1/webhooks", "", http.StatusUnauthorized},
		{"POST", "/api/v1/cron/jobs", "", http.StatusUnauthorized},
		{"DELETE", "/api/v1/sessions/123", "", http.StatusUnauthorized},
		{"PUT", "/api/v1/config/llm", "", http.StatusUnauthorized},
		{"POST", "/api/v1/cron/jobs", "Bearer secret-token", http.StatusOK},
		{"POST", "/api/v1/cron/jobs", "Bearer wrong", http.StatusUnauthorized},
		{"POST", "/health", "", http.StatusOK},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(tt.method, tt.path, nil)
		if tt.token != "" {
			req.Header.Set("Authorization", tt.token)
		}
		req.RemoteAddr = "192.168.1.100:12345"

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)

		if w.Code != tt.expect {
			t.Errorf("%s %s token=%q: status=%d, want %d",
				tt.method, tt.path, tt.token, w.Code, tt.expect)
		}
	}
}

func TestApiAuth_NoToken_LocalhostAllowed(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = ""
	s := &Server{cfg: cfg, logCollector: NewLogCollector(10)}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := s.apiAuthMiddleware(inner)

	// Go HTTP server 使用 [::1]:port 格式
	for _, addr := range []string{"127.0.0.1:1234", "[::1]:1234"} {
		req := httptest.NewRequest("DELETE", "/api/v1/sessions/123", nil)
		req.RemoteAddr = addr
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("RemoteAddr=%s: status=%d, want 200", addr, w.Code)
		}
	}

	req := httptest.NewRequest("DELETE", "/api/v1/sessions/123", nil)
	req.RemoteAddr = "10.0.0.1:9999"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("remote: status=%d, want 403", w.Code)
	}
}

// ── 4. 日志 API 未经认证即可访问 ──

func TestLogsApi_RequiresAuth(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "secret"
	s := &Server{cfg: cfg, logCollector: NewLogCollector(100)}
	s.logCollector.Info("system", "敏感操作: API Key=sk-1234")

	inner := http.HandlerFunc(s.handleGetLogs)
	handler := s.apiAuthMiddleware(inner)

	// 无 token 的远程请求应被拒绝
	req := httptest.NewRequest("GET", "/api/v1/logs", nil)
	req.RemoteAddr = "evil.com:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("remote without token: status=%d, want 401", w.Code)
	}

	// 有正确 token 应通过
	req2 := httptest.NewRequest("GET", "/api/v1/logs", nil)
	req2.Header.Set("Authorization", "Bearer secret")
	req2.RemoteAddr = "evil.com:1234"
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Errorf("with valid token: status=%d, want 200", w2.Code)
	}

	// localhost 无 token 也应通过（未配置 token 的场景）
	cfg2 := config.DefaultConfig()
	cfg2.Server.APIToken = ""
	s2 := &Server{cfg: cfg2, logCollector: NewLogCollector(100)}
	handler2 := s2.apiAuthMiddleware(http.HandlerFunc(s2.handleGetLogs))

	req3 := httptest.NewRequest("GET", "/api/v1/logs", nil)
	req3.RemoteAddr = "127.0.0.1:1234"
	w3 := httptest.NewRecorder()
	handler2.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Errorf("localhost without token: status=%d, want 200", w3.Code)
	}
}

// ── 5. handleChat 日志泄露用户消息 ──

func TestHandleChat_LogsUserMessage(t *testing.T) {
	sensitiveMsg := "我的银行卡密码是 123456"
	s := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader(`{"user_id":"u1","message":"`+sensitiveMsg+`"}`))
	w := httptest.NewRecorder()
	s.handleChat(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}

	entries, _ := s.logCollector.Query("", "", "银行卡", 10, 0)
	if len(entries) != 0 {
		t.Fatalf("sensitive content leaked into logs: %+v", entries)
	}
}

// ── 6. writeJSON 忽略编码错误 ──

func TestWriteJSON_EncoderErrorIgnored(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, map[string]any{"normal": "ok"})
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}

	w2 := httptest.NewRecorder()
	writeJSON(w2, http.StatusOK, map[string]any{"bad": make(chan int)})
	if w2.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "响应序列化失败") {
		t.Fatalf("unexpected body: %q", w2.Body.String())
	}
}

// ── 7. LogCollector 字段名 cap 遮蔽内置 cap ──

func TestLogCollector_FieldNameShadowsBuiltin(t *testing.T) {
	c := NewLogCollector(10)
	if c.capacity != 10 {
		t.Errorf("capacity = %d, want 10", c.capacity)
	}
}

// ── 8. LogEntry.Timestamp 是 string 而非 time.Time ──

func TestLogEntry_TimestampFormat(t *testing.T) {
	c := NewLogCollector(10)
	c.Add("info", "test", "msg", nil)

	entries, _ := c.Query("", "", "", 1, 0)
	if len(entries) == 0 {
		t.Fatal("no entries")
	}

	_, err := time.Parse(time.RFC3339Nano, entries[0].Timestamp)
	if err != nil {
		t.Fatalf("invalid timestamp format: %v", err)
	}
}

// ── 9. CORS + Auth 组合：预检请求绕过 ──

func TestCorsAuth_PreflightDoesNotHitAuth(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Server.APIToken = "secret"
	s := &Server{cfg: cfg, logCollector: NewLogCollector(10)}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := s.apiAuthMiddleware(corsMiddleware(inner))

	req := httptest.NewRequest("OPTIONS", "/api/v1/cron/jobs", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.RemoteAddr = "evil.com:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
}

// ── 10. handleLogStream 在非 WebSocket 请求时的行为 ──

func TestHandleLogStream_NonWebSocket_CleanupSubscriber(t *testing.T) {
	s := &Server{logCollector: NewLogCollector(10)}

	req := httptest.NewRequest("GET", "/api/v1/logs/stream", nil)
	w := httptest.NewRecorder()

	s.handleLogStream(w, req)

	s.logCollector.subMu.RLock()
	count := len(s.logCollector.subscribers)
	s.logCollector.subMu.RUnlock()

	if count != 0 {
		t.Errorf("subscriber leaked after failed WebSocket upgrade: count=%d", count)
	}
}

// ── 11. handleChat context 取消 ──

func TestHandleChat_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	eng := &mockEngine{err: context.Canceled}
	s := &Server{
		cfg:          config.DefaultConfig(),
		logCollector: NewLogCollector(10),
		engine:       eng,
	}

	body := strings.NewReader(`{"message": "hello"}`)
	req := httptest.NewRequest("POST", "/api/v1/chat", body).WithContext(ctx)
	w := httptest.NewRecorder()
	s.handleChat(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("cancelled context: status=%d, want 500", w.Code)
	}
}

// ── 12. CORS: 不检查 http://localhost:3000/path ──

func TestCorsMiddleware_OriginWithPath(t *testing.T) {
	// Origin header 理论上不应包含路径，但如果恶意客户端发送了呢？
	// "http://localhost:3000/evil" 应该被拒绝
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "http://localhost:3000/evil")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// 当前实现: port = "3000/evil", 包含 '/' → isLocalhost=false ✓
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("origin with path should be rejected, got ACAO=%q", got)
	}
}

// ── 13. 基准 ──

func BenchmarkHandleGetLogs_EndToEnd(b *testing.B) {
	s := &Server{logCollector: NewLogCollector(5000)}
	for i := range 5000 {
		s.logCollector.Add("info", "test", fmt.Sprintf("message %d", i), nil)
	}
	b.ResetTimer()
	for range b.N {
		req := httptest.NewRequest("GET", "/api/v1/logs?limit=100", nil)
		w := httptest.NewRecorder()
		s.handleGetLogs(w, req)
	}
}

func BenchmarkHandleGetLogs_WithKeyword(b *testing.B) {
	s := &Server{logCollector: NewLogCollector(5000)}
	for i := range 5000 {
		s.logCollector.Add("info", "test", fmt.Sprintf("processing request #%d", i), nil)
	}
	b.ResetTimer()
	for range b.N {
		req := httptest.NewRequest("GET", "/api/v1/logs?keyword=processing&limit=10", nil)
		w := httptest.NewRecorder()
		s.handleGetLogs(w, req)
	}
}

func BenchmarkHandleGetLogStats_EndToEnd(b *testing.B) {
	s := &Server{logCollector: NewLogCollector(5000)}
	levels := []string{"info", "warn", "error", "debug"}
	for i := range 5000 {
		s.logCollector.Add(levels[i%4], fmt.Sprintf("src-%d", i%10), "msg", nil)
	}
	b.ResetTimer()
	for range b.N {
		req := httptest.NewRequest("GET", "/api/v1/logs/stats", nil)
		w := httptest.NewRecorder()
		s.handleGetLogStats(w, req)
	}
}

func BenchmarkCorsMiddleware(b *testing.B) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := corsMiddleware(inner)

	b.ResetTimer()
	for range b.N {
		req := httptest.NewRequest("GET", "/api/v1/logs", nil)
		req.Header.Set("Origin", "http://localhost:5173")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
	}
}
