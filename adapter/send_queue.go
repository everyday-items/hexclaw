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
	send   func(context.Context, string, *Reply) error
	done   chan error
}

// SendQueue serializes outbound sends for a single adapter instance.
type SendQueue struct {
	send        func(context.Context, string, *Reply) error
	minInterval time.Duration
	tasks       chan *sendTask
	stopCh      chan struct{}
	// admissionMu linearizes the stopped check with registration in admitted.
	// Stop closes the gate while holding this mutex, then waits until every Send
	// that crossed it has returned before draining the channel.
	admissionMu sync.Mutex
	admitted    sync.WaitGroup
	workerCtx   context.Context
	workerStop  context.CancelFunc
	doneCh      chan struct{}
	cleanupDone chan struct{}
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
	workerCtx, workerStop := context.WithCancel(context.Background())
	q := &SendQueue{
		send:        send,
		minInterval: time.Second / time.Duration(ratePerSecond),
		tasks:       make(chan *sendTask, queueSize),
		stopCh:      make(chan struct{}),
		workerCtx:   workerCtx,
		workerStop:  workerStop,
		doneCh:      make(chan struct{}),
		cleanupDone: make(chan struct{}),
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
	return q.SendWith(ctx, chatID, reply, nil)
}

// SendWith enqueues one send through the same bounded/rate-limited worker but
// allows an adapter-specific sender for this task. DingTalk uses it to retain
// the provider processQueryKey without creating a second queue that could
// violate the platform-wide rate limit. A nil sender uses the queue default.
func (q *SendQueue) SendWith(ctx context.Context, chatID string, reply *Reply, send func(context.Context, string, *Reply) error) error {
	if reply == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if q == nil {
		return fmt.Errorf("send queue 未初始化")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	q.admissionMu.Lock()
	if q.stopped.Load() {
		q.admissionMu.Unlock()
		return fmt.Errorf("send queue 已停止")
	}
	q.admitted.Add(1)
	q.admissionMu.Unlock()
	defer q.admitted.Done()

	task := &sendTask{
		ctx:    ctx,
		chatID: chatID,
		reply:  reply,
		send:   send,
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
func (q *SendQueue) Stop(ctx context.Context) error {
	if q == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	q.stopOnce.Do(func() {
		q.admissionMu.Lock()
		q.stopped.Store(true)
		q.workerStop()
		close(q.stopCh)
		q.admissionMu.Unlock()
		go q.finishStop()
	})
	select {
	case <-q.cleanupDone:
	case <-ctx.Done():
		select {
		case <-q.cleanupDone:
		default:
			return ctx.Err()
		}
	}
	return nil
}

// finishStop performs the non-cancellable half of shutdown. A Stop caller may
// time out while an adapter sender ignores cancellation; the cleanup keeps
// running, and a later Stop converges on cleanupDone without leaking queued
// task references.
func (q *SendQueue) finishStop() {
	q.admitted.Wait()
	<-q.doneCh
	// 排空残留任务，释放 ctx/reply 引用并通知调用方
	for {
		select {
		case task := <-q.tasks:
			if task != nil {
				task.done <- fmt.Errorf("send queue 已停止")
			}
		default:
			close(q.cleanupDone)
			return
		}
	}
}

func (q *SendQueue) run() {
	defer q.wg.Done()
	defer close(q.doneCh)

	var lastSend time.Time
	for {
		select {
		case <-q.stopCh:
			return
		default:
		}
		var task *sendTask
		select {
		case <-q.stopCh:
			return // 残留任务由 Stop 方法排空
		case task = <-q.tasks:
		}
		if task == nil {
			continue
		}
		if q.workerCtx.Err() != nil {
			task.done <- fmt.Errorf("send queue 已停止")
			return
		}
		if err := task.ctx.Err(); err != nil {
			task.done <- err
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

		send := task.send
		if send == nil {
			send = q.send
		}
		if send == nil {
			task.done <- fmt.Errorf("send queue 未配置发送函数")
			continue
		}

		sendCtx, cancelSend := context.WithCancel(task.ctx)
		stopCancel := context.AfterFunc(q.workerCtx, cancelSend)
		// context.AfterFunc runs asynchronously. Re-check synchronously after it
		// is installed so a task selected concurrently with Stop cannot call the
		// adapter sender merely because the cancellation callback has not run yet.
		if q.workerCtx.Err() != nil {
			stopCancel()
			cancelSend()
			task.done <- fmt.Errorf("send queue 已停止")
			return
		}
		if err := task.ctx.Err(); err != nil {
			stopCancel()
			cancelSend()
			task.done <- err
			continue
		}
		err := send(sendCtx, task.chatID, task.reply)
		stopCancel()
		cancelSend()
		lastSend = time.Now()
		task.done <- err
	}
}

// HealthChecker exposes adapter-specific health information.
type HealthChecker interface {
	Health(ctx context.Context) error
}

// ConfigValidator validates adapter configuration without requiring Start().
// Used by the channel config test endpoint for pre-save validation.
type ConfigValidator interface {
	ValidateConfig(ctx context.Context) error
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
