package render

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hexagon-codes/hexclaw/httpua"
	"github.com/hexagon-codes/toolkit/net/ssrf"
)

// PreprocessConfig 预处理配置。
type PreprocessConfig struct {
	// HTTPClient 用于拉远程图片；为 nil 时构造默认带超时的客户端。
	HTTPClient *http.Client
	// PerImageTimeout 单图拉取超时。零值 = 5 秒。
	PerImageTimeout time.Duration
	// MaxImageBytes 单图字节上限（解码前）。零值 = MaxDataURLBytes。
	MaxImageBytes int64
}

// imageRefPattern 匹配 markdown 内联图片：![alt](url) 或 ![alt](url "title")
//
// 简化匹配，不处理 reference-style（[ref]:）—— P0 范围内的 LLM 输出几乎都是内联式。
// 后续可升级为完整 markdown AST。
var imageRefPattern = regexp.MustCompile(
	`!\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)

// PreprocessMarkdown 对 markdown 中的图片引用做预处理：
//
//   - https?:// → 拉到内存（限大小/超时/重定向跳数）后转 base64 data URL 注入回 markdown
//   - data:image/... → 保留（pandoc 默认能识别）
//   - file:// 或绝对路径 → 拒绝（防越权读盘）
//   - ./relative 或 ../relative → 不解析（artifact 是文本流无 cwd），保留 alt 文本占位
//
// 调用处必须把 pandoc 的 `--embed-resources` 关掉——否则 pandoc 会自己再去
// fetch URL，绕过我们这条统一的图片拉取路径（大小/超时限制）。
func PreprocessMarkdown(ctx context.Context, content string, cfg PreprocessConfig) (string, error) {
	if cfg.PerImageTimeout == 0 {
		cfg.PerImageTimeout = 5 * time.Second
	}
	if cfg.MaxImageBytes == 0 {
		cfg.MaxImageBytes = MaxDataURLBytes
	}
	client := cfg.HTTPClient
	if client == nil {
		client = newSafeHTTPClient(cfg.PerImageTimeout)
	}

	// ReplaceAllStringFunc 不能传 error；用回调累积错误
	var firstErr error
	out := imageRefPattern.ReplaceAllStringFunc(content, func(match string) string {
		if firstErr != nil {
			return match
		}
		groups := imageRefPattern.FindStringSubmatch(match)
		if len(groups) < 3 {
			return match
		}
		alt, rawURL := groups[1], groups[2]

		replacement, err := processImageRef(ctx, alt, rawURL, client, cfg.MaxImageBytes)
		if err != nil {
			firstErr = err
			return match
		}
		return replacement
	})

	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// processImageRef 按图片来源类型分发处理。返回替换后的 markdown 片段。
func processImageRef(ctx context.Context, alt, rawURL string, client *http.Client, maxBytes int64) (string, error) {
	// data: URL — 保留
	if strings.HasPrefix(rawURL, "data:") {
		return fmt.Sprintf("![%s](%s)", alt, rawURL), nil
	}

	// file:// — 拒绝
	if strings.HasPrefix(rawURL, "file://") {
		return "", &RenderError{
			Code:   CodeInvalidInput,
			Detail: fmt.Sprintf("file:// images not allowed: %s", rawURL),
		}
	}

	// 绝对文件系统路径 / 反斜杠（Windows 风格）— 拒绝
	if strings.HasPrefix(rawURL, "/") || strings.HasPrefix(rawURL, `\`) {
		return "", &RenderError{
			Code:   CodeInvalidInput,
			Detail: fmt.Sprintf("absolute filesystem path not allowed in image refs: %s", rawURL),
		}
	}

	// 相对路径 — 不解析，留 alt 文本（artifact 无 cwd 可参照）
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		// 保留 alt 文本，但去掉图片引用（避免 pandoc 报错）
		return alt, nil
	}

	// http(s) — 经 SSRF 闸门后下载
	dataURL, err := fetchToDataURL(ctx, rawURL, client, maxBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("![%s](%s)", alt, dataURL), nil
}

// fetchToDataURL 拉取 URL 并转 data URL。
//
// 约束：仅 http/https、单图字节上限、超时、跟随重定向（限跳数）。
func fetchToDataURL(ctx context.Context, rawURL string, client *http.Client, maxBytes int64) (string, error) {
	// SSRF 前置闸门：在发起任何连接前校验目标 URL，拒绝私网/回环/云元数据端点
	// （169.254.169.254 等），并抵御 DNS rebinding（RU-13）。此前注释谎称"经 SSRF
	// 闸门"但实际未接入，用户 markdown 里的图片 URL 可直连内网/元数据窃取凭据。
	if err := ssrf.ValidateURL(ctx, rawURL); err != nil {
		return "", &RenderError{Code: CodeInvalidInput, Detail: fmt.Sprintf("SSRF check failed for %s: %v", rawURL, err)}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", &RenderError{Code: CodeInvalidInput, Detail: err.Error()}
	}
	httpua.Set(req) // 默认浏览器 UA，避免反爬站对 Go 默认 UA 返回 HTML（AP-016）
	safeClient, err := clientWithSafeRedirects(client)
	if err != nil {
		return "", &RenderError{Code: CodeRenderFailed, Detail: err.Error()}
	}
	resp, err := safeClient.Do(req)
	if err != nil {
		if errors.Is(err, errRenderSSRFBlocked) {
			return "", &RenderError{Code: CodeInvalidInput, Detail: fmt.Sprintf("fetch %s: %v", rawURL, err)}
		}
		return "", &RenderError{Code: CodeRenderFailed, Detail: fmt.Sprintf("fetch %s: %v", rawURL, err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &RenderError{
			Code:   CodeRenderFailed,
			Detail: fmt.Sprintf("fetch %s: HTTP %d", rawURL, resp.StatusCode),
		}
	}

	// 限读 maxBytes + 1 用于检测超限
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return "", &RenderError{Code: CodeRenderFailed, Detail: err.Error()}
	}
	if int64(len(body)) > maxBytes {
		return "", &RenderError{
			Code:   CodeInputTooLarge,
			Detail: fmt.Sprintf("image %s exceeds %d bytes", rawURL, maxBytes),
		}
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// 防止 Content-Type 头里塞 charset 等参数把 data URL 弄歪
	if i := strings.Index(contentType, ";"); i > 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}

	return fmt.Sprintf("data:%s;base64,%s",
		contentType, base64.StdEncoding.EncodeToString(body)), nil
}

var errRenderSSRFBlocked = errors.New("render: SSRF blocked at connection boundary")

// renderSSRFGuardedTransport is intentionally package-private. It exists only
// for in-package transports that never open a socket (deterministic test
// fixtures). An arbitrary caller-provided RoundTripper cannot be made safe
// against DNS rebinding after the fact, so all other custom transports fail
// closed below.
type renderSSRFGuardedTransport interface {
	http.RoundTripper
	renderSSRFGuarded()
}

// clientWithSafeRedirects returns a per-request copy so redirect and transport
// hardening do not mutate a caller-owned client that may be shared by concurrent
// renders. Every real TCP connection is pinned to an address resolved and
// checked at dial time; the actual peer is checked again after connect.
func clientWithSafeRedirects(client *http.Client) (*http.Client, error) {
	if client == nil {
		client = &http.Client{}
	}
	cloned := *client
	transport, err := renderSSRFSafeTransport(client.Transport)
	if err != nil {
		return nil, err
	}
	cloned.Transport = transport
	callerCheckRedirect := client.CheckRedirect
	cloned.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		target := ""
		var redirectCtx context.Context
		if req != nil && req.URL != nil {
			target = req.URL.String()
		}
		if req != nil {
			redirectCtx = req.Context()
		}
		if err := ssrf.ValidateURL(redirectCtx, target); err != nil {
			return fmt.Errorf("SSRF check failed for redirect %s: %w", target, err)
		}
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects (%d)", len(via))
		}
		if callerCheckRedirect != nil {
			return callerCheckRedirect(req, via)
		}
		return nil
	}
	return &cloned, nil
}

func renderSSRFSafeTransport(base http.RoundTripper) (http.RoundTripper, error) {
	if guarded, ok := base.(renderSSRFGuardedTransport); ok {
		return guarded, nil
	}

	var transport *http.Transport
	switch typed := base.(type) {
	case nil:
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("%w: default HTTP transport is not cloneable", errRenderSSRFBlocked)
		}
		transport = defaultTransport.Clone()
	case *http.Transport:
		transport = typed.Clone()
	default:
		return nil, fmt.Errorf("%w: custom HTTP transport has no connect-time guard", errRenderSSRFBlocked)
	}

	// A forward proxy resolves and connects to the target outside this process,
	// beyond the dial-time address check. Remote image rendering therefore uses
	// a direct connection. Custom TLS dialers are cleared for the same reason:
	// HTTPS must pass through the guarded DialContext below.
	transport.Proxy = nil
	//lint:ignore SA1019 The legacy field must also be cleared; otherwise a caller
	// can bypass the guarded DialContext on Go versions that still honor it.
	transport.DialTLS = nil
	transport.DialTLSContext = nil

	originalDial := transport.DialContext
	if originalDial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		originalDial = dialer.DialContext
	}
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid dial address %q: %v", errRenderSSRFBlocked, addr, err)
		}

		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		resolved, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve %q: %v", errRenderSSRFBlocked, host, err)
		}
		if len(resolved) == 0 {
			return nil, fmt.Errorf("%w: no addresses for %q", errRenderSSRFBlocked, host)
		}
		// Reject the whole answer set if any address is unsafe. Picking a public
		// member while ignoring a private member would leave rebinding/round-robin
		// behavior dependent on resolver ordering.
		for _, candidate := range resolved {
			if !renderPublicIP(candidate.IP) {
				return nil, fmt.Errorf("%w: %q resolved to private/reserved IP %s", errRenderSSRFBlocked, host, candidate.IP)
			}
		}

		var lastErr error
		for _, candidate := range resolved {
			// Dial the checked literal address, never the hostname, so the actual
			// dial cannot trigger a second DNS resolution.
			pinnedAddr := net.JoinHostPort(candidate.IP.String(), port)
			conn, dialErr := originalDial(ctx, network, pinnedAddr)
			if dialErr != nil {
				lastErr = dialErr
				continue
			}
			if err := validateRenderPeer(conn.RemoteAddr()); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return conn, nil
		}
		return nil, lastErr
	}
	return transport, nil
}

func renderPublicIP(ip net.IP) bool {
	return ip != nil && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

func validateRenderPeer(addr net.Addr) error {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok || !renderPublicIP(tcpAddr.IP) {
		return fmt.Errorf("%w: connected peer %v is private, reserved, or unverifiable", errRenderSSRFBlocked, addr)
	}
	return nil
}

// newSafeHTTPClient 构造带超时、限制重定向跳数的 HTTP 客户端。
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects (%d)", len(via))
			}
			return nil
		},
	}
}

// MarkdownTextSize 估算"纯 markdown 文本"大小（剥离 data URL 后），
// 用于配额检查 5 MB markdown 上限。
//
// 实现简单：把所有 data:...;base64,... 视为 0 字节。
func MarkdownTextSize(content string) int64 {
	// 替换所有 data URL 为占位
	dataURLPattern := regexp.MustCompile(`data:[^;,)]+;base64,[A-Za-z0-9+/=]+`)
	stripped := dataURLPattern.ReplaceAllString(content, "")
	return int64(len(stripped))
}

// 确保 url.Parse 等使用过；当前 ValidateURL 已自带，本文件未直接用 url
var _ = url.URL{}
