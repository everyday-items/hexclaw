package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/instances"
)

// fakeSMTP 是 api 测试用的最小本地 SMTP 服务器，监听 127.0.0.1（loopback 允许明文 AUTH PLAIN）。
// authOK 控制 AUTH 返回 235/535，覆盖成功/失败路径。
type fakeSMTP struct {
	ln     net.Listener
	authOK bool
}

func startFakeSMTP(t *testing.T, authOK bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	s := &fakeSMTP{ln: ln, authOK: authOK}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeSMTP) port() int { return s.ln.Addr().(*net.TCPAddr).Port }

func (s *fakeSMTP) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	write := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}
	write("220 fake ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			write("250-fake")
			write("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(cmd, "AUTH"):
			if s.authOK {
				write("235 ok")
			} else {
				write("535 bad creds")
			}
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 bye")
			return
		default:
			write("250 ok")
		}
	}
}

func postConnectionsTest(t *testing.T, body string) *connectionTestResponse {
	t.Helper()
	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/test", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.handleConnectionsTest(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp connectionTestResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, w.Body.String())
	}
	return &resp
}

// TestHandleListConnections 验证脱敏只读列表：返回 id/provider/name/capabilities/status/enabled，
// 且响应体中**绝不**出现凭据/config 字段。
func TestHandleListConnections(t *testing.T) {
	mgr, cleanup := newTestInstanceManager(t)
	defer cleanup()

	if err := mgr.Upsert(context.Background(), &instances.Instance{
		Provider: "feishu",
		Name:     "feishu-main",
		Enabled:  true,
		Config:   []byte(`{"app_id":"cli_topsecret","app_secret":"shhh"}`),
	}); err != nil {
		t.Fatalf("保存实例失败: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
	srv.SetInstanceManager(mgr)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/connections", nil)
	w := httptest.NewRecorder()
	srv.handleListConnections(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	// 凭据/config 绝不出现在响应里。
	for _, leak := range []string{"cli_topsecret", "shhh", "app_secret", "app_id", "config", "config_json"} {
		if strings.Contains(body, leak) {
			t.Fatalf("响应体泄露了不应出现的字段/凭据 %q：%s", leak, body)
		}
	}

	var resp struct {
		Connections []connectionSummary `json:"connections"`
		Total       int                 `json:"total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, body)
	}
	if resp.Total != 1 || len(resp.Connections) != 1 {
		t.Fatalf("期望 1 个连接，实际 %+v", resp)
	}
	c := resp.Connections[0]
	if c.Provider != "feishu" || c.Name != "feishu-main" || !c.Enabled {
		t.Fatalf("连接摘要字段不符: %+v", c)
	}
	if c.ID == "" {
		t.Fatal("连接摘要应含 id")
	}
	wantCaps := []string{"receive", "send"}
	if len(c.Capabilities) != 2 || c.Capabilities[0] != wantCaps[0] || c.Capabilities[1] != wantCaps[1] {
		t.Fatalf("feishu capabilities 应为 %v，实际 %v", wantCaps, c.Capabilities)
	}
}

// TestConnectionCapabilities 单测 provider → capabilities 推导。
func TestConnectionCapabilities(t *testing.T) {
	// IM 平台已接入 instances.Manager → 可收可发。
	for _, p := range []string{"feishu", "dingtalk", "discord", "telegram", "wecom", "wechat"} {
		caps := connectionCapabilities(p)
		if len(caps) != 2 || caps[0] != "receive" || caps[1] != "send" {
			t.Fatalf("%s capabilities 应为 [receive send]，实际 %v", p, caps)
		}
	}
	// email 发件未接入 instances.Manager（BuildAdapter/modeForProvider 均无 email）→ 仅 receive（BUG-20260625 §3-5）。
	if caps := connectionCapabilities("email"); len(caps) != 1 || caps[0] != "receive" {
		t.Fatalf("email capabilities 应为 [receive]（发件未接入），实际 %v", caps)
	}
	if caps := connectionCapabilities("unknown"); len(caps) != 0 {
		t.Fatalf("未知 provider capabilities 应为空切片，实际 %v", caps)
	}
}

// TestHandleConnectionsTest_Validation 覆盖请求级校验：缺 type / 缺 config / 不支持的 type。
func TestHandleConnectionsTest_Validation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
	}{
		{"缺 type", `{"config":{"smtp":{}}}`, http.StatusBadRequest},
		{"缺 config", `{"type":"email"}`, http.StatusBadRequest},
		{"空 type", `{"type":"  ","config":{"a":1}}`, http.StatusBadRequest},
		{"不支持的 type", `{"type":"carrierpigeon","config":{"a":1}}`, http.StatusBadRequest},
		{"非法 JSON", `{not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewServer(config.DefaultConfig(), &mockEngine{}, nil, nil)
			req := httptest.NewRequest(http.MethodPost, "/api/v1/connections/test", strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			srv.handleConnectionsTest(w, req)
			if w.Code != tc.wantCode {
				t.Fatalf("期望 %d，实际 %d: %s", tc.wantCode, w.Code, w.Body.String())
			}
		})
	}
}

// TestHandleConnectionsTest_IMFieldValidation 覆盖 IM 类型的字段校验失败路径（不发外部请求）。
// 空凭据下 ValidateConfig 在拉 token 前即因字段缺失返回错误。
func TestHandleConnectionsTest_IMFieldValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"feishu 空凭据", `{"type":"feishu","config":{"app_id":"","app_secret":""}}`},
		{"dingtalk 空凭据", `{"type":"dingtalk","config":{"app_key":"","app_secret":"","robot_code":""}}`},
		{"telegram 空 token", `{"type":"telegram","config":{"token":""}}`},
		{"discord 空 token", `{"type":"discord","config":{"token":""}}`},
		{"wecom 空凭据", `{"type":"wecom","config":{"corp_id":"","secret":""}}`},
		{"wechat 空凭据", `{"type":"wechat","config":{"app_id":"","app_secret":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postConnectionsTest(t, tc.body)
			if resp.OK {
				t.Fatalf("空凭据应校验失败，得到 ok=true detail=%q", resp.Detail)
			}
			if resp.Detail == "" {
				t.Fatal("失败时 detail 不应为空")
			}
		})
	}
}

// TestHandleConnectionsTest_DingtalkFieldsPresent 验证字段齐备时钉钉（仅字段校验）返回成功。
func TestHandleConnectionsTest_DingtalkFieldsPresent(t *testing.T) {
	resp := postConnectionsTest(t, `{"type":"dingtalk","config":{"app_key":"k","app_secret":"s","robot_code":"r"}}`)
	if !resp.OK {
		t.Fatalf("字段齐备时钉钉应通过字段校验，得到 ok=false detail=%q", resp.Detail)
	}
}

// TestHandleConnectionsTest_EmailSMTPSuccess 覆盖 email 成功路径（SMTP AUTH 通过、未配 IMAP 跳过）。
func TestHandleConnectionsTest_EmailSMTPSuccess(t *testing.T) {
	srv := startFakeSMTP(t, true)
	body := fmt.Sprintf(`{"type":"email","config":{"smtp":{"host":"127.0.0.1","port":%d,"username":"u@example.com","password":"p","from":"u@example.com"}}}`, srv.port())
	resp := postConnectionsTest(t, body)
	if !resp.OK {
		t.Fatalf("SMTP AUTH 应成功，得到 ok=false detail=%q", resp.Detail)
	}
	if !strings.Contains(resp.Detail, "SMTP") {
		t.Fatalf("detail 应提及 SMTP，得到 %q", resp.Detail)
	}
}

// TestHandleConnectionsTest_EmailSMTPAuthFailure 覆盖 email 失败路径（SMTP AUTH 拒绝）。
func TestHandleConnectionsTest_EmailSMTPAuthFailure(t *testing.T) {
	srv := startFakeSMTP(t, false)
	body := fmt.Sprintf(`{"type":"email","config":{"smtp":{"host":"127.0.0.1","port":%d,"username":"u@example.com","password":"wrong","from":"u@example.com"}}}`, srv.port())
	resp := postConnectionsTest(t, body)
	if resp.OK {
		t.Fatal("SMTP AUTH 失败时应返回 ok=false")
	}
	if !strings.Contains(resp.Detail, "SMTP 认证失败") {
		t.Fatalf("detail 应说明 SMTP 认证失败，得到 %q", resp.Detail)
	}
}

// TestHandleConnectionsTest_EmailMissingSMTPFields 覆盖 host 无法推导 + 缺字段路径。
func TestHandleConnectionsTest_EmailMissingSMTPFields(t *testing.T) {
	resp := postConnectionsTest(t, `{"type":"email","config":{"smtp":{"username":"","password":""}}}`)
	if resp.OK {
		t.Fatal("缺 SMTP host/username/password 应失败")
	}
}

// TestDeriveMailHost 单测主机推导工具。
func TestDeriveMailHost(t *testing.T) {
	cases := []struct {
		prefix, primary, fallback, want string
	}{
		{"smtp", "alice@example.com", "", "smtp.example.com"},
		{"imap", "", "bob@mail.org", "imap.mail.org"},
		{"smtp", "no-at-sign", "", ""},
		{"smtp", "trailing@", "", ""},
		{"smtp", "", "", ""},
	}
	for _, c := range cases {
		if got := deriveMailHost(c.prefix, c.primary, c.fallback); got != c.want {
			t.Errorf("deriveMailHost(%q,%q,%q)=%q want %q", c.prefix, c.primary, c.fallback, got, c.want)
		}
	}
}
