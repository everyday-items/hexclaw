package engine

// BUG-20260710-H2（/review-go 审查 High）：main.go 在装配完成前启动本地预热
// goroutine，预热链路无锁读两处装配期写入的共享状态，构成 go test -race 可测的
// 数据竞争：
//  1. resolveLLMSelection 无锁读 e.agentRouter（react.go:3347），而 SetAgentRouter
//     在 e.mu 下写（react.go:552）——锁只挡写写，读侧裸奔；
//  2. ResolveMode→packMatches 读包级变量 modeKeywordMatcher（agent_mode.go），
//     SetModeKeywordMatcher 裸写同一变量。
//
// 期望不变量：预热与装配 setter 并发执行必须 race-free（本文件配 -race 跑，
// 检出竞争即 FAIL）。main.go 侧的启动时序修正（预热移到装配完成后）另行处理
// 工具集前缀失配问题；本测试锁的是引擎自身的并发安全底线。

import (
	"context"
	"testing"

	"github.com/hexagon-codes/hexclaw/config"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
)

func TestBUG20260710_H2_WarmupConcurrentWithAssemblyIsRaceFree(t *testing.T) {
	var cap warmupCapture
	srv := newFakeOllamaServer(&cap)
	defer srv.Close()

	eng := newWarmupTestEngine(t, map[string]config.LLMProviderConfig{
		"Ollama (本地)": {BaseURL: srv.URL + "/v1", Model: "m"},
	}, "Ollama (本地)")

	t.Cleanup(func() { SetModeKeywordMatcher(nil) }) // 还原包级状态，避免污染其他测试

	const rounds = 30
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < rounds; i++ {
			_, _ = eng.WarmupLocalDefaultModel(context.Background())
		}
	}()
	for i := 0; i < rounds; i++ {
		eng.SetAgentRouter(agentrouter.New())
		SetModeKeywordMatcher(func(AgentMode, string) bool { return false })
	}
	<-done
}
