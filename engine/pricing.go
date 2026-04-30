// pricing.go 实现 v0.4.0 F8 模型定价 4 层架构（feature-flag gated）。
//
// 老路径 EstimateCost 直接读硬编码 pricingTable —— 表过期 / 新模型上市要发版。
// F8 引入分层 Pricer：
//
//   层 1 UserOverride  : settings 中显式覆盖（最高优先；让用户对私有部署 / 折扣定价生效）
//   层 2 Cache         : 远端拉到的定价缓存（带 TTL，避免每次请求都查网络）
//   层 3 Remote        : 实际网络拉取（HTTP / API；接口预留，调用方注入实现）
//   层 4 BuiltinFallback: 编译期硬编码 pricingTable（兜底，避免无定价时退化为 0）
//
// 每层都返回 (price, ok)；ChainPricer 按顺序查询，第一个命中即返回。
//
// 设计取舍：
//   - 不强制接入 EstimateCost 老 API（在热路径上，改造风险高）；F8 提供新 API 让
//     调用方主动选择。挂 flag pricing.layered.v1，flag OFF 时新 Pricer 不被构造。
//   - Remote 层只定义接口 + 默认 NopRemote 实现，避免在本 phase 引入 HTTP 请求；
//     真实 Remote 实现（拉 openrouter / models.dev）留给下一个 phase。
package engine

import (
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagPricingLayeredV1 控制 F8 ChainPricer 是否启用。
const FlagPricingLayeredV1 = "pricing.layered.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagPricingLayeredV1,
		Default:      true, // alpha 强制 OFF
		Description:  "Use layered Pricer (UserOverride > Cache > Remote > BuiltinFallback) for model cost estimation.",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// ModelPrice 是定价表的 1K-token 单价。
type ModelPrice struct {
	Input  float64 // 每 1K 输入 token 价格 (USD)
	Output float64 // 每 1K 输出 token 价格 (USD)
}

// Zero 表示价格未定义（避免和真实零价混淆 —— 真零价 Provider 应该用 ok=true 返回 0）。
var ZeroPrice = ModelPrice{}

// Pricer 是单层定价查询接口。Lookup 返回 ok=false 时 ChainPricer 会继续问下一层。
type Pricer interface {
	Lookup(provider, model string) (ModelPrice, bool)
}

// ChainPricer 把多个 Pricer 按优先级顺序组合：第一个 ok=true 即返回。
type ChainPricer struct {
	layers []Pricer
}

// NewChainPricer 构造 ChainPricer。layers 顺序即查询顺序（前置高优先）。
func NewChainPricer(layers ...Pricer) *ChainPricer {
	out := &ChainPricer{layers: make([]Pricer, 0, len(layers))}
	for _, l := range layers {
		if l != nil {
			out.layers = append(out.layers, l)
		}
	}
	return out
}

// Lookup 按层查询。所有层都 miss 时返回 (ZeroPrice, false)。
func (c *ChainPricer) Lookup(provider, model string) (ModelPrice, bool) {
	if c == nil {
		return ZeroPrice, false
	}
	for _, l := range c.layers {
		if p, ok := l.Lookup(provider, model); ok {
			return p, true
		}
	}
	return ZeroPrice, false
}

// EstimateCost 用 ChainPricer 计算成本（USD）。未命中返回 0。
func (c *ChainPricer) EstimateCost(provider, model string, inputTokens, outputTokens int) float64 {
	p, ok := c.Lookup(provider, model)
	if !ok {
		return 0
	}
	return float64(inputTokens)/1000*p.Input + float64(outputTokens)/1000*p.Output
}

// ============== 层 1：UserOverride ==============

// UserOverridePricer 是用户在 settings 中声明的覆盖。线程安全。
type UserOverridePricer struct {
	mu       sync.RWMutex
	overrides map[string]map[string]ModelPrice // provider → model → price
}

// NewUserOverridePricer 用初始 overrides 构造。
func NewUserOverridePricer(overrides map[string]map[string]ModelPrice) *UserOverridePricer {
	cp := make(map[string]map[string]ModelPrice, len(overrides))
	for prov, models := range overrides {
		inner := make(map[string]ModelPrice, len(models))
		for m, p := range models {
			inner[m] = p
		}
		cp[prov] = inner
	}
	return &UserOverridePricer{overrides: cp}
}

// Set 添加 / 更新单条覆盖。
func (u *UserOverridePricer) Set(provider, model string, price ModelPrice) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.overrides == nil {
		u.overrides = map[string]map[string]ModelPrice{}
	}
	if u.overrides[provider] == nil {
		u.overrides[provider] = map[string]ModelPrice{}
	}
	u.overrides[provider][model] = price
}

// Lookup 读用户覆盖。
func (u *UserOverridePricer) Lookup(provider, model string) (ModelPrice, bool) {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if models, ok := u.overrides[provider]; ok {
		if p, ok := models[model]; ok {
			return p, true
		}
	}
	return ZeroPrice, false
}

// ============== 层 2：Cache ==============

// CachePricer 是带 TTL 的内存缓存。线程安全。
type CachePricer struct {
	mu  sync.RWMutex
	ttl time.Duration
	now func() time.Time

	entries map[string]map[string]priceCacheEntry
}

type priceCacheEntry struct {
	price   ModelPrice
	storedAt time.Time
}

// NewCachePricer 用 ttl 构造。ttl <= 0 时使用 1h 默认。
func NewCachePricer(ttl time.Duration) *CachePricer {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &CachePricer{
		ttl:     ttl,
		now:     time.Now,
		entries: map[string]map[string]priceCacheEntry{},
	}
}

// Put 把定价写入缓存（用于 Remote 层拉到结果后回填）。
func (c *CachePricer) Put(provider, model string, price ModelPrice) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[provider] == nil {
		c.entries[provider] = map[string]priceCacheEntry{}
	}
	c.entries[provider][model] = priceCacheEntry{price: price, storedAt: c.now()}
}

// Lookup 读缓存（过期返回 ok=false 让链询问下一层）。
func (c *CachePricer) Lookup(provider, model string) (ModelPrice, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if models, ok := c.entries[provider]; ok {
		if e, ok := models[model]; ok {
			if c.now().Sub(e.storedAt) <= c.ttl {
				return e.price, true
			}
		}
	}
	return ZeroPrice, false
}

// ============== 层 3：Remote ==============

// RemoteFetcher 是远端定价拉取的抽象。命中后会被 Cache 层回填。
//
// 默认实现 NopRemote 永远返回 ok=false；真实接入（如 openrouter API）由
// 调用方注入。Remote 层在 Lookup 时会自动把命中结果 Put 到关联的 Cache。
type RemoteFetcher interface {
	Fetch(provider, model string) (ModelPrice, bool)
}

// NopRemote 永远 miss 的 RemoteFetcher。
type NopRemote struct{}

// Lookup 永远返回 (ZeroPrice, false)。
func (NopRemote) Fetch(_, _ string) (ModelPrice, bool) { return ZeroPrice, false }

// RemotePricer 把 Fetcher 适配为 Pricer，并在命中后回写 Cache。
type RemotePricer struct {
	fetcher RemoteFetcher
	cache   *CachePricer // 可空：no cache 时 fetch 命中也不回写
}

// NewRemotePricer 构造 RemotePricer。fetcher nil 时退化为 NopRemote。
func NewRemotePricer(fetcher RemoteFetcher, cache *CachePricer) *RemotePricer {
	if fetcher == nil {
		fetcher = NopRemote{}
	}
	return &RemotePricer{fetcher: fetcher, cache: cache}
}

// Lookup 调 fetcher，命中后回写 cache。
func (r *RemotePricer) Lookup(provider, model string) (ModelPrice, bool) {
	if r == nil || r.fetcher == nil {
		return ZeroPrice, false
	}
	p, ok := r.fetcher.Fetch(provider, model)
	if !ok {
		return ZeroPrice, false
	}
	if r.cache != nil {
		r.cache.Put(provider, model, p)
	}
	return p, true
}

// ============== 层 4：BuiltinFallback ==============

// BuiltinFallbackPricer 用编译期硬编码的 pricingTable（在 budget.go 中维护）。
// 这是最后兜底，确保即使所有上游 miss 也能返回常用模型的近似价格。
type BuiltinFallbackPricer struct{}

// NewBuiltinFallbackPricer 构造。
func NewBuiltinFallbackPricer() *BuiltinFallbackPricer { return &BuiltinFallbackPricer{} }

// Lookup 转调老 pricingTable。
func (BuiltinFallbackPricer) Lookup(provider, model string) (ModelPrice, bool) {
	models, ok := pricingTable[provider]
	if !ok {
		return ZeroPrice, false
	}
	mp, ok := models[model]
	if !ok {
		return ZeroPrice, false
	}
	return ModelPrice(mp), true
}

// ============== Default chain 工厂 ==============

// NewDefaultPricer 是 v0.4.0 F8 推荐的 4 层默认配置。userOverride 可空。
//
// 顺序：UserOverride → Cache → Remote → BuiltinFallback。Cache 与 Remote
// 之间共享同一 *CachePricer 实例，让 Remote 命中后自动回填。
func NewDefaultPricer(userOverride *UserOverridePricer, remote RemoteFetcher, cacheTTL time.Duration) *ChainPricer {
	cache := NewCachePricer(cacheTTL)
	return NewChainPricer(
		userOverride,
		cache,
		NewRemotePricer(remote, cache),
		NewBuiltinFallbackPricer(),
	)
}
