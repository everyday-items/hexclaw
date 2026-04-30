package featureflag

import "sync"

// Mutable Flags 实现：允许运行时 Set，仅供 test / dev 使用。
//
// 生产路径用 Static —— 构造后不可变避免热路径锁竞争。
type Mutable struct {
	mu        sync.RWMutex
	flags     map[string]Flag
	overrides map[string]bool
}

// NewMutable 创建可变实例。
func NewMutable(registered []Flag) *Mutable {
	flags := make(map[string]Flag, len(registered))
	for _, f := range registered {
		flags[f.Name] = f
	}
	return &Mutable{flags: flags, overrides: make(map[string]bool)}
}

// IsEnabled 同 Static.IsEnabled。
func (m *Mutable) IsEnabled(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.flags[name]
	if !ok {
		return false
	}
	if v, override := m.overrides[name]; override {
		return v
	}
	return f.effectiveDefault()
}

// Set 设置 override 值；name 未注册返回 false。
func (m *Mutable) Set(name string, enabled bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.flags[name]; !ok {
		return false
	}
	m.overrides[name] = enabled
	return true
}

// Clear 删除某 flag 的 override，回归 default。
func (m *Mutable) Clear(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.overrides, name)
}

// Snapshot 同 Static.Snapshot。
func (m *Mutable) Snapshot() []FlagStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]FlagStatus, 0, len(m.flags))
	for _, f := range m.flags {
		v, override := m.overrides[f.Name]
		enabled := f.effectiveDefault()
		if override {
			enabled = v
		}
		out = append(out, FlagStatus{
			Name:         f.Name,
			Enabled:      enabled,
			Default:      f.effectiveDefault(),
			UserOverride: override,
			Description:  f.Description,
			Stage:        f.Stage,
			SinceVersion: f.SinceVersion,
		})
	}
	statusSort(out)
	return out
}
