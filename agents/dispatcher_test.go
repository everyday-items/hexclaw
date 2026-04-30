package agents

import (
	"context"
	"errors"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/featureflag"
)

// scriptedProvider 实现 llm.Provider 仅用于 Dispatcher 测试。
type scriptedProvider struct {
	completeContent string
	completeErr     error
}

func (p *scriptedProvider) Name() string { return "scripted" }
func (p *scriptedProvider) Complete(_ context.Context, _ llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if p.completeErr != nil {
		return nil, p.completeErr
	}
	return &llm.CompletionResponse{Content: p.completeContent}, nil
}
func (p *scriptedProvider) Stream(_ context.Context, _ llm.CompletionRequest) (*llm.Stream, error) {
	return &llm.Stream{}, nil
}
func (p *scriptedProvider) Models() []llm.ModelInfo                  { return nil }
func (p *scriptedProvider) CountTokens(_ []llm.Message) (int, error) { return 0, nil }

func withDispatchFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagAgentFactoryReal: on,
	})
	return featureflag.WithContext(ctx, flags)
}

func TestDispatcher_FlagOffReturnsErrDisabled(t *testing.T) {
	d := NewHexagonDispatcher(NewFactory(), &scriptedProvider{completeContent: "ok"})
	ctx := withDispatchFlag(context.Background(), false)
	_, err := d.Dispatch(ctx, "assistant", "hi")
	if !errors.Is(err, ErrDispatchDisabled) {
		t.Errorf("flag OFF 应返回 ErrDispatchDisabled；got %v", err)
	}
}

func TestDispatcher_RejectsUnknownRole(t *testing.T) {
	d := NewHexagonDispatcher(NewFactory(), &scriptedProvider{completeContent: "ok"})
	ctx := withDispatchFlag(context.Background(), true)
	_, err := d.Dispatch(ctx, "nonexistent", "hi")
	if err == nil {
		t.Error("未知角色应报错")
	}
}

func TestDispatcher_RejectsNilProvider(t *testing.T) {
	d := NewHexagonDispatcher(NewFactory(), nil)
	ctx := withDispatchFlag(context.Background(), true)
	_, err := d.Dispatch(ctx, "assistant", "hi")
	if err == nil {
		t.Error("nil provider 应报错")
	}
}

func TestDispatcher_RejectsNilDispatcher(t *testing.T) {
	var d *HexagonDispatcher
	ctx := withDispatchFlag(context.Background(), true)
	_, err := d.Dispatch(ctx, "assistant", "hi")
	if err == nil {
		t.Error("nil dispatcher 应安全报错")
	}
}

func TestDispatcher_HappyPath_InvokesAgent(t *testing.T) {
	provider := &scriptedProvider{completeContent: "agent says hello"}
	d := NewHexagonDispatcher(NewFactory(), provider)
	ctx := withDispatchFlag(context.Background(), true)
	out, err := d.Dispatch(ctx, "assistant", "hi")
	if err != nil {
		t.Fatalf("dispatch 应成功；got %v", err)
	}
	if out == "" {
		t.Error("Agent output 不应为空")
	}
}

func TestDispatcher_PropagatesProviderError(t *testing.T) {
	provider := &scriptedProvider{completeErr: errors.New("upstream timeout")}
	d := NewHexagonDispatcher(NewFactory(), provider)
	ctx := withDispatchFlag(context.Background(), true)
	_, err := d.Dispatch(ctx, "assistant", "hi")
	if err == nil {
		t.Error("provider 错误应传播")
	}
}

func TestDispatcher_NoToolsAlsoWorks(t *testing.T) {
	// 不传 tools 时 dispatcher 仍能调到 hexagon Agent.Invoke
	provider := &scriptedProvider{completeContent: "no-tool ok"}
	d := NewHexagonDispatcher(NewFactory(), provider)
	ctx := withDispatchFlag(context.Background(), true)
	out, err := d.Dispatch(ctx, "assistant", "hi")
	if err != nil {
		t.Fatalf("无 tools dispatch 应成功；got %v", err)
	}
	if out == "" {
		t.Error("output 不应为空")
	}
}
