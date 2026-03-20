package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
)

// RateLimitLayer 速率限制层 (Layer 2)
//
// 基于滑动窗口的速率限制：
//   - 每用户每分钟请求数限制
//   - 每用户每小时请求数限制
//
// 使用内存存储计数器，重启后重置。
// 空窗口清理由后台 goroutine 定期执行（每 30 秒），
// 避免在每次 Check 时做 O(n) 全量扫描。
type RateLimitLayer struct {
	cfg       config.RateLimitConfig
	mu        sync.Mutex
	windows   map[string]*userWindow // key: userID
	cleanOnce sync.Once
	stopClean chan struct{}
}

// userWindow 用户请求窗口
type userWindow struct {
	minuteRequests []time.Time // 最近一分钟内的请求时间戳
	hourRequests   []time.Time // 最近一小时内的请求时间戳
}

// NewRateLimitLayer 创建速率限制层
func NewRateLimitLayer(cfg config.RateLimitConfig) *RateLimitLayer {
	l := &RateLimitLayer{
		cfg:       cfg,
		windows:   make(map[string]*userWindow),
		stopClean: make(chan struct{}),
	}
	go l.periodicCleanup()
	return l
}

// periodicCleanup 后台定期清理空窗口，避免每次 Check 做 O(n) 扫描
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

	now := time.Now()
	minuteAgo := now.Add(-1 * time.Minute)
	hourAgo := now.Add(-1 * time.Hour)

	for uid, uw := range l.windows {
		uw.minuteRequests = filterAfterCopy(uw.minuteRequests, minuteAgo)
		uw.hourRequests = filterAfterCopy(uw.hourRequests, hourAgo)
		if len(uw.minuteRequests) == 0 && len(uw.hourRequests) == 0 {
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

	// 仅清理当前用户的过期记录（O(1) per user, not O(N) global scan）
	minuteAgo := now.Add(-1 * time.Minute)
	hourAgo := now.Add(-1 * time.Hour)
	w.minuteRequests = filterAfterCopy(w.minuteRequests, minuteAgo)
	w.hourRequests = filterAfterCopy(w.hourRequests, hourAgo)

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

// filterAfterCopy 过滤出 after 之后的时间戳
// 使用新切片避免旧引用在底层数组中驻留（[:0] 会保留已过滤元素的引用）
func filterAfterCopy(times []time.Time, after time.Time) []time.Time {
	var result []time.Time
	for _, t := range times {
		if t.After(after) {
			result = append(result, t)
		}
	}
	return result
}
