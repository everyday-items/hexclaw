# HexClaw 插件开发指南

## 概述

HexClaw 的插件系统基于 [Hexagon](https://github.com/everyday-items/hexagon) 框架的 `plugin` 包构建，扩展了 HexClaw 专属插件类型。

## 插件类型

| 类型 | 接口 | 说明 |
|------|------|------|
| **SkillPlugin** | `plugin.SkillPlugin` | 提供额外技能 |
| **AdapterPlugin** | `plugin.AdapterPlugin` | 提供新平台适配器 |
| **HookPlugin** | `plugin.HookPlugin` | 消息/回复处理钩子 |

基础插件类型继承自 Hexagon:
- `ProviderPlugin` — LLM Provider 插件
- `ToolPlugin` — 工具插件
- `MemoryPlugin` — 记忆插件

## 生命周期

```
Register → Init(config) → Start() → [运行中] → Stop()
```

所有插件实现 Hexagon 的 `plugin.Plugin` 接口：

```go
type Plugin interface {
    Info() PluginInfo
    Init(ctx context.Context, config map[string]any) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Health() HealthStatus
}
```

## 快速开始

### 1. 创建 Skill 插件

```go
package myplugin

import (
    "context"

    "github.com/everyday-items/hexagon/plugin"
    hcplugin "github.com/everyday-items/hexclaw/plugin"
    "github.com/everyday-items/hexclaw/skill"
)

type MyPlugin struct {
    plugin.BasePlugin // 继承基础实现
}

func New() *MyPlugin {
    return &MyPlugin{
        BasePlugin: *plugin.NewBasePlugin(plugin.PluginInfo{
            Name:        "my-skill-plugin",
            Version:     "1.0.0",
            Type:        hcplugin.TypeSkill,
            Description: "示例 Skill 插件",
            Author:      "your-name",
        }),
    }
}

// Skills 返回该插件提供的 Skill 列表
func (p *MyPlugin) Skills() []skill.Skill {
    return []skill.Skill{
        &MySkill{},
    }
}

// MySkill 自定义技能
type MySkill struct{}

func (s *MySkill) Name() string        { return "my-skill" }
func (s *MySkill) Description() string  { return "我的自定义技能" }
func (s *MySkill) Match(content string) bool { return false } // 仅通过 LLM Tool Use 调用

func (s *MySkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
    query, _ := args["query"].(string)
    return &skill.Result{
        Content: "处理结果: " + query,
    }, nil
}
```

### 2. 创建 Hook 插件

```go
type LoggingHook struct {
    plugin.BasePlugin
}

func (h *LoggingHook) OnMessage(ctx context.Context, msg *adapter.Message) (*adapter.Message, error) {
    log.Printf("[入站] %s: %s", msg.UserID, msg.Content)
    return msg, nil // 返回原消息或修改后的消息
}

func (h *LoggingHook) OnReply(ctx context.Context, reply *adapter.Reply) (*adapter.Reply, error) {
    log.Printf("[出站] %s", reply.Content[:min(50, len(reply.Content))])
    return reply, nil
}
```

### 3. 创建 Adapter 插件

```go
type MatrixPlugin struct {
    plugin.BasePlugin
    adapter *MatrixAdapter
}

func (p *MatrixPlugin) Adapter() adapter.Adapter {
    return p.adapter
}
```

## 注册插件

在应用启动时注册：

```go
mgr := hcplugin.NewManager()

// 注册插件
mgr.Register(myplugin.New())
mgr.Register(loggingHook)

// 启动所有插件
mgr.StartAll(ctx, configs)

// 收集 Skill 和 Adapter
skills := mgr.Skills()
adapters := mgr.Adapters()
```

## 配置

在 `hexclaw.yaml` 中配置插件：

```yaml
plugins:
  - name: my-skill-plugin
    enabled: true
    config:
      api_key: ${MY_PLUGIN_API_KEY}
      timeout: 30s

  - name: logging-hook
    enabled: true
```

## 最佳实践

1. **继承 BasePlugin** — 使用 `plugin.BasePlugin` 获得默认的生命周期实现
2. **健康检查** — 重写 `Health()` 方法返回真实的健康状态
3. **优雅停止** — 在 `Stop()` 中释放所有资源，关闭连接
4. **配置验证** — 在 `Init()` 中验证必需配置项
5. **错误处理** — Skill 执行失败时返回明确的错误信息
6. **超时控制** — 使用 `ctx` 上下文控制超时，避免阻塞

## 参考

- [Hexagon Plugin 包](https://github.com/everyday-items/hexagon/tree/main/plugin) — 基础插件接口和注册表
- [HexClaw Skill 接口](../skill/skill.go) — Skill 接口定义
- [HexClaw Adapter 接口](../adapter/adapter.go) — 适配器接口定义
