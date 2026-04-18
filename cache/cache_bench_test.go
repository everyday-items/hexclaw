package cache

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

// BenchmarkCache_Put 衡量 Put 的单次写入成本（含哈希 + 淘汰）
func BenchmarkCache_Put(b *testing.B) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 100000})
	resp := "cached response body used for benchmark"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Put("input-"+strconv.Itoa(i), resp, "deepseek", "deepseek-chat")
	}
}

// BenchmarkCache_GetHit 衡量命中路径的单次查询成本（含哈希 + 锁 + 过期判断）
func BenchmarkCache_GetHit(b *testing.B) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 100000})
	const N = 1000
	keys := make([]string, N)
	for i := 0; i < N; i++ {
		keys[i] = "input-" + strconv.Itoa(i)
		c.Put(keys[i], "cached response", "deepseek", "deepseek-chat")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(keys[i%N], "deepseek", "deepseek-chat")
	}
}

// BenchmarkCache_GetMiss 衡量未命中路径的成本（哈希 + map 查询失败）
func BenchmarkCache_GetMiss(b *testing.B) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 100000})
	for i := 0; i < 1000; i++ {
		c.Put("input-"+strconv.Itoa(i), "resp", "deepseek", "deepseek-chat")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get("miss-"+strconv.Itoa(i), "deepseek", "deepseek-chat")
	}
}

// BenchmarkCache_HashInput 衡量归一化 + SHA256 的纯哈希开销
func BenchmarkCache_HashInput(b *testing.B) {
	input := "  What is the weather like in Beijing today? 今天北京天气如何？  "
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = hashInput(input, "deepseek", "deepseek-chat")
	}
}

// BenchmarkCache_PutWithEviction 衡量触发淘汰时的 Put 成本
// 故意使用较小的 maxEntries，让每次 Put 都触发 evict。
func BenchmarkCache_PutWithEviction(b *testing.B) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 100})
	// 预填满
	for i := 0; i < 100; i++ {
		c.Put(fmt.Sprintf("seed-%d", i), "seed", "deepseek", "deepseek-chat")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Put("key-"+strconv.Itoa(i), "response", "deepseek", "deepseek-chat")
	}
}
