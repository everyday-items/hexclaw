// Package featureflag 提供 v0.4.x 单版本闭环（方案 A）所需的 feature flag 基建。
//
// 设计目标：
//
//   - **声明式**：所有 flag 在 init time 通过 Register 集中声明，包含名称 / 默认值 /
//     描述 / 阶段（alpha/beta/ga/deprecated）/ 引入版本。flag 不在散落代码里硬编码。
//
//   - **白名单制**：未注册的 flag 名永远返回 default=false（fail-closed），避免 typo
//     导致代码静默走旧路径。
//
//   - **运行时只读**：Static 实现一旦构造就不可变，避免并发竞态。Test/dev 用 Mutable
//     实现允许修改。
//
//   - **ctx 友好**：调用方通过 featureflag.Enabled(ctx, "name") 查询，避免到处传 Flags
//     实例。Disabled fallback 在 ctx 没注入 Flags 时全 OFF（fail-closed）。
//
//   - **可观测**：Snapshot 返回所有 flag 当前状态（含 user override / default / stage），
//     桌面端 Settings 页直接渲染。
//
// 使用模式：
//
//	// 1. init time 注册（在使用 flag 的包里）
//	func init() {
//	    featureflag.Register(featureflag.Flag{
//	        Name:         "agent_factory_v2",
//	        Default:      false,
//	        Stage:        featureflag.StageAlpha,
//	        Description:  "Agent factory 真接入 hexagon（替代 prompt overlay）",
//	        SinceVersion: "0.4.0",
//	    })
//	}
//
//	// 2. 启动时构造 + 注入 ctx
//	flags := featureflag.NewStatic(featureflag.Registered(), cfg.Features)
//	ctx = featureflag.WithContext(ctx, flags)
//
//	// 3. 业务路径查询
//	if featureflag.Enabled(ctx, "agent_factory_v2") {
//	    return r.factory.Run(ctx, mode, msg)
//	}
//	return r.react.Process(ctx, msg)
package featureflag

import (
	"sync"
)

// Stage 表示 flag 当前所处生命周期阶段。
//
// 阶段决定默认值策略：
//
//   - StageAlpha    内部测试，强制 default=false（即便 Register 写 true 也覆盖）
//   - StageBeta     邀请用户测试，default 由 Register 决定（推荐 false）
//   - StageGA       生产可用，default=true；下一版应移除 flag
//   - StageDeprecated 即将移除；查询时打 warn log
type Stage int

const (
	StageAlpha Stage = iota
	StageBeta
	StageGA
	StageDeprecated
)

// String 返回阶段的人类可读名称。
func (s Stage) String() string {
	switch s {
	case StageAlpha:
		return "alpha"
	case StageBeta:
		return "beta"
	case StageGA:
		return "ga"
	case StageDeprecated:
		return "deprecated"
	}
	return "unknown"
}

// Flag 定义一个 feature flag。
type Flag struct {
	// Name flag 标识，snake_case，必须全局唯一。
	Name string
	// Default 用户未 override 时的默认值。Alpha 阶段强制 false。
	Default bool
	// Description 一句话说明（日志 / Settings UI 用）。
	Description string
	// Stage 见 Stage 文档。
	Stage Stage
	// SinceVersion 引入此 flag 的版本（如 "0.4.0"）。
	SinceVersion string
}

// effectiveDefault 返回考虑 Stage 强制的 default 值。
func (f Flag) effectiveDefault() bool {
	if f.Stage == StageAlpha {
		return false
	}
	return f.Default
}

// FlagStatus 单个 flag 的运行时快照（用于 Settings UI 与日志）。
type FlagStatus struct {
	Name         string
	Enabled      bool
	Default      bool
	UserOverride bool
	Description  string
	Stage        Stage
	SinceVersion string
}

// Flags 是运行时查询接口。
type Flags interface {
	// IsEnabled 查询某 flag 是否启用；未注册的 flag 永远返回 false（fail-closed）。
	IsEnabled(name string) bool
	// Snapshot 返回所有已注册 flag 的当前状态（按名称排序）。
	Snapshot() []FlagStatus
}

// ============== 全局注册表 ==============

var registry = struct {
	mu    sync.Mutex
	flags map[string]Flag
}{
	flags: make(map[string]Flag),
}

// Register 在 init time 注册一个 flag。
//
// 同名重复注册会 panic（init time 失败 = 启动失败，避免静默冲突）。
func Register(f Flag) {
	if f.Name == "" {
		panic("featureflag.Register: flag name is empty")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.flags[f.Name]; exists {
		panic("featureflag.Register: duplicate flag " + f.Name)
	}
	registry.flags[f.Name] = f
}

// Registered 返回当前注册的所有 flag（拷贝，按 Name 字典序）。
//
// 用于启动时构造 Static 实例。
func Registered() []Flag {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	out := make([]Flag, 0, len(registry.flags))
	for _, f := range registry.flags {
		out = append(out, f)
	}
	sortFlags(out)
	return out
}

// Lookup 按名称查询单个注册项；未注册返回 (zero, false)。
func Lookup(name string) (Flag, bool) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	f, ok := registry.flags[name]
	return f, ok
}

// ResetForTest 清空注册表（仅测试用）。
func ResetForTest() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.flags = make(map[string]Flag)
}

func sortFlags(flags []Flag) {
	for i := 1; i < len(flags); i++ {
		for j := i; j > 0 && flags[j-1].Name > flags[j].Name; j-- {
			flags[j-1], flags[j] = flags[j], flags[j-1]
		}
	}
}
