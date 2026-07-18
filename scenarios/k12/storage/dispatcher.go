package k12storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"
)

// Consumer 一个 Outbox 消费者。Handle 必须幂等（at-least-once 投递；
// Dispatcher 以 (consumer, event_id) 去重挡重复投递，崩溃窗口内仍可能重放）。
type Consumer interface {
	Name() string
	Handle(ctx context.Context, ev OutboxEvent) error
}

// DispatcherMaxAttempts 连续失败进入 dead-letter 的阈值（§6.15）。
const DispatcherMaxAttempts = 5

// DispatcherPollInterval 轮询兜底间隔（写路径另有即时 nudge）。
const DispatcherPollInterval = 5 * time.Second

// Dispatcher 单进程 Outbox 投递器（§6.15）：顺序消费 pending 事件，
// 逐消费者投递并以 event_id 去重；全部消费者成功 → delivered；
// 失败累计 attempts，达上限 → dead（可取证，不静默丢弃）。
type Dispatcher struct {
	db        *sql.DB
	consumers []Consumer
	nudge     chan struct{}

	mu      sync.Mutex // 串行化 ProcessPending（顺序消费保证）
	started bool
}

// NewDispatcher 创建投递器并绑定 store 的追加通知（写提交后立即醒来）。
func NewDispatcher(store *Store, consumers ...Consumer) *Dispatcher {
	d := &Dispatcher{db: store.DB(), consumers: consumers, nudge: make(chan struct{}, 1)}
	store.SetOutboxNotifier(d.Nudge)
	return d
}

// NewSyncDispatcher 创建**同步**投递器：域写提交后在调用方 goroutine 内立即消费
// （测试/简单接线用——投递时序确定，不需 Start）。at-least-once 与去重语义不变。
func NewSyncDispatcher(store *Store, consumers ...Consumer) *Dispatcher {
	d := &Dispatcher{db: store.DB(), consumers: consumers, nudge: make(chan struct{}, 1)}
	store.SetOutboxNotifier(func() { _ = d.ProcessPending(context.Background()) })
	return d
}

// Nudge 通知有新事件（非阻塞合并）。
func (d *Dispatcher) Nudge() {
	select {
	case d.nudge <- struct{}{}:
	default:
	}
}

// Start 启动后台投递循环（nudge 即时 + 周期兜底轮询）。ctx 结束即停。
func (d *Dispatcher) Start(ctx context.Context) {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return
	}
	d.started = true
	d.mu.Unlock()
	go func() {
		ticker := time.NewTicker(DispatcherPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-d.nudge:
			case <-ticker.C:
			}
			if err := d.ProcessPending(ctx); err != nil && ctx.Err() == nil {
				log.Printf("[k12storage] outbox 投递一轮失败: %v", err)
			}
		}
	}()
}

// ProcessPending 同步消费当前全部 pending 事件（测试与启动补投也走此入口）。
func (d *Dispatcher) ProcessPending(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		events, err := PendingEvents(ctx, d.db, 100)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		for _, ev := range events {
			if err := d.deliverOne(ctx, ev); err != nil {
				return err
			}
		}
		if len(events) < 100 {
			return nil
		}
	}
}

// deliverOne 逐消费者投递一条事件。消费失败只记账（attempts/last_error/dead），
// 不向调用方冒泡业务错误——投影失败不撤销成功域写（§6.9），重试只补投影。
func (d *Dispatcher) deliverOne(ctx context.Context, ev OutboxEvent) error {
	var firstErr error
	for _, c := range d.consumers {
		consumed, err := d.alreadyConsumed(ctx, c.Name(), ev.EventID)
		if err != nil {
			return err
		}
		if consumed {
			continue // event_id 去重（§6.9）：重放/重试不重复投递
		}
		if err := c.Handle(ctx, ev); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("consumer %s: %w", c.Name(), err)
			}
			continue
		}
		if err := d.markConsumed(ctx, c.Name(), ev.EventID); err != nil {
			return err
		}
	}
	now := nowUnix()
	if firstErr == nil {
		_, err := d.db.ExecContext(ctx,
			`UPDATE outbox_events SET status = ?, updated_at = ? WHERE event_id = ?`,
			OutboxDelivered, now, ev.EventID)
		return err
	}
	attempts := ev.Attempts + 1
	status := OutboxPending
	if attempts >= DispatcherMaxAttempts {
		status = OutboxDead // dead-letter：last_error 留取证（§6.15）
		log.Printf("[k12storage] outbox 事件 %s (%s) 达重试上限进入 dead-letter: %v", ev.EventID, ev.EventType, firstErr)
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE outbox_events SET status = ?, attempts = ?, last_error = ?, updated_at = ? WHERE event_id = ?`,
		status, attempts, firstErr.Error(), now, ev.EventID)
	return err
}

func (d *Dispatcher) alreadyConsumed(ctx context.Context, consumer, eventID string) (bool, error) {
	var one int
	err := d.db.QueryRowContext(ctx,
		`SELECT 1 FROM outbox_consumptions WHERE consumer = ? AND event_id = ?`,
		consumer, eventID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("k12storage: 查消费去重: %w", err)
	}
	return true, nil
}

func (d *Dispatcher) markConsumed(ctx context.Context, consumer, eventID string) error {
	_, err := d.db.ExecContext(ctx, `INSERT OR IGNORE INTO outbox_consumptions
        (consumer, event_id, consumed_at) VALUES (?, ?, ?)`, consumer, eventID, nowUnix())
	if err != nil {
		return fmt.Errorf("k12storage: 记消费去重: %w", err)
	}
	return nil
}
