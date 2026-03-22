package cache

import (
	"testing"
	"time"
)

// TestCache_PutAndGet 测试基本存取
func TestCache_PutAndGet(t *testing.T) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 100})

	c.Put("你好", "你好！有什么可以帮你的？", "deepseek", "deepseek-chat")

	resp, ok := c.Get("你好", "deepseek", "deepseek-chat")
	if !ok {
		t.Fatal("应命中缓存")
	}
	if resp != "你好！有什么可以帮你的？" {
		t.Fatalf("期望缓存响应，实际 %s", resp)
	}
}

// TestCache_Normalization 测试输入归一化
func TestCache_Normalization(t *testing.T) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 100})

	// 存入
	c.Put("  你好  ", "回复", "deepseek", "deepseek-chat")

	// 变体应命中（大小写、空白归一化）
	tests := []string{
		"你好",
		"  你好  ",
		"你好",
	}
	for _, input := range tests {
		if _, ok := c.Get(input, "deepseek", "deepseek-chat"); !ok {
			t.Errorf("输入 %q 应命中缓存", input)
		}
	}
}

// TestCache_Expiry 测试过期淘汰
func TestCache_Expiry(t *testing.T) {
	c := New(Options{Enabled: true, TTL: 50 * time.Millisecond, MaxEntries: 100})

	c.Put("test", "response", "deepseek", "deepseek-chat")

	// 立即应命中
	if _, ok := c.Get("test", "deepseek", "deepseek-chat"); !ok {
		t.Fatal("应命中")
	}

	// 等待过期
	time.Sleep(100 * time.Millisecond)

	if _, ok := c.Get("test", "deepseek", "deepseek-chat"); ok {
		t.Fatal("过期后不应命中")
	}
}

// TestCache_MaxEntries 测试最大条目数淘汰
func TestCache_MaxEntries(t *testing.T) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 3})

	c.Put("a", "1", "deepseek", "deepseek-chat")
	c.Put("b", "2", "deepseek", "deepseek-chat")
	c.Put("c", "3", "deepseek", "deepseek-chat")
	c.Put("d", "4", "deepseek", "deepseek-chat") // 应淘汰 "a"

	if _, ok := c.Get("a", "deepseek", "deepseek-chat"); ok {
		t.Fatal("a 应被淘汰")
	}
	if _, ok := c.Get("d", "deepseek", "deepseek-chat"); !ok {
		t.Fatal("d 应存在")
	}
}

// TestCache_Disabled 测试禁用模式
func TestCache_Disabled(t *testing.T) {
	c := New(Options{Enabled: false})

	c.Put("test", "response", "deepseek", "deepseek-chat")
	if _, ok := c.Get("test", "deepseek", "deepseek-chat"); ok {
		t.Fatal("禁用模式不应命中")
	}
}

// TestCache_Stats 测试统计信息
func TestCache_Stats(t *testing.T) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 100})

	c.Put("test", "response", "deepseek", "deepseek-chat")
	c.Get("test", "deepseek", "deepseek-chat") // hit
	c.Get("test", "deepseek", "deepseek-chat") // hit
	c.Get("miss", "deepseek", "deepseek-chat") // miss

	stats := c.Stats()
	if stats.Hits != 2 {
		t.Fatalf("期望 2 次命中，实际 %d", stats.Hits)
	}
	if stats.Misses != 1 {
		t.Fatalf("期望 1 次未命中，实际 %d", stats.Misses)
	}
	if stats.Entries != 1 {
		t.Fatalf("期望 1 条目，实际 %d", stats.Entries)
	}
}

func TestCache_StatsExcludesExpiredEntries(t *testing.T) {
	c := New(Options{Enabled: true, TTL: 20 * time.Millisecond, MaxEntries: 10})

	c.Put("hello", "world", "deepseek", "deepseek-chat")
	time.Sleep(50 * time.Millisecond)

	stats := c.Stats()
	if stats.Entries != 0 {
		t.Fatalf("过期条目不应计入当前条目数，实际 %d", stats.Entries)
	}
	if _, ok := c.Get("hello", "deepseek", "deepseek-chat"); ok {
		t.Fatal("过期条目在统计清理后不应再命中")
	}
}

func TestCache_ModelIsolation(t *testing.T) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 10})
	c.Put("你好", "glm-5 响应", "zhipu", "glm-5")

	if _, ok := c.Get("你好", "zhipu", "glm-4"); ok {
		t.Fatal("不同模型不应命中同一缓存条目")
	}
	if resp, ok := c.Get("你好", "zhipu", "glm-5"); !ok || resp != "glm-5 响应" {
		t.Fatalf("相同 provider/model 应命中缓存，resp=%q ok=%v", resp, ok)
	}
}

func TestCache_Reconfigure(t *testing.T) {
	c := New(Options{Enabled: true, TTL: time.Hour, MaxEntries: 4})
	c.Put("a", "1", "openai", "gpt-4o")
	c.Put("b", "2", "openai", "gpt-4o")
	c.Put("c", "3", "openai", "gpt-4o")

	c.Reconfigure(Options{Enabled: true, TTL: time.Minute, MaxEntries: 2})

	stats := c.Stats()
	if stats.Entries != 2 {
		t.Fatalf("缩小容量后应完成淘汰，实际条目数 %d", stats.Entries)
	}

	c.Reconfigure(Options{Enabled: false})
	if _, ok := c.Get("b", "openai", "gpt-4o"); ok {
		t.Fatal("禁用后不应再命中缓存")
	}
}
