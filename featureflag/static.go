package featureflag

// Static Flags 实现：运行时只读，构造后不可变（线程安全无需锁）。
//
// 这是生产路径的默认实现。Mutable 实现见 mutable.go（仅 test/dev 用）。
type Static struct {
	flags     map[string]Flag
	overrides map[string]bool
}

// NewStatic 用注册的 flag 列表 + 用户 override 创建实例。
//
//   - registered: 通常来自 Registered()
//   - overrides: 来自 config 文件 features: 段或 CLI flag
//
// 未注册的 override key 会被忽略（不报错，但未来可改为 warn log）。
func NewStatic(registered []Flag, overrides map[string]bool) *Static {
	flags := make(map[string]Flag, len(registered))
	for _, f := range registered {
		flags[f.Name] = f
	}
	clean := make(map[string]bool, len(overrides))
	for k, v := range overrides {
		if _, ok := flags[k]; ok {
			clean[k] = v
		}
	}
	return &Static{flags: flags, overrides: clean}
}

// IsEnabled 实现 Flags.IsEnabled。
func (s *Static) IsEnabled(name string) bool {
	f, ok := s.flags[name]
	if !ok {
		return false // fail-closed：未注册的 flag 永远 OFF
	}
	if v, override := s.overrides[name]; override {
		return v
	}
	return f.effectiveDefault()
}

// Snapshot 实现 Flags.Snapshot。
func (s *Static) Snapshot() []FlagStatus {
	out := make([]FlagStatus, 0, len(s.flags))
	for _, f := range s.flags {
		v, override := s.overrides[f.Name]
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

func statusSort(items []FlagStatus) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j-1].Name > items[j].Name; j-- {
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}

// ============== fallback 实现 ==============

type defaultFlags struct{}

func (defaultFlags) IsEnabled(name string) bool {
	f, ok := Lookup(name)
	if !ok {
		return false
	}
	return f.effectiveDefault()
}

func (defaultFlags) Snapshot() []FlagStatus {
	flags := Registered()
	out := make([]FlagStatus, 0, len(flags))
	for _, f := range flags {
		out = append(out, FlagStatus{
			Name:         f.Name,
			Enabled:      f.effectiveDefault(),
			Default:      f.effectiveDefault(),
			UserOverride: false,
			Description:  f.Description,
			Stage:        f.Stage,
			SinceVersion: f.SinceVersion,
		})
	}
	statusSort(out)
	return out
}

// Defaults 按全局注册表的有效默认值查询。仅供显式需要注册默认值的调用方使用；
// featureflag.FromContext 在缺失 Flags 时仍 fail-closed。未注册 flag 返回 false。
var Defaults Flags = defaultFlags{}

type disabledFlags struct{}

func (disabledFlags) IsEnabled(string) bool  { return false }
func (disabledFlags) Snapshot() []FlagStatus { return nil }

// Disabled 是显式全 OFF 实现。测试或回退路径需要强制关闭所有功能时注入它。
var Disabled Flags = disabledFlags{}
