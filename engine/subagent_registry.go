package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexagon-codes/toolkit/util/logger"
)

// SubAgentRunRecord 是一次子 Agent 派生执行的注册表记录——对齐 OpenClaw subagent registry。
// 它是「运行时纵深」五项能力的统一骨架：承载角色(1)/工具策略(2)/持久化(3)/session(4)/续接(5)。
type SubAgentRunRecord struct {
	ID        string    `json:"id"`
	ParentID  string    `json:"parent_id,omitempty"`  // 发起方 run id（构成派生树）
	Agent     string    `json:"agent"`                // 角色名（researcher/coder/…）
	Role      string    `json:"role"`                 // main | orchestrator | leaf（按深度判定）
	Depth     int       `json:"depth"`                // 派生深度
	Mode      string    `json:"mode"`                 // run | session
	Status    string    `json:"status"`               // running | ok | error | timeout
	SessionID string    `json:"session_id,omitempty"` // 子 Agent 会话 id（session-mode 续聊用）
	Task      string    `json:"task,omitempty"`
	Output    string    `json:"output,omitempty"`
	Error     string    `json:"error,omitempty"`
	ToolAllow []string  `json:"tool_allow,omitempty"` // 继承的工具白名单（父收窄子，链式）
	ToolDeny  []string  `json:"tool_deny,omitempty"`  // 继承的工具黑名单
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
}

// SubAgentRegistry 是子 Agent 派生运行的注册表：内存 map + JSON 文件持久化（重启不丢，支撑
// 可观测 + 续接 + session 续聊）。LRU 淘汰。所有方法 nil-safe（registry 为 nil 时全部 no-op，
// 让未注入注册表的调用方零侵入降级）。
type SubAgentRegistry struct {
	mu       sync.RWMutex
	runs     map[string]*SubAgentRunRecord
	order    []string
	maxRuns  int
	filePath string

	// 🟡 合并落盘：原实现每次 Start/Finish 都在数据锁 mu 内 json.MarshalIndent 全量重写——fan-out N 子
	// = 2N 次全文件重写，且都在 mu 内**串行化了本应并行的** Start/Finish。改为：marshal+write 移出 mu、
	// 由独立 writeMu 串行化；persistGen 每次状态变更 +1，writtenGen 记已落盘最高代——并发变更里抢到
	// writeMu 者落「当前」最新快照并推高 writtenGen，落后者见 writtenGen 已追上自己的代即跳过，把一阵
	// fan-out 的多次变更合并成极少几次真写。全同步、无后台 goroutine（不引入泄漏 / 生命周期负担）。
	writeMu    sync.Mutex
	persistGen atomic.Uint64
	writtenGen atomic.Uint64
	writeFn    func(path string, data []byte) error // 落盘 seam（默认 tmp+rename 原子写；测试可注入计数/阻塞）
}

// NewSubAgentRegistry 创建注册表并从文件加载历史。filePath 为空则纯内存（不持久化）。
func NewSubAgentRegistry(filePath string) *SubAgentRegistry {
	r := &SubAgentRegistry{
		runs:     make(map[string]*SubAgentRunRecord),
		maxRuns:  2000,
		filePath: filePath,
		writeFn:  atomicWriteFile,
	}
	r.load()
	return r
}

// atomicWriteFile 原子落盘：写同目录临时文件 + fsync + rename（POSIX rename 原子）——保证「要么旧内容、
// 要么新内容」，绝无半写中间态。对齐 hexclaw/memory/atomic_write.go 既有最佳实践（其「缺陷C」：os.WriteFile
// 的 O_TRUNC+write 在崩溃/断电/ENOSPC 时会留下空/半写主文件）。桌面 app 常被强退，故 fsync 后再 rename。
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后 tmp 已不存在；任何失败路径清理残留
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil { // 落盘后再 rename → 断电不丢
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// DefaultSubAgentRegistryFile 返回默认持久化路径 ~/.hexclaw/subagent_runs.json。
func DefaultSubAgentRegistryFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".hexclaw", "subagent_runs.json")
}

// Start 登记一次子 Agent 运行（status=running），返回登记后的 ID。rec.ID 为空时调用方应先填好。
func (r *SubAgentRegistry) Start(rec *SubAgentRunRecord) {
	if r == nil || rec == nil {
		return
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now()
	}
	if rec.Status == "" {
		rec.Status = subAgentStatusRunning
	}
	r.mu.Lock()
	r.runs[rec.ID] = rec
	r.order = append(r.order, rec.ID)
	for len(r.order) > r.maxRuns {
		oldest := r.order[0]
		r.order = r.order[1:]
		delete(r.runs, oldest)
	}
	gen := r.persistGen.Add(1)
	r.mu.Unlock()
	r.persist(gen) // 落盘在数据锁外（合并 + 不阻塞并行 Start/Finish）
}

// Finish 更新一次运行到终态（status/output/error/sessionID）。
func (r *SubAgentRegistry) Finish(id, status, output, errStr, sessionID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	rec, ok := r.runs[id]
	if ok {
		rec.Status = status
		rec.Output = output
		rec.Error = errStr
		if sessionID != "" {
			rec.SessionID = sessionID
		}
		rec.EndedAt = time.Now()
	}
	var gen uint64
	if ok {
		gen = r.persistGen.Add(1)
	}
	r.mu.Unlock()
	if ok {
		r.persist(gen)
	}
}

// Get 取一条运行记录的快照副本。
func (r *SubAgentRegistry) Get(id string) (SubAgentRunRecord, bool) {
	if r == nil {
		return SubAgentRunRecord{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.runs[id]
	if !ok {
		return SubAgentRunRecord{}, false
	}
	return *rec, true
}

// Children 返回某个父运行的全部直接子运行（按 StartedAt 升序）。
func (r *SubAgentRegistry) Children(parentID string) []SubAgentRunRecord {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []SubAgentRunRecord
	for _, rec := range r.runs {
		if rec.ParentID == parentID {
			out = append(out, *rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// List 返回全部运行记录（按 StartedAt 降序，最新在前），上限 limit（<=0 不限）。
func (r *SubAgentRegistry) List(limit int) []SubAgentRunRecord {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SubAgentRunRecord, 0, len(r.runs))
	for _, rec := range r.runs {
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// persist 把当前注册表状态落盘。reqGen 是触发本次落盘的状态代号。多个并发变更各自调 persist，但抢到
// writeMu 者会快照「当前」状态（可能已含更晚变更）、落盘并把 writtenGen 推到该代；落后者见 writtenGen
// 已 >= 自己的 reqGen 即跳过——把一阵 fan-out 的多次变更合并成极少的真写。marshal+write 全在数据锁 mu 外，
// 故并行的 Start/Finish 数据变更不被落盘阻塞。
func (r *SubAgentRegistry) persist(reqGen uint64) {
	if r == nil || r.filePath == "" {
		return
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if r.writtenGen.Load() >= reqGen {
		return // 更新的快照已落盘（含本次变更）——合并，免重复 marshal+write
	}
	// 数据锁内只做廉价的值拷贝快照（marshal 在锁外，不阻塞并行变更；值拷贝避免与并发 Finish 改记录竞态）。
	r.mu.RLock()
	curGen := r.persistGen.Load()
	snapshot := make(map[string]SubAgentRunRecord, len(r.runs))
	for id, rec := range r.runs {
		snapshot[id] = *rec
	}
	r.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	if err := r.writeFn(r.filePath, data); err != nil {
		logger.Error("写子Agent注册表失败", "error", err)
		return
	}
	r.writtenGen.Store(curGen) // 仅写成功才推高，失败留待下次变更重试（durability）
}

// load 从文件加载历史并按 StartedAt 重建 LRU 顺序。
func (r *SubAgentRegistry) load() {
	if r.filePath == "" {
		return
	}
	data, err := os.ReadFile(r.filePath)
	if err != nil {
		return
	}
	var runs map[string]*SubAgentRunRecord
	if err := json.Unmarshal(data, &runs); err != nil {
		return
	}
	r.runs = runs
	r.order = make([]string, 0, len(runs))
	for id := range runs {
		r.order = append(r.order, id)
	}
	sort.Slice(r.order, func(i, j int) bool {
		return runs[r.order[i]].StartedAt.Before(runs[r.order[j]].StartedAt)
	})
}
