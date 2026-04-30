// dispatch.go 实现 v0.4.0 G1 接入：在 ReActEngine 主流程之外提供一条
// 可选的 hexagon.Agent 真分派路径（feature-flag gated）。
//
// 调用方使用方式（基于 message metadata 决定走哪条路径）：
//
//	if dispatcher := e.HexagonDispatcher(); dispatcher != nil {
//	    if role := msg.Metadata["dispatch_role"]; role != "" {
//	        if out, err := dispatcher.Dispatch(ctx, role, msg.Content); err == nil {
//	            // 走 hexagon Agent 路径
//	            return out, nil
//	        } else if !errors.Is(err, agents.ErrDispatchDisabled) {
//	            return "", err
//	        }
//	        // ErrDispatchDisabled → fallthrough 到 ReAct 老路径
//	    }
//	}
//
// flag agent.factory.real 关闭时 Dispatch 立即返回 ErrDispatchDisabled，调用方
// 不会触碰 hexagon Agent 创建路径。
package engine

import (
	"context"
	"errors"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/agents"
)

// HexagonDispatcher 是 ReActEngine 持有的可选 dispatcher 引用。
// nil 表示未配置 —— 所有任务走 ReAct 老路径。
func (e *ReActEngine) HexagonDispatcher() *agents.HexagonDispatcher {
	if e == nil {
		return nil
	}
	return e.hexagonDispatcher
}

// SetHexagonDispatcher 注入 dispatcher（cmd/hexclaw 启动时调）。
func (e *ReActEngine) SetHexagonDispatcher(d *agents.HexagonDispatcher) {
	if e == nil {
		return
	}
	e.hexagonDispatcher = d
}

// TryDispatchHexagon 在 message metadata 中含 dispatch_role 时，尝试通过
// hexagon Agent 路径处理；返回 (output, true, nil) 表示已处理；返回
// (_, false, nil) 表示应回落到 ReAct 路径；返回 (_, _, err) 表示真错误。
//
// flag agent.factory.real 关闭 / 没注入 dispatcher / metadata 没 dispatch_role
// 三种情况都返回 (false, nil)，调用方继续走 ReAct。
func (e *ReActEngine) TryDispatchHexagon(ctx context.Context, role, query string) (string, bool, error) {
	if e == nil || e.hexagonDispatcher == nil || role == "" {
		return "", false, nil
	}
	out, err := e.hexagonDispatcher.Dispatch(ctx, role, query)
	if err != nil {
		if errors.Is(err, agents.ErrDispatchDisabled) {
			return "", false, nil
		}
		return "", false, err
	}
	return out, true, nil
}

// 编译期校验 —— 防止 hexagon.Provider 被 import 后误删
var _ hexagon.Provider = (hexagon.Provider)(nil)
