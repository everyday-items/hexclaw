package engine

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexclaw/adapter"
)

// specRecorder 捕获每次派发的 spec，并可指定回传的子会话 id。
type specRecorder struct {
	mu      sync.Mutex
	specs   []SubAgentSpec
	session string
}

func (r *specRecorder) fn(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return SubAgentResult{Output: "out:" + spec.Agent, SessionID: r.session}, nil
}
func (r *specRecorder) agents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.specs))
	for i, s := range r.specs {
		out[i] = s.Agent
	}
	return out
}

// ───────── feature 1: 三级角色 ─────────

func TestRoleForDepth(t *testing.T) {
	old := maxSpawnDepth
	maxSpawnDepth = 2
	defer func() { maxSpawnDepth = old }()
	cases := map[int]string{0: subAgentRoleMain, 1: subAgentRoleOrchestrator, 2: subAgentRoleLeaf, 3: subAgentRoleLeaf, -1: subAgentRoleMain}
	for depth, want := range cases {
		if got := roleForDepth(depth); got != want {
			t.Errorf("roleForDepth(%d)=%q 期望 %q", depth, got, want)
		}
	}
}

// ───────── feature 2: 工具继承链 ─────────

func TestNarrowChildTools(t *testing.T) {
	// 父白名单收窄子（交集）；任一为空表示该侧不限。
	allow, _ := narrowChildTools([]string{"a", "b", "c"}, nil, []string{"b", "c", "d"}, nil)
	if !reflect.DeepEqual(allow, []string{"b", "c"}) {
		t.Errorf("交集应 [b c]，得 %v", allow)
	}
	if allow, _ := narrowChildTools(nil, nil, []string{"x"}, nil); !reflect.DeepEqual(allow, []string{"x"}) {
		t.Errorf("父空→取子，得 %v", allow)
	}
	if allow, _ := narrowChildTools([]string{"p"}, nil, nil, nil); !reflect.DeepEqual(allow, []string{"p"}) {
		t.Errorf("子空→取父，得 %v", allow)
	}
	// deny 求并。
	if _, deny := narrowChildTools(nil, []string{"a"}, nil, []string{"b"}); !reflect.DeepEqual(deny, []string{"a", "b"}) {
		t.Errorf("deny 并集应 [a b]，得 %v", deny)
	}
}

func toolSet(names ...string) []llm.ToolDefinition {
	out := make([]llm.ToolDefinition, len(names))
	for i, n := range names {
		out[i] = llm.NewToolDefinition(n, "", &llm.Schema{Type: "object"})
	}
	return out
}

func TestApplyInheritedToolPolicy(t *testing.T) {
	tools := toolSet("search", "browser", "file_edit", "knowledge_ingest")
	// allow 白名单：只留 search/browser。
	msg := &adapter.Message{Metadata: map[string]string{"tool_allow": "search,browser"}}
	got := applyInheritedToolPolicy(msg, tools)
	if len(got) != 2 || got[0].Function.Name != "search" || got[1].Function.Name != "browser" {
		t.Errorf("allow 过滤后应只剩 search/browser，得 %v", joinToolNames(got))
	}
	// deny 黑名单：去掉 file_edit。
	msg2 := &adapter.Message{Metadata: map[string]string{"tool_deny": "file_edit"}}
	got2 := applyInheritedToolPolicy(msg2, tools)
	if strings.Contains(joinToolNames(got2), "file_edit") {
		t.Errorf("deny 应剔除 file_edit，得 %v", joinToolNames(got2))
	}
	if len(got2) != 3 {
		t.Errorf("deny 一个应剩 3，得 %d", len(got2))
	}
	// 无策略：原样。
	if got3 := applyInheritedToolPolicy(&adapter.Message{}, tools); len(got3) != 4 {
		t.Errorf("无策略应原样 4，得 %d", len(got3))
	}
}

func TestInheritedTools_MessageRoundTrip(t *testing.T) {
	msg := &adapter.Message{Metadata: map[string]string{"tool_allow": "a, b ,a", "tool_deny": "x,y"}}
	allow, deny := inheritedToolsFromMessage(msg)
	if !reflect.DeepEqual(allow, []string{"a", "b"}) {
		t.Errorf("allow 解析去重应 [a b]，得 %v", allow)
	}
	if !reflect.DeepEqual(deny, []string{"x", "y"}) {
		t.Errorf("deny 解析应 [x y]，得 %v", deny)
	}
}

// childSpec 链式收窄：父 ctx 继承 allow=[a,b]，子请求 [b,c] → 子 spec allow=[b]。
func TestChildSpec_ToolInheritanceChain(t *testing.T) {
	ctx := withInheritedTools(context.Background(), []string{"a", "b"}, nil)
	spec := childSpec(ctx, "r1", "coder", "t", []string{"b", "c"}, nil, "run", "")
	if !reflect.DeepEqual(spec.ToolAllow, []string{"b"}) {
		t.Errorf("链式收窄后子 allow 应 [b]，得 %v", spec.ToolAllow)
	}
	if spec.Depth != 1 {
		t.Errorf("子深度应为 1（根 ctx 深度 0 +1），得 %d", spec.Depth)
	}
}

// ───────── feature 3: 注册表 + 持久化 ─────────

func TestSubAgentRegistry_PersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subagent_runs.json")
	reg := NewSubAgentRegistry(path)
	reg.Start(&SubAgentRunRecord{ID: "p1", Agent: "orchestrate", Role: subAgentRoleMain, Depth: 0})
	reg.Start(&SubAgentRunRecord{ID: "c1", ParentID: "p1", Agent: "researcher", Role: subAgentRoleLeaf, Depth: 1})
	reg.Finish("c1", subAgentStatusOK, "done-R", "", "sess-1")

	// 重启：新注册表从同文件加载。
	reg2 := NewSubAgentRegistry(path)
	if rec, ok := reg2.Get("c1"); !ok || rec.Status != subAgentStatusOK || rec.Output != "done-R" || rec.SessionID != "sess-1" {
		t.Fatalf("重启后 c1 未还原：%+v ok=%v", rec, ok)
	}
	kids := reg2.Children("p1")
	if len(kids) != 1 || kids[0].Agent != "researcher" {
		t.Fatalf("Children(p1) 应有 1 个 researcher，得 %+v", kids)
	}
	if len(reg2.List(0)) != 2 {
		t.Fatalf("List 应有 2 条，得 %d", len(reg2.List(0)))
	}
}

func TestSubAgentRegistry_NilSafe(t *testing.T) {
	var reg *SubAgentRegistry // nil
	reg.Start(&SubAgentRunRecord{ID: "x"})
	reg.Finish("x", subAgentStatusOK, "", "", "")
	if _, ok := reg.Get("x"); ok {
		t.Error("nil 注册表 Get 应返回 false")
	}
	if reg.List(0) != nil || reg.Children("x") != nil {
		t.Error("nil 注册表 List/Children 应返回 nil")
	}
}

// orchestrate 跑完应在注册表留下父运行 + 各子运行记录。
func TestOrchestrate_RecordsToRegistry(t *testing.T) {
	reg := NewSubAgentRegistry(filepath.Join(t.TempDir(), "r.json"))
	rec := &specRecorder{}
	o := NewOrchestrateSkill(rec.fn, reg)
	res, err := o.Execute(context.Background(), map[string]any{"subtasks": []any{
		map[string]any{"agent": "researcher", "task": "t1"},
		map[string]any{"agent": "coder", "task": "t2"},
	}})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	orchID := res.Metadata["orchestrate_run_id"]
	if orchID == "" {
		t.Fatal("应回传 orchestrate_run_id")
	}
	kids := reg.Children(orchID)
	if len(kids) != 2 {
		t.Fatalf("应登记 2 个子运行，得 %d", len(kids))
	}
	for _, k := range kids {
		if k.Status != subAgentStatusOK || k.ParentID != orchID {
			t.Errorf("子运行登记异常：%+v", k)
		}
	}
}

// ───────── feature 4: session-mode ─────────

func TestSpawn_SessionMode(t *testing.T) {
	reg := NewSubAgentRegistry(filepath.Join(t.TempDir(), "r.json"))
	rec := &specRecorder{session: "child-sess-9"}
	s := NewSpawnSkill(rec.fn, reg)
	res, err := s.Execute(context.Background(), map[string]any{
		"agent_name": "coder", "task": "x", "mode": "session", "session_id": "prev-sess",
	})
	if err != nil {
		t.Fatalf("spawn 报错：%v", err)
	}
	// spec 应带 mode/session 续聊 id。
	if len(rec.specs) != 1 || rec.specs[0].SessionID != "prev-sess" || rec.specs[0].Mode != "session" {
		t.Fatalf("spec 未带 session 续聊参数：%+v", rec.specs)
	}
	// 结果应把子会话 id 透给 LLM。
	if !strings.Contains(res.Content, "child-sess-9") {
		t.Errorf("session-mode 结果应含子会话 id，得：%s", res.Content)
	}
	// 注册表记录 mode=session + sessionID。
	runID := res.Metadata["subagent_run_id"]
	if r, ok := reg.Get(runID); !ok || r.Mode != "session" || r.SessionID != "child-sess-9" {
		t.Errorf("注册表 session 记录异常：%+v", r)
	}
}

// ───────── feature 5: 续接 resume ─────────

func TestOrchestrate_Resume_ReusesSucceeded(t *testing.T) {
	reg := NewSubAgentRegistry(filepath.Join(t.TempDir(), "r.json"))
	// 预置一次「上次运行」：researcher 成功、coder 失败。
	reg.Start(&SubAgentRunRecord{ID: "prev", Agent: "orchestrate", Depth: 0})
	reg.Start(&SubAgentRunRecord{ID: "prev-r", ParentID: "prev", Agent: "researcher", Task: "t1"})
	reg.Finish("prev-r", subAgentStatusOK, "RESEARCH_OK", "", "")
	reg.Start(&SubAgentRunRecord{ID: "prev-c", ParentID: "prev", Agent: "coder", Task: "t2"})
	reg.Finish("prev-c", subAgentStatusError, "", "boom", "")

	rec := &specRecorder{}
	o := NewOrchestrateSkill(rec.fn, reg)
	res, err := o.Execute(context.Background(), map[string]any{
		"resume_run_id": "prev",
		"subtasks": []any{
			map[string]any{"agent": "researcher", "task": "t1"},
			map[string]any{"agent": "coder", "task": "t2"},
		},
	})
	if err != nil {
		t.Fatalf("resume orchestrate 报错：%v", err)
	}
	// researcher 应复用（不重跑），coder 应重跑。
	dispatched := rec.agents()
	if len(dispatched) != 1 || dispatched[0] != "coder" {
		t.Fatalf("续接应只重跑 coder，实际派发：%v", dispatched)
	}
	if !strings.Contains(res.Content, "RESEARCH_OK") {
		t.Errorf("应复用上次 researcher 输出 RESEARCH_OK，得：%s", res.Content)
	}
	if !strings.Contains(res.Content, "续接复用") {
		t.Errorf("应标注续接复用，得：%s", res.Content)
	}
}

// ───────── 数量闸 + provider 自适应并发 ─────────

func TestSetMaxOrchestrateConcurrency_Clamp(t *testing.T) {
	defer func(old int) { maxOrchestrateConcurrency = old }(maxOrchestrateConcurrency)
	SetMaxOrchestrateConcurrency(0)
	if maxOrchestrateConcurrency != 1 {
		t.Errorf("下限应 clamp 到 1，得 %d", maxOrchestrateConcurrency)
	}
	SetMaxOrchestrateConcurrency(99)
	if maxOrchestrateConcurrency != 16 {
		t.Errorf("上限应 clamp 到 16，得 %d", maxOrchestrateConcurrency)
	}
	SetMaxOrchestrateConcurrency(2)
	if maxOrchestrateConcurrency != 2 {
		t.Errorf("正常值应设 2，得 %d", maxOrchestrateConcurrency)
	}
}

func TestOrchestrate_CountGate_Truncates(t *testing.T) {
	defer func(old int) { maxChildrenPerAgent = old }(maxChildrenPerAgent)
	SetMaxChildrenPerAgent(3)
	rec := &specRecorder{}
	o := NewOrchestrateSkill(rec.fn, nil)
	subtasks := make([]any, 10)
	for i := 0; i < 10; i++ {
		subtasks[i] = map[string]any{"agent": "a" + string(rune('0'+i)), "task": "t"}
	}
	res, err := o.Execute(context.Background(), map[string]any{"subtasks": subtasks})
	if err != nil {
		t.Fatalf("orchestrate 报错：%v", err)
	}
	if len(rec.agents()) != 3 {
		t.Fatalf("数量闸应只跑前 3 个，实际派发 %d", len(rec.agents()))
	}
	if !strings.Contains(res.Content, "已截断") {
		t.Errorf("应提示已截断，得：%s", res.Content)
	}
}

var _ = time.Second // keep time import if unused elsewhere
