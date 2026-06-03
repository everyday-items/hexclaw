package render

import (
	"context"
	"fmt"
)

// Service 渲染服务编排层。组合 Renderer + Cache + 并发控制 + 输入校验。
//
// 流程（参考 .claude/doc-generation-architecture.md §3.2 产物流转）：
//
//  1. semaphore acquire（限并发 ≤ MaxConcurrent，超出返回 CONCURRENCY_REJECTED）
//  2. 输入校验（markdown 大小 ≤ MaxMarkdownBytes）
//  3. 缓存查找（命中直接返回 cache file path）
//  4. 调 Renderer 渲染到 temp file
//  5. 大小检查 + 后处理
//  6. 入缓存（rename 到 cache 目录）
//  7. 返回 RenderResult{Path: cache file, CacheHit: 视情况}
type Service struct {
	renderer Renderer
	cache    *Cache
	sem      chan struct{}
}

// ServiceConfig 服务配置。Renderer 必填；Cache 可空（不启用缓存）。
type ServiceConfig struct {
	Renderer Renderer
	Cache    *Cache
}

// NewService 构造服务。
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Renderer == nil {
		return nil, fmt.Errorf("render: Renderer is required")
	}
	return &Service{
		renderer: cfg.Renderer,
		cache:    cfg.Cache,
		sem:      make(chan struct{}, MaxConcurrent),
	}, nil
}

// Render 编排完整渲染流程。
func (s *Service) Render(ctx context.Context, content string, format Format, opts RenderOptions) (*RenderResult, error) {
	// 1. 输入校验：纯 markdown 文本（剥离 data URL 后）≤ 5 MB
	textSize := MarkdownTextSize(content)
	if textSize > MaxMarkdownBytes {
		return nil, &RenderError{Code: CodeInputTooLarge, Format: format,
			Detail: fmt.Sprintf("markdown text size %d > limit %d", textSize, MaxMarkdownBytes)}
	}
	if !format.IsValid() {
		return nil, &RenderError{Code: CodeFormatUnsupported, Format: format}
	}
	if err := opts.Validate(); err != nil {
		return nil, &RenderError{Code: CodeFormatUnsupported, Format: format, Detail: err.Error()}
	}

	// 2. 并发限制（非阻塞，立即拒绝避免请求堆积）
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		return nil, &RenderError{Code: CodeConcurrencyRejected, Format: format}
	}

	// 3. 输入预处理：远程图片预下载 + SSRF 校验 + data URL 注入
	//    必须在缓存查找前做，因为缓存键基于"已内联资源后的 markdown"
	processed, err := PreprocessMarkdown(ctx, content, PreprocessConfig{})
	if err != nil {
		// PreprocessMarkdown 已返回 *RenderError
		if re, ok := err.(*RenderError); ok {
			re.Format = format
			return nil, re
		}
		return nil, &RenderError{Code: CodeRenderFailed, Format: format, Detail: err.Error()}
	}

	// 4. 缓存查找
	var cacheKey string
	if s.cache != nil {
		cacheKey = s.cache.Key(processed, format, opts)
		if hit, ok := s.cache.Lookup(cacheKey, format); ok {
			return hit, nil
		}
	}

	// 5. 实际渲染（temp file）
	result, err := s.renderer.Render(ctx, processed, format, opts)
	if err != nil {
		return nil, err
	}

	// 6. 入缓存（rename temp → cache 目录；失败时 result 仍有效，按 temp file 返回）
	if s.cache != nil && cacheKey != "" {
		if cachedPath, putErr := s.cache.Put(cacheKey, result.Path); putErr == nil {
			result.Path = cachedPath
			result.CacheHit = false // 是首次写入，不算 hit
		}
		// Put 失败时不阻塞响应，让 result 仍指向 temp file
	}

	return result, nil
}
