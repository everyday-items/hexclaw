package sqliteutil

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hexagon-codes/toolkit/util/retry"
)

// IsBusyError 判断错误是否为 SQLite 写写冲突类的可重试错误。
//
// modernc.org/sqlite 在 WAL 模式下并发写时会抛出两类瞬时错误：
//   - SQLITE_BUSY (5)：另一个连接持有写锁，busy_timeout 到期仍未拿到锁
//   - SQLITE_BUSY_SNAPSHOT (517)：WAL 快照过期，写写冲突（busy_timeout 不覆盖此码）
//
// 这两类错误是瞬时的——冲突方提交后重试通常即可成功，因此应有限退避重试，
// 而不是当作永久失败直接吞掉或上抛。错误文本统一为 "database is locked"，
// 部分场景带 "(5)" / "(517)" / "SQLITE_BUSY" 等标记，这里做大小写无关的子串匹配。
func IsBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "database is locked"):
		return true
	case strings.Contains(msg, "database table is locked"):
		return true
	case strings.Contains(msg, "sqlite_busy"):
		return true
	case strings.Contains(msg, "(517)"):
		return true
	case strings.Contains(msg, "(5)"):
		return true
	default:
		return false
	}
}

// busyRetryOptions 返回针对 BUSY/517 的有限退避重试选项。
//
// 退避策略：最多 5 次尝试，初始 10ms，指数退避（×2），上限 200ms。
// 总等待最坏约 10+20+40+80 = 150ms，足以让大多数瞬时写写冲突的对方提交，
// 同时不会让单次写阻塞过久。仅对 IsBusyError 命中的错误重试，其余错误立即返回。
func busyRetryOptions() []retry.Option {
	return []retry.Option{
		retry.Attempts(5),
		retry.Delay(10 * time.Millisecond),
		retry.MaxDelay(200 * time.Millisecond),
		retry.Multiplier(2.0),
		retry.RetryIf(IsBusyError),
	}
}

// RetryOnBusy 对写操作做 BUSY/517 有限退避重试。
//
// fn 应为幂等的写入闭包（如插入消息）。仅当错误命中 IsBusyError 时才重试，
// 其余错误（包括业务错误）原样返回，不被 retry 包装。命中重试上限后返回
// 最后一次的原始 BUSY 错误（已解包 retry.ErrMaxAttemptsReached），保持错误语义清晰。
func RetryOnBusy(ctx context.Context, fn func() error) error {
	// 记录最后一次原始错误，避免 retry.Do 把它包进 ErrMaxAttemptsReached 哨兵，
	// 让调用方拿到的仍是可识别的 BUSY 错误（IsBusyError 仍为真）。
	var lastErr error
	wrapped := func() error {
		lastErr = fn()
		return lastErr
	}
	if err := retry.DoWithContext(ctx, wrapped, busyRetryOptions()...); err != nil {
		// retry 在达到上限时返回 ErrMaxAttemptsReached 包装错误；统一回退到原始错误，
		// 使上层 IsBusyError 判断与日志保持一致。
		if errors.Is(err, retry.ErrMaxAttemptsReached) && lastErr != nil {
			return lastErr
		}
		return err
	}
	return nil
}
