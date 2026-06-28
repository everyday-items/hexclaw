// goalloop_state.go 实现 Loop Engineering #5：**状态外置 / level-triggered**。
//
// "Agent 会忘，仓库不会忘"。reconcile loop 的每轮进度都落盘，使它可被杀死/重启/换模型
// 后从上次的轮次续跑，而不是从头重来——这正是 K8s controller 把状态放 etcd、自身无状态、
// 每轮重读做 diff 的同构。RunState 是 per-goal 的"status 子资源"：我在哪、卡没卡、跑了几轮。
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Outcome 是一次 reconcile 的终态（#4 终止语义化）。空 = 仍在运行。
type Outcome string

const (
	OutcomeRunning   Outcome = ""
	OutcomeDone      Outcome = "done"      // 验收通过
	OutcomeCapped    Outcome = "capped"    // 触顶：轮数/墙钟/token 预算
	OutcomeEscalated Outcome = "escalated" // 升级给人：卡死/低置信/高风险
	OutcomeError     Outcome = "error"
)

// RoundRecord 是一轮的留痕（观测：收敛曲线/为什么停）。
type RoundRecord struct {
	Round      int     `json:"round"`
	Done       bool    `json:"done"`
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
	NoProgress bool    `json:"no_progress"`
	Reason     string  `json:"reason"`
}

// RunState 是一个目标的跨轮持久状态（落盘续跑的最小充分集）。
type RunState struct {
	GoalID           string        `json:"goal_id"`
	Round            int           `json:"round"`                   // 已完成轮数
	LastOutput       string        `json:"last_output"`             // 上轮产出（喂回 + diff）
	LastScore        float64       `json:"last_score"`              // 上轮验收得分（acceptance 模式）
	NoProgressStreak int           `json:"no_progress_streak"`      // 连续无进展计数
	OutputHashes     []string      `json:"output_hashes,omitempty"` // 历史产出哈希（震荡检测，有界）
	TokensUsed       int           `json:"tokens_used"`
	Outcome          Outcome       `json:"outcome"`
	StartedUnixNano  int64         `json:"started_unix_nano"` // 起始时刻（墙钟闸）
	History          []RoundRecord `json:"history,omitempty"`
}

// StateStore 是 RunState 的持久化抽象（level-triggered 的"仓库"）。
type StateStore interface {
	Load(ctx context.Context, goalID string) (RunState, bool, error)
	Save(ctx context.Context, st RunState) error
}

// MemStateStore 进程内存储（测试 / 无需跨重启时）。
type MemStateStore struct {
	mu sync.Mutex
	m  map[string]RunState
}

// NewMemStateStore 构造内存存储。
func NewMemStateStore() *MemStateStore { return &MemStateStore{m: map[string]RunState{}} }

// Load 实现 StateStore。
func (s *MemStateStore) Load(_ context.Context, id string) (RunState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.m[id]
	return st, ok, nil
}

// Save 实现 StateStore。
func (s *MemStateStore) Save(_ context.Context, st RunState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[st.GoalID] = st
	return nil
}

// FileStateStore 把 RunState 以 JSON 落盘（per-goal 一文件），原子替换写入——
// 桌面 app 常被强退，进度必须落盘可续。无 DB 依赖。
type FileStateStore struct{ dir string }

// NewFileStateStore 构造文件存储；dir 在首次 Save 时按需创建。
func NewFileStateStore(dir string) *FileStateStore { return &FileStateStore{dir: dir} }

func (s *FileStateStore) path(id string) string { return filepath.Join(s.dir, fileSafe(id)+".json") }

// Load 实现 StateStore；文件不存在返回 (zero,false,nil)。
func (s *FileStateStore) Load(_ context.Context, id string) (RunState, bool, error) {
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return RunState{}, false, nil
		}
		return RunState{}, false, err
	}
	var st RunState
	if err := json.Unmarshal(b, &st); err != nil {
		return RunState{}, false, fmt.Errorf("decode run state %s: %w", id, err)
	}
	return st, true, nil
}

// Save 实现 StateStore，先写 .tmp 再 rename，避免读到写一半的状态。
func (s *FileStateStore) Save(_ context.Context, st RunState) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	// 唯一 tmp 名：并发保存同一 goal 时不互相覆盖写半成品（tmp 唯一化纪律）（#7）。
	f, err := os.CreateTemp(s.dir, fileSafe(st.GoalID)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, werr := f.Write(b); werr != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return werr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	return os.Rename(tmp, s.path(st.GoalID))
}

// fileSafe 把 goalID 归一成安全文件名。
func fileSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "goal"
	}
	return b.String()
}
