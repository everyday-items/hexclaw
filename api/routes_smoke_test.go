package api

import (
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
)

// TestRoutesNoDuplicateRegistration 冒烟测试：构造 Server 并构建路由表必须不 panic。
//
// Go 1.22+ 的 http.ServeMux 对「同一 method+pattern 注册两次」会在 HandleFunc 内 panic
// （服务启动即崩）。本测试把这类「复制粘贴重复注册」回归钉死在编译期之外、上线之前。
// 背景：assistant/soul 路由曾被并行改动重复注册两次，因无 routes() 覆盖而漏到人工审查。
func TestRoutesNoDuplicateRegistration(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("routes() panic（疑似重复路由注册或 pattern 冲突）: %v", r)
		}
	}()

	if h := srv.routes(); h == nil {
		t.Fatal("routes() 返回 nil handler")
	}
}
