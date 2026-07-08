package engineadapter

import (
	"context"
	"fmt"
	"os"

	"github.com/hexagon-codes/hexclaw/render"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
)

// maxExportBytes 导出产物读取上限（错题本导出很小；防大文件撑爆 heap）。
const maxExportBytes = 30 << 20 // 30MB

// renderService 文档渲染（*render.Service 满足）。
type renderService interface {
	Render(ctx context.Context, content string, format render.Format, opts render.RenderOptions) (*render.RenderResult, error)
}

// RenderAdapter 把 Markdown 渲染成 PDF/Word 等（经平台 render 服务）。
type RenderAdapter struct{ svc renderService }

// NewRenderAdapter 创建 adapter。svc 可为 nil（render 服务未启用 → 调用方降级 markdown）。
func NewRenderAdapter(svc renderService) *RenderAdapter { return &RenderAdapter{svc: svc} }

var _ usecase.Renderer = (*RenderAdapter)(nil)

// Render 把 markdown 渲染为指定格式，返回二进制 + Content-Type。
// svc 为 nil 或格式非法 → 返回 ErrRenderUnavailable，调用方回退 markdown。
func (a *RenderAdapter) Render(ctx context.Context, markdown, format string) ([]byte, string, error) {
	if a.svc == nil {
		return nil, "", usecase.ErrRenderUnavailable
	}
	f := render.Format(format)
	if !f.IsValid() {
		return nil, "", fmt.Errorf("render: 不支持格式 %q", format)
	}
	res, err := a.svc.Render(ctx, markdown, f, render.RenderOptions{Locale: "zh-CN"})
	if err != nil {
		return nil, "", err // RenderError（含 ENGINE_MISSING 等），调用方按需降级
	}
	if res.Size > maxExportBytes {
		if !res.CacheHit {
			_ = os.Remove(res.Path)
		}
		return nil, "", fmt.Errorf("render: 产物过大 %d 字节", res.Size)
	}
	data, err := os.ReadFile(res.Path)
	if !res.CacheHit { // temp 文件，读完清理（cache 命中的不删）
		_ = os.Remove(res.Path)
	}
	if err != nil {
		return nil, "", fmt.Errorf("render: 读产物: %w", err)
	}
	return data, res.ContentType, nil
}
