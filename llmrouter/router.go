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
	"sort"
	"sync"

	"github.com/everyday-items/hexagon"
	"github.com/everyday-items/hexclaw/config"
)

// costPriority 成本优先策略的 Provider 优先级（数字越小越优先）
//
// 优先选择低成本 Provider：本地模型 > 国产模型 > 国际大厂
var costPriority = map[string]int{
	"ollama": 1, "deepseek": 2, "qwen": 3, "ark": 4,
	"gemini": 5, "openai": 6, "anthropic": 7,
}

// qualityPriority 质量优先策略的 Provider 优先级（数字越小越优先）
//
// 优先选择高质量 Provider：顶级模型 > 中端模型 > 本地模型
var qualityPriority = map[string]int{
	"anthropic": 1, "openai": 2, "gemini": 3,
	"deepseek": 4, "qwen": 5, "ark": 6, "ollama": 7,
}

// latencyPriority 延迟优先策略的 Provider 优先级（数字越小越优先）
//
// 优先选择低延迟 Provider：本地模型 > 轻量 API > 重量级 API
var latencyPriority = map[string]int{
	"ollama": 1, "deepseek": 2, "openai": 3,
	"gemini": 4, "qwen": 5, "ark": 6, "anthropic": 7,
}

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
// 策略说明：
//   - "default": 使用默认 Provider
//   - "cost-aware": 优先选择低成本 Provider（DeepSeek > Qwen > Ollama > OpenAI > Claude）
//   - "quality-first": 优先选择高质量 Provider（Claude > OpenAI > Gemini > DeepSeek > Qwen）
//   - "latency-first": 优先选择低延迟 Provider（Ollama > DeepSeek > OpenAI > Claude）
func (r *Selector) Route(_ context.Context) (hexagon.Provider, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 如果路由未启用或策略为空/default，直接返回默认 Provider
	strategy := r.cfg.Routing.Strategy
	if !r.cfg.Routing.Enabled || strategy == "" || strategy == "default" {
		p, ok := r.providers[r.defaultP]
		if !ok {
			return nil, "", fmt.Errorf("默认 Provider %s 不可用", r.defaultP)
		}
		return p, r.defaultP, nil
	}

	// 根据策略选择优先级映射
	var priorities map[string]int
	switch strategy {
	case "cost-aware":
		priorities = costPriority
	case "quality-first":
		priorities = qualityPriority
	case "latency-first":
		priorities = latencyPriority
	default:
		// 未知策略，回退到默认 Provider
		log.Printf("未知路由策略 %q，使用默认 Provider", strategy)
		p, ok := r.providers[r.defaultP]
		if !ok {
			return nil, "", fmt.Errorf("默认 Provider %s 不可用", r.defaultP)
		}
		return p, r.defaultP, nil
	}

	// 按优先级排序可用的 Provider
	best := r.selectByPriority(priorities)
	if best == "" {
		return nil, "", fmt.Errorf("没有可用的 Provider (策略: %s)", strategy)
	}
	return r.providers[best], best, nil
}

// selectByPriority 根据优先级映射选择最优的已注册 Provider
//
// 遍历所有已加载的 Provider，按照优先级映射中的数值排序，返回优先级最高（数值最小）的 Provider 名称。
// 未在映射中的 Provider 赋予最低优先级（999）。
func (r *Selector) selectByPriority(priorities map[string]int) string {
	type ranked struct {
		name     string
		priority int
	}

	var candidates []ranked
	for name := range r.providers {
		p, ok := priorities[name]
		if !ok {
			p = 999 // 未知 Provider 排到最后
		}
		candidates = append(candidates, ranked{name: name, priority: p})
	}

	if len(candidates) == 0 {
		return ""
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].priority < candidates[j].priority
	})

	return candidates[0].name
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
