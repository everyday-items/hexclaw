# HexClaw Plugin Development Guide

**English | [中文](plugin-dev.md)**

## Overview

HexClaw's plugin system is built on the `plugin` package from the [Hexagon](https://github.com/hexagon-codes/hexagon) framework, extended with HexClaw-specific plugin types.

## Plugin Types

| Type | Interface | Description |
|------|-----------|-------------|
| **SkillPlugin** | `plugin.SkillPlugin` | Provides additional skills |
| **AdapterPlugin** | `plugin.AdapterPlugin` | Provides new platform adapters |
| **HookPlugin** | `plugin.HookPlugin` | Message/reply processing hooks |

Base plugin types inherited from Hexagon:
- `ProviderPlugin` — LLM provider plugin
- `ToolPlugin` — Tool plugin
- `MemoryPlugin` — Memory plugin

## Lifecycle

```
Register → Init(config) → Start() → [Running] → Stop()
```

All plugins implement Hexagon's `plugin.Plugin` interface:

```go
type Plugin interface {
    Info() PluginInfo
    Init(ctx context.Context, config map[string]any) error
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
    Health() HealthStatus
}
```

> **Note**: `Register` and `Unregister` emit event callbacks after releasing the write lock,
> so it is safe to call other Registry methods from within `OnLoaded`/`OnUnloaded` handlers.

## Quick Start

### 1. Create a Skill Plugin

```go
package myplugin

import (
    "context"

    "github.com/hexagon-codes/hexagon/plugin"
    hcplugin "github.com/hexagon-codes/hexclaw/plugin"
    "github.com/hexagon-codes/hexclaw/skill"
)

type MyPlugin struct {
    plugin.BasePlugin // Embed base implementation
}

func New() *MyPlugin {
    return &MyPlugin{
        BasePlugin: *plugin.NewBasePlugin(plugin.PluginInfo{
            Name:        "my-skill-plugin",
            Version:     "1.0.0",
            Type:        hcplugin.TypeSkill,
            Description: "Example Skill Plugin",
            Author:      "your-name",
        }),
    }
}

// Skills returns the list of skills provided by this plugin
func (p *MyPlugin) Skills() []skill.Skill {
    return []skill.Skill{
        &MySkill{},
    }
}

// MySkill is a custom skill
type MySkill struct{}

func (s *MySkill) Name() string        { return "my-skill" }
func (s *MySkill) Description() string  { return "My custom skill" }
func (s *MySkill) Match(content string) bool { return false } // Only invoked via LLM Tool Use

func (s *MySkill) Execute(ctx context.Context, args map[string]any) (*skill.Result, error) {
    query, _ := args["query"].(string)
    return &skill.Result{
        Content: "Result: " + query,
    }, nil
}
```

### 2. Create a Hook Plugin

```go
type LoggingHook struct {
    plugin.BasePlugin
}

func (h *LoggingHook) OnMessage(ctx context.Context, msg *adapter.Message) (*adapter.Message, error) {
    log.Printf("[inbound] %s: %s", msg.UserID, msg.Content)
    return msg, nil // Return original or modified message
}

func (h *LoggingHook) OnReply(ctx context.Context, reply *adapter.Reply) (*adapter.Reply, error) {
    log.Printf("[outbound] %s", reply.Content[:min(50, len(reply.Content))])
    return reply, nil
}
```

### 3. Create an Adapter Plugin

```go
type MatrixPlugin struct {
    plugin.BasePlugin
    adapter *MatrixAdapter
}

func (p *MatrixPlugin) Adapter() adapter.Adapter {
    return p.adapter
}
```

## Registering Plugins

Register at application startup:

```go
mgr := hcplugin.NewManager()

// Register plugins
mgr.Register(myplugin.New())
mgr.Register(loggingHook)

// Start all plugins
mgr.StartAll(ctx, configs)

// Collect skills and adapters
skills := mgr.Skills()
adapters := mgr.Adapters()
```

## Configuration

Configure plugins in `hexclaw.yaml`:

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

## Best Practices

1. **Embed BasePlugin** — Use `plugin.BasePlugin` for default lifecycle implementation
2. **Health checks** — Override `Health()` to return real health status
3. **Graceful shutdown** — Release all resources and close connections in `Stop()`
4. **Config validation** — Validate required config values in `Init()`
5. **Error handling** — Return clear error messages when skill execution fails
6. **Timeout control** — Use `ctx` context to control timeouts and avoid blocking
7. **Event handler safety** — Registry methods can safely be called from `OnLoaded`/`OnUnloaded` (lock is released)

## References

- [Hexagon Plugin Package](https://github.com/hexagon-codes/hexagon/tree/main/plugin) — Base plugin interfaces and registry
- [HexClaw Skill Interface](../skill/skill.go) — Skill interface definition
- [HexClaw Adapter Interface](../adapter/adapter.go) — Adapter interface definition
- [GitHub Issues](https://github.com/hexagon-codes/hexclaw/issues) — Bug reports and feedback
