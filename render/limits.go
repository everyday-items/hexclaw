package render

import "time"

// 配额硬上限（详见 .claude/doc-generation-architecture.md §3.2 并发与配额）。
//
// 这些值是渲染服务的安全门槛，不是 nice-to-have：
//   - 防 OOM（3 并发 × 100 MB 输出 = 300 MB heap，已会击穿 sidecar RSS 预算；
//     即使采用 path-based 接口 Go heap 也会有过渡 buffer，所以仍要硬限）
//   - 防巨型 markdown 拖死 pandoc reader
//   - 防恶意 base64 data URL 把请求体撑爆
const (
	// MaxRequestBodyBytes 请求体总字节上限（含 data URL 图片 base64 膨胀）。
	MaxRequestBodyBytes int64 = 50 * 1024 * 1024 // 50 MB

	// MaxMarkdownBytes 纯 markdown 文本（剥离 data URL 后）上限。
	MaxMarkdownBytes int64 = 5 * 1024 * 1024 // 5 MB

	// MaxDataURLBytes 单张 data URL 图片解码前上限。
	MaxDataURLBytes int64 = 10 * 1024 * 1024 // 10 MB

	// MaxOutputBytes 渲染产物上限。
	MaxOutputBytes int64 = 100 * 1024 * 1024 // 100 MB

	// MaxConcurrent 单进程并发渲染上限（semaphore）。
	MaxConcurrent = 3

	// PandocTimeout pandoc 渲染硬超时。
	PandocTimeout = 30 * time.Second

	// PandocPDFTimeout pandoc + typst 引擎 PDF 渲染硬超时（typst 编译稍慢）。
	PandocPDFTimeout = 60 * time.Second

	// CacheMaxBytes 磁盘 LRU 缓存上限。
	CacheMaxBytes int64 = 200 * 1024 * 1024 // 200 MB

	// CacheTTL 缓存条目 TTL。
	CacheTTL = 7 * 24 * time.Hour // 7 天

	// SandboxMaxBytes sidecar 受控临时目录上限（超出触发清理）。
	SandboxMaxBytes int64 = 500 * 1024 * 1024 // 500 MB
)

// timeoutFor 按 format 返回合适的硬超时。
func timeoutFor(f Format) time.Duration {
	if f == FormatPDF {
		return PandocPDFTimeout
	}
	return PandocTimeout
}
