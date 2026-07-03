package featureflag

import "context"

// ctxKey 是 ctx 中存放 Flags 实例的私有 key。
type ctxKey struct{}

// WithContext 把 Flags 实例注入 ctx，供 Enabled() 查询。
//
// 入口：cmd/hexclaw/main.go 启动时构造 Static 后注入 root ctx；
// trace.NewRequest / 适配器消息处理 / 异步 goroutine 都会派生这个 ctx。
func WithContext(ctx context.Context, f Flags) context.Context {
	if f == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, f)
}

// FromContext 从 ctx 取出 Flags；未注入时 fail-closed。
func FromContext(ctx context.Context) Flags {
	if ctx == nil {
		return Disabled
	}
	if f, ok := ctx.Value(ctxKey{}).(Flags); ok && f != nil {
		return f
	}
	return Disabled
}

// Enabled 是常用查询的快捷方法：等价于 FromContext(ctx).IsEnabled(name)。
//
// 业务路径调用：
//
//	if featureflag.Enabled(ctx, "agent_factory_v2") { ... }
func Enabled(ctx context.Context, name string) bool {
	return FromContext(ctx).IsEnabled(name)
}
