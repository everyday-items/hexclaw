package main

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
)

// BUG-20260704：本地聊天回复慢到 ~57s（直连 ollama 裸测 TTFT 仅 0.66s）。根因=KB 检索的辅助
// LLM（查询扩展 / LLM 重排）经 router 路由到默认 provider（实机默认=本地 Ollama），而 ollama
// 常 `-np 1` 单槽——后台辅助调用与前台主聊天生成争抢同一槽，辅助即使客户端预算取消，服务端仍占
// 槽跑完，主回复被迫排在其后造成头阻塞。契约：辅助 LLM 路由到**本地 provider** 时必须跳过
// （退化确定性检索），绝不占用本地稀缺算力与前台竞争；云端 provider 照常。
type recordingProvider struct{ called *bool }

func (p *recordingProvider) Name() string { return "rec" }
func (p *recordingProvider) Complete(_ context.Context, _ hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
	*p.called = true
	return &hexagon.CompletionResponse{Content: "ok"}, nil
}
func (p *recordingProvider) Stream(_ context.Context, _ hexagon.CompletionRequest) (*hexagon.LLMStream, error) {
	return nil, errors.New("unused")
}
func (p *recordingProvider) Models() []llm.ModelInfo                { return nil }
func (p *recordingProvider) CountTokens([]llm.Message) (int, error) { return 0, nil }

type fakeRetrievalRouter struct {
	name    string
	isLocal bool
	called  *bool
}

func (r *fakeRetrievalRouter) Route(_ context.Context) (hexagon.Provider, string, error) {
	return &recordingProvider{called: r.called}, r.name, nil
}
func (r *fakeRetrievalRouter) IsLocalProviderName(name string) bool {
	return r.isLocal && name == r.name
}

// 本地 provider：辅助 LLM 必须跳过，绝不调用 Complete（不占本地单槽与前台争用）。
func TestBug20260704_RetrievalAux_SkipsLocalProvider(t *testing.T) {
	called := false
	fn := newRetrievalRerankLLM(&fakeRetrievalRouter{name: "Ollama (本地)", isLocal: true, called: &called})

	_, err := fn(context.Background(), "改写查询：分布式一致性")
	if !errors.Is(err, errRetrievalAuxSkippedLocal) {
		t.Fatalf("BUG-20260704: 本地 provider 未跳过辅助 LLM（应返回 skip 错），实际 err=%v", err)
	}
	if called {
		t.Fatal("BUG-20260704: 本地 provider 竟调用了 Complete——占用本地单槽与前台主聊天争抢（头阻塞根因）")
	}
}

// 云端 provider：辅助 LLM 照常启用（有并行、无自争用）。
func TestBug20260704_RetrievalAux_UsesCloudProvider(t *testing.T) {
	called := false
	fn := newRetrievalRerankLLM(&fakeRetrievalRouter{name: "硅基流动", isLocal: false, called: &called})

	out, err := fn(context.Background(), "改写查询")
	if err != nil {
		t.Fatalf("云端 provider 不应跳过: %v", err)
	}
	if !called {
		t.Fatal("云端 provider 辅助 LLM 未被调用——不该误伤云端增强")
	}
	if out != "ok" {
		t.Fatalf("云端应返回 Complete 结果，实际 %q", out)
	}
}
