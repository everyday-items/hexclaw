package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// 回归锁（修复⑤）：配了 APIToken 时，非回环、无 Authorization 的 K12 写请求必须被拦（401），
// 与 /api/v1 写端点一致；loopback 仍放行（cron http_get 到本机、桌面 sidecar）。
func TestK12WriteEndpointsRequireAuth(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	s.cfg.Server.APIToken = "secret-token"
	// BUG-4：场景鉴权前缀从挂载注册表派生——注册 K12 子路由，守卫才认它（等价 composition root 的 Mount）。
	s.Mount("/api/k12", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	reached := false
	guarded := s.apiAuthMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	call := func(method, path, remote, auth string) (int, bool) {
		reached = false
		req := httptest.NewRequest(method, path, nil)
		req.RemoteAddr = remote
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, req)
		return rec.Code, reached
	}

	// 非回环 + 无 token → K12 写端点必须 401（此前 bug：直接 200 放行）。
	for _, p := range []string{"/api/k12/grade", "/api/k12/restore", "/api/k12/cron/provision", "/api/k12/bind-im"} {
		if code, hit := call(http.MethodPost, p, "203.0.113.7:5555", ""); code != http.StatusUnauthorized || hit {
			t.Errorf("%s 非回环无 token 应 401 且不进 handler，got code=%d hit=%v", p, code, hit)
		}
	}
	// 非回环 + 正确 token → 放行。
	if code, hit := call(http.MethodPost, "/api/k12/grade", "203.0.113.7:5555", "Bearer secret-token"); code != http.StatusOK || !hit {
		t.Errorf("带正确 token 应放行，got code=%d hit=%v", code, hit)
	}
	// loopback 无 token → 仍放行（cron/桌面）。
	if code, hit := call(http.MethodPost, "/api/k12/grade", "127.0.0.1:1234", ""); code != http.StatusOK || !hit {
		t.Errorf("loopback 应放行（cron http_get），got code=%d hit=%v", code, hit)
	}
	// K12 读端点（GET）也须鉴权——backup/export/profile/mistakes 含孩子 PII，非回环无 token 必拦。
	for _, p := range []string{"/api/k12/backup", "/api/k12/export", "/api/k12/profile", "/api/k12/mistakes"} {
		if code, hit := call(http.MethodGet, p, "203.0.113.7:5555", ""); code != http.StatusUnauthorized || hit {
			t.Errorf("%s 读端点非回环无 token 应 401（含 PII），got code=%d hit=%v", p, code, hit)
		}
	}
	// loopback GET 仍放行（桌面直接拉）。
	if code, _ := call(http.MethodGet, "/api/k12/mistakes", "127.0.0.1:1234", ""); code != http.StatusOK {
		t.Errorf("loopback 读端点应放行，got code=%d", code)
	}
}
