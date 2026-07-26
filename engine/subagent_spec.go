package engine

import (
	"context"
	"strconv"

	"github.com/hexagon-codes/hexclaw/adapter"
)

// SubAgentSpec 是一次子 Agent 派生的完整参数，替代原来裸的 (agentName, task)。承载工具继承(2)、
// 模式/会话(4)、注册表 run id(3)、深度——让 spawn/orchestrate 的派生在一套统一参数上扩展。
type SubAgentSpec struct {
	RunID     string   // 本次子 Agent 运行的注册表 id（skill 生成）
	Agent     string   // 角色名
	Task      string   // 子任务
	ToolAllow []string // 收窄后的工具白名单（空=不限）
	ToolDeny  []string // 工具黑名单
	Mode      string   // run | session
	SessionID string   // session-mode 续聊：复用已有子会话（空=新建）
	Depth     int      // 子的派生深度（= 调用方深度 + 1）
	Source    string   // 派生来源（空=spawn）；SolveSkill 用 "solve" 标记受信内部解题验证
}

// SubAgentResult 是子 Agent 执行结果。
type SubAgentResult struct {
	Output    string
	SessionID string // 子会话 id，session-mode 回传供后续续聊
}

// SubAgentExecFunc 执行一个子 Agent spec。由 cmd/hexclaw 注入（内部经 eng.Process 跑子任务）。
type SubAgentExecFunc func(ctx context.Context, spec SubAgentSpec) (SubAgentResult, error)

// SubAgentCallInterceptor observes exactly one physical SubAgentExecFunc call.
// Durable scenario adapters use it to establish before-send ledgers without
// coupling the engine to scenario storage.
type SubAgentCallInterceptor interface {
	ExecuteSubAgentCall(
		context.Context,
		SubAgentSpec,
		SubAgentExecFunc,
	) (SubAgentResult, error)
}

type SubAgentCallInterceptorFunc func(
	context.Context,
	SubAgentSpec,
	SubAgentExecFunc,
) (SubAgentResult, error)

func (f SubAgentCallInterceptorFunc) ExecuteSubAgentCall(
	ctx context.Context,
	spec SubAgentSpec,
	next SubAgentExecFunc,
) (SubAgentResult, error) {
	return f(ctx, spec, next)
}

type subAgentCallInterceptorContextKey struct{}

func WithSubAgentCallInterceptor(ctx context.Context, interceptor SubAgentCallInterceptor) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, subAgentCallInterceptorContextKey{}, interceptor)
}

func executeSubAgentCall(
	ctx context.Context,
	execFn SubAgentExecFunc,
	spec SubAgentSpec,
) (SubAgentResult, error) {
	if interceptor, ok := ctx.Value(subAgentCallInterceptorContextKey{}).(SubAgentCallInterceptor); ok &&
		interceptor != nil {
		return interceptor.ExecuteSubAgentCall(ctx, spec, execFn)
	}
	return execFn(ctx, spec)
}

// ── 当前运行 id 的 ctx 透传（构建注册表派生树：子的 ParentID = 当前 run id）──

type ctxKeyCurrentRunIDType struct{}

var ctxKeyCurrentRunID = ctxKeyCurrentRunIDType{}

func withCurrentRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyCurrentRunID, id)
}

func currentRunIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if id, ok := ctx.Value(ctxKeyCurrentRunID).(string); ok {
		return id
	}
	return ""
}

// childSpec 在给定 ctx（含当前深度 + 继承工具策略）下，为一个子任务构造 SubAgentSpec。
// 这是工具继承链(2)与深度(1)的汇聚点：子深度 = 当前 +1，子工具策略 = 父策略链式收窄。
func childSpec(ctx context.Context, runID, agent, task string, reqAllow, reqDeny []string, mode, sessionID string) SubAgentSpec {
	pAllow, pDeny := inheritedToolsFromContext(ctx)
	allow, deny := narrowChildTools(pAllow, pDeny, reqAllow, reqDeny)
	return SubAgentSpec{
		RunID:     runID,
		Agent:     agent,
		Task:      task,
		ToolAllow: allow,
		ToolDeny:  deny,
		Mode:      mode,
		SessionID: sessionID,
		Depth:     spawnDepthFromContext(ctx) + 1,
	}
}

// newSubAgentRecord 由 spec + ctx 构造一条注册表记录（status=running）。
func newSubAgentRecord(ctx context.Context, spec SubAgentSpec) *SubAgentRunRecord {
	return &SubAgentRunRecord{
		ID:        spec.RunID,
		ParentID:  currentRunIDFromContext(ctx),
		Agent:     spec.Agent,
		Role:      roleForDepth(spec.Depth),
		Depth:     spec.Depth,
		Mode:      firstNonEmptyStr(spec.Mode, "run"),
		Status:    subAgentStatusRunning,
		Task:      spec.Task,
		ToolAllow: spec.ToolAllow,
		ToolDeny:  spec.ToolDeny,
	}
}

func firstNonEmptyStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ApplySpecToMessage 把 spec 落到子任务消息的 metadata，供引擎侧（Process/工具装配）消费。
// 导出供 cmd/hexclaw 注入的 execFn 在调 eng.Process 前调用。
func ApplySpecToMessage(msg *adapter.Message, spec SubAgentSpec) {
	if msg.Metadata == nil {
		msg.Metadata = map[string]string{}
	}
	msg.Metadata["role"] = spec.Agent
	msg.Metadata["source"] = firstNonEmptyStr(spec.Source, spawnDispatchSource)
	msg.Metadata["spawn_depth"] = strconv.Itoa(spec.Depth)
	msg.Metadata["subagent_run_id"] = spec.RunID
	if len(spec.ToolAllow) > 0 {
		msg.Metadata["tool_allow"] = joinCSV(spec.ToolAllow)
	}
	if len(spec.ToolDeny) > 0 {
		msg.Metadata["tool_deny"] = joinCSV(spec.ToolDeny)
	}
	// session-mode 续聊：复用已有子会话，使 Process 加载其历史（多轮）。
	if spec.SessionID != "" {
		msg.SessionID = spec.SessionID
	}
}
