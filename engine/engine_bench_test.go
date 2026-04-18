package engine

import (
	"fmt"
	"testing"
	"time"
)

// ===== BudgetController =====

// BenchmarkBudget_Check 衡量预算检查单次成本（三个维度的 atomic.Load）
func BenchmarkBudget_Check(b *testing.B) {
	bc := NewBudgetController(BudgetConfig{
		MaxTokens:   1_000_000,
		MaxDuration: time.Hour,
		MaxCost:     100,
	})
	bc.RecordTokens(1000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = bc.Check()
	}
}

// BenchmarkBudget_RecordTokens 衡量 RecordTokens 单次调用成本
// 第一次会触发 sync.Once 初始化 startTime，其余为纯 atomic.Add。
func BenchmarkBudget_RecordTokens(b *testing.B) {
	bc := NewBudgetController(BudgetConfig{
		MaxTokens:   1 << 62,
		MaxDuration: time.Hour,
		MaxCost:     1e9,
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bc.RecordTokens(10)
	}
}

// BenchmarkBudget_EstimateCost 衡量定价表查找 + 成本估算
func BenchmarkBudget_EstimateCost(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EstimateCost("anthropic", "claude-sonnet-4-6", 1000, 500)
	}
}

// ===== ToolCache =====

// BenchmarkToolCache_GetHit 衡量工具结果缓存命中路径（含 sha256 + json.Marshal）
func BenchmarkToolCache_GetHit(b *testing.B) {
	c := NewToolCache(10000, 5*time.Minute)
	args := map[string]any{"query": "weather", "city": "Beijing"}
	c.Put("search", args, "sunny 25°C")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get("search", args)
	}
}

// BenchmarkToolCache_Put 衡量工具缓存 Put 单次写入成本（含哈希 + json + 锁）
// 使用 64 个固定 key 循环覆盖，避免无限增长 map 触发 O(N) evictOldest 导致卡顿。
func BenchmarkToolCache_Put(b *testing.B) {
	c := NewToolCache(128, 5*time.Minute)
	const keyPool = 64
	argsPool := make([]map[string]any, keyPool)
	for i := 0; i < keyPool; i++ {
		argsPool[i] = map[string]any{"query": "weather", "city": "Beijing", "seq": i}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Put("search", argsPool[i%keyPool], "sunny 25°C")
	}
}

// BenchmarkToolCache_CacheKey 衡量 cacheKey 纯计算成本
func BenchmarkToolCache_CacheKey(b *testing.B) {
	args := map[string]any{
		"query": "weather in Beijing",
		"lang":  "zh-CN",
		"limit": 10,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cacheKey("search", args)
	}
}

// BenchmarkToolCache_GetMiss 未命中路径（不同 args 每次都是 miss）
func BenchmarkToolCache_GetMiss(b *testing.B) {
	c := NewToolCache(1000, 5*time.Minute)
	// 预填充，让 map 非空
	for i := 0; i < 100; i++ {
		c.Put("search", map[string]any{"seed": i}, "result")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get("search", map[string]any{"unknown": i})
	}
}

// ===== CheckpointManager =====

// BenchmarkCheckpoint_ShouldSave 衡量 ShouldSave 的模运算开销
// （热路径：每一轮 ReAct 循环都会调用一次）
func BenchmarkCheckpoint_ShouldSave(b *testing.B) {
	cm := NewCheckpointManager(nil, 5)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cm.ShouldSave(i)
	}
}

// BenchmarkCheckpoint_ShouldSaveVariedTurn 衡量非均匀 turn 下的判断（避免编译器特化）
func BenchmarkCheckpoint_ShouldSaveVariedTurn(b *testing.B) {
	cm := NewCheckpointManager(nil, 5)
	turns := []int{1, 4, 5, 9, 10, 14, 15, 100, 101, 500}
	b.ReportAllocs()
	b.ResetTimer()
	var sink bool
	for i := 0; i < b.N; i++ {
		sink = cm.ShouldSave(turns[i%len(turns)])
	}
	// 避免 ShouldSave 被消除
	if sink && fmt.Sprintf("%v", sink) == "" {
		b.Fatal("unreachable")
	}
}
