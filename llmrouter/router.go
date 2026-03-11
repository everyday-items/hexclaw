// Package llmrouter 提供 LLM 智能路由
//
// 根据任务复杂度、成本预算、Provider 可用性自动选择最优模型：
//
//	简单任务 (问候/闲聊)     → 低成本模型 (DeepSeek/Qwen)
//	中等任务 (搜索/摘要)     → 中等模型 (GPT-4o-mini/Sonnet)
//	复杂任务 (代码/推理)     → 高端模型 (GPT-4o/Opus)
//
// 附加能力：
//   - 降级策略: 主模型不可用时自动切换备用模型
//   - 成本感知: 接近预算上限时降级到低成本模型
//   - 兼容接入: 支持自定义 base_url，兼容 API 中转/私有部署
package llmrouter

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/everyday-items/hexagon"
	"github.com/everyday-items/hexclaw/config"
)

// Selector LLM 智能路由选择器
//
// 管理多个 LLM Provider，根据策略选择最优 Provider 处理请求。
// 线程安全，可并发调用。
type Selector struct {
	mu        sync.RWMutex
	providers map[string]hexagon.Provider // 已初始化的 Provider
	cfg       config.LLMConfig           // LLM 配置
	defaultP  string                     // 默认 Provider 名称
}

// New 创建 LLM 路由器
//
// 根据配置初始化所有 Provider。
// 支持官方 API、API 中转、私有部署等多种接入方式。
func New(cfg config.LLMConfig) (*Selector, error) {
	r := &Selector{
		providers: make(map[string]hexagon.Provider),
		cfg:       cfg,
		defaultP:  cfg.Default,
	}

	// 初始化所有配置的 Provider
	for name, pc := range cfg.Providers {
		if pc.APIKey == "" {
			continue // 跳过无 API Key 的 Provider
		}
		provider := r.createProvider(name, pc)
		r.providers[name] = provider
		log.Printf("LLM Provider 已加载: %s (model: %s)", name, pc.Model)
	}

	if len(r.providers) == 0 {
		return nil, fmt.Errorf("没有可用的 LLM Provider，请检查 API Key 配置")
	}

	// 如果默认 Provider 不可用，选择第一个可用的
	if _, ok := r.providers[r.defaultP]; !ok {
		for name := range r.providers {
			r.defaultP = name
			break
		}
		log.Printf("默认 Provider 不可用，已切换到: %s", r.defaultP)
	}

	return r, nil
}

// createProvider 根据配置创建 Provider 实例
//
// 核心兼容策略：
//   - 所有 Provider 统一使用 hexagon.NewOpenAI 创建（OpenAI 兼容协议）
//   - 通过 hexagon.OpenAIWithBaseURL 设置自定义端点
//   - 通过 hexagon.OpenAIWithModel 设置模型名称
//
// 这意味着：DeepSeek、Qwen 等声明 OpenAI 兼容的 Provider，
// 以及任何 API 中转/私有部署，都可以通过此方式接入。
func (r *Selector) createProvider(name string, pc config.LLMProviderConfig) hexagon.Provider {
	opts := []hexagon.OpenAIOption{}
	if pc.BaseURL != "" {
		opts = append(opts, hexagon.OpenAIWithBaseURL(pc.BaseURL))
	}
	if pc.Model != "" {
		opts = append(opts, hexagon.OpenAIWithModel(pc.Model))
	}
	return hexagon.NewOpenAI(pc.APIKey, opts...)
}

// Get 获取指定名称的 Provider
func (r *Selector) Get(name string) (hexagon.Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Default 获取默认 Provider
func (r *Selector) Default() hexagon.Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[r.defaultP]
}

// DefaultName 返回默认 Provider 名称
func (r *Selector) DefaultName() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultP
}

// Route 根据策略选择最优 Provider
//
// 当前实现：返回默认 Provider。
// TODO: 实现 cost-aware / quality-first / latency-first 策略
func (r *Selector) Route(_ context.Context) (hexagon.Provider, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.providers[r.defaultP]
	if !ok {
		return nil, "", fmt.Errorf("默认 Provider %s 不可用", r.defaultP)
	}
	return p, r.defaultP, nil
}

// Fallback 降级到备用 Provider
//
// 当指定的 Provider 不可用时，返回第一个可用的其他 Provider。
func (r *Selector) Fallback(exclude string) (hexagon.Provider, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for name, p := range r.providers {
		if name != exclude {
			return p, name, nil
		}
	}
	return nil, "", fmt.Errorf("没有可用的备用 Provider")
}

// Providers 返回所有已加载的 Provider 名称
func (r *Selector) Providers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	return names
}
