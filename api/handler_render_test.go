package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/render"
)

// 鉴权 + 路由测试（不依赖 pandoc，纯 HTTP 层契约）。
//
// 详见 .claude/doc-generation-architecture.md §8.4 安全测试要求。

type stubRenderer struct{}

func (s *stubRenderer) Render(ctx context.Context, content string, format render.Format, opts render.RenderOptions) (*render.RenderResult, error) {
	return nil, &render.RenderError{Code: render.CodeRenderFailed, Format: format, Detail: "stub: not implemented"}
}

func newTestRenderServer(t *testing.T, withRenderSvc bool) *Server {
	t.Helper()
	s := &Server{cfg: &config.Config{}}
	if withRenderSvc {
		svc, err := render.NewService(render.ServiceConfig{Renderer: &stubRenderer{}})
		if err != nil {
			t.Fatal(err)
		}
		s.renderSvc = svc
	}
	return s
}

// TestRenderRoute_OldPath404 — 旧路径 /api/render（无 v1 前缀）必须 404。
//
// 关键安全门槛：apiAuthMiddleware 只对 /api/v1/* 写操作鉴权
// （server.go: apiAuthMiddleware）；旧路径会绕过鉴权链。
func TestRenderRoute_OldPath404(t *testing.T) {
	srv := newTestRenderServer(t, true)
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodPost, "/api/render", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("/api/render (no v1) should 404, got %d", rec.Code)
	}
}

// TestRenderRoute_NotMountedWithoutService — 未配置 renderSvc 时端点不挂载。
// 注：routes() 默认 fallback 可能返回 403（more secure than 404）；只要不是 200/2xx 即可。
func TestRenderRoute_NotMountedWithoutService(t *testing.T) {
	srv := newTestRenderServer(t, false)
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/render", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code >= 200 && rec.Code < 300 {
		t.Errorf("/api/v1/render should not be reachable when renderSvc nil, got 2xx %d", rec.Code)
	}
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected deny-default 401/403/404/405 when renderSvc nil, got %d", rec.Code)
	}
}

// TestRenderRoute_MountedWithService — 配置 renderSvc 后端点存在。
func TestRenderRoute_MountedWithService(t *testing.T) {
	srv := newTestRenderServer(t, true)
	handler := srv.routes()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/render",
		strings.NewReader(`{"content":"hi","format":"md"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// 不应是 404；具体 status 取决于 stub renderer 返回（500 RENDER_FAILED）
	if rec.Code == http.StatusNotFound {
		t.Errorf("/api/v1/render should be mounted when renderSvc set, got 404")
	}
}

// TestRenderAuth_LocalhostBypass — localhost 自动放行（兼容桌面 sidecar）。
func TestRenderAuth_LocalhostBypass(t *testing.T) {
	srv := newTestRenderServer(t, true)
	srv.cfg.Server.APIToken = "secret-token-12345"

	handler := srv.apiAuthMiddleware(srv.routes())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/render",
		strings.NewReader(`{"content":"x","format":"md"}`))
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Errorf("localhost should be authorized without token, got 401")
	}
}

// TestRenderAuth_NonLocalhostRequiresToken — 非 localhost 必须 Bearer。
func TestRenderAuth_NonLocalhostRequiresToken(t *testing.T) {
	srv := newTestRenderServer(t, true)
	srv.cfg.Server.APIToken = "secret-token-12345"

	handler := srv.apiAuthMiddleware(srv.routes())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/render",
		strings.NewReader(`{"content":"x","format":"md"}`))
	req.RemoteAddr = "10.0.0.5:54321"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("non-localhost without token should 401, got %d", rec.Code)
	}
}

// TestRenderAuth_WrongTokenRejected — 错误 token 也 401。
func TestRenderAuth_WrongTokenRejected(t *testing.T) {
	srv := newTestRenderServer(t, true)
	srv.cfg.Server.APIToken = "secret-token-12345"

	handler := srv.apiAuthMiddleware(srv.routes())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/render",
		strings.NewReader(`{"content":"x","format":"md"}`))
	req.RemoteAddr = "10.0.0.5:54321"
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token should 401, got %d", rec.Code)
	}
}
