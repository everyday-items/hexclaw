package skill

import (
	"fmt"
	"sync"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// DefaultRegistry 默认 Skill 注册中心实现
//
// 线程安全，支持动态注册/查找 Skill。
type DefaultRegistry struct {
	mu     sync.RWMutex
	skills map[string]Skill
	order  []string // 保持注册顺序，用于确定性的 Match 遍历
	enabled map[string]bool
}

// NewRegistry 创建 Skill 注册中心
func NewRegistry() *DefaultRegistry {
	return &DefaultRegistry{
		skills: make(map[string]Skill),
		enabled: make(map[string]bool),
	}
}

// Register 注册 Skill
// 如果已存在同名 Skill 则返回错误
func (r *DefaultRegistry) Register(skill Skill) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := skill.Name()
	if _, exists := r.skills[name]; exists {
		return fmt.Errorf("skill %q 已注册", name)
	}

	r.skills[name] = skill
	r.order = append(r.order, name)
	r.enabled[name] = true
	return nil
}

// Get 按名称获取 Skill
func (r *DefaultRegistry) Get(name string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.skills[name]
	return s, ok
}

// Match 快速路径匹配
//
// 遍历所有 Skill（按注册顺序），返回第一个 Match() 为 true 的 Skill。
// 如果没有匹配则返回 false。
func (r *DefaultRegistry) Match(msg *adapter.Message) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, name := range r.order {
		s := r.skills[name]
		if !r.enabled[name] {
			continue
		}
		if s.Match(msg.Content) {
			return s, true
		}
	}
	return nil, false
}

// All 返回所有已注册的 Skill（按注册顺序）
func (r *DefaultRegistry) All() []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Skill, 0, len(r.order))
	for _, name := range r.order {
		result = append(result, r.skills[name])
	}
	return result
}

// SetEnabled 设置技能的运行时启用状态。
func (r *DefaultRegistry) SetEnabled(name string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, ok := r.skills[name]; !ok {
		return fmt.Errorf("skill %q 未注册", name)
	}
	r.enabled[name] = enabled
	return nil
}

// IsEnabled 返回技能的运行时启用状态。
func (r *DefaultRegistry) IsEnabled(name string) (bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if _, ok := r.skills[name]; !ok {
		return false, false
	}
	enabled, ok := r.enabled[name]
	if !ok {
		return true, true
	}
	return enabled, true
}
