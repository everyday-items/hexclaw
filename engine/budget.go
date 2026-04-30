package engine

import (
	"context"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// BudgetController 三维预算控制器
//
// 三个维度独立计量，任一耗尽即停止：
//   - Token: 输入+输出 token 总量
//   - Duration: 从首次工具调用开始的已用时间
//   - Cost: 估算美元成本
//
// 线程安全：使用 atomic 操作，支持并行 Sub-agent 共享预算。
// 对标 OpenClaw Budget 但更精细（三维度 vs OpenClaw 仅 token 上限）。
type BudgetController struct {
	maxTokens   int64
	maxDuration time.Duration
	maxCost     float64 // USD

	usedTokens atomic.Int64
	usedCost   atomic.Int64 // 存储 cost * 1_000_000 micro-dollars (避免浮点原子操作)
	startTime  atomic.Int64 // UnixNano，用 atomic 避免数据竞争
	startOnce  sync.Once

	cancelFunc context.CancelFunc // 级联取消子 Agent
}

// BudgetConfig 预算配置
type BudgetConfig struct {
	MaxTokens   int64         `yaml:"max_tokens"`   // 默认 500000
	MaxDuration time.Duration `yaml:"max_duration"` // 默认 30m
	MaxCost     float64       `yaml:"max_cost"`     // 默认 5.00 USD
}

// DefaultBudgetConfig 返回默认预算
func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{
		MaxTokens:   500000,
		MaxDuration: 30 * time.Minute,
		MaxCost:     5.0,
	}
}

// NewBudgetController 创建预算控制器
func NewBudgetController(cfg BudgetConfig) *BudgetController {
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = 500000
	}
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = 30 * time.Minute
	}
	if cfg.MaxCost <= 0 {
		cfg.MaxCost = 5.0
	}
	return &BudgetController{
		maxTokens:   cfg.MaxTokens,
		maxDuration: cfg.MaxDuration,
		maxCost:     cfg.MaxCost,
	}
}

// Check 检查预算是否耗尽，返回 nil 表示预算充足
func (b *BudgetController) Check() error {
	if b.usedTokens.Load() >= b.maxTokens {
		return fmt.Errorf("token budget exhausted: %d/%d", b.usedTokens.Load(), b.maxTokens)
	}
	if st := b.startTime.Load(); st > 0 && time.Since(time.Unix(0, st)) > b.maxDuration {
		return fmt.Errorf("duration budget exhausted: %v/%v", time.Since(time.Unix(0, st)).Round(time.Second), b.maxDuration)
	}
	usedCostCents := b.usedCost.Load()
	maxCostCents := int64(b.maxCost * 1_000_000)
	if usedCostCents >= maxCostCents {
		return fmt.Errorf("cost budget exhausted: $%.4f/$%.2f", float64(usedCostCents)/1_000_000, b.maxCost)
	}
	return nil
}

// RecordTokens 记录 token 使用量
func (b *BudgetController) RecordTokens(tokens int) {
	b.startOnce.Do(func() { b.startTime.Store(time.Now().UnixNano()) })
	b.usedTokens.Add(int64(tokens))
}

// RecordCost 记录成本 (USD)
//
// 内部以 micro-dollar (1/1,000,000 USD) 存储，支持 $0.000001 精度，
// 避免 Gemini Flash 等低价模型的微小成本累积丢失。
func (b *BudgetController) RecordCost(cost float64) {
	b.usedCost.Add(int64(math.Round(cost * 1_000_000)))
}

// modelPrice 每 1K token 的价格 (USD)
type modelPrice struct {
	Input  float64 // 每 1K 输入 token 价格
	Output float64 // 每 1K 输出 token 价格
}

// pricingTable 常见 provider+model 组合的定价表
//
// 价格单位: USD per 1K tokens，基于 2025 年公开定价。
// 未匹配的 model 返回零成本（不阻断流程）。
var pricingTable = map[string]map[string]modelPrice{
	"openai": {
		"gpt-4o":      {Input: 0.0025, Output: 0.01},
		"gpt-4o-mini": {Input: 0.00015, Output: 0.0006},
		"gpt-4.1":     {Input: 0.002, Output: 0.008},
		"o3-mini":     {Input: 0.0011, Output: 0.0044},
	},
	"anthropic": {
		"claude-sonnet-4-6": {Input: 0.003, Output: 0.015},
		"claude-haiku-4-5":  {Input: 0.0008, Output: 0.004},
		"claude-opus-4-6":   {Input: 0.015, Output: 0.075},
	},
	"google": {
		"gemini-2.0-flash": {Input: 0.0001, Output: 0.0004},
		"gemini-2.5-pro":   {Input: 0.00125, Output: 0.01},
	},
	// Fix 8: router uses "gemini" as provider name, not "google"
	"gemini": {
		"gemini-2.0-flash": {Input: 0.0001, Output: 0.0004},
		"gemini-2.5-pro":   {Input: 0.00125, Output: 0.01},
	},
	"deepseek": {
		"deepseek-chat":     {Input: 0.00014, Output: 0.00028},
		"deepseek-reasoner": {Input: 0.00055, Output: 0.0022},
	},
}

// EstimateCost 根据定价表估算本次调用成本 (USD)
//
// 未匹配的 provider/model 返回 0（不阻断流程，只是不计费）。
//
// v0.4.0 F8 接入：若 SetGlobalPricer 已注入了 ChainPricer，本函数优先用它
// （UserOverride > Cache > Remote > BuiltinFallback）。flag pricing.layered.v1
// 关闭时全局 pricer 也是 nil → 退化到老 pricingTable，行为与 v0.3 完全一致。
func EstimateCost(provider, model string, inputTokens, outputTokens int) float64 {
	if cp := getGlobalPricer(); cp != nil {
		return cp.EstimateCost(provider, model, inputTokens, outputTokens)
	}
	models, ok := pricingTable[provider]
	if !ok {
		return 0
	}
	price, ok := models[model]
	if !ok {
		return 0
	}
	return float64(inputTokens)/1000*price.Input + float64(outputTokens)/1000*price.Output
}

// globalPricer 是 SetGlobalPricer 注入的 ChainPricer（可空）。
// 注：使用 atomic.Pointer 而非 sync.RWMutex，因为读路径在 react.go 1477 等热点上，
// atomic 比 mutex 快 1-2 个数量级。
var globalPricer atomic.Pointer[ChainPricer]

// SetGlobalPricer 注入全局 ChainPricer。在 cmd/hexclaw 启动时调一次。
// 传 nil 会清除注入，回退到老 pricingTable 路径。
func SetGlobalPricer(p *ChainPricer) {
	globalPricer.Store(p)
}

// getGlobalPricer 取出当前全局 pricer（可空）。
func getGlobalPricer() *ChainPricer {
	return globalPricer.Load()
}

// Allocate 从当前预算预分配一份子预算
//
// 用于 OrchestrateSkill 给子 Agent 分配预算。
// fraction: 0-1 之间的比例 (如 0.33 表示 1/3)
func (b *BudgetController) Allocate(fraction float64) *BudgetController {
	remaining := b.Remaining()
	child := &BudgetController{
		maxTokens:   int64(float64(remaining.Tokens) * fraction),
		maxDuration: time.Duration(float64(remaining.Duration) * fraction),
		maxCost:     remaining.Cost * fraction,
	}
	return child
}

// Release 将子预算未用部分归还父预算。
//
// Fix: cap the subtraction so parent counters never go below 0.
// Without pre-deduction in Allocate(), blindly subtracting the unused
// portion would drive parent.usedTokens / usedCost negative.
func (b *BudgetController) Release(parent *BudgetController) {
	// 未用 = 分配额 - 已用
	unusedTokens := b.maxTokens - b.usedTokens.Load()
	if unusedTokens > 0 {
		// Only subtract down to 0, never below
		current := parent.usedTokens.Load()
		if unusedTokens > current {
			unusedTokens = current
		}
		if unusedTokens > 0 {
			parent.usedTokens.Add(-unusedTokens)
		}
	}
	unusedCostCents := int64(b.maxCost*1_000_000) - b.usedCost.Load()
	if unusedCostCents > 0 {
		currentCost := parent.usedCost.Load()
		if unusedCostCents > currentCost {
			unusedCostCents = currentCost
		}
		if unusedCostCents > 0 {
			parent.usedCost.Add(-unusedCostCents)
		}
	}
}

// BudgetRemaining 剩余预算
type BudgetRemaining struct {
	Tokens   int64
	Duration time.Duration
	Cost     float64
}

// Remaining 返回剩余预算
func (b *BudgetController) Remaining() BudgetRemaining {
	tokens := b.maxTokens - b.usedTokens.Load()
	if tokens < 0 {
		tokens = 0
	}
	dur := b.maxDuration
	if st := b.startTime.Load(); st > 0 {
		dur = b.maxDuration - time.Since(time.Unix(0, st))
		if dur < 0 {
			dur = 0
		}
	}
	costCents := int64(b.maxCost*1_000_000) - b.usedCost.Load()
	cost := float64(costCents) / 1_000_000
	if cost < 0 {
		cost = 0
	}
	return BudgetRemaining{Tokens: tokens, Duration: dur, Cost: cost}
}

// Summary 返回使用概况
func (b *BudgetController) Summary() string {
	return fmt.Sprintf("tokens: %d/%d, duration: %v/%v, cost: $%.4f/$%.2f",
		b.usedTokens.Load(), b.maxTokens,
		time.Since(time.Unix(0, b.startTime.Load())).Round(time.Second), b.maxDuration,
		float64(b.usedCost.Load())/1_000_000, b.maxCost)
}

// BudgetStatus 预算状态快照
type BudgetStatus struct {
	TokensUsed     int64   `json:"tokens_used"`
	TokensMax      int64   `json:"tokens_max"`
	TokensRemain   int64   `json:"tokens_remaining"`
	CostUsed       float64 `json:"cost_used"`
	CostMax        float64 `json:"cost_max"`
	CostRemain     float64 `json:"cost_remaining"`
	DurationUsed   string  `json:"duration_used"`
	DurationMax    string  `json:"duration_max"`
	DurationRemain string  `json:"duration_remaining"`
	Exhausted      bool    `json:"exhausted"`
}

// Status 返回结构化的预算状态快照
func (b *BudgetController) Status() BudgetStatus {
	rem := b.Remaining()
	usedTokens := b.usedTokens.Load()
	usedCost := float64(b.usedCost.Load()) / 1_000_000

	var durUsed time.Duration
	if st := b.startTime.Load(); st > 0 {
		durUsed = time.Since(time.Unix(0, st))
	}

	return BudgetStatus{
		TokensUsed:     usedTokens,
		TokensMax:      b.maxTokens,
		TokensRemain:   rem.Tokens,
		CostUsed:       usedCost,
		CostMax:        b.maxCost,
		CostRemain:     rem.Cost,
		DurationUsed:   durUsed.Round(time.Second).String(),
		DurationMax:    b.maxDuration.String(),
		DurationRemain: rem.Duration.Round(time.Second).String(),
		Exhausted:      b.Check() != nil,
	}
}

// SetCancelFunc 设置级联取消函数 (用于 Orchestrate 子 Agent 取消)
func (b *BudgetController) SetCancelFunc(cancel context.CancelFunc) {
	b.cancelFunc = cancel
}

// Cancel 取消关联的上下文
func (b *BudgetController) Cancel() {
	if b.cancelFunc != nil {
		b.cancelFunc()
	}
}
