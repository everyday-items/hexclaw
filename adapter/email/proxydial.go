package email

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// dialTimeout 是建立到目标（或代理）TCP 连接的默认超时，与原 net.Dialer 配置保持一致。
const dialTimeout = 10 * time.Second

// dialTCPContext 建立到 host:port 的 TCP 连接，并遵循环境中的代理设置：
//
//   - ALL_PROXY（socks5://…）        → SOCKS5 拨号
//   - ALL_PROXY/HTTPS_PROXY/HTTP_PROXY（http(s)://…） → HTTP CONNECT 隧道
//   - 均未设置 / 目标命中 NO_PROXY / loopback → 直连
//
// 邮件 SMTP/IMAP 是裸 TCP，标准库 net.Dial 不会读取 *_PROXY 环境变量；而桌面 sidecar
// 已把宿主机系统代理注入这些变量（应用其余 HTTP 流量经 http.ProxyFromEnvironment 走同一
// 出口）。若邮件拨号仍直连，在"必须经代理才能出网"的网络（如需代理访问 Gmail）下会被
// 重置，表现为 smtp.NewClient 读 greeting 得 EOF。本函数让邮件与其余流量共用代理出口。
func dialTCPContext(ctx context.Context, host string, port int) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	base := &net.Dialer{Timeout: dialTimeout}

	pu := selectProxyURL(host)
	if pu == nil {
		return base.DialContext(ctx, "tcp", addr)
	}

	switch strings.ToLower(pu.Scheme) {
	case "socks5", "socks5h", "socks":
		var auth *xproxy.Auth
		if pu.User != nil {
			pw, _ := pu.User.Password()
			auth = &xproxy.Auth{User: pu.User.Username(), Password: pw}
		}
		d, err := xproxy.SOCKS5("tcp", pu.Host, auth, base)
		if err != nil {
			return nil, fmt.Errorf("配置 SOCKS5 代理 %s 失败: %w", pu.Host, err)
		}
		if cd, ok := d.(xproxy.ContextDialer); ok {
			return cd.DialContext(ctx, "tcp", addr)
		}
		return d.Dial("tcp", addr)
	case "http", "https":
		return httpConnectDial(ctx, base, pu, addr)
	default:
		// 未知 scheme：保守地直连，不静默吞掉拨号。
		return base.DialContext(ctx, "tcp", addr)
	}
}

// dialTLSContext 在 dialTCPContext 之上做 TLS 握手，用于隐式 TLS 端口（SMTPS 465 / IMAPS 993）。
// 经代理时，先建立到目标的明文隧道，再在隧道之上与目标做 TLS，证书按目标 host 校验。
func dialTLSContext(ctx context.Context, host string, port int) (net.Conn, error) {
	raw, err := dialTCPContext(ctx, host, port)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	tlsConn := tls.Client(raw, &tls.Config{ServerName: host})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = tlsConn.SetDeadline(time.Time{}) // 清除握手期 deadline，交回调用方控制
	return tlsConn, nil
}

// selectProxyURL 按优先级从环境变量解析适用于裸 TCP 的代理 URL；目标命中 NO_PROXY 或
// loopback 时返回 nil（直连）。优先级：ALL_PROXY > HTTPS_PROXY > HTTP_PROXY（大小写各一份）。
func selectProxyURL(host string) *url.URL {
	if isNoProxyHost(host) {
		return nil
	}
	for _, key := range []string{"ALL_PROXY", "all_proxy", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			continue
		}
		// 容忍裸 host:port（无 scheme），按 http 代理处理。
		if !strings.Contains(v, "://") {
			v = "http://" + v
		}
		u, err := url.Parse(v)
		if err != nil || u.Host == "" {
			continue
		}
		return u
	}
	return nil
}

// isNoProxyHost 判断 host 是否应绕过代理：loopback 永远绕过（与多数代理客户端一致，
// 也避免本地回环连接被错误地经代理），其余按 NO_PROXY 列表匹配（精确或域名后缀，"*" 全绕过）。
func isNoProxyHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}

	np := strings.TrimSpace(os.Getenv("NO_PROXY"))
	if np == "" {
		np = strings.TrimSpace(os.Getenv("no_proxy"))
	}
	if np == "" {
		return false
	}
	for _, entry := range strings.Split(np, ",") {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		entry = strings.TrimPrefix(entry, ".")
		if h == entry || strings.HasSuffix(h, "."+entry) {
			return true
		}
	}
	return false
}

// httpConnectDial 经 HTTP(S) 代理用 CONNECT 方法建立到 targetAddr 的隧道。CONNECT 可隧道
// 任意 TCP，故适用于 SMTP/IMAP；响应头只读到 "\r\n\r\n" 为止，绝不预读隧道后续字节
// （即目标服务器的 SMTP/IMAP greeting）。
func httpConnectDial(ctx context.Context, base *net.Dialer, pu *url.URL, targetAddr string) (net.Conn, error) {
	proxyAddr := pu.Host
	if _, _, err := net.SplitHostPort(proxyAddr); err != nil {
		if strings.EqualFold(pu.Scheme, "https") {
			proxyAddr = net.JoinHostPort(proxyAddr, "443")
		} else {
			proxyAddr = net.JoinHostPort(proxyAddr, "80")
		}
	}

	conn, err := base.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("连接 HTTP 代理 %s 失败: %w", proxyAddr, err)
	}
	if strings.EqualFold(pu.Scheme, "https") {
		host, _, _ := net.SplitHostPort(proxyAddr)
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("与 HTTPS 代理 %s TLS 握手失败: %w", proxyAddr, err)
		}
		conn = tlsConn
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", targetAddr, targetAddr)
	if pu.User != nil {
		pw, _ := pu.User.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(pu.User.Username() + ":" + pw))
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", cred)
	}
	b.WriteString("\r\n")
	if _, err := conn.Write([]byte(b.String())); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("向代理发送 CONNECT 失败: %w", err)
	}

	status, err := readConnectStatus(conn)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("读取代理 CONNECT 响应失败: %w", err)
	}
	if status != 200 {
		_ = conn.Close()
		return nil, fmt.Errorf("代理拒绝 CONNECT %s: HTTP %d", targetAddr, status)
	}
	_ = conn.SetDeadline(time.Time{}) // 清除握手期 deadline
	return conn, nil
}

// readConnectStatus 逐字节读取 CONNECT 响应直到响应头结束（\r\n\r\n），解析状态码。
// 逐字节读取确保不会越过响应头消费到隧道里的目标 greeting。
func readConnectStatus(conn net.Conn) (int, error) {
	buf := make([]byte, 0, 128)
	one := make([]byte, 1)
	for {
		n, err := conn.Read(one)
		if n > 0 {
			buf = append(buf, one[0])
			if len(buf) >= 4 && string(buf[len(buf)-4:]) == "\r\n\r\n" {
				break
			}
			if len(buf) > 8192 {
				return 0, fmt.Errorf("CONNECT 响应头过长")
			}
		}
		if err != nil {
			return 0, err
		}
	}
	line := string(buf)
	if i := strings.Index(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	// 形如 "HTTP/1.1 200 Connection established"
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return 0, fmt.Errorf("无效的 CONNECT 状态行: %q", line)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, fmt.Errorf("无效的 CONNECT 状态码: %q", parts[1])
	}
	return code, nil
}
