// dispatcher.go 实现 v0.4.0 G1 Agent factory 真接入 hexagon（feature-flag gated）。
//
// HexClaw 的 ReActEngine 历来直接调用 hexagon.Provider.Stream/Complete，自己跑工具
// 循环 —— 没有真用 hexagon.Agent / hexagon.Team 的高阶 Agent 抽象。本包之前的
// agents/factory.go 已提供 Factory.CreateAgent，但只在 orchestrator 子任务里被用到。
//
// G1 把"真正调度到 hexagon.Agent.Invoke"封装成 Dispatcher 抽象，挂 feature flag
// agent.factory.real：flag OFF 时 Dispatcher.Dispatch 立即返回 ErrDispatchDisabled，
// 调用方走原 ReActEngine 路径；flag ON 时通过 factory 创建 Agent 并 Invoke 真执行。
//
// 这给后续 v0.4.x / v0.5 阶段把"特定角色 / 特定模式"逐步迁到 hexagon Agent 留好扩展点：
//   - Researcher / Writer 协作链 → 用 hexagon.Team
//   - Reflection / Self-Discovery 模式 → 用对应 hexagon.Agent 实现
// 真做这些迁移会触及 react.go 主流程（intricate），所以 v0.4.0 阶段仅落基建。
package agents

import (
	"context"
	"errors"
	"fmt"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/agent"
	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagAgentFactoryReal 控制 G1 dispatch 是否生效。alpha 强制 OFF。
const FlagAgentFactoryReal = "agent.factory.real"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagAgentFactoryReal,
		Default:      true, // alpha 强制 OFF
		Description:  "Allow engine to dispatch tasks to a real hexagon.Agent (Factory.CreateAgent + Invoke) instead of inline ReAct loop.",
		Stage:        featureflag.StageAlpha,
		SinceVersion: "0.4.0",
	})
}

// Dispatcher 是 engine 把任务转交给 hexagon Agent 的抽象。
//
// 即便 flag 关闭，实现也应优雅返回 ErrDispatchDisabled，调用方据此 fallback 到
// 现有 ReActEngine 流程。
type Dispatcher interface {
	// Dispatch 用指定 role 创建 Agent 并 Invoke。返回 Agent 输出文本或 error。
	Dispatch(ctx context.Context, role, query string) (string, error)
}

// HexagonDispatcher 用 Factory.CreateAgent 把任务真分派给 hexagon.Agent.Invoke。
type HexagonDispatcher struct {
	factory  *Factory
	provider hexagon.Provider
	tools    []hexagon.Tool
}

// NewHexagonDispatcher 构造 dispatcher。factory / provider 不能为 nil（运行时校验）。
func NewHexagonDispatcher(factory *Factory, provider hexagon.Provider, tools ...hexagon.Tool) *HexagonDispatcher {
	return &HexagonDispatcher{factory: factory, provider: provider, tools: tools}
}

// Dispatch 在 flag 开启时创建 Agent 并 Invoke；flag 关闭时返回 ErrDispatchDisabled。
//
// 调用方典型用法：
//
//	out, err := dispatcher.Dispatch(ctx, "researcher", query)
//	if errors.Is(err, agents.ErrDispatchDisabled) {
//	    // fall back to ReActEngine
//	    out, err = engine.Process(ctx, msg)
//	}
func (d *HexagonDispatcher) Dispatch(ctx context.Context, role, query string) (string, error) {
	if !featureflag.Enabled(ctx, FlagAgentFactoryReal) {
		return "", ErrDispatchDisabled
	}
	if d == nil || d.factory == nil {
		return "", fmt.Errorf("HexagonDispatcher: nil factory")
	}
	if d.provider == nil {
		return "", fmt.Errorf("HexagonDispatcher: nil provider")
	}

	a, err := d.factory.CreateAgent(role, d.provider, d.tools...)
	if err != nil {
		return "", fmt.Errorf("create agent for role %q: %w", role, err)
	}
	out, err := a.Invoke(ctx, agent.Input{Query: query})
	if err != nil {
		return "", fmt.Errorf("hexagon agent invoke: %w", err)
	}
	return out.Content, nil
}

// ErrDispatchDisabled 在 flag OFF 时返回，调用方应据此回退到 ReActEngine 路径。
var ErrDispatchDisabled = errors.New("hexagon agent dispatch disabled (flag agent.factory.real OFF)")
