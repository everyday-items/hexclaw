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
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hexagon-codes/ai-core/llm/anthropic"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/util/logger"
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
	mu             sync.RWMutex
	providers      map[string]hexagon.Provider // 已初始化的 Provider
	cfg            config.LLMConfig            // 当前活跃的 LLM 配置（仅包含已加载 Provider）
	defaultP       string                      // 默认 Provider 名称
	unhealthyUntil map[string]time.Time        // 运行时短期熔断，避免刚失败的 provider 立即被再次选中
	now            func() time.Time
}

const defaultProviderCooldown = 2 * time.Minute

// New 创建 LLM 路由器
//
// 根据配置初始化所有 Provider。
// 支持官方 API、API 中转、私有部署等多种接入方式。
//
// 启动校验：cfg.Default 必须在 providers map 中，否则自动选第一个可用 provider
// 并 log warn —— 避免配置漂移（如 default: apimart 但 providers 里没这个 key）
// 导致 router.Default() 返 nil 触发隐式 fallback 到首个 provider（参考本次
// "default→Ollama" 错配踩坑教训）。
func New(cfg config.LLMConfig) (*Selector, error) {
	providers, activeCfg, defaultP := buildSelectorState(cfg)
	if len(providers) == 0 {
		return nil, fmt.Errorf("没有可用的 LLM Provider，请检查 API Key 配置")
	}
	// 校验 default 名字
	if _, ok := providers[defaultP]; !ok {
		picked := pickFallbackDefault(providers)
		logger.Warn("[llmrouter] 配置 default 漂移：在 providers 中找不到，自动改用 fallback",
			"configured_default", defaultP, "fallback", picked,
			"available", providerNames(providers))
		defaultP = picked
	}
	return &Selector{
		providers:      providers,
		cfg:            activeCfg,
		defaultP:       defaultP,
		unhealthyUntil: make(map[string]time.Time),
		now:            time.Now,
	}, nil
}

// pickFallbackDefault 当配置 default 缺失时挑一个合理的 fallback。
// 偏好远端 provider（避免桌面端 Ollama 在没安装时尝试连）。
func pickFallbackDefault(providers map[string]hexagon.Provider) string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic: pick the smallest qualifying name, not a random one
	// 第一遍：偏好远端 (非 ollama 名)
	for _, name := range names {
		if !strings.Contains(strings.ToLower(name), "ollama") {
			return name
		}
	}
	// 第二遍：只剩 ollama 兜底
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

func providerNames(m map[string]hexagon.Provider) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// NewWithProviders 使用显式注入的 Provider 创建路由器。
//
// 主要用于测试和自定义 Provider 装配，避免依赖真实网络 Provider。
func NewWithProviders(cfg config.LLMConfig, providers map[string]hexagon.Provider) *Selector {
	r := &Selector{
		providers:      make(map[string]hexagon.Provider, len(providers)),
		cfg:            cloneLLMConfig(cfg),
		defaultP:       cfg.Default,
		unhealthyUntil: make(map[string]time.Time),
		now:            time.Now,
	}
	for name, provider := range providers {
		r.providers[name] = provider
	}
	if _, ok := r.providers[r.defaultP]; !ok {
		r.defaultP = pickFallbackDefault(r.providers)
	}
	return r
}

// localHostMarkers identify a base URL pointing at the local machine or a
// nearby private deployment (Ollama etc.). Such providers don't need an API
// key and must rank LAST in fallback (BUG-20260612), so the matcher covers
// loopback variants and common local/container hostnames, not just localhost.
var localHostMarkers = []string{
	"localhost", "127.0.0.1", "::1", "0.0.0.0",
	"host.docker.internal", "host.containers.internal",
	"//ollama", ".local",
}

// isLocalProviderName reports whether a provider should rank last in fallback.
// It honors the configured base URL and, for providers assembled without a
// matching cfg entry (NewWithProviders), the name — keeping the local/remote
// classification consistent with pickFallbackDefault's ollama heuristic so an
// injected local provider can't win over a real remote one.
func (r *Selector) isLocalProviderName(name string) bool {
	// canonical 解析：与其它按名访问器一致，容忍 Title Case 名（AP-144 举一反三）——
	// 否则本地 provider 用大写名查会漏 base_url 判定、误分类为云端（影响 AP-098）。
	key := name
	if canon, ok := r.canonicalNameLocked(name); ok {
		key = canon
	}
	if isLocalProvider(r.cfg.Providers[key]) {
		return true
	}
	return strings.Contains(strings.ToLower(name), "ollama")
}

// IsLocalProviderName 报告指定 provider 是否本地部署（如 Ollama，base_url 指向 localhost 或名含 ollama）。
// 供上层据本地/云端差异调整策略（如 AP-098：本地慢/reasoning 模型放宽后台抽取超时）。
func (r *Selector) IsLocalProviderName(name string) bool {
	return r.isLocalProviderName(name)
}

// isLocalProvider 检查 provider 是否为本地部署（如 Ollama），本地 provider 不需要 API Key
func isLocalProvider(pc config.LLMProviderConfig) bool {
	u := strings.ToLower(pc.BaseURL)
	for _, m := range localHostMarkers {
		if strings.Contains(u, m) {
			return true
		}
	}
	return false
}

func buildSelectorState(cfg config.LLMConfig) (map[string]hexagon.Provider, config.LLMConfig, string) {
	providers := make(map[string]hexagon.Provider)
	activeCfg := cloneLLMConfig(cfg)
	activeCfg.Providers = make(map[string]config.LLMProviderConfig)

	providerNames := make([]string, 0, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		if pc.Enabled != nil && !*pc.Enabled {
			logger.Info("[router] 跳过已禁用 provider（配置/Key 保留，不参与路由）", "provider", name)
			continue
		}
		if strings.TrimSpace(pc.APIKey) == "" && !isLocalProvider(pc) {
			logger.Warn("[router] 跳过无 API Key 的远程 provider", "provider", name, "base_url", pc.BaseURL)
			continue
		}
		logger.Info("[router] 加载 provider", "provider", name, "base_url", pc.BaseURL, "local", isLocalProvider(pc))
		providerNames = append(providerNames, name)
		activeCfg.Providers[name] = pc
	}
	sort.Strings(providerNames)

	for _, name := range providerNames {
		providers[name] = (&Selector{}).createProvider(name, cfg.Providers[name])
	}

	defaultP := strings.TrimSpace(cfg.Default)
	if _, ok := providers[defaultP]; !ok {
		if len(providerNames) > 0 {
			defaultP = providerNames[0]
			logger.Info("默认 Provider 不可用，已切换到", "provider", defaultP)
		} else {
			defaultP = ""
		}
	}
	activeCfg.Default = defaultP
	return providers, activeCfg, defaultP
}

func cloneLLMConfig(cfg config.LLMConfig) config.LLMConfig {
	cloned := cfg
	cloned.Providers = make(map[string]config.LLMProviderConfig, len(cfg.Providers))
	for name, provider := range cfg.Providers {
		cloned.Providers[name] = provider
	}
	return cloned
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
	// Anthropic 使用原生 SDK（非 OpenAI 兼容协议）
	if name == "anthropic" {
		var aopts []anthropic.Option
		if pc.BaseURL != "" {
			aopts = append(aopts, anthropic.WithBaseURL(pc.BaseURL))
		}
		if pc.Model != "" {
			aopts = append(aopts, anthropic.WithModel(pc.Model))
		}
		return anthropic.New(pc.APIKey, aopts...)
	}

	// 其他 Provider 统一使用 OpenAI 兼容协议
	opts := []hexagon.OpenAIOption{}
	if pc.BaseURL != "" {
		opts = append(opts, hexagon.OpenAIWithBaseURL(pc.BaseURL))
	}
	if pc.Model != "" {
		opts = append(opts, hexagon.OpenAIWithModel(pc.Model))
	}
	return hexagon.NewOpenAI(pc.APIKey, opts...)
}

// canonicalNameLocked 把外部传入的 provider 名解析为注册时的配置 key。
// 先精确匹配，miss 后大小写不敏感兜底（BUG-20260703/AP-144：前端展示层与
// 持久化的 Agent 配置可能存 Title Case 名，运行期按名查表不能因大小写错位
// 报「provider 不存在」或让模型偏好/健康标记静默失效）。
// 调用方必须已持有 r.mu。
func (r *Selector) canonicalNameLocked(name string) (string, bool) {
	if _, ok := r.providers[name]; ok {
		return name, true
	}
	if _, ok := r.cfg.Providers[name]; ok {
		return name, true
	}
	trimmed := strings.TrimSpace(name)
	for key := range r.providers {
		if strings.EqualFold(strings.TrimSpace(key), trimmed) {
			return key, true
		}
	}
	for key := range r.cfg.Providers {
		if strings.EqualFold(strings.TrimSpace(key), trimmed) {
			return key, true
		}
	}
	return name, false
}

// Get 获取指定名称的 Provider
func (r *Selector) Get(name string) (hexagon.Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, ok := r.canonicalNameLocked(name)
	if !ok {
		return nil, false
	}
	p, ok := r.providers[key]
	return p, ok
}

// Default 获取默认 Provider
//
// New() 在无可用 Provider 时返回 nil *Selector（"无 LLM" 是合法状态，调用方多处以
// `router != nil` 守卫）。读访问器对 nil 接收者按空 selector 处理，避免漏写守卫的
// 调用点（如 cmd/hexclaw 启动期）在无 Provider 时 SIGSEGV。
func (r *Selector) Default() hexagon.Provider {
	if r == nil {
		return nil
	}
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

	if len(r.providers) == 0 {
		return nil, "", fmt.Errorf("没有可用的 LLM Provider")
	}

	// 如果路由未启用或策略为空/default，直接返回默认 Provider
	strategy := r.cfg.Routing.Strategy
	if !r.cfg.Routing.Enabled || strategy == "" || strategy == "default" {
		p, ok := r.providers[r.defaultP]
		if !ok {
			return nil, "", fmt.Errorf("默认 Provider %s 不可用", r.defaultP)
		}
		if !r.isProviderHealthyLocked(r.defaultP) {
			fallback := r.fallbackNameLocked([]string{r.defaultP})
			if fallback == "" {
				return nil, "", fmt.Errorf("默认 Provider %s 暂不可用且无可用备用 Provider", r.defaultP)
			}
			slog.Warn("[router] default provider unhealthy, route fallback selected",
				"source", "llm", "default", r.defaultP, "selected", fallback)
			return r.providers[fallback], fallback, nil
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
		logger.Info("未知路由策略", "strategy", strategy)
		p, ok := r.providers[r.defaultP]
		if !ok {
			return nil, "", fmt.Errorf("默认 Provider %s 不可用", r.defaultP)
		}
		if !r.isProviderHealthyLocked(r.defaultP) {
			fallback := r.fallbackNameLocked([]string{r.defaultP})
			if fallback == "" {
				return nil, "", fmt.Errorf("默认 Provider %s 暂不可用且无可用备用 Provider", r.defaultP)
			}
			return r.providers[fallback], fallback, nil
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

// RouteModel 根据当前路由策略选择 Provider，并返回该 Provider 当前配置的模型名。
//
// Route 的第二返回值是 provider 名称，不是模型名；需要传给 llm.CompletionRequest.Model
// 的调用点应使用本方法，避免把用户可见 provider 名（如“硅基流动”）误当模型名。
func (r *Selector) RouteModel(ctx context.Context) (hexagon.Provider, string, error) {
	p, providerName, err := r.Route(ctx)
	if err != nil {
		return nil, "", err
	}
	model := strings.TrimSpace(r.ProviderModel(providerName))
	if model == "" {
		return nil, "", fmt.Errorf("provider %s 未配置 model", providerName)
	}
	return p, model, nil
}

// Reload 使用新配置重建 Provider 集合并立即生效。
func (r *Selector) Reload(cfg config.LLMConfig) error {
	providers, activeCfg, defaultP := buildSelectorState(cfg)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers = providers
	r.cfg = activeCfg
	r.defaultP = defaultP
	r.unhealthyUntil = make(map[string]time.Time)
	if r.now == nil {
		r.now = time.Now
	}
	return nil
}

// ActiveConfig 返回当前已加载 Provider 的活跃配置快照。
func (r *Selector) ActiveConfig() config.LLMConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return cloneLLMConfig(r.cfg)
}

// ProviderModel 返回当前活跃配置中某个 Provider 的默认模型。
func (r *Selector) ProviderModel(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, ok := r.canonicalNameLocked(name)
	if !ok {
		return ""
	}
	if provider, ok := r.cfg.Providers[key]; ok {
		return provider.Model
	}
	return ""
}

// ProviderConfig 返回指定 Provider 的配置（BaseURL、APIKey 等）。
func (r *Selector) ProviderConfig(name string) (config.LLMProviderConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	key, ok := r.canonicalNameLocked(name)
	if !ok {
		return config.LLMProviderConfig{}, false
	}
	pc, ok := r.cfg.Providers[key]
	return pc, ok
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
		if !r.isProviderHealthyLocked(name) {
			continue
		}
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
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority < candidates[j].priority
		}
		return candidates[i].name < candidates[j].name // stable tie-break
	})

	return candidates[0].name
}

// Fallback 降级到备用 Provider
//
// 当指定的 Provider 不可用时，返回第一个可用的其他 Provider。
// 支持排除多个 Provider（用于级联降级场景）。
func (r *Selector) Fallback(exclude ...string) (hexagon.Provider, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	name := r.fallbackNameLocked(exclude)
	if name != "" {
		slog.Warn("[router] provider fallback selected",
			"source", "llm", "excluded", strings.Join(exclude, ","),
			"selected", name, "is_local", r.isLocalProviderName(name))
		return r.providers[name], name, nil
	}
	return nil, "", fmt.Errorf("没有可用的备用 Provider")
}

func (r *Selector) fallbackNameLocked(exclude []string) string {
	excludeSet := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		excludeSet[e] = true
	}

	// Deterministic selection, remote providers first: local deployments
	// (Ollama etc.) structurally cannot serve full engine prompts before
	// ResponseHeaderTimeout, so they are last-resort only (BUG-20260612 —
	// plain alphabetical order made the local provider win every fallback).
	var remote, local []string
	for name := range r.providers {
		if excludeSet[name] || !r.isProviderHealthyLocked(name) {
			continue
		}
		if r.isLocalProviderName(name) {
			local = append(local, name)
		} else {
			remote = append(remote, name)
		}
	}
	sort.Strings(remote)
	sort.Strings(local)
	names := append(remote, local...)
	if len(names) > 0 {
		return names[0]
	}
	return ""
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

// MarkProviderUnhealthy 将 provider 临时标记为不可路由，用于 429/服务不可用/模型下线等
// 短期故障后的下一轮 Route/Fallback 避让。ttl<=0 时使用保守默认值。
func (r *Selector) MarkProviderUnhealthy(name, reason string, ttl time.Duration) {
	if r == nil || strings.TrimSpace(name) == "" {
		return
	}
	if ttl <= 0 {
		ttl = defaultProviderCooldown
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.unhealthyUntil == nil {
		r.unhealthyUntil = make(map[string]time.Time)
	}
	if r.now == nil {
		r.now = time.Now
	}
	// 健康表以注册 key 为准：engine 侧回传的 ProviderName 可能是 Title Case
	// 展示名，不归一会让熔断标记永远打不到路由表上（BUG-20260703）。
	if key, ok := r.canonicalNameLocked(name); ok {
		name = key
	}
	until := r.now().Add(ttl)
	if cur, ok := r.unhealthyUntil[name]; !ok || until.After(cur) {
		r.unhealthyUntil[name] = until
	}
	slog.Warn("[router] provider marked unhealthy", "source", "llm", "provider", name, "reason", reason, "until", until.Format(time.RFC3339))
}

func (r *Selector) isProviderHealthyLocked(name string) bool {
	if r == nil || len(r.unhealthyUntil) == 0 {
		return true
	}
	until, ok := r.unhealthyUntil[name]
	if !ok {
		return true
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	if now().Before(until) {
		return false
	}
	return true
}
