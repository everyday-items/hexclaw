package plugin

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/everyday-items/hexagon/plugin"
	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/skill"
)

// Manager 插件管理器
//
// 基于 Hexagon plugin.Registry，扩展 HexClaw 专属能力：
// 收集所有 SkillPlugin 的 Skill、所有 AdapterPlugin 的 Adapter、
// 按顺序执行 HookPlugin 链。
type Manager struct {
	mu       sync.RWMutex
	registry *plugin.Registry
	plugins  []plugin.Plugin // 保持注册顺序
	hooks    []HookPlugin
}

// NewManager 创建插件管理器
func NewManager() *Manager {
	return &Manager{
		registry: plugin.NewRegistry(),
	}
}

// Register 注册插件
func (m *Manager) Register(p plugin.Plugin) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.registry.Register(p); err != nil {
		return fmt.Errorf("注册插件 %s 失败: %w", p.Info().Name, err)
	}
	m.plugins = append(m.plugins, p)

	if hook, ok := p.(HookPlugin); ok {
		m.hooks = append(m.hooks, hook)
	}

	log.Printf("插件已注册: %s (%s)", p.Info().Name, p.Info().Type)
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
		log.Printf("插件已启动: %s", name)
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
			log.Printf("停止插件 %s 失败: %v", name, err)
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
func (m *Manager) List() []plugin.PluginInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	infos := make([]plugin.PluginInfo, len(m.plugins))
	for i, p := range m.plugins {
		infos[i] = p.Info()
	}
	return infos
}
