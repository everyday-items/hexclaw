package api

// Bug 20260627: 添加 Gmail 邮箱报错 "SMTP 认证失败: 创建 SMTP 客户端失败: EOF"。
//
// 根因之一（本文件锁定）：testEmailConnection 把**任何** ProbeSMTP 失败都前缀成
// "SMTP 认证失败: "。但 EOF 发生在读取 SMTP greeting 阶段——TCP 连上了、服务端却在
// 发出 banner 前断开（境外邮箱被网络重置 / 代理未生效的典型症状），认证根本没发生。
// 把"连接层失败"报成"认证失败"会误导用户去反复检查密码。
//
// 期望：连接层失败的 detail 应表述为"连接失败"且**不含**"认证失败"；真正的 AUTH
// 拒绝（另有测试 TestHandleConnectionsTest_EmailSMTPAuthFailure）才说"认证失败"。

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// startClosingSMTP 启一个最小 TCP 服务器：接受连接后立即关闭，不发送任何 greeting。
// 这复现 smtp.NewClient 读取 220 应答时拿到 EOF 的连接层失败。
func startClosingSMTP(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close() // 立即关闭 → 对端读 greeting 得 EOF
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func TestEmailConnection_ConnFailureNotLabeledAuth(t *testing.T) {
	port := startClosingSMTP(t)
	raw := json.RawMessage(fmt.Sprintf(
		`{"smtp":{"host":"127.0.0.1","port":%d,"username":"u@example.com","password":"p","from":"u@example.com"}}`,
		port,
	))

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	ok, detail := testEmailConnection(ctx, raw)
	if ok {
		t.Fatalf("连接层 EOF 应判失败，得到 ok=true detail=%q", detail)
	}
	if strings.Contains(detail, "认证失败") {
		t.Fatalf("连接层失败不应被标为「认证失败」（认证根本没发生），得到 detail=%q", detail)
	}
	if !strings.Contains(detail, "连接失败") {
		t.Fatalf("连接层失败应明确表述为「连接失败」并给出网络/代理提示，得到 detail=%q", detail)
	}
}
