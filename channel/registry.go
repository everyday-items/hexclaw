package channel

import (
	"fmt"
	"sort"
	"sync"
)

// Registry 通道注册表（name→ChannelPort）。composition root 装配一处注册；
// 未配置通道 Get 返回 ErrNotConfigured，调用方诚实降级（不静默换通道）。
type Registry struct {
	mu    sync.RWMutex
	ports map[string]Port
}

// NewRegistry 建空注册表。
func NewRegistry() *Registry {
	return &Registry{ports: map[string]Port{}}
}

// Register 注册通道（同名覆盖：装配是单处一次性动作，后注册视为显式替换）。
func (r *Registry) Register(p Port) {
	if p == nil || p.Name() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ports[p.Name()] = p
}

// Get 按通道名取端口；未配置 → ErrNotConfigured（诚实降级契约）。
func (r *Registry) Get(name string) (Port, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.ports[name]
	if !ok {
		return nil, fmt.Errorf("通道 %q 未配置: %w", name, ErrNotConfigured)
	}
	return p, nil
}

// Names 已注册通道名（排序稳定，供自感知/日志）。
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.ports))
	for name := range r.ports {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
