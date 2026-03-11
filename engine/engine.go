// Package engine 提供 HexClaw 的 Agent 引擎
//
// Engine 是 HexClaw 的核心处理单元，负责：
//   - 接收统一消息并调度 Agent 处理
//   - 管理会话状态和上下文
//   - 协调 LLM、Skill、Memory 等组件
//   - 支持同步和流式两种响应模式
//
// Engine 基于 Hexagon 框架的 ReAct Agent 实现。
package engine

import (
	"context"

	"github.com/everyday-items/hexclaw/adapter"
)

// Engine Agent 引擎接口
//
// 引擎接收统一消息，通过 Agent 处理后返回回复。
// 引擎内部管理会话、记忆、Skill 调度等逻辑。
//
// 生命周期: Start() → Process/ProcessStream → Stop()
type Engine interface {
	// Start 启动引擎，初始化内部组件
	Start(ctx context.Context) error

	// Stop 停止引擎，释放资源
	Stop(ctx context.Context) error

	// Process 同步处理消息
	// 等待 Agent 完成全部处理后返回完整回复
	Process(ctx context.Context, msg *adapter.Message) (*adapter.Reply, error)

	// ProcessStream 流式处理消息
	// 返回一个 channel，Agent 处理过程中逐块输出
	ProcessStream(ctx context.Context, msg *adapter.Message) (<-chan *adapter.ReplyChunk, error)

	// Health 健康检查
	// 返回 nil 表示引擎正常运行
	Health(ctx context.Context) error
}
