package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/toolkit/util/rate"
)

// RateLimitLayer 速率限制层 (Layer 2)
//
// 基于 toolkit/util/rate.SlidingWindow 的速率限制：
//   - 每用户每分钟请求数限制
//   - 每用户每小时请求数限制
//
// 使用内存存储计数器，重启后重置。
// 空窗口清理由后台 goroutine 定期执行（每 30 秒），
// 回收不活跃用户的限流器以防止内存泄漏。
type RateLimitLayer struct {
	cfg       config.RateLimitConfig
	mu        sync.Mutex
	windows   map[string]*userWindows // key: userID
	stopClean chan struct{}
}

// userWindows 用户的滑动窗口限流器
type userWindows struct {
	minute *rate.SlidingWindow // 每分钟限流（nil 表示未配置）
	hour   *rate.SlidingWindow // 每小时限流（nil 表示未配置）
}

// NewRateLimitLayer 创建速率限制层
func NewRateLimitLayer(cfg config.RateLimitConfig) *RateLimitLayer {
	l := &RateLimitLayer{
		cfg:       cfg,
		windows:   make(map[string]*userWindows),
		stopClean: make(chan struct{}),
	}
	go l.periodicCleanup()
	return l
}

// newUserWindows 为用户创建滑动窗口限流器
func (l *RateLimitLayer) newUserWindows() *userWindows {
	uw := &userWindows{}
	if l.cfg.RequestsPerMinute > 0 {
		uw.minute = rate.MustNewSlidingWindow(l.cfg.RequestsPerMinute, time.Minute)
	}
	if l.cfg.RequestsPerHour > 0 {
		uw.hour = rate.MustNewSlidingWindow(l.cfg.RequestsPerHour, time.Hour)
	}
	return uw
}

// periodicCleanup 后台定期清理空窗口，避免不活跃用户占用内存
func (l *RateLimitLayer) periodicCleanup() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			l.cleanupEmptyWindows()
		case <-l.stopClean:
			return
		}
	}
}

func (l *RateLimitLayer) cleanupEmptyWindows() {
	l.mu.Lock()
	defer l.mu.Unlock()

	for uid, uw := range l.windows {
		minuteEmpty := uw.minute == nil || uw.minute.Count() == 0
		hourEmpty := uw.hour == nil || uw.hour.Count() == 0
		if minuteEmpty && hourEmpty {
			delete(l.windows, uid)
		}
	}

	const maxWindows = 100000
	if len(l.windows) > maxWindows {
		for uid := range l.windows {
			delete(l.windows, uid)
			if len(l.windows) <= maxWindows {
				break
			}
		}
	}
}

func (l *RateLimitLayer) Name() string { return "rate_limit" }

// Check 检查请求是否超过速率限制
func (l *RateLimitLayer) Check(_ context.Context, msg *adapter.Message) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	userID := msg.UserID
	if userID == "" {
		userID = "_anonymous"
	}

	w, ok := l.windows[userID]
	if !ok {
		w = l.newUserWindows()
		l.windows[userID] = w
	}

	// 检查每分钟限制
	if w.minute != nil && !w.minute.Allow() {
		return &GatewayError{
			Layer:   "rate_limit",
			Code:    "minute_exceeded",
			Message: "请求过于频繁，请稍后再试",
		}
	}

	// 检查每小时限制
	if w.hour != nil && !w.hour.Allow() {
		// 分钟窗口已消费了一个配额，但小时窗口被拒绝，
		// 这里无需回滚——分钟窗口多记录一次不影响正确性，
		// 因为小时限制更严格时，分钟窗口的多余记录会自然过期。
		return &GatewayError{
			Layer:   "rate_limit",
			Code:    "hour_exceeded",
			Message: "已达到每小时请求上限，请稍后再试",
		}
	}

	return nil
}
