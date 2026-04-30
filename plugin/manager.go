package plugin

import (
	"context"
	"fmt"
	"github.com/hexagon-codes/toolkit/util/logger"
	"sync"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/skill"
)

// Manager 插件管理器
//
// 基于 Hexagon plugin.Registry，扩展 HexClaw 专属能力：
// 收集所有 SkillPlugin 的 Skill、所有 AdapterPlugin 的 Adapter、
// 按顺序执行 HookPlugin 链。
//
// v0.4.0 H5：当 plugin 实现 ExtensionPlugin（暴露 Manifest）且 flag
// plugin.extension.v1 开启时，Register 会校验 Manifest 兼容性 + capability
// 白名单；校验失败拒绝注册。
type Manager struct {
	mu          sync.RWMutex
	registry    *hexagon.PluginRegistry
	plugins     []hexagon.PluginPlugin // 保持注册顺序
	hooks       []HookPlugin
	hostVersion string                          // 用于 Manifest.MinHostVersion 校验
	hostFlags   featureflagFlagsAdapter         // featureflag.Flags 适配（接受 nil）
}

// NewManager 创建插件管理器
func NewManager() *Manager {
	return &Manager{
		registry: hexagon.NewPluginRegistry(),
	}
}

// SetHostContext 注入 host 版本号 + featureflag.Flags，供 Register 时校验 Manifest 用。
//
// 在 cmd/hexclaw 启动后立刻调一次：
//
//	mgr.SetHostContext("0.4.0", flags)
//
// 不调时 Register 表现等价于 v0.3（不校验 Manifest）。
func (m *Manager) SetHostContext(hostVersion string, flags featureflagFlagsAdapter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hostVersion = hostVersion
	m.hostFlags = flags
}

// featureflagFlagsAdapter 是 featureflag.Flags 的薄类型别名，避免 plugin 包
// 反向 import featureflag（已在 extension.go 中 import 过）。
type featureflagFlagsAdapter interface {
	IsEnabled(name string) bool
}

// Register 注册插件
func (m *Manager) Register(p hexagon.PluginPlugin) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// v0.4.0 H5：在 flag 开启 + plugin 实现 ExtensionPlugin 时强制校验 Manifest
	if ext, ok := p.(ExtensionPlugin); ok && m.hostFlags != nil && m.hostFlags.IsEnabled(FlagPluginExtensionV1) {
		hostV := m.hostVersion
		if hostV == "" {
			hostV = "0.0.0"
		}
		if err := ValidateManifest(ext.Manifest(), hostV); err != nil {
			return fmt.Errorf("注册插件 %s 失败: manifest 校验未通过: %w", p.Info().Name, err)
		}
	}

	if err := m.registry.Register(p); err != nil {
		return fmt.Errorf("注册插件 %s 失败: %w", p.Info().Name, err)
	}
	m.plugins = append(m.plugins, p)

	if hook, ok := p.(HookPlugin); ok {
		m.hooks = append(m.hooks, hook)
	}

	logger.Info("name", "name", p.Info().Name, "type", p.Info().Type)
	return nil
}

// StartAll 初始化并启动所有已注册插件
func (m *Manager) StartAll(ctx context.Context, configs map[string]map[string]any) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, p := range m.plugins {
		name := p.Info().Name
		cfg := configs[name]
		if err := p.Init(ctx, cfg); err != nil {
			return fmt.Errorf("初始化插件 %s 失败: %w", name, err)
		}
		if err := p.Start(ctx); err != nil {
			return fmt.Errorf("启动插件 %s 失败: %w", name, err)
		}
		logger.Info("name", "name", name)
	}
	return nil
}

// StopAll 按注册逆序停止所有插件
func (m *Manager) StopAll(ctx context.Context) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for i := len(m.plugins) - 1; i >= 0; i-- {
		name := m.plugins[i].Info().Name
		if err := m.plugins[i].Stop(ctx); err != nil {
			logger.Error("停止插件", "name", name, "error", err)
		}
	}
}

// Skills 收集所有 SkillPlugin 提供的 Skill
func (m *Manager) Skills() []skill.Skill {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var skills []skill.Skill
	for _, p := range m.plugins {
		if sp, ok := p.(SkillPlugin); ok {
			skills = append(skills, sp.Skills()...)
		}
	}
	return skills
}

// Adapters 收集所有 AdapterPlugin 提供的 Adapter
func (m *Manager) Adapters() []adapter.Adapter {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var adapters []adapter.Adapter
	for _, p := range m.plugins {
		if ap, ok := p.(AdapterPlugin); ok {
			adapters = append(adapters, ap.Adapter())
		}
	}
	return adapters
}

// RunMessageHooks 执行消息钩子链
func (m *Manager) RunMessageHooks(ctx context.Context, msg *adapter.Message) (*adapter.Message, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	current := msg
	for _, hook := range m.hooks {
		result, err := hook.OnMessage(ctx, current)
		if err != nil {
			return nil, fmt.Errorf("钩子 %s.OnMessage 失败: %w", hook.Info().Name, err)
		}
		if result != nil {
			current = result
		}
	}
	return current, nil
}

// RunReplyHooks 执行回复钩子链
func (m *Manager) RunReplyHooks(ctx context.Context, reply *adapter.Reply) (*adapter.Reply, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	current := reply
	for _, hook := range m.hooks {
		result, err := hook.OnReply(ctx, current)
		if err != nil {
			return nil, fmt.Errorf("钩子 %s.OnReply 失败: %w", hook.Info().Name, err)
		}
		if result != nil {
			current = result
		}
	}
	return current, nil
}

// List 列出所有已注册插件信息
func (m *Manager) List() []hexagon.PluginInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]hexagon.PluginInfo, len(m.plugins))
	for i, p := range m.plugins {
		infos[i] = p.Info()
	}
	return infos
}
