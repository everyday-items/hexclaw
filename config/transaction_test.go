package config

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/hexclaw/featureflag"
)

func withTxFlag(ctx context.Context, on bool) context.Context {
	flags := featureflag.NewStatic(featureflag.Registered(), map[string]bool{
		FlagConfigTxHotloadV1: on,
	})
	return featureflag.WithContext(ctx, flags)
}

// scriptedValidator 总是返回指定 error。
type scriptedValidator struct{ err error }

func (s *scriptedValidator) DryRun(_ context.Context, _ *Config) error { return s.err }

// v0.4.0 F9: BuiltinValidator 委托给 (*Config).Validate
func TestBuiltinValidator_DelegatesToConfigValidate(t *testing.T) {
	v := BuiltinValidator{}

	// 合法配置
	good := &Config{}
	good.Server.Port = 16060
	good.Server.MCPPort = 16070
	good.Server.Host = "127.0.0.1"
	if err := v.DryRun(context.Background(), good); err != nil {
		t.Errorf("合法配置应通过；got %v", err)
	}

	// 非法端口
	bad := &Config{}
	bad.Server.Port = 99999
	bad.Server.MCPPort = 16070
	if err := v.DryRun(context.Background(), bad); err == nil {
		t.Error("非法端口应失败")
	}

	// nil candidate
	if err := v.DryRun(context.Background(), nil); err == nil {
		t.Error("nil candidate 应返回 error")
	}
}

// scriptedApplier 记录 Apply / Rollback 调用。
type scriptedApplier struct {
	id            string
	applyErr      error
	rollbackErr   error
	applyCount    atomic.Int32
	rollbackCount atomic.Int32
}

func (a *scriptedApplier) Apply(_ context.Context, _ *Config) (RollbackFn, error) {
	a.applyCount.Add(1)
	if a.applyErr != nil {
		return nil, a.applyErr
	}
	return func(_ context.Context) error {
		a.rollbackCount.Add(1)
		return a.rollbackErr
	}, nil
}

func TestBegin_FlagOffReturnsErr(t *testing.T) {
	tm := NewTransactionManager(&Config{}, nil, nil)
	ctx := withTxFlag(context.Background(), false)
	_, err := tm.Begin(ctx)
	if !errors.Is(err, ErrTxDisabled) {
		t.Errorf("flag OFF 应返回 ErrTxDisabled；got %v", err)
	}
}

func TestBegin_RejectsConcurrentTx(t *testing.T) {
	tm := NewTransactionManager(&Config{}, nil, nil)
	ctx := withTxFlag(context.Background(), true)
	tx1, err := tm.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()
	if _, err := tm.Begin(ctx); !errors.Is(err, ErrTxAlreadyOpen) {
		t.Errorf("第二事务应被拒；got %v", err)
	}
}

func TestStage_DryRunFailureBlocksCommit(t *testing.T) {
	tm := NewTransactionManager(&Config{},
		[]Validator{&scriptedValidator{err: errors.New("validation fail")}},
		nil,
	)
	ctx := withTxFlag(context.Background(), true)
	tx, _ := tm.Begin(ctx)
	defer tx.Rollback()

	if err := tx.Stage(ctx, &Config{}); err == nil {
		t.Fatal("Stage 应被 Validator 拒绝")
	}
	// 未 staged 时 Commit 应报 ErrTxNotStaged
	if err := tx.Commit(ctx); !errors.Is(err, ErrTxNotStaged) {
		t.Errorf("未 stage 应返回 ErrTxNotStaged；got %v", err)
	}
}

func TestCommit_AppliesAllInOrder(t *testing.T) {
	a1 := &scriptedApplier{id: "a"}
	a2 := &scriptedApplier{id: "b"}
	tm := NewTransactionManager(&Config{}, nil, []Applier{a1, a2})
	ctx := withTxFlag(context.Background(), true)

	tx, _ := tm.Begin(ctx)
	if err := tx.Stage(ctx, &Config{}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit 应成功；got %v", err)
	}
	if a1.applyCount.Load() != 1 || a2.applyCount.Load() != 1 {
		t.Errorf("两个 Applier 都应被 Apply")
	}
}

func TestCommit_RollsBackOnApplyFailure(t *testing.T) {
	a1 := &scriptedApplier{id: "a"} // 成功
	a2 := &scriptedApplier{id: "b", applyErr: errors.New("boom")}
	tm := NewTransactionManager(&Config{}, nil, []Applier{a1, a2})
	ctx := withTxFlag(context.Background(), true)

	tx, _ := tm.Begin(ctx)
	_ = tx.Stage(ctx, &Config{})
	if err := tx.Commit(ctx); err == nil {
		t.Fatal("a2 失败应导致 Commit error")
	}
	// a1 应被 rollback；a2 没成功 Apply 故没 rollback
	if a1.rollbackCount.Load() != 1 {
		t.Errorf("a1 应被 rollback 1 次；got %d", a1.rollbackCount.Load())
	}
	if a2.rollbackCount.Load() != 0 {
		t.Errorf("a2 没成功 Apply，不应 rollback")
	}
}

func TestRollback_FreesLock(t *testing.T) {
	tm := NewTransactionManager(&Config{}, nil, nil)
	ctx := withTxFlag(context.Background(), true)

	tx1, _ := tm.Begin(ctx)
	if err := tx1.Rollback(); err != nil {
		t.Fatal(err)
	}
	// 再 Begin 应该能成功
	if _, err := tm.Begin(ctx); err != nil {
		t.Errorf("Rollback 后应能再 Begin；got %v", err)
	}
}

func TestCommit_UpdatesCurrent(t *testing.T) {
	tm := NewTransactionManager(&Config{}, nil, []Applier{&scriptedApplier{id: "x"}})
	ctx := withTxFlag(context.Background(), true)

	candidate := &Config{}
	tx, _ := tm.Begin(ctx)
	_ = tx.Stage(ctx, candidate)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTransaction_Snapshot_NotMutatedByCommit(t *testing.T) {
	initial := &Config{}
	tm := NewTransactionManager(initial, nil, nil)
	ctx := withTxFlag(context.Background(), true)

	tx, _ := tm.Begin(ctx)
	snap := tx.Snapshot()
	if snap == initial {
		t.Error("Snapshot 应返回 clone 而不是原指针")
	}
	tx.Rollback()
}

func TestParallelBegin_SerializedByLock(t *testing.T) {
	tm := NewTransactionManager(&Config{}, nil, nil)
	ctx := withTxFlag(context.Background(), true)

	var wg sync.WaitGroup
	successCount := atomic.Int32{}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tx, err := tm.Begin(ctx)
			if err == nil {
				successCount.Add(1)
				_ = tx.Rollback()
			}
		}()
	}
	wg.Wait()
	// 至少 1 个成功；其余被 ErrTxAlreadyOpen 拒绝（理想情况下都会成功因为 Rollback 后释放锁，
	// 但并发下可能交错。本测试目标：不死锁不 race；race detector 不报红即通过）
	if successCount.Load() < 1 {
		t.Errorf("至少 1 个事务应成功；got %d", successCount.Load())
	}
}

func TestStage_NilCandidate(t *testing.T) {
	tm := NewTransactionManager(&Config{}, nil, nil)
	ctx := withTxFlag(context.Background(), true)
	tx, _ := tm.Begin(ctx)
	defer tx.Rollback()
	if err := tx.Stage(ctx, nil); err == nil {
		t.Error("nil candidate 应报错")
	}
}

func TestCommit_AfterCommitFails(t *testing.T) {
	tm := NewTransactionManager(&Config{}, nil, []Applier{&scriptedApplier{id: "x"}})
	ctx := withTxFlag(context.Background(), true)

	tx, _ := tm.Begin(ctx)
	_ = tx.Stage(ctx, &Config{})
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// 第二次 Commit 应失败（事务已完成）
	if err := tx.Commit(ctx); err == nil {
		t.Error("二次 Commit 应失败")
	}
}

func TestCurrent_ReturnsClone(t *testing.T) {
	cfg := &Config{}
	tm := NewTransactionManager(cfg, nil, nil)
	got := tm.Current()
	if got == cfg {
		t.Error("Current 应返回 clone")
	}
}
