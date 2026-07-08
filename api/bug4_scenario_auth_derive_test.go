package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// BUG-4 (Low)：场景包鉴权前缀原来硬编码 `/api/k12/`——未来新场景包挂到 `/api/<其他>/`
// 会重现 AP-184 绕过鉴权（落在 /api/v1 守卫外、非回环无凭证可达）。守护前缀集必须从
// 挂载注册表 extraMounts 派生：任何 Mount 进来的场景子路由都自动纳入鉴权。
func TestBUG4_MountedScenarioPrefixDerivesAuthGuard(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	s.cfg.Server.APIToken = "secret-token"
	// 注册一个「非 k12」的场景挂载，模拟未来新场景包。
	s.Mount("/api/xx", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

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

	// 非回环无 token 写请求 → 派生守卫应拦 401（此前硬编码只认 k12 → 直接放行 200）。
	if code, hit := call(http.MethodPost, "/api/xx/write", "203.0.113.7:5555", ""); code != http.StatusUnauthorized || hit {
		t.Errorf("已挂载场景 /api/xx 写端点非回环无 token 应 401，got code=%d hit=%v", code, hit)
	}
	// 读端点同样纳入守卫（场景读端点可能含 PII）。
	if code, hit := call(http.MethodGet, "/api/xx/list", "203.0.113.7:5555", ""); code != http.StatusUnauthorized || hit {
		t.Errorf("已挂载场景 /api/xx 读端点非回环无 token 应 401，got code=%d hit=%v", code, hit)
	}
	// 正确 token 放行。
	if code, hit := call(http.MethodPost, "/api/xx/write", "203.0.113.7:5555", "Bearer secret-token"); code != http.StatusOK || !hit {
		t.Errorf("带正确 token 应放行，got code=%d hit=%v", code, hit)
	}
	// loopback 放行（cron/桌面 sidecar）。
	if code, hit := call(http.MethodPost, "/api/xx/write", "127.0.0.1:1234", ""); code != http.StatusOK || !hit {
		t.Errorf("loopback 应放行，got code=%d hit=%v", code, hit)
	}
	// 未注册的前缀不应被场景守卫误纳（仍走默认规则；GET 非写 → 放行）。
	if code, _ := call(http.MethodGet, "/api/unmounted/x", "203.0.113.7:5555", ""); code != http.StatusOK {
		t.Errorf("未挂载前缀不应被场景守卫拦，got code=%d", code)
	}
}
