package adapter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

type sendTask struct {
	ctx    context.Context
	chatID string
	reply  *Reply
	done   chan error
}

// SendQueue serializes outbound sends for a single adapter instance.
type SendQueue struct {
	send        func(context.Context, string, *Reply) error
	minInterval time.Duration
	tasks       chan *sendTask
	stopCh      chan struct{}
	wg          sync.WaitGroup
	stopOnce    sync.Once
	stopped     atomic.Bool
}

// NewSendQueue creates a bounded, rate-limited send queue.
func NewSendQueue(ratePerSecond, queueSize int, send func(context.Context, string, *Reply) error) *SendQueue {
	if ratePerSecond <= 0 {
		ratePerSecond = 5
	}
	if queueSize <= 0 {
		queueSize = 128
	}
	q := &SendQueue{
		send:        send,
		minInterval: time.Second / time.Duration(ratePerSecond),
		tasks:       make(chan *sendTask, queueSize),
		stopCh:      make(chan struct{}),
	}
	q.wg.Add(1)
	go q.run()
	return q
}

// NewPlatformSendQueue creates a send queue using provider-specific defaults.
func NewPlatformSendQueue(platform Platform, send func(context.Context, string, *Reply) error) *SendQueue {
	rate, size := sendQueueConfig(platform)
	return NewSendQueue(rate, size, send)
}

// Send enqueues an outbound send and waits for completion.
func (q *SendQueue) Send(ctx context.Context, chatID string, reply *Reply) error {
	if reply == nil {
		return nil
	}
	if q == nil {
		return fmt.Errorf("send queue 未初始化")
	}
	if q.stopped.Load() {
		return fmt.Errorf("send queue 已停止")
	}
	task := &sendTask{
		ctx:    ctx,
		chatID: chatID,
		reply:  reply,
		done:   make(chan error, 1),
	}
	select {
	case q.tasks <- task:
	case <-q.stopCh:
		return fmt.Errorf("send queue 已停止")
	case <-ctx.Done():
		return ctx.Err()
	}

	select {
	case err := <-task.done:
		return err
	case <-q.stopCh:
		return fmt.Errorf("send queue 已停止")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop stops the queue worker and rejects future sends.
func (q *SendQueue) Stop(context.Context) error {
	if q == nil {
		return nil
	}
	q.stopOnce.Do(func() {
		q.stopped.Store(true)
		close(q.stopCh)
	})
	q.wg.Wait()

	// 排空残留任务，释放 ctx/reply 引用并通知调用方
	for {
		select {
		case task := <-q.tasks:
			if task != nil {
				task.done <- fmt.Errorf("send queue 已停止")
			}
		default:
			return nil
		}
	}
}

func (q *SendQueue) run() {
	defer q.wg.Done()

	var lastSend time.Time
	for {
		var task *sendTask
		select {
		case <-q.stopCh:
			return // 残留任务由 Stop 方法排空
		case task = <-q.tasks:
		}
		if task == nil {
			continue
		}

		if q.minInterval > 0 && !lastSend.IsZero() {
			wait := q.minInterval - time.Since(lastSend)
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-q.stopCh:
					timer.Stop()
					task.done <- fmt.Errorf("send queue 已停止")
					return
				case <-task.ctx.Done():
					timer.Stop()
					task.done <- task.ctx.Err()
					continue
				}
			}
		}

		if q.send == nil {
			task.done <- fmt.Errorf("send queue 未配置发送函数")
			continue
		}

		err := q.send(task.ctx, task.chatID, task.reply)
		lastSend = time.Now()
		task.done <- err
	}
}

// HealthChecker exposes adapter-specific health information.
type HealthChecker interface {
	Health(ctx context.Context) error
}

func sendQueueConfig(platform Platform) (ratePerSecond, queueSize int) {
	switch platform {
	case PlatformSlack:
		return 1, 128
	case PlatformDiscord, PlatformTelegram:
		return 5, 128
	case PlatformFeishu, PlatformDingtalk, PlatformWecom, PlatformWechat:
		return 3, 128
	case PlatformLINE, PlatformWhatsApp, PlatformMatrix:
		return 2, 128
	default:
		return 5, 128
	}
}
