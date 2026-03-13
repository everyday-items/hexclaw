// Package plugin 提供 HexClaw 插件系统
//
// 基于 Hexagon 框架的 plugin 包，扩展 HexClaw 专属插件类型：
//   - SkillPlugin: 提供额外 Skill 的插件
//   - AdapterPlugin: 提供平台适配器的插件
//   - HookPlugin: 消息处理钩子插件
//
// 插件生命周期由 Hexagon plugin.Lifecycle 管理。
package plugin

import (
	"context"

	"github.com/everyday-items/hexagon/plugin"
	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/skill"
)

// HexClaw 扩展插件类型
const (
	TypeSkill   plugin.PluginType = "skill"
	TypeAdapter plugin.PluginType = "adapter"
	TypeHook    plugin.PluginType = "hook"
)

// SkillPlugin 提供额外 Skill 的插件
type SkillPlugin interface {
	plugin.Plugin
	// Skills 返回该插件提供的 Skill 列表
	Skills() []skill.Skill
}

// AdapterPlugin 提供平台适配器的插件
type AdapterPlugin interface {
	plugin.Plugin
	// Adapter 返回该插件提供的适配器实例
	Adapter() adapter.Adapter
}

// HookPlugin 消息处理钩子插件
//
// 在消息进入引擎前和回复发出前执行。
// 可用于日志、过滤、消息变换等。
type HookPlugin interface {
	plugin.Plugin
	// OnMessage 消息预处理（入站）
	OnMessage(ctx context.Context, msg *adapter.Message) (*adapter.Message, error)
	// OnReply 回复后处理（出站）
	OnReply(ctx context.Context, reply *adapter.Reply) (*adapter.Reply, error)
}
