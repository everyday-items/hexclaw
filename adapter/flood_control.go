package adapter

import (
	"sync"
	"time"
)

// StreamEditLimiter 流式消息编辑限流器
//
// 用于 IM 适配器（飞书 / 钉钉 / Telegram / Discord 等）在流式输出阶段
// 连续编辑同一条消息时，避免触发平台限流（429）或超配额风控。
//
// 限流策略（三级保护）：
//  1. 字符阈值：累积字符 < minChars 时不编辑（减少 API 调用）
//  2. 时间间隔：距上次编辑 < currentDelay 时不编辑
//  3. 失败降级：连续 3 次失败 → currentDelay × 2（封顶 maxInterval）
//
// 成功后 fail 计数清零，currentDelay 也重置回 minInterval。
//
// 为什么要抽公共组件：历史上各适配器自己实现限流（飞书用 50 字符/1s 硬编码），
// 策略零散、无自适应、无失败降级。本组件统一这些模式。
type StreamEditLimiter struct {
	minChars    int           // 最少累积字符（默认 40）
	minInterval time.Duration // 正常情况的最小间隔（默认 1s）
	maxInterval time.Duration // 降级后的最大间隔（默认 5s）

	mu           sync.Mutex
	lastLen      int
	lastEmit     time.Time
	consecFail   int           // 连续失败次数
	currentDelay time.Duration // 当前生效的延迟
}

// NewStreamEditLimiter 用默认策略构造限流器：40 字符 / 1s / 最大 5s。
func NewStreamEditLimiter() *StreamEditLimiter {
	return NewStreamEditLimiterWith(40, time.Second, 5*time.Second)
}

// NewStreamEditLimiterWith 自定义参数构造。
func NewStreamEditLimiterWith(minChars int, minInterval, maxInterval time.Duration) *StreamEditLimiter {
	if minChars <= 0 {
		minChars = 40
	}
	if minInterval <= 0 {
		minInterval = time.Second
	}
	if maxInterval < minInterval {
		maxInterval = minInterval * 5
	}
	return &StreamEditLimiter{
		minChars:     minChars,
		minInterval:  minInterval,
		maxInterval:  maxInterval,
		currentDelay: minInterval,
	}
}

// ShouldEmit 检查当前状态是否允许 emit 一次编辑。
// currentLen 是累积输出的当前总字符数，应当单调递增（流式写入语义）。
// 若调用方传入回退值（currentLen < lastLen），一律拒绝 emit 以避免死锁/异常行为。
func (l *StreamEditLimiter) ShouldEmit(currentLen int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// 防御：回退或负长度 → 拒绝 emit（调用方契约违反）
	if currentLen < 0 || currentLen < l.lastLen {
		return false
	}
	if currentLen-l.lastLen < l.minChars {
		return false
	}
	// 首次 emit：lastEmit.IsZero 为 true，立即允许
	if l.lastEmit.IsZero() {
		return true
	}
	return time.Since(l.lastEmit) >= l.currentDelay
}

// Emitted 记录一次成功的 emit：清零连续失败计数、恢复 minInterval。
// currentLen 若小于 lastLen 则忽略（防止回退污染状态）。
func (l *StreamEditLimiter) Emitted(currentLen int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if currentLen < l.lastLen {
		return // 契约违反：忽略
	}
	l.lastLen = currentLen
	l.lastEmit = time.Now()
	l.consecFail = 0
	l.currentDelay = l.minInterval
}

// Failed 记录一次失败。连续 3 次失败 → currentDelay 翻倍（封顶 maxInterval）。
// 返回此次失败后是否已进入降级状态。
func (l *StreamEditLimiter) Failed() (degraded bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.consecFail++
	if l.consecFail >= 3 {
		l.currentDelay *= 2
		if l.currentDelay > l.maxInterval {
			l.currentDelay = l.maxInterval
		}
		l.consecFail = 0 // 重置计数，等待下一轮 3 连败才再次降级
		return true
	}
	return false
}

// CurrentDelay 返回当前生效的延迟（用于测试和日志）
func (l *StreamEditLimiter) CurrentDelay() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.currentDelay
}

// IsDegraded 是否已降级到 maxInterval（监控用）
func (l *StreamEditLimiter) IsDegraded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.currentDelay >= l.maxInterval
}
