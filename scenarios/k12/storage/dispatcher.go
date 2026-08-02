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

const dispatcherBatchSize = 100

// Dispatcher 单进程 Outbox 投递器（§6.15）：顺序消费 pending 事件，
// 逐消费者投递并以 event_id 去重；全部消费者成功 → delivered；
// 失败累计 attempts，达上限 → dead（可取证，不静默丢弃）。
type Dispatcher struct {
	db        *sql.DB
	consumers []Consumer
	nudge     chan struct{}

	startMu     sync.Mutex
	processGate chan struct{} // 可取消的串行门闩，保证 ProcessPending 顺序消费
	started     bool
}

func newDispatcher(db *sql.DB, consumers ...Consumer) *Dispatcher {
	d := &Dispatcher{
		db:          db,
		consumers:   consumers,
		nudge:       make(chan struct{}, 1),
		processGate: make(chan struct{}, 1),
	}
	d.processGate <- struct{}{}
	return d
}

// NewDispatcher 创建投递器并绑定 store 的追加通知（写提交后立即醒来）。
func NewDispatcher(store *Store, consumers ...Consumer) *Dispatcher {
	d := newDispatcher(store.DB(), consumers...)
	store.SetOutboxNotifier(d.Nudge)
	return d
}

// NewSyncDispatcher 创建**同步**投递器：域写提交后在调用方 goroutine 内立即消费
// （测试/简单接线用——投递时序确定，不需 Start）。at-least-once 与去重语义不变。
func NewSyncDispatcher(store *Store, consumers ...Consumer) *Dispatcher {
	d := newDispatcher(store.DB(), consumers...)
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
	d.startMu.Lock()
	if d.started {
		d.startMu.Unlock()
		return
	}
	d.started = true
	d.startMu.Unlock()
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

// ProcessPending 同步消费调用开始时可见的全部 pending 事件（测试与启动补投也走此入口）。
// 同一次调用中每条事件至多尝试一次；调用期间新增事件留给下一次调用。
func (d *Dispatcher) ProcessPending(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.processGate:
	}
	defer func() { d.processGate <- struct{}{} }()

	horizon, ok, err := pendingEventHorizon(ctx, d.db)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	var after *pendingEventCursor
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		events, err := pendingEventsPage(ctx, d.db, horizon, after, dispatcherBatchSize)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		for _, ev := range events {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := d.deliverOne(ctx, ev); err != nil {
				return err
			}
		}
		if len(events) < dispatcherBatchSize {
			return nil
		}
		last := events[len(events)-1]
		after = &pendingEventCursor{CreatedAt: last.CreatedAt, EventID: last.EventID}
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
