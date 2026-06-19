package render

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

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
//   - https?:// → 走 ssrf.ValidateURL（私有段拦截 + DNS 解析校验 + 重定向再校验），
//     拉到内存后转 base64 data URL 注入回 markdown
//   - data:image/... → 保留（pandoc 默认能识别）
//   - file:// 或绝对路径 → 拒绝（防越权读盘）
//   - ./relative 或 ../relative → 不解析（artifact 是文本流无 cwd），保留 alt 文本占位
//
// 调用处必须把 pandoc 的 `--embed-resources` 关掉——否则 pandoc 会自己再去
// fetch URL，绕过我们的 SSRF 闸门。
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

// fetchToDataURL 安全拉取 URL 并转 data URL。
//
// 安全清单：
//   - ssrf.ValidateURL（私有段 / loopback / cloud metadata / DNS rebinding）
//   - 仅 http/https
//   - 跟随重定向但**对每个重定向目标重新校验**
//   - 单图字节上限
//   - 超时
func fetchToDataURL(ctx context.Context, rawURL string, client *http.Client, maxBytes int64) (string, error) {
	if err := ssrf.ValidateURL(rawURL); err != nil {
		return "", &RenderError{
			Code:   CodeInvalidInput,
			Detail: fmt.Sprintf("SSRF check failed for %s: %v", rawURL, err),
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", &RenderError{Code: CodeInvalidInput, Detail: err.Error()}
	}
	resp, err := client.Do(req)
	if err != nil {
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

// newSafeHTTPClient 构造对每个重定向都重新做 SSRF 校验的 HTTP 客户端。
//
// 这是关键：标准 http.Client.Do 默认会跟随重定向到任意 URL，包括
// 攻击者可控的内网/loopback。CheckRedirect 钩子拦下每跳，重新跑 ValidateURL。
func newSafeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// 跳数限制
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects (%d)", len(via))
			}
			// 重新校验目标
			target := req.URL.String()
			if err := ssrf.ValidateURL(target); err != nil {
				return fmt.Errorf("redirect target %s blocked: %w", target, err)
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
