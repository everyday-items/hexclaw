// Package skill 提供 Skill（技能/工具）管理
//
// HexClaw 的 Skill 系统支持双路径调度：
//   - 快速路径: 通过 Match() 关键词匹配直接触发，跳过 LLM
//   - 主路径: LLM 通过 Tool Use 调用 Skill
//
// 所有 Skill 在沙箱中执行，受超时、内存、网络限制。
// 内置 Skill 包括搜索、天气、翻译、摘要等。
package skill

import (
	"context"

	"github.com/everyday-items/hexclaw/internal/adapter"
)

// Skill 技能接口
//
// 每个 Skill 实现此接口，可被 Agent 引擎调用。
// Skill 支持两种触发方式：
//   - Match() 返回 true 时直接执行（快速路径，不经过 LLM）
//   - 作为 Tool 注册到 LLM，由 LLM 决定何时调用（主路径）
type Skill interface {
	// Name 返回 Skill 名称（唯一标识）
	Name() string

	// Description 返回 Skill 描述（供 LLM 理解用途）
	Description() string

	// Match 快速路径匹配
	// 如果消息内容匹配此 Skill 的触发条件，返回 true
	// 用于跳过 LLM 直接执行的简单场景（如 "天气 北京"）
	Match(content string) bool

	// Execute 执行 Skill
	// args 为参数映射，来自 LLM Tool Use 或 Match 后的解析结果
	Execute(ctx context.Context, args map[string]any) (*Result, error)
}

// Result Skill 执行结果
type Result struct {
	Content  string            // 结果文本
	Data     any               // 结构化数据（可选）
	Metadata map[string]string // 附加元数据
}

// Registry Skill 注册中心
//
// 管理所有已注册的 Skill，提供查找和匹配功能。
type Registry interface {
	// Register 注册 Skill
	Register(skill Skill) error

	// Get 按名称获取 Skill
	Get(name string) (Skill, bool)

	// Match 快速路径匹配
	// 遍历所有 Skill，返回第一个匹配的 Skill
	Match(msg *adapter.Message) (Skill, bool)

	// All 返回所有已注册的 Skill
	All() []Skill
}
