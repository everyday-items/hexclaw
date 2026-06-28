package email

// Bug 20260627: 添加 Gmail 邮箱报错 "创建 SMTP 客户端失败: EOF"。
//
// 根因（本文件锁定）：邮件 SMTP/IMAP 走裸 net.Dial/tls.Dial，**不读取环境 *_PROXY**，
// 因此即便桌面 sidecar 已注入系统代理（HTTP_PROXY/HTTPS_PROXY/ALL_PROXY，应用其余 HTTP
// 流量都经此出网），邮件拨号仍直连——在需要代理才能访问 Gmail 的网络下被重置 → EOF。
//
// 期望：SMTP/IMAP 拨号遵循环境代理（SOCKS5 / HTTP CONNECT），与应用其余流量同一出口；
// 同时连接层失败被分类为"连接失败"而非"认证失败"。

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// fakeConnectProxy: 最小 HTTP CONNECT 代理，记录被请求 CONNECT 的目标并隧道到 backend。
// ---------------------------------------------------------------------------

type fakeConnectProxy struct {
	ln       net.Listener
	tunnelTo string
	mu       sync.Mutex
	lastConn string
}

func newFakeConnectProxy(t *testing.T, tunnelTo string) *fakeConnectProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("代理监听失败: %v", err)
	}
	p := &fakeConnectProxy{ln: ln, tunnelTo: tunnelTo}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *fakeConnectProxy) addr() string { return p.ln.Addr().String() }

func (p *fakeConnectProxy) lastConnect() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastConn
}

func (p *fakeConnectProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *fakeConnectProxy) handle(c net.Conn) {
	br := bufio.NewReader(c)
	reqLine, err := br.ReadString('\n')
	if err != nil {
		_ = c.Close()
		return
	}
	for { // 读完请求头直到空行
		line, err := br.ReadString('\n')
		if err != nil {
			_ = c.Close()
			return
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	fields := strings.Fields(reqLine)
	if len(fields) < 2 || strings.ToUpper(fields[0]) != "CONNECT" {
		_, _ = c.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		_ = c.Close()
		return
	}
	p.mu.Lock()
	p.lastConn = fields[1]
	p.mu.Unlock()

	back, err := net.Dial("tcp", p.tunnelTo)
	if err != nil {
		_, _ = c.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		_ = c.Close()
		return
	}
	_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() { _, _ = io.Copy(back, br); _ = back.Close() }()
	go func() { _, _ = io.Copy(c, back); _ = c.Close() }()
}

// ---------------------------------------------------------------------------
// fakeSocks5: 最小无认证 SOCKS5 代理（RFC 1928），记录目标并隧道到 backend。
// ---------------------------------------------------------------------------

type fakeSocks5 struct {
	ln       net.Listener
	tunnelTo string
	mu       sync.Mutex
	lastConn string
}

func newFakeSocks5(t *testing.T, tunnelTo string) *fakeSocks5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("SOCKS5 监听失败: %v", err)
	}
	p := &fakeSocks5{ln: ln, tunnelTo: tunnelTo}
	go p.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *fakeSocks5) addr() string { return p.ln.Addr().String() }

func (p *fakeSocks5) lastConnect() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastConn
}

func (p *fakeSocks5) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *fakeSocks5) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	// 1) 握手：VER NMETHODS METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 0x05 {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil { // 选 no-auth
		return
	}
	// 2) 请求：VER CMD RSV ATYP DST.ADDR DST.PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[1] != 0x01 { // 仅支持 CONNECT
		return
	}
	var host string
	switch req[3] {
	case 0x01: // IPv4
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 0x03: // 域名
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	case 0x04: // IPv6
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		return
	}
	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(c, portBuf); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(portBuf)
	p.mu.Lock()
	p.lastConn = net.JoinHostPort(host, strconv.Itoa(int(port)))
	p.mu.Unlock()

	back, err := net.Dial("tcp", p.tunnelTo)
	if err != nil {
		// REP=0x01 general failure
		_, _ = c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		return
	}
	defer func() { _ = back.Close() }()
	// REP=0x00 success, BND.ADDR 0.0.0.0:0
	if _, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(back, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, back); done <- struct{}{} }()
	<-done
}

// clearProxyEnv 清空所有代理相关环境变量（t.Setenv 自动在测试结束恢复），
// 避免宿主机已设的 *_PROXY 干扰断言。
func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ALL_PROXY", "all_proxy", "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
}

// ---------------------------------------------------------------------------
// RED: SMTP 拨号必须遵循环境代理
// ---------------------------------------------------------------------------

func TestProbeSMTP_HonorsHTTPProxy(t *testing.T) {
	backend := newFakeSMTPServer(t, true) // 隧道终点：可应答的 SMTP
	proxy := newFakeConnectProxy(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(backend.port())))

	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://"+proxy.addr())

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	// 目标是不可直连的保留域名 .test：唯有走代理 CONNECT 才可能"到达"。
	_ = ProbeSMTP(ctx, SMTPConfig{
		Host:     "smtp.unreachable.test",
		Port:     587,
		Username: "u@example.com",
		Password: "p",
		From:     "u@example.com",
	})

	if got := proxy.lastConnect(); got != "smtp.unreachable.test:587" {
		t.Fatalf("SMTP 拨号未经 HTTP 代理：proxy 收到的 CONNECT=%q，期望 smtp.unreachable.test:587", got)
	}
}

func TestProbeSMTP_HonorsSOCKS5Proxy(t *testing.T) {
	backend := newFakeSMTPServer(t, true)
	socks := newFakeSocks5(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(backend.port())))

	clearProxyEnv(t)
	t.Setenv("ALL_PROXY", "socks5://"+socks.addr())

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	_ = ProbeSMTP(ctx, SMTPConfig{
		Host:     "smtp.unreachable.test",
		Port:     587,
		Username: "u@example.com",
		Password: "p",
		From:     "u@example.com",
	})

	if got := socks.lastConnect(); got != "smtp.unreachable.test:587" {
		t.Fatalf("SMTP 拨号未经 SOCKS5 代理：socks 收到的目标=%q，期望 smtp.unreachable.test:587", got)
	}
}

func TestDialIMAP_HonorsHTTPProxy(t *testing.T) {
	backend := newFakeSMTPServer(t, true) // 任意 TCP backend 即可，只断言代理被使用
	proxy := newFakeConnectProxy(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(backend.port())))

	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://"+proxy.addr())

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	client, _ := dialIMAP(ctx, IMAPConfig{
		Host: "imap.unreachable.test",
		Port: 143,
		TLS:  false,
	})
	if client != nil {
		_ = client.Close()
	}

	if got := proxy.lastConnect(); got != "imap.unreachable.test:143" {
		t.Fatalf("IMAP 拨号未经 HTTP 代理：proxy 收到的 CONNECT=%q，期望 imap.unreachable.test:143", got)
	}
}

// ---------------------------------------------------------------------------
// RED: ProbeSMTP 连接层失败必须分类为"连接失败"而非"认证失败"
// ---------------------------------------------------------------------------

func TestProbeSMTP_GreetingEOFClassifiedAsConnError(t *testing.T) {
	// 接受连接后立即关闭 → smtp.NewClient 读 greeting 得 EOF。
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
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err = ProbeSMTP(ctx, SMTPConfig{
		Host:     "127.0.0.1",
		Port:     port,
		Username: "u@example.com",
		Password: "p",
		From:     "u@example.com",
	})
	if err == nil {
		t.Fatal("greeting EOF 应返回错误")
	}
	msg := err.Error()
	if strings.Contains(msg, "认证失败") {
		t.Fatalf("连接层 EOF 不应分类为「认证失败」，得到: %v", msg)
	}
	if !strings.Contains(msg, "连接失败") {
		t.Fatalf("连接层 EOF 应分类为「连接失败」，得到: %v", msg)
	}
}

// ---------------------------------------------------------------------------
// 回归锁：selectProxyURL 的优先级 + NO_PROXY/loopback 旁路（纯函数，确定性）
// ---------------------------------------------------------------------------

func TestSelectProxyURL_PrecedenceAndNoProxy(t *testing.T) {
	t.Run("loopback 永不走代理", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1080")
		for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
			if u := selectProxyURL(h); u != nil {
				t.Fatalf("%s 应旁路代理，得到 %v", h, u)
			}
		}
	})

	t.Run("ALL_PROXY 优先于 HTTPS/HTTP_PROXY", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("ALL_PROXY", "socks5://10.0.0.1:1080")
		t.Setenv("HTTPS_PROXY", "http://10.0.0.2:8080")
		t.Setenv("HTTP_PROXY", "http://10.0.0.3:8080")
		u := selectProxyURL("smtp.gmail.com")
		if u == nil || u.Scheme != "socks5" || u.Host != "10.0.0.1:1080" {
			t.Fatalf("应选 ALL_PROXY 的 socks5://10.0.0.1:1080，得到 %v", u)
		}
	})

	t.Run("仅 HTTP_PROXY 时回退到它", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTP_PROXY", "http://10.0.0.3:8080")
		u := selectProxyURL("smtp.gmail.com")
		if u == nil || u.Scheme != "http" || u.Host != "10.0.0.3:8080" {
			t.Fatalf("应回退到 HTTP_PROXY，得到 %v", u)
		}
	})

	t.Run("NO_PROXY 命中域名后缀则旁路", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("ALL_PROXY", "socks5://10.0.0.1:1080")
		t.Setenv("NO_PROXY", "example.com,.corp.internal")
		if u := selectProxyURL("mail.corp.internal"); u != nil {
			t.Fatalf("mail.corp.internal 命中 .corp.internal 应旁路，得到 %v", u)
		}
		if u := selectProxyURL("smtp.gmail.com"); u == nil {
			t.Fatal("smtp.gmail.com 未命中 NO_PROXY，应走代理")
		}
	})

	t.Run("裸 host:port 当作 http 代理", func(t *testing.T) {
		clearProxyEnv(t)
		t.Setenv("HTTPS_PROXY", "10.0.0.9:7890")
		u := selectProxyURL("smtp.gmail.com")
		if u == nil || u.Scheme != "http" || u.Host != "10.0.0.9:7890" {
			t.Fatalf("裸 host:port 应按 http 处理，得到 %v", u)
		}
	})

	t.Run("无任何代理变量则直连", func(t *testing.T) {
		clearProxyEnv(t)
		if u := selectProxyURL("smtp.gmail.com"); u != nil {
			t.Fatalf("无代理环境应直连(nil)，得到 %v", u)
		}
	})
}
