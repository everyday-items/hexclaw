package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBug20260622_RecoverMiddleware 回归锁定 [BUG-API-NO-RECOVER]：
// HTTP 中间件链此前为 apiAuthMiddleware(corsMiddleware(mux))，无顶层 panic 兜底。
// handler panic（nil 解引用 / 切片越界 / 类型断言失败）会中断响应、不返回结构化错误。
// 修复后顶层 recoverMiddleware 必须把 panic 兜成 500 JSON，且不向上传播（进程存活）。
func TestBug20260622_RecoverMiddleware(t *testing.T) {
	panicHandler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom from handler")
	})
	h := recoverMiddleware(panicHandler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/boom", nil)

	// 不应有 panic 逃逸出 ServeHTTP（若逃逸，test 直接 panic 失败）
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("panic 应被兜底为 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("应返回结构化 JSON 错误体, got %q", rec.Body.String())
	}

	// 中间件包装的正常 handler 仍照常工作（兜底不影响正常路径）
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	rec2 := httptest.NewRecorder()
	recoverMiddleware(okHandler).ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK || rec2.Body.String() != "ok" {
		t.Errorf("正常 handler 被兜底中间件破坏: code=%d body=%q", rec2.Code, rec2.Body.String())
	}
}
