// applier.go 实现 v0.4.0 F9 接入：把 llmrouter.Selector 适配为
// config.Applier，让事务热加载能在 Commit 阶段重新构造 LLM Provider 集合。
//
// 用法（cmd/hexclaw 启动时）：
//
//	tm := config.NewTransactionManager(initial,
//	    []config.Validator{...},
//	    []config.Applier{llmrouter.NewSelectorApplier(selector)},
//	)
//	// 用户改了 cfg.LLM.Providers 时：
//	tx, _ := tm.Begin(ctx)
//	tx.Stage(ctx, newCfg)
//	tx.Commit(ctx)  // → 调 SelectorApplier.Apply → selector.Reload → Provider 立即生效
//	                // 任一 Applier 失败 → 自动 RollbackFn 还原 Selector 到上一 cfg
package llmrouter

import (
	"context"
	"fmt"

	"github.com/hexagon-codes/hexclaw/config"
)

// SelectorApplier 把 *Selector 适配为 config.Applier。
type SelectorApplier struct {
	selector *Selector
}

// NewSelectorApplier 构造 applier。selector 不能为 nil。
func NewSelectorApplier(s *Selector) *SelectorApplier {
	return &SelectorApplier{selector: s}
}

// Apply 把 candidate.LLM 推到 selector.Reload。返回 RollbackFn 在 Commit 失败时
// 用 Begin 时的 cfg 快照恢复 Selector。
func (a *SelectorApplier) Apply(_ context.Context, candidate *config.Config) (config.RollbackFn, error) {
	if a == nil || a.selector == nil {
		return nil, fmt.Errorf("SelectorApplier: nil selector")
	}
	if candidate == nil {
		return nil, fmt.Errorf("SelectorApplier: nil candidate")
	}
	// 拍下当前 cfg 用作 rollback 快照
	previous := a.selector.ActiveConfig()
	if err := a.selector.Reload(candidate.LLM); err != nil {
		return nil, fmt.Errorf("selector reload: %w", err)
	}
	rollback := func(_ context.Context) error {
		// 失败时还原；忽略二次 reload 的错误（best-effort）
		return a.selector.Reload(previous)
	}
	return rollback, nil
}
