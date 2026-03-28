package engine

import (
	"context"
	"fmt"
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
	usedCost   atomic.Int64 // 存储 cost * 10000 (避免浮点原子操作)
	startTime  time.Time
	started    atomic.Bool

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
	if b.started.Load() && time.Since(b.startTime) > b.maxDuration {
		return fmt.Errorf("duration budget exhausted: %v/%v", time.Since(b.startTime).Round(time.Second), b.maxDuration)
	}
	usedCostCents := b.usedCost.Load()
	maxCostCents := int64(b.maxCost * 10000)
	if usedCostCents >= maxCostCents {
		return fmt.Errorf("cost budget exhausted: $%.4f/$%.2f", float64(usedCostCents)/10000, b.maxCost)
	}
	return nil
}

// RecordTokens 记录 token 使用量
func (b *BudgetController) RecordTokens(tokens int) {
	if !b.started.Load() {
		b.started.Store(true)
		b.startTime = time.Now()
	}
	b.usedTokens.Add(int64(tokens))
}

// RecordCost 记录成本 (USD)
func (b *BudgetController) RecordCost(cost float64) {
	b.usedCost.Add(int64(cost * 10000))
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

// Release 将子预算未用部分归还父预算
func (b *BudgetController) Release(parent *BudgetController) {
	// 未用 = 分配额 - 已用
	unusedTokens := b.maxTokens - b.usedTokens.Load()
	if unusedTokens > 0 {
		parent.usedTokens.Add(-unusedTokens) // 归还
	}
	unusedCostCents := int64(b.maxCost*10000) - b.usedCost.Load()
	if unusedCostCents > 0 {
		parent.usedCost.Add(-unusedCostCents)
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
	if b.started.Load() {
		dur = b.maxDuration - time.Since(b.startTime)
		if dur < 0 {
			dur = 0
		}
	}
	costCents := int64(b.maxCost*10000) - b.usedCost.Load()
	cost := float64(costCents) / 10000
	if cost < 0 {
		cost = 0
	}
	return BudgetRemaining{Tokens: tokens, Duration: dur, Cost: cost}
}

// Summary 返回使用概况
func (b *BudgetController) Summary() string {
	return fmt.Sprintf("tokens: %d/%d, duration: %v/%v, cost: $%.4f/$%.2f",
		b.usedTokens.Load(), b.maxTokens,
		time.Since(b.startTime).Round(time.Second), b.maxDuration,
		float64(b.usedCost.Load())/10000, b.maxCost)
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
