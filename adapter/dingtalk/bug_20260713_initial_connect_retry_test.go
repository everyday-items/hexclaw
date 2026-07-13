package dingtalk

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// BUG-20260713：官方 Stream SDK 只会在「首次连接成功后的断线」触发 AutoReconnect；
// 首次 GetConnectionEndpoint 超时会直接从 Start 返回。适配器此前也随即退出连接 goroutine，
// 一次瞬时网络抖动就会让钉钉永久无回复，直到整个实例被 stop 后再 start。
func TestDingtalkInitialConnectRetriesUntilRecovered_BUG20260713(t *testing.T) {
	var attempts atomic.Int32
	var failures atomic.Int32

	err := retryInitialStreamConnect(
		context.Background(),
		func(context.Context) error {
			if attempts.Add(1) < 3 {
				return errors.New("temporary gateway timeout")
			}
			return nil
		},
		func(error) { failures.Add(1) },
		func(int) time.Duration { return 0 },
	)

	if err != nil {
		t.Fatalf("瞬时失败后应自动恢复，got err=%v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("连接尝试次数 = %d，want 3（两次失败后第三次成功）", got)
	}
	if got := failures.Load(); got != 2 {
		t.Fatalf("失败回调次数 = %d，want 2", got)
	}
}

func TestDingtalkInitialConnectRetryIsCancelledByStop_BUG20260713(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- retryInitialStreamConnect(
			ctx,
			func(context.Context) error {
				close(started)
				return errors.New("offline")
			},
			nil,
			func(int) time.Duration { return time.Hour },
		)
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("首次连接尝试未启动")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("取消后的错误 = %v，want context.Canceled", err)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("Stop/ctx cancel 未立即打断首次连接重试退避")
	}
}
