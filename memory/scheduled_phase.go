package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ── 低频后台「做梦」相调度：墙钟持久化 + 开机补跑 ───────────────────────────
//
// 记忆的做梦诸相（轻相反思 / 深相整合 / 会话回放 / 画像蒸馏）都是低频后台维护。旧实现各用一个
// 「启动后计时」的 time.Ticker：关机即清零、重启从头计时——对「用一会就关」的桌面用户，深相
// （每周）几乎永不触发、轻相（每天）也常常落空（违背注释「cron 0 3 低频后台」本意，也照不出
// Claude/ChatGPT「跨会话沉淀」的体感）。
//
// 本调度改为**墙钟 + 持久化 last_run + 开机补跑**：每相记一个时间戳落 <memory dir>/.phase_state.json；
// 启动时若 now-last_run ≥ interval 就先在后台静默补跑一次，再恢复 ticker。首次启动只锚定不补跑
// （无历史可沉淀）。run 失败不推进墙钟 → 下次启动/tick 仍会补跑。诸相复用同一原语（StartScheduledPhase）。

const phaseStateFile = ".phase_state.json"

// 做梦诸相的持久化 key（导出供 engine 级回放相复用同一原语）。
const (
	PhaseReflect = "reflect" // 轻相机械反思（ReflectAll）
	PhaseDream   = "dream"   // 深相 LLM 聚类合成留史（DeepReflectAll）
	PhaseReplay  = "replay"  // 会话回放补漏（ReplayRecentSessions，engine 级）
	PhaseProfile = "profile" // 画像蒸馏（DistillProfileAll）
)

// nowFunc 墙钟来源，便于测试注入。
var nowFunc = time.Now

// phaseClock 持久化各相上次运行的墙钟时间。诸相共享一个文件，单写器加锁。
type phaseClock struct {
	path string
	mu   sync.Mutex
}

func newPhaseClock(dir string) *phaseClock {
	return &phaseClock{path: filepath.Join(dir, phaseStateFile)}
}

func (c *phaseClock) read() map[string]time.Time {
	m := map[string]time.Time{}
	if b, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (c *phaseClock) lastRun(phase string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read()[phase]
}

func (c *phaseClock) mark(phase string, t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.read() // read-modify-write：诸相共享文件，保留兄弟相时间戳
	m[phase] = t
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = atomicWriteFile(c.path, b, 0o600)
	}
}

// phaseStartAction 开机时某相的动作（纯函数决策，便于单测）。
type phaseStartAction int

const (
	phaseAnchor  phaseStartAction = iota // 首次：仅锚定墙钟、不补跑（无历史可沉淀）
	phaseCatchUp                         // 已到期：静默补跑一次
	phaseWait                            // 未到期：直接等下一个 ticker
)

// decidePhaseStart 据上次运行墙钟 last、周期 interval、当前 now 决定开机动作。
func decidePhaseStart(last time.Time, interval time.Duration, now time.Time) phaseStartAction {
	switch {
	case last.IsZero():
		return phaseAnchor
	case now.Sub(last) >= interval:
		return phaseCatchUp
	default:
		return phaseWait
	}
}

// StartScheduledPhase 启动一个低频后台相：墙钟持久化 last_run + 开机补跑 + 周期 ticker。
//
//   - phase:    持久化 key（PhaseReflect/PhaseDream/PhaseReplay/PhaseProfile）
//   - interval: 周期；≤0 用 fallback
//   - run:      实际工作。收 since=该相上次运行墙钟（首次锚定后为锚定时刻），可用作增量水位
//     （如会话回放）；不需要的相忽略即可。返回 nil 才推进墙钟（失败下次仍补跑）。
//
// 返回 stop()。补跑发生在后台 goroutine 内、不阻塞启动 = 「开机静默补跑」。
func (fm *FileMemory) StartScheduledPhase(ctx context.Context, phase string, interval, fallback time.Duration, run func(ctx context.Context, since time.Time) error) func() {
	if interval <= 0 {
		interval = fallback
	}
	if fm.clock == nil { // 兜底：直接构造的 FileMemory（正常经 New 已初始化）
		fm.clock = newPhaseClock(fm.dir)
	}
	clock := fm.clock
	loopCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	fire := func() {
		since := clock.lastRun(phase)
		if err := run(loopCtx, since); err != nil {
			return // 失败不推进墙钟 → 下次启动/tick 仍补跑
		}
		clock.mark(phase, nowFunc())
	}
	go func() {
		defer close(done)
		switch decidePhaseStart(clock.lastRun(phase), interval, nowFunc()) {
		case phaseAnchor:
			clock.mark(phase, nowFunc()) // 锚定墙钟，但不补跑
		case phaseCatchUp:
			fire() // 开机静默补跑
		case phaseWait:
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-t.C:
				fire()
			}
		}
	}()
	// 优雅停机：cancel 后等 goroutine 真正退出（含在途 mark 落盘完成）才返回——
	// run 受 loopCtx 取消、退出有界；避免「停机后异步写」竞态（如把 .phase_state.json 写进正被清理的目录）。
	return func() {
		cancel()
		<-done
	}
}
