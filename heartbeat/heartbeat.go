// Package heartbeat 提供 Agent 主动巡查能力
//
// 将 Agent 从被动响应变为主动服务：
//   - 配置心跳间隔和巡查任务
//   - Agent 空闲时自动唤醒，执行巡查逻辑
//   - 发现需要关注的事项时主动通知用户
//
// 巡查任务通过 HEARTBEAT.md 文件定义，包含：
//   - 检查项列表（邮件、日历、待办等）
//   - 通知条件（什么时候应该通知用户）
//   - 优先级规则
//
// 对标 OpenClaw 的 HEARTBEAT.md 机制。
//
// 用法：
//
//	hb := heartbeat.New(heartbeat.Config{
//	    Interval:     30 * time.Minute,
//	    Instructions: "检查邮件收件箱，如果有紧急邮件就通知我",
//	})
//	hb.Start(ctx, func(ctx context.Context, inst string) (string, error) {
//	    // 调用引擎处理
//	})
package heartbeat

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Config 心跳配置
type Config struct {
	Enabled      bool          `yaml:"enabled"`       // 是否启用心跳
	Interval     time.Duration `yaml:"interval"`      // 心跳间隔，默认 30 分钟
	Instructions string        `yaml:"instructions"`  // 巡查指令（内联或文件路径）
	LightContext bool          `yaml:"light_context"` // 精简上下文模式
	QuietHours   QuietHours    `yaml:"quiet_hours"`   // 免打扰时段
}

// QuietHours 免打扰时段
type QuietHours struct {
	Enabled bool   `yaml:"enabled"`
	Start   string `yaml:"start"` // 开始时间 "22:00"
	End     string `yaml:"end"`   // 结束时间 "08:00"
}

// Executor 心跳执行回调
//
// instructions 为巡查指令，返回值为 Agent 的处理结果。
// 如果结果不为空，表示有需要通知用户的内容。
type Executor func(ctx context.Context, instructions string) (result string, err error)

// NotifyFunc 通知回调
//
// 当心跳发现需要关注的事项时调用，负责将结果推送给用户。
type NotifyFunc func(ctx context.Context, result string) error

// Heartbeat Agent 主动巡查器
//
// 按配置间隔唤醒 Agent，执行巡查任务。
// 如果 Agent 发现有需要关注的事项，通过 NotifyFunc 通知用户。
type Heartbeat struct {
	mu           sync.RWMutex
	config       Config
	executor     Executor
	notifier     NotifyFunc
	instructions string // 解析后的巡查指令
	stopCh       chan struct{}
	stopped      bool
	lastRunAt    time.Time
	runCount     int
}

// New 创建心跳巡查器
func New(cfg Config) *Heartbeat {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Minute
	}

	hb := &Heartbeat{
		config: cfg,
	}

	// 加载巡查指令
	hb.instructions = hb.loadInstructions(cfg.Instructions)

	return hb
}

// Start 启动心跳巡查
//
// executor: Agent 处理回调，传入巡查指令，返回处理结果
// notifier: 通知回调，将有价值的结果推送给用户
func (h *Heartbeat) Start(_ context.Context, executor Executor, notifier NotifyFunc) {
	h.mu.Lock()
	h.executor = executor
	h.notifier = notifier
	h.stopCh = make(chan struct{})
	h.stopped = false
	h.mu.Unlock()

	go h.runLoop()
	log.Printf("Heartbeat 已启动: 间隔=%s", h.config.Interval)
}

// Stop 停止心跳巡查
func (h *Heartbeat) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.stopped {
		h.stopped = true
		close(h.stopCh)
		log.Println("Heartbeat 已停止")
	}
}

// Stats 心跳统计
type Stats struct {
	RunCount  int       `json:"run_count"`   // 执行次数
	LastRunAt time.Time `json:"last_run_at"` // 上次执行时间
	Interval  string    `json:"interval"`    // 心跳间隔
	Enabled   bool      `json:"enabled"`     // 是否启用
}

// Stats 获取心跳统计信息
func (h *Heartbeat) Stats() Stats {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return Stats{
		RunCount:  h.runCount,
		LastRunAt: h.lastRunAt,
		Interval:  h.config.Interval.String(),
		Enabled:   h.config.Enabled && !h.stopped,
	}
}

// --- 内部方法 ---

// runLoop 心跳主循环
func (h *Heartbeat) runLoop() {
	ticker := time.NewTicker(h.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.beat()
		}
	}
}

// beat 执行一次心跳
func (h *Heartbeat) beat() {
	// 检查免打扰时段
	if h.isQuietHour() {
		return
	}

	h.mu.RLock()
	executor := h.executor
	notifier := h.notifier
	instructions := h.instructions
	h.mu.RUnlock()

	if executor == nil || instructions == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	log.Println("Heartbeat: 开始巡查")

	// 执行巡查
	result, err := executor(ctx, instructions)
	if err != nil {
		log.Printf("Heartbeat: 巡查执行失败: %v", err)
		return
	}

	// 更新统计
	h.mu.Lock()
	h.lastRunAt = time.Now()
	h.runCount++
	h.mu.Unlock()

	// 如果有结果且 notifier 存在，通知用户
	if result != "" && notifier != nil {
		result = strings.TrimSpace(result)
		// 检查 Agent 是否认为有需要通知的内容
		// Agent 返回空字符串或 "NO_NOTIFY" 表示无需通知
		if result != "" && result != "NO_NOTIFY" && result != "无需通知" {
			if err := notifier(ctx, result); err != nil {
				log.Printf("Heartbeat: 通知发送失败: %v", err)
			} else {
				log.Printf("Heartbeat: 已发送通知")
			}
		}
	}
}

// isQuietHour 检查当前是否在免打扰时段
func (h *Heartbeat) isQuietHour() bool {
	if !h.config.QuietHours.Enabled {
		return false
	}

	now := time.Now()
	startH, startM := parseHourMinute(h.config.QuietHours.Start)
	endH, endM := parseHourMinute(h.config.QuietHours.End)

	nowMinutes := now.Hour()*60 + now.Minute()
	startMinutes := startH*60 + startM
	endMinutes := endH*60 + endM

	if startMinutes <= endMinutes {
		// 同一天内：如 09:00 - 18:00
		return nowMinutes >= startMinutes && nowMinutes < endMinutes
	}
	// 跨天：如 22:00 - 08:00
	return nowMinutes >= startMinutes || nowMinutes < endMinutes
}

// loadInstructions 加载巡查指令
//
// 支持两种格式：
//   - 直接文本：作为巡查指令
//   - 文件路径：从文件中读取（支持 Markdown）
func (h *Heartbeat) loadInstructions(input string) string {
	if input == "" {
		return defaultInstructions
	}

	// 检查是否为文件路径
	if strings.HasSuffix(input, ".md") || strings.HasSuffix(input, ".txt") {
		// 展开 ~
		if strings.HasPrefix(input, "~/") {
			home, _ := os.UserHomeDir()
			input = filepath.Join(home, input[2:])
		}
		data, err := os.ReadFile(input)
		if err != nil {
			log.Printf("Heartbeat: 读取指令文件失败: %v，使用默认指令", err)
			return defaultInstructions
		}
		return string(data)
	}

	return input
}

// parseHourMinute 解析 "HH:MM" 格式的时间
func parseHourMinute(s string) (int, int) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0
	}
	h, m := 0, 0
	for _, c := range parts[0] {
		if c < '0' || c > '9' {
			return 0, 0 // 非数字字符，返回零值
		}
		h = h*10 + int(c-'0')
	}
	for _, c := range parts[1] {
		if c < '0' || c > '9' {
			return 0, 0 // 非数字字符，返回零值
		}
		m = m*10 + int(c-'0')
	}
	// 范围校验
	if h > 23 || m > 59 {
		return 0, 0
	}
	return h, m
}

// defaultInstructions 默认巡查指令
const defaultInstructions = `你正在执行定期心跳巡查。请检查以下事项：

1. 是否有需要用户关注的紧急事项
2. 是否有待处理的提醒或任务
3. 是否有异常状态需要报告

如果没有需要通知的内容，请回复 "NO_NOTIFY"。
如果有需要通知的内容，请简洁明了地说明情况。`
