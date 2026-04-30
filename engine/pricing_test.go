package engine

import (
	"testing"
	"time"
)

func TestUserOverridePricer_LookupHitMiss(t *testing.T) {
	u := NewUserOverridePricer(map[string]map[string]ModelPrice{
		"openai": {"gpt-4o": {Input: 0.1, Output: 0.2}},
	})
	p, ok := u.Lookup("openai", "gpt-4o")
	if !ok {
		t.Fatal("应命中")
	}
	if p.Input != 0.1 || p.Output != 0.2 {
		t.Errorf("price 不匹配 %+v", p)
	}
	if _, ok := u.Lookup("openai", "missing"); ok {
		t.Error("不应命中")
	}
}

func TestUserOverridePricer_Set(t *testing.T) {
	u := NewUserOverridePricer(nil)
	u.Set("anthropic", "claude-test", ModelPrice{Input: 1, Output: 2})
	p, ok := u.Lookup("anthropic", "claude-test")
	if !ok {
		t.Fatal("Set 后应命中")
	}
	if p.Input != 1 {
		t.Errorf("got %v", p)
	}
}

func TestCachePricer_TTLExpiry(t *testing.T) {
	c := NewCachePricer(50 * time.Millisecond)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.Put("p", "m", ModelPrice{Input: 1, Output: 1})
	if _, ok := c.Lookup("p", "m"); !ok {
		t.Error("立即查应命中")
	}
	// 过期后
	c.now = func() time.Time { return now.Add(time.Hour) }
	if _, ok := c.Lookup("p", "m"); ok {
		t.Error("超过 TTL 应 miss")
	}
}

func TestCachePricer_DefaultTTL(t *testing.T) {
	c := NewCachePricer(0) // 默认 1h
	c.Put("p", "m", ModelPrice{Input: 1})
	if _, ok := c.Lookup("p", "m"); !ok {
		t.Error("默认 TTL 1h 内应命中")
	}
}

type fakeRemote struct {
	results map[string]ModelPrice
}

func (f *fakeRemote) Fetch(provider, model string) (ModelPrice, bool) {
	if p, ok := f.results[provider+"/"+model]; ok {
		return p, true
	}
	return ZeroPrice, false
}

func TestRemotePricer_HitWritesToCache(t *testing.T) {
	cache := NewCachePricer(time.Hour)
	remote := &fakeRemote{results: map[string]ModelPrice{
		"new/llm": {Input: 9, Output: 8},
	}}
	r := NewRemotePricer(remote, cache)

	p, ok := r.Lookup("new", "llm")
	if !ok || p.Input != 9 {
		t.Errorf("应命中 remote；got %+v ok=%v", p, ok)
	}
	// 应该被回填到 cache
	if cp, ok := cache.Lookup("new", "llm"); !ok || cp.Input != 9 {
		t.Error("Remote 命中后 cache 应被回填")
	}
}

func TestRemotePricer_NopRemoteAlwaysMiss(t *testing.T) {
	r := NewRemotePricer(nil, nil) // nil → NopRemote
	if _, ok := r.Lookup("x", "y"); ok {
		t.Error("Nop 应永远 miss")
	}
}

func TestBuiltinFallbackPricer_KnownAndUnknown(t *testing.T) {
	b := NewBuiltinFallbackPricer()
	if _, ok := b.Lookup("openai", "gpt-4o"); !ok {
		t.Error("内置表中存在的 model 应命中")
	}
	if _, ok := b.Lookup("openai", "made-up-model"); ok {
		t.Error("不存在的 model 应 miss")
	}
	if _, ok := b.Lookup("unknown-provider", "x"); ok {
		t.Error("不存在的 provider 应 miss")
	}
}

func TestChainPricer_OrderRespected(t *testing.T) {
	user := NewUserOverridePricer(map[string]map[string]ModelPrice{
		"openai": {"gpt-4o": {Input: 99, Output: 99}}, // 用户特价
	})
	chain := NewChainPricer(user, NewBuiltinFallbackPricer())

	p, ok := chain.Lookup("openai", "gpt-4o")
	if !ok || p.Input != 99 {
		t.Errorf("用户覆盖应胜过 builtin；got %+v", p)
	}
}

func TestChainPricer_FallsThrough(t *testing.T) {
	user := NewUserOverridePricer(nil)
	chain := NewChainPricer(user, NewBuiltinFallbackPricer())
	// gpt-4o 没在 user 里 → 应走 builtin
	if _, ok := chain.Lookup("openai", "gpt-4o"); !ok {
		t.Error("应 fallthrough 到 builtin")
	}
}

func TestChainPricer_AllMissReturnsZero(t *testing.T) {
	chain := NewChainPricer(NewUserOverridePricer(nil))
	if _, ok := chain.Lookup("x", "y"); ok {
		t.Error("全 miss 应返回 ok=false")
	}
}

func TestChainPricer_NilSafe(t *testing.T) {
	var chain *ChainPricer
	if _, ok := chain.Lookup("x", "y"); ok {
		t.Error("nil 链应返回 ok=false")
	}
}

func TestChainPricer_EstimateCostMath(t *testing.T) {
	user := NewUserOverridePricer(map[string]map[string]ModelPrice{
		"x": {"y": {Input: 0.001, Output: 0.002}},
	})
	chain := NewChainPricer(user)
	cost := chain.EstimateCost("x", "y", 1000, 1000)
	want := 0.001 + 0.002
	if cost < want-1e-9 || cost > want+1e-9 {
		t.Errorf("cost 计算错；got %v want %v", cost, want)
	}
}

func TestChainPricer_NilLayersIgnored(t *testing.T) {
	chain := NewChainPricer(nil, NewBuiltinFallbackPricer(), nil)
	if _, ok := chain.Lookup("openai", "gpt-4o"); !ok {
		t.Error("nil 层应被跳过；builtin 应正常工作")
	}
}

func TestNewDefaultPricer_PrioritizesOverride(t *testing.T) {
	user := NewUserOverridePricer(map[string]map[string]ModelPrice{
		"openai": {"gpt-4o": {Input: 999}},
	})
	chain := NewDefaultPricer(user, nil, time.Hour)
	p, _ := chain.Lookup("openai", "gpt-4o")
	if p.Input != 999 {
		t.Errorf("default chain 应优先 user override；got %+v", p)
	}
}

func TestNewDefaultPricer_FallsToBuiltin(t *testing.T) {
	chain := NewDefaultPricer(NewUserOverridePricer(nil), nil, time.Hour)
	if _, ok := chain.Lookup("openai", "gpt-4o"); !ok {
		t.Error("无 user/cache/remote 时应 fallthrough 到 builtin")
	}
}

func TestNewDefaultPricer_RemoteRoundTrip(t *testing.T) {
	remote := &fakeRemote{results: map[string]ModelPrice{
		"newprov/newmodel": {Input: 7, Output: 7},
	}}
	chain := NewDefaultPricer(NewUserOverridePricer(nil), remote, time.Hour)
	p, ok := chain.Lookup("newprov", "newmodel")
	if !ok || p.Input != 7 {
		t.Errorf("远端定价应被命中；got %+v ok=%v", p, ok)
	}
}
