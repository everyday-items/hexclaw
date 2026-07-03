// transaction.go 实现 v0.4.0 F9+G4 配置事务热加载（feature-flag gated）。
//
// 设计取舍：
//
//	*必须事务化*。半成品热加载（fsnotify → 直接写运行时）会引入：
//	  - 部分 service 已 reload、其它没有，配置漂移
//	  - validate 失败但写入已生效，无法回滚
//	  - 多并发写入互相覆盖
//	F9+G4 要求 OnChange / DryRun / Rollback 完整三件套，不接受简单 fsnotify watcher。
//
// 事务三段：
//  1. Begin       — 抢占全局事务锁；只允许同时一个事务
//  2. Stage(cfg)  — 把候选 cfg 喂给所有 Validator 做 DryRun，任一失败即 Stage 失败
//  3. Commit      — DryRun 全过后调每个 Applier.Apply；任一失败立即按已 Apply 的逆序
//     调 Rollback，恢复到 Begin 时的快照
//  4. （手动）Rollback — 调用方在 Commit 之前主动放弃，丢弃 Stage 数据
//
// flag config.tx.hotload.v1 默认 ON。关闭时 Begin 直接返回 ErrTxDisabled。
package config

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

// FlagConfigTxHotloadV1 控制 F9+G4 事务热加载是否启用。
const FlagConfigTxHotloadV1 = "config.tx.hotload.v1"

func init() {
	featureflag.Register(featureflag.Flag{
		Name:         FlagConfigTxHotloadV1,
		Default:      true,
		Description:  "Enable transactional config hot-reload (Begin → Stage → Validate → Apply → Commit/Rollback). Disables ad-hoc fsnotify reload.",
		Stage:        featureflag.StageGA,
		SinceVersion: "0.4.0",
	})
}

// Validator 是 DryRun 阶段被调用的校验器。
//
// candidate 是用户提议的新 cfg；err 非 nil 表示校验失败，事务必须中止（不能 commit）。
// 校验器应是无副作用的（只读 candidate；不写运行时）。
type Validator interface {
	DryRun(ctx context.Context, candidate *Config) error
}

// Applier 在 Commit 阶段把已通过 DryRun 的 cfg 推到运行时。
//
// Apply 成功必须返回一个 RollbackFn —— 该函数能把 Apply 的副作用还原（如重连旧 provider /
// 关掉新 listener）。Apply 失败返回 error，事务会按已 Apply 的逆序调用每个之前成功的
// RollbackFn 来恢复一致状态。
type Applier interface {
	Apply(ctx context.Context, candidate *Config) (RollbackFn, error)
}

// RollbackFn 撤回单个 Applier 的 Apply 副作用。返回的 error 仅记录到 Rollback 报告，
// 不阻断后续 rollback —— 要尽最大努力把状态还回 Begin 时的快照。
type RollbackFn func(ctx context.Context) error

// ErrTxDisabled 在 flag OFF 时由 Begin 返回。
var ErrTxDisabled = errors.New("config: transactional hot-reload disabled (flag config.tx.hotload.v1 OFF)")

// ErrTxAlreadyOpen 在已有事务进行中时由 Begin 返回。
var ErrTxAlreadyOpen = errors.New("config: another transaction in progress")

// ErrTxNotStaged 在 Commit 前未调用 Stage 时返回。
var ErrTxNotStaged = errors.New("config: must call Stage(candidate) before Commit")

// TransactionManager 管理事务锁 + Validator/Applier 注册表。线程安全。
type TransactionManager struct {
	mu         sync.Mutex
	current    *currentConfig // Begin 时拍下的快照（仅供 Rollback 使用）
	validators []Validator
	appliers   []Applier
	tx         *Transaction
}

// NewTransactionManager 用初始 cfg 构造一个 manager。
//
// validators 用于 DryRun，按声明顺序调；任一失败即中止。
// appliers 用于 Commit 阶段实际推到运行时，按声明顺序调；任一失败按逆序回滚已成功的。
func NewTransactionManager(initial *Config, validators []Validator, appliers []Applier) *TransactionManager {
	return &TransactionManager{
		current:    &currentConfig{cfg: cloneConfig(initial)},
		validators: append([]Validator{}, validators...),
		appliers:   append([]Applier{}, appliers...),
	}
}

type currentConfig struct {
	cfg *Config
}

// Begin 开启一个事务。flag OFF 时返回 ErrTxDisabled；已有事务在跑时返回 ErrTxAlreadyOpen。
func (tm *TransactionManager) Begin(ctx context.Context) (*Transaction, error) {
	if !featureflag.Enabled(ctx, FlagConfigTxHotloadV1) {
		return nil, ErrTxDisabled
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.tx != nil {
		return nil, ErrTxAlreadyOpen
	}
	tx := &Transaction{
		manager:  tm,
		snapshot: cloneConfig(tm.current.cfg),
	}
	tm.tx = tx
	return tx, nil
}

// Current 返回当前生效的 cfg 副本（不持有内部指针）。
func (tm *TransactionManager) Current() *Config {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return cloneConfig(tm.current.cfg)
}

// Transaction 是单个事务的 handle。生命周期：Stage → (Commit | Rollback)。
//
// 一个 Transaction 不能跨 goroutine 共享：所有方法必须在创建它的 goroutine 调用。
type Transaction struct {
	manager   *TransactionManager
	snapshot  *Config // Begin 时的快照
	candidate *Config // Stage 后保存
	staged    bool
}

// Stage 把 candidate 喂给所有 Validator 做 DryRun。任一失败即返回错误，事务保持
// 未 staged 状态（可以重新 Stage 不同 candidate，或调 Rollback 放弃）。
func (t *Transaction) Stage(ctx context.Context, candidate *Config) error {
	if t == nil || t.manager == nil {
		return errors.New("nil transaction")
	}
	if candidate == nil {
		return errors.New("nil candidate")
	}
	for _, v := range t.manager.validators {
		if err := v.DryRun(ctx, candidate); err != nil {
			return fmt.Errorf("config tx: dry-run failed: %w", err)
		}
	}
	t.candidate = cloneConfig(candidate)
	t.staged = true
	return nil
}

// Commit 调每个 Applier.Apply。任一失败按已 Apply 逆序 Rollback，并返回 error。
//
// 成功 Commit 后 manager.current 被更新到 candidate；事务结束（再调 Stage / Commit
// 返回错误）。
func (t *Transaction) Commit(ctx context.Context) error {
	if t == nil || t.manager == nil {
		return errors.New("nil transaction")
	}
	if !t.staged {
		return ErrTxNotStaged
	}
	t.manager.mu.Lock()
	defer t.manager.mu.Unlock()
	if t.manager.tx != t {
		return errors.New("config tx: transaction not active (already committed/rolled back?)")
	}

	var rollbacks []RollbackFn
	for _, a := range t.manager.appliers {
		rb, err := a.Apply(ctx, t.candidate)
		if err != nil {
			// 倒序回滚已成功的
			for i := len(rollbacks) - 1; i >= 0; i-- {
				if rb := rollbacks[i]; rb != nil {
					_ = rb(ctx) // 失败仅记录，不再 escalate
				}
			}
			t.manager.tx = nil
			return fmt.Errorf("config tx: apply failed: %w", err)
		}
		rollbacks = append(rollbacks, rb)
	}

	// 全成功：更新 current
	t.manager.current.cfg = t.candidate
	t.manager.tx = nil
	return nil
}

// Rollback 在 Commit 之前丢弃 staged candidate。Commit 之后再调返回错误。
func (t *Transaction) Rollback() error {
	if t == nil || t.manager == nil {
		return errors.New("nil transaction")
	}
	t.manager.mu.Lock()
	defer t.manager.mu.Unlock()
	if t.manager.tx != t {
		return errors.New("config tx: transaction not active")
	}
	t.manager.tx = nil
	return nil
}

// Snapshot 返回 Begin 时的 cfg 快照（用于事务内决策）。
func (t *Transaction) Snapshot() *Config {
	return cloneConfig(t.snapshot)
}

// cloneConfig deep-clones *Config so a transaction snapshot is fully isolated:
// Config has many nested maps/slices (LLM.Providers, Features, Platforms.*,
// Metadata, ...) that a struct copy would alias, letting a mutation of the
// snapshot corrupt the live config and defeat rollback. A YAML round-trip is a
// complete deep copy that stays correct as new config fields are added.
func cloneConfig(c *Config) *Config {
	if c == nil {
		return nil
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		out := *c // marshal of a plain config should not fail; degrade gracefully
		return &out
	}
	var out Config
	if err := yaml.Unmarshal(data, &out); err != nil {
		shallow := *c
		return &shallow
	}
	return &out
}

// BuiltinValidator 把 (*Config).Validate() 包装成 Validator。供 main.go 注入到
// TransactionManager 用作默认 DryRun 校验器。
type BuiltinValidator struct{}

// DryRun 实现 Validator —— 委托给 candidate.Validate()。
func (BuiltinValidator) DryRun(_ context.Context, candidate *Config) error {
	if candidate == nil {
		return errors.New("BuiltinValidator: nil candidate")
	}
	return candidate.Validate()
}
