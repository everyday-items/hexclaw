package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/everyday-items/hexclaw/adapter"
	"github.com/everyday-items/hexclaw/config"
)

// RateLimitLayer 速率限制层 (Layer 2)
//
// 基于滑动窗口的速率限制：
//   - 每用户每分钟请求数限制
//   - 每用户每小时请求数限制
//
// 使用内存存储计数器，重启后重置。
type RateLimitLayer struct {
	cfg     config.RateLimitConfig
	mu      sync.Mutex
	windows map[string]*userWindow // key: userID
}

// userWindow 用户请求窗口
type userWindow struct {
	minuteRequests []time.Time // 最近一分钟内的请求时间戳
	hourRequests   []time.Time // 最近一小时内的请求时间戳
}

// NewRateLimitLayer 创建速率限制层
func NewRateLimitLayer(cfg config.RateLimitConfig) *RateLimitLayer {
	return &RateLimitLayer{
		cfg:     cfg,
		windows: make(map[string]*userWindow),
	}
}

func (l *RateLimitLayer) Name() string { return "rate_limit" }

// Check 检查请求是否超过速率限制
func (l *RateLimitLayer) Check(_ context.Context, msg *adapter.Message) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	userID := msg.UserID
	if userID == "" {
		userID = "_anonymous"
	}

	w, ok := l.windows[userID]
	if !ok {
		w = &userWindow{}
		l.windows[userID] = w
	}

	// 清理过期记录
	minuteAgo := now.Add(-1 * time.Minute)
	hourAgo := now.Add(-1 * time.Hour)
	w.minuteRequests = filterAfter(w.minuteRequests, minuteAgo)
	w.hourRequests = filterAfter(w.hourRequests, hourAgo)

	// 清理无活动用户的窗口，防止内存泄漏
	if len(w.minuteRequests) == 0 && len(w.hourRequests) == 0 && ok {
		delete(l.windows, userID)
		// 重新创建当前用户的窗口以继续处理
		w = &userWindow{}
		l.windows[userID] = w
	}

	// 检查每分钟限制
	if l.cfg.RequestsPerMinute > 0 && len(w.minuteRequests) >= l.cfg.RequestsPerMinute {
		return &GatewayError{
			Layer:   "rate_limit",
			Code:    "minute_exceeded",
			Message: "请求过于频繁，请稍后再试",
		}
	}

	// 检查每小时限制
	if l.cfg.RequestsPerHour > 0 && len(w.hourRequests) >= l.cfg.RequestsPerHour {
		return &GatewayError{
			Layer:   "rate_limit",
			Code:    "hour_exceeded",
			Message: "已达到每小时请求上限，请稍后再试",
		}
	}

	// 记录本次请求
	w.minuteRequests = append(w.minuteRequests, now)
	w.hourRequests = append(w.hourRequests, now)

	return nil
}

// filterAfter 过滤出 after 之后的时间戳
func filterAfter(times []time.Time, after time.Time) []time.Time {
	result := times[:0]
	for _, t := range times {
		if t.After(after) {
			result = append(result, t)
		}
	}
	return result
}
