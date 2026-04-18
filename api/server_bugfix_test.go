package api

import (
	"os"
	"strings"
	"testing"
)

// TestServerStart_Signature_Fix5 静态断言：Server.Start 必须接受 onReady 回调，
// 且实现中 net.Listen 必须先于 onReady 调用。
//
// 修复前：Start(ctx) 直接 ListenAndServe，无法区分 bind 成功和失败；
// main.go 在 Start 前打印"已就绪"，导致日志时序错误。
// 修复后：Start(ctx, onReady)，onReady 只在 net.Listen 成功后触发。
func TestServerStart_Signature_Fix5(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	content := string(src)

	// 新签名
	if !strings.Contains(content, "func (s *Server) Start(ctx context.Context, onReady func()) error") {
		t.Fatal("Start 方法签名未包含 onReady 回调")
	}
	// net.Listen 先于 onReady
	listenIdx := strings.Index(content, `net.Listen("tcp"`)
	readyIdx := strings.Index(content, "onReady()")
	if listenIdx < 0 {
		t.Fatal("实现中未找到 net.Listen(\"tcp\", ...)")
	}
	if readyIdx < 0 {
		t.Fatal("实现中未找到 onReady() 调用")
	}
	if readyIdx < listenIdx {
		t.Fatalf("时序错误：onReady 在 net.Listen 之前（readyIdx=%d listenIdx=%d）", readyIdx, listenIdx)
	}
	t.Logf("修复后：net.Listen 位置 %d，onReady 位置 %d（时序正确）", listenIdx, readyIdx)
}
