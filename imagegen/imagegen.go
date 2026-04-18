// Package imagegen 提供文本到图像生成能力。
//
// 定义 Provider 接口，支持多种图像生成 Provider 接入：
//   - OpenAI DALL-E (DALL·E 3 / DALL·E 2)
//   - 智谱 CogView (CogView-4 / CogView-3-Plus)
//   - 兼容协议（Stability、Flux、自托管）可后续扩展
//
// 与 voice 包一致的"接口 + Service"模式：
//
//	svc := imagegen.NewService(map[string]Provider{...}, defaultName)
//	resp, _ := svc.Generate(ctx, "openai", imagegen.Request{Prompt: "..."})
package imagegen

import (
	"context"
	"fmt"
)

// Provider 文生图 Provider 接口。
//
// 输入：文本 prompt + 可选参数（尺寸、张数、风格等）。
// 输出：URL 列表（公网链接）或 base64 列表（无外网时直接内嵌）。
type Provider interface {
	// Name 返回 Provider 名称（如 "openai-dalle"、"zhipu-cogview"）
	Name() string
	// Generate 生成图像
	Generate(ctx context.Context, req Request) (*Result, error)
	// SupportedModels 返回该 Provider 支持的模型 ID 列表（如 ["dall-e-3", "dall-e-2"]）
	SupportedModels() []string
}

// Request 图像生成请求。
type Request struct {
	Model    string `json:"model"`              // 模型 ID（如 "dall-e-3"、"cogview-4"）
	Prompt   string `json:"prompt"`             // 提示词（必填）
	Negative string `json:"negative,omitempty"` // 反向提示词（部分 Provider 支持）
	Size     string `json:"size,omitempty"`     // 尺寸 "1024x1024" / "1792x1024" 等
	N        int    `json:"n,omitempty"`        // 张数，默认 1
	Style    string `json:"style,omitempty"`    // "vivid" / "natural"（DALL-E 3）
	Quality  string `json:"quality,omitempty"`  // "standard" / "hd"（DALL-E 3）
	UserID   string `json:"user,omitempty"`     // 终端用户标识（合规追踪）
}

// Image 单张生成结果。Provider 二选一返回 URL 或 B64JSON；落盘后填 FilePath。
type Image struct {
	URL           string `json:"url,omitempty"`
	B64JSON       string `json:"b64_json,omitempty"`
	FilePath      string `json:"file_path,omitempty"`      // 持久化后的相对路径（如 "202604/abc.png"），前端拼 /api/v1/files/generated/{file_path} 访问
	RevisedPrompt string `json:"revised_prompt,omitempty"` // DALL-E 3 改写后的 prompt
}

// Result 生成结果。
type Result struct {
	Provider string  `json:"provider"`           // Provider 名称（如 "openai-dalle"）
	Model    string  `json:"model"`              // 实际使用的模型
	Created  int64   `json:"created"`            // 时间戳（秒）
	Images   []Image `json:"images"`             // 生成的图像列表
	UsageMs  int64   `json:"usage_ms,omitempty"` // 耗时（毫秒）
}

// Service 文生图服务。
//
// 多 Provider 路由：按 Provider 名或模型名分发请求。
type Service struct {
	providers map[string]Provider // key = Provider name
	// modelIndex 模型 ID 反查 Provider，用于按 model 字段自动路由
	modelIndex map[string]Provider
	// defaultProvider 默认 Provider 名（用户未指定时使用）
	defaultProvider string
}

// NewService 创建文生图服务。
//
// providers 中 key 是 Provider 名称（与 Name() 一致），自动建立 model→provider 反查表。
// defaultName 可为空，仅当所有请求都显式带 provider 字段时不需要。
func NewService(providers map[string]Provider, defaultName string) *Service {
	s := &Service{
		providers:       make(map[string]Provider, len(providers)),
		modelIndex:      make(map[string]Provider),
		defaultProvider: defaultName,
	}
	for k, p := range providers {
		if p == nil {
			continue
		}
		s.providers[k] = p
		for _, m := range p.SupportedModels() {
			// 后注册的 Provider 不覆盖前面已索引的模型，保留首选
			if _, exists := s.modelIndex[m]; !exists {
				s.modelIndex[m] = p
			}
		}
	}
	return s
}

// Generate 调用指定 Provider（或按模型自动路由）生成图像。
//
// 路由优先级：
//  1. providerName 非空 → 按名查找
//  2. 否则按 req.Model 在 modelIndex 中查找
//  3. 否则使用 defaultProvider
func (s *Service) Generate(ctx context.Context, providerName string, req Request) (*Result, error) {
	if req.Prompt == "" {
		return nil, fmt.Errorf("prompt 不能为空")
	}
	p := s.resolveProvider(providerName, req.Model)
	if p == nil {
		return nil, fmt.Errorf("未找到可用的图像生成 Provider（请求 provider=%q model=%q）", providerName, req.Model)
	}
	return p.Generate(ctx, req)
}

func (s *Service) resolveProvider(name, model string) Provider {
	if name != "" {
		if p, ok := s.providers[name]; ok {
			return p
		}
	}
	if model != "" {
		if p, ok := s.modelIndex[model]; ok {
			return p
		}
	}
	if s.defaultProvider != "" {
		return s.providers[s.defaultProvider]
	}
	return nil
}

// HasProvider 是否注册了任意 Provider。
func (s *Service) HasProvider() bool {
	return len(s.providers) > 0
}

// Providers 返回已注册 Provider 名列表（稳定顺序由调用方排序）。
func (s *Service) Providers() []string {
	names := make([]string, 0, len(s.providers))
	for k := range s.providers {
		names = append(names, k)
	}
	return names
}

// Models 返回所有支持的模型 ID（去重）。
func (s *Service) Models() []string {
	models := make([]string, 0, len(s.modelIndex))
	for m := range s.modelIndex {
		models = append(models, m)
	}
	return models
}
