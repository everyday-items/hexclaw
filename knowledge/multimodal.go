package knowledge

import (
	"context"
	"fmt"
	"strings"
)

// 多模态图像摄取（best practice，provider-agnostic）。
//
// HexClaw 的向量库是纯文本的（modernc SQLite + 暴力余弦，无多模态 embedder）。
// 业界对齐 OpenClaw 的「label + embedding」做法：先用视觉模型（VLM）把图像转写成
// 文本描述（caption），再把这段 caption 走完整的既有 RAG 管线（分块→嵌入→存储→
// 混合检索→重排），并打上 SourceType="image" 标签，使图像可被现有文本检索栈召回、
// 可被元数据过滤（Filter{SourceTypes:["image"]}）筛选与展示。
//
// 关键设计：不复制 AddDocument 的写入机制——AddImageDocument 把 caption 组装成文档
// 正文后直接复用 AddDocument（含分块/嵌入/upsert/竞态回退）。source_type 标记走
// 「image:」源前缀约定（见 knowledge.go 的 sourceTypeFromSource），与 upload:/cron:
// 同套机制，让 source 字符串本身即编码类型，零额外写入分支。

const (
	// imageSourcePrefix 标记一条 source 来自图像摄取；sourceTypeFromSource 识别它 → "image"。
	imageSourcePrefix = "image:"
	// imageCaptionPrefix 前缀让检索/回答端明确这段文本是「图像描述」而非普通正文。
	imageCaptionPrefix = "【图像内容】\n"
)

// Captioner 把图像转写为可检索的文本描述（VLM caption）。
//
// 与 RerankLLM 同形的可插拔能力面：注入一个视觉模型（chat router 的 vision 模型）即可
// 让知识库摄取图像。不注入时 AddImageDocument 优雅报错（不静默写入垃圾）。
type Captioner interface {
	// Caption 返回 image（mime 为其 MIME 类型，如 "image/png"）的文本描述。
	Caption(ctx context.Context, image []byte, mime string) (string, error)
}

// CaptionerFunc 把普通函数适配为 Captioner。
type CaptionerFunc func(ctx context.Context, image []byte, mime string) (string, error)

// Caption 实现 Captioner。
func (f CaptionerFunc) Caption(ctx context.Context, image []byte, mime string) (string, error) {
	return f(ctx, image, mime)
}

// WithCaptioner 注入图像转写器（通常复用 chat router 的 vision 模型）。
// 不注入时 AddImageDocument 返回明确错误（图像无法摄取），其余功能不受影响。
func WithCaptioner(c Captioner) ManagerOption {
	return func(m *Manager) { m.captioner = c }
}

// HasCaptioner 报告当前知识库是否已配置视觉转写能力。
func (m *Manager) HasCaptioner() bool {
	return m != nil && m.captioner != nil
}

// CaptionImage 使用知识库已配置的视觉模型把图像转写为可检索文本。
//
// 这是给文档摄取层复用的窄能力面：PDF 扫描页、DOCX 内嵌图片等都应复用同一
// captioner，而不是绕开知识库配置另起模型调用路径。
func (m *Manager) CaptionImage(ctx context.Context, image []byte, mime string) (string, error) {
	if len(image) == 0 {
		return "", fmt.Errorf("图像内容不能为空")
	}
	if m == nil || m.captioner == nil {
		return "", fmt.Errorf("图片入库需要先配置具备视觉能力的模型（视觉模型 / VLM），当前未配置；请在设置中为知识库配置视觉模型后重试")
	}
	caption, err := m.captioner.Caption(ctx, image, mime)
	if err != nil {
		return "", fmt.Errorf("图像转写失败（请确认所用模型为支持图片的视觉模型）: %w", err)
	}
	caption = strings.TrimSpace(caption)
	if caption == "" {
		return "", fmt.Errorf("图像转写结果为空，已跳过摄取")
	}
	return caption, nil
}

// AddImageDocument 把一张图像摄取进知识库（多模态：VLM caption → 文本 RAG）。
//
// 流程：caption 图像 → 组装「【图像内容】\n<caption>」正文 → 复用 AddDocument 走完整
// 写入管线，文档 SourceType 经「image:」源前缀约定标记为 "image"。title 为空时从 caption
// 派生。未注入 Captioner、空图像、空 caption 一律返回明确错误（绝不摄取空/垃圾文档）。
func (m *Manager) AddImageDocument(ctx context.Context, title string, image []byte, mime, source string) (*Document, error) {
	caption, err := m.CaptionImage(ctx, image, mime)
	if err != nil {
		return nil, err
	}

	title = strings.TrimSpace(title)
	if title == "" {
		title = deriveImageTitle(caption)
	}
	content := imageCaptionPrefix + caption

	// 复用 AddDocument（分块/嵌入/upsert/竞态回退）；imageSource 让 source 携带「image:」
	// 前缀，AddDocument 内部据此把 SourceType 设为 "image"，无需第二条写入路径。
	return m.AddDocument(ctx, title, content, imageSource(source))
}

// imageSource 规整 source，确保其携带「image:」前缀（幂等），使 sourceTypeFromSource → "image"。
// 空 source 退化为裸前缀 "image:"（仍归类为 image）。
func imageSource(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return imageSourcePrefix
	}
	if strings.HasPrefix(source, imageSourcePrefix) {
		return source
	}
	return imageSourcePrefix + source
}

// deriveImageTitle 从 caption 首行派生默认标题（裁剪到 24 rune），全空时退化为「图像」。
// 与 deriveSnapshotBaseTitle 同风格，但用图像语义的兜底文案。
func deriveImageTitle(caption string) string {
	line := strings.TrimSpace(caption)
	if idx := strings.IndexAny(line, "\r\n"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	if line == "" {
		return "图像"
	}
	if r := []rune(line); len(r) > 24 {
		return string(r[:24])
	}
	return line
}
