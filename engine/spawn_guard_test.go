package engine

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
)

// ---- ctx / metadata 深度透传 ----

func TestSpawnDepth_CtxRoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := spawnDepthFromContext(ctx); got != 0 {
		t.Fatalf("空 ctx 深度应为 0，得 %d", got)
	}
	ctx = withSpawnDepth(ctx, 1)
	if got := SpawnDepthFromContext(ctx); got != 1 {
		t.Fatalf("导出访问器应返回 1，得 %d", got)
	}
	if got := spawnDepthFromContext(withSpawnDepth(ctx, 3)); got != 3 {
		t.Fatalf("覆写深度应为 3，得 %d", got)
	}
}

func TestSpawnDepthFromMessage(t *testing.T) {
	cases := map[string]int{"": 0, "0": 0, "1": 1, "2": 2, "bad": 0, "-1": 0}
	for raw, want := range cases {
		msg := &adapter.Message{Metadata: map[string]string{"spawn_depth": raw}}
		if got := spawnDepthFromMessage(msg); got != want {
			t.Errorf("spawn_depth=%q 期望 %d，得 %d", raw, want, got)
		}
	}
	if got := spawnDepthFromMessage(nil); got != 0 {
		t.Errorf("nil msg 应为 0，得 %d", got)
	}
}

// ---- P0-2: leaf 剥工具 ----

func multiAgentToolSet() []llm.ToolDefinition {
	return []llm.ToolDefinition{
		llm.NewToolDefinition("spawn_agent", "", &llm.Schema{Type: "object"}),
		llm.NewToolDefinition("orchestrate", "", &llm.Schema{Type: "object"}),
		llm.NewToolDefinition("transfer_to_agent", "", &llm.Schema{Type: "object"}),
		llm.NewToolDefinition("search", "", &llm.Schema{Type: "object"}),
		llm.NewToolDefinition("browser", "", &llm.Schema{Type: "object"}),
	}
}

func joinToolNames(tools []llm.ToolDefinition) string {
	var b strings.Builder
	for _, t := range tools {
		b.WriteString(t.Function.Name + " ")
	}
	return b.String()
}

func TestStripSpawnRecursiveTools_TopLevelKeepsAll(t *testing.T) {
	// 顶层（无 spawn_depth）不剥：根 Agent 必须能 spawn/orchestrate。
	msg := &adapter.Message{Metadata: map[string]string{}}
	got := stripSpawnRecursiveTools(msg, multiAgentToolSet())
	if len(got) != 5 {
		t.Fatalf("顶层应保留全部 5 个工具，得 %d：%s", len(got), joinToolNames(got))
	}
}

func TestStripSpawnRecursiveTools_LeafStripsOrchestration(t *testing.T) {
	// leaf（depth>=maxSpawnDepth=2）必须看不到 spawn/orchestrate/transfer。
	msg := &adapter.Message{Metadata: map[string]string{"source": "spawn", "spawn_depth": "2"}}
	got := stripSpawnRecursiveTools(msg, multiAgentToolSet())
	names := joinToolNames(got)
	for _, banned := range multiAgentRecursiveTools {
		if strings.Contains(names, banned) {
			t.Errorf("leaf 子 Agent 不应持有 %q，实得：%s", banned, names)
		}
	}
	if !strings.Contains(names, "search") || !strings.Contains(names, "browser") {
		t.Errorf("非编排工具应保留，实得：%s", names)
	}
	if len(got) != 2 {
		t.Fatalf("应只剩 2 个普通工具，得 %d：%s", len(got), names)
	}
}

func TestStripSpawnRecursiveTools_OrchestratorKeepsSpawn(t *testing.T) {
	// orchestrator（depth 1 < maxSpawnDepth=2）仍可派生，不剥多 Agent 工具。
	msg := &adapter.Message{Metadata: map[string]string{"source": "spawn", "spawn_depth": "1"}}
	got := stripSpawnRecursiveTools(msg, multiAgentToolSet())
	if len(got) != 5 {
		t.Fatalf("orchestrator 应保留全部 5 个工具(含 spawn/orchestrate)，得 %d：%s", len(got), joinToolNames(got))
	}
	if got := roleForDepth(1); got != subAgentRoleOrchestrator {
		t.Errorf("depth 1 应判定 orchestrator，得 %q", got)
	}
	if got := roleForDepth(0); got != subAgentRoleMain {
		t.Errorf("depth 0 应判定 main，得 %q", got)
	}
	if got := roleForDepth(2); got != subAgentRoleLeaf {
		t.Errorf("depth 2 应判定 leaf，得 %q", got)
	}
}

// ---- 测试桩 ----

// recordingExec 记录每次子任务派发，并可模拟耗时以观测并发。
type recordingExec struct {
	mu       sync.Mutex
	calls    []string
	inflight int32
	maxSeen  int32
	delay    time.Duration
}

func (r *recordingExec) fn(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
	cur := atomic.AddInt32(&r.inflight, 1)
	for {
		old := atomic.LoadInt32(&r.maxSeen)
		if cur <= old || atomic.CompareAndSwapInt32(&r.maxSeen, old, cur) {
			break
		}
	}
	r.mu.Lock()
	r.calls = append(r.calls, spec.Agent)
	r.mu.Unlock()
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	atomic.AddInt32(&r.inflight, -1)
	return SubAgentResult{Output: "out:" + spec.Agent}, nil
}

func (r *recordingExec) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// ---- P0-1: spawn 深度闸 ----

func TestSpawnSkill_AllowsAtRoot(t *testing.T) {
	rec := &recordingExec{}
	s := NewSpawnSkill(rec.fn, nil)
	res, err := s.Execute(context.Background(), map[string]any{"agent_name": "coder", "task": "x"})
	if err != nil {
		t.Fatalf("根层 spawn 不应报错：%v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("根层应实际派发一次子 Agent，得 %d", rec.count())
	}
	if !strings.Contains(res.Content, "out:coder") {
		t.Errorf("应返回子 Agent 输出，得：%s", res.Content)
	}
}

func TestSpawnSkill_RefusesAtMaxDepth(t *testing.T) {
	rec := &recordingExec{}
	s := NewSpawnSkill(rec.fn, nil)
	ctx := withSpawnDepth(context.Background(), maxSpawnDepth) // 已到顶
	res, err := s.Execute(ctx, map[string]any{"agent_name": "coder", "task": "x"})
	if err != nil {
		t.Fatalf("到顶应优雅返回而非 error：%v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("到顶不得再派发子 Agent，却派了 %d 次", rec.count())
	}
	if !strings.Contains(res.Content, "深度上限") {
		t.Errorf("结果应说明深度上限，得：%s", res.Content)
	}
}

// ---- P0-1 + P0-3: orchestrate 深度闸 + 并发上限 ----

func TestOrchestrateSkill_RefusesAtMaxDepth(t *testing.T) {
	rec := &recordingExec{}
	o := NewOrchestrateSkill(rec.fn, nil)
	ctx := withSpawnDepth(context.Background(), maxSpawnDepth)
	res, err := o.Execute(ctx, map[string]any{"subtasks": []any{
		map[string]any{"agent": "a", "task": "t1"},
		map[string]any{"agent": "b", "task": "t2"},
	}})
	if err != nil {
		t.Fatalf("到顶应优雅返回：%v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("到顶不得 fan-out，却派了 %d 次", rec.count())
	}
	if !strings.Contains(res.Content, "深度上限") {
		t.Errorf("结果应说明深度上限，得：%s", res.Content)
	}
}

func TestOrchestrateSkill_ConcurrencyCapped(t *testing.T) {
	defer func(old int) { maxChildrenPerAgent = old }(maxChildrenPerAgent)
	SetMaxChildrenPerAgent(100) // 抬高数量闸以隔离测「并发」（否则被数量闸截断到 8）
	rec := &recordingExec{delay: 20 * time.Millisecond}
	o := NewOrchestrateSkill(rec.fn, nil)
	const n = 20
	subtasks := make([]any, n)
	for i := 0; i < n; i++ {
		subtasks[i] = map[string]any{"agent": "a" + strconv.Itoa(i), "task": "t"}
	}
	_, err := o.Execute(context.Background(), map[string]any{"subtasks": subtasks})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if rec.count() != n {
		t.Fatalf("全部 %d 个子任务都应完成，得 %d", n, rec.count())
	}
	if peak := atomic.LoadInt32(&rec.maxSeen); peak > int32(maxOrchestrateConcurrency) {
		t.Fatalf("峰值并发 %d 超过上限 %d", peak, maxOrchestrateConcurrency)
	}
}
