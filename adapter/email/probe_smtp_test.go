package email

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSMTPServer 是一个最小化的本地 SMTP 服务器，仅用于测试 ProbeSMTP 的 AUTH 流程，
// 不实现完整 SMTP。它在 127.0.0.1:0 监听（loopback，使 net/smtp 允许明文 AUTH PLAIN）。
//
// authOK 控制 AUTH 命令返回 235（成功）还是 535（失败），从而覆盖成功/失败两条路径。
type fakeSMTPServer struct {
	ln     net.Listener
	authOK bool
}

func newFakeSMTPServer(t *testing.T, authOK bool) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	s := &fakeSMTPServer{ln: ln, authOK: authOK}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeSMTPServer) port() int {
	return s.ln.Addr().(*net.TCPAddr).Port
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)

	write := func(line string) {
		_, _ = w.WriteString(line + "\r\n")
		_ = w.Flush()
	}

	write("220 fake-smtp ready")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			// 不广告 STARTTLS：让 ProbeSMTP 直接进入明文 AUTH PLAIN（loopback 允许）。
			write("250-fake-smtp")
			write("250 AUTH PLAIN LOGIN")
		case strings.HasPrefix(cmd, "AUTH"):
			if s.authOK {
				write("235 2.7.0 Authentication successful")
			} else {
				write("535 5.7.8 Authentication credentials invalid")
			}
		case strings.HasPrefix(cmd, "QUIT"):
			write("221 Bye")
			return
		default:
			write("250 OK")
		}
	}
}

func TestProbeSMTP_Success(t *testing.T) {
	srv := newFakeSMTPServer(t, true)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := ProbeSMTP(ctx, SMTPConfig{
		Host:     "127.0.0.1",
		Port:     srv.port(),
		Username: "user@example.com",
		Password: "secret",
		From:     "user@example.com",
	})
	if err != nil {
		t.Fatalf("期望 AUTH 成功，得到错误: %v", err)
	}
}

func TestProbeSMTP_AuthFailure(t *testing.T) {
	srv := newFakeSMTPServer(t, false)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := ProbeSMTP(ctx, SMTPConfig{
		Host:     "127.0.0.1",
		Port:     srv.port(),
		Username: "user@example.com",
		Password: "wrong",
		From:     "user@example.com",
	})
	if err == nil {
		t.Fatal("期望 AUTH 失败返回错误，得到 nil")
	}
	if !strings.Contains(err.Error(), "认证失败") {
		t.Fatalf("错误信息应包含 \"认证失败\"，得到: %v", err)
	}
}

func TestProbeSMTP_MissingFields(t *testing.T) {
	err := ProbeSMTP(context.Background(), SMTPConfig{Host: "smtp.example.com"})
	if err == nil {
		t.Fatal("缺少 username/password 时应返回错误")
	}
}

func TestProbeSMTP_DialFailure(t *testing.T) {
	// 监听后立即关闭，得到一个无人接听的端口，触发连接失败路径。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err = ProbeSMTP(ctx, SMTPConfig{
		Host:     "127.0.0.1",
		Port:     port,
		Username: "user@example.com",
		Password: "secret",
	})
	if err == nil {
		t.Fatal("连接不可用端口时应返回错误")
	}
}
