package session_test

// audit_e2e_test 端到端闭环测试：会话生命周期 + engine 处理闭环。
//
// 本文件用外部测试包 session_test，因此可以同时 import engine 和 session
// （engine 单向依赖 session，session 不依赖 engine，外部测试包不会构成循环依赖）。
//
// 覆盖的真实跨层闭环（单测难以覆盖、容易在层与层之间断裂的部分）：
//
//	新建会话 → 追加用户消息 → engine.Process（mock LLM provider）
//	  → 工具/skill 执行 → 助手响应落库 → BuildContext 可读回
//	  → 触发上下文压缩/归档 → 历史一致性
//
// 所有外部依赖均被 mock：
//   - LLM provider：hexagon/testing/mock，确定性返回固定内容或 tool_call
//   - 存储：真实 sqlite（t.TempDir() 临时库），验证响应真正落盘可读
//   - 不打真实网络、不依赖墙钟（无 time.Sleep 决策、无固定等待）
//
// 这些测试只新增、不改源码。失败即视作疑似跨层集成缺陷。

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/engine"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/session"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/storage"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// ============================================================================
// 测试基础设施：真实 sqlite store + mock LLM provider + 可配置 engine
// ============================================================================

// e2eHarness 聚合一个完整闭环测试所需的全部组件。
type e2eHarness struct {
	eng      *engine.ReActEngine
	store    storage.Store
	mgr      *session.Manager // 直接读会话层，验证落库后可读
	provider *mockllm.LLMProvider
}

// newE2EHarness 构造一个端到端 harness：真实 sqlite + 注入的 mock provider。
//
// compaction 控制是否启用上下文压缩；启用时 MaxMessages/KeepRecent 取自参数，
// 用于精确触发压缩路径而不需要塞 50+ 条消息。
func newE2EHarness(t *testing.T, provider *mockllm.LLMProvider, opts e2eOptions) *e2eHarness {
	t.Helper()

	dir := t.TempDir()
	real, err := sqlitestore.New(filepath.Join(dir, "e2e.db"))
	if err != nil {
		t.Fatalf("创建 sqlite 存储失败: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("初始化存储失败: %v", err)
	}

	// 引擎使用的 store 可被串行化包装，用于把并发收敛成确定性串行（隔离测试）。
	var store storage.Store = real
	if opts.serializeWrites {
		store = &serializedStore{Store: real}
	}

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "mock"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"mock": {Model: "mock-model"},
	}
	// 默认关闭知识库检索，避免无关 RAG 干扰闭环断言。
	cfg.Knowledge.Enabled = false
	// 工具开关：默认让 mock provider（非本地）走 auto=启用，保证 tool 定义被下发。
	cfg.LLM.Tools.Enabled = "on"

	// 压缩配置：按需启用，精确控制阈值。
	cfg.Compaction.Enabled = opts.compactionEnabled
	if opts.compactionEnabled {
		cfg.Compaction.MaxMessages = opts.maxMessages
		cfg.Compaction.KeepRecent = opts.keepRecent
	}
	// 历史窗口：避免 BuildContext 的 token 预算意外截断短测试消息。
	cfg.Memory.Conversation.MaxTurns = opts.maxTurns
	if cfg.Memory.Conversation.MaxTurns == 0 {
		cfg.Memory.Conversation.MaxTurns = 200
	}

	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{
		"mock": provider,
	})

	// 引擎在生产中总是拿到一个非 nil 的 skill 注册表（cmd/hexclaw 构造），
	// matchSkillFastPath → skills.Match 不做 nil 检查，因此 harness 也始终注入一个。
	skills := opts.skills
	if skills == nil {
		skills = skill.NewRegistry()
	}
	eng := engine.NewReActEngine(cfg, router, store, skills)
	eng.SetToolCollector(engine.NewToolCollector(skills, nil, 40))
	eng.SetToolExecutor(engine.NewToolExecutor(skills, nil))
	// 生产中 cmd/hexclaw 总是注入 session 锁来串行化同会话请求
	// （main.go: eng.SetSessionLock(session.NewSessionLock())）。
	// 默认 harness 与生产保持一致；opts.noSessionLock 可关闭以暴露无锁路径。
	if !opts.noSessionLock {
		eng.SetSessionLock(session.NewSessionLock())
	}
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动引擎失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	return &e2eHarness{
		eng:      eng,
		store:    store,
		mgr:      session.NewManager(store, cfg.Memory),
		provider: provider,
	}
}

type e2eOptions struct {
	skills            *skill.DefaultRegistry
	compactionEnabled bool
	maxMessages       int
	keepRecent        int
	maxTurns          int
	noSessionLock     bool // true 时不注入 session 锁，用于暴露无锁并发路径
	serializeWrites   bool // true 时用全局 mutex 串行化写，消除 SQLite 并发抖动（确定性隔离测试）
}

// serializedStore 用单个全局 mutex 串行化所有 store 调用，消除 SQLite WAL
// 写写冲突（SQLITE_BUSY/517），让"会话隔离"这类断言不受底层并发抖动影响、确定可重复。
// 它不改变任何业务语义，只是把并发收敛成串行，等价于一个完美的写串行化层。
type serializedStore struct {
	storage.Store
	mu sync.Mutex
}

func (s *serializedStore) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.SaveMessage(ctx, msg)
}
func (s *serializedStore) CreateSession(ctx context.Context, sess *storage.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.CreateSession(ctx, sess)
}
func (s *serializedStore) UpdateSession(ctx context.Context, sess *storage.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.UpdateSession(ctx, sess)
}
func (s *serializedStore) SaveCost(ctx context.Context, rec *storage.CostRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.SaveCost(ctx, rec)
}
func (s *serializedStore) ListMessages(ctx context.Context, sessionID string, limit, offset int) ([]*storage.MessageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.ListMessages(ctx, sessionID, limit, offset)
}
func (s *serializedStore) GetSession(ctx context.Context, id string) (*storage.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.GetSession(ctx, id)
}
func (s *serializedStore) FindSessionByScope(ctx context.Context, userID, platform, instanceID, chatID string) (*storage.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.FindSessionByScope(ctx, userID, platform, instanceID, chatID)
}
func (s *serializedStore) WithTx(ctx context.Context, fn func(storage.Store) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Store.WithTx(ctx, fn)
}

// dbMessages 直接从存储读取某会话的全部消息（按时间正序）。
func (h *e2eHarness) dbMessages(t *testing.T, sessionID string) []*storage.MessageRecord {
	t.Helper()
	recs, err := h.store.ListMessages(context.Background(), sessionID, 1000, 0)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	return recs
}

// ============================================================================
// 测试用 Skill：返回固定结果，且不触发关键词快速路径（Match 恒 false），
// 强制走 "LLM 决策 → tool_call → ToolExecutor 执行 → 结果回灌 → 最终回复" 闭环。
// ============================================================================

// weatherSkill 一个被 LLM 以 tool_call 形式调用的工具。
type weatherSkill struct {
	calls   atomic.Int32
	lastArg atomic.Value // string
}

func (s *weatherSkill) Name() string        { return "get_weather" }
func (s *weatherSkill) Description() string { return "查询指定城市天气" }
func (s *weatherSkill) Match(string) bool   { return false } // 永不走关键词快速路径
func (s *weatherSkill) Execute(_ context.Context, args map[string]any) (*skill.Result, error) {
	s.calls.Add(1)
	city, _ := args["city"].(string)
	s.lastArg.Store(city)
	return &skill.Result{Content: "天气(" + city + "): 晴 25C"}, nil
}
func (s *weatherSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("get_weather", "查询指定城市天气", nil)
}

// boomSkill 一个执行时必定失败的工具，用于验证工具错误跨层传播。
type boomSkill struct{ calls atomic.Int32 }

func (s *boomSkill) Name() string        { return "boom" }
func (s *boomSkill) Description() string { return "总是失败的工具" }
func (s *boomSkill) Match(string) bool   { return false }
func (s *boomSkill) Execute(context.Context, map[string]any) (*skill.Result, error) {
	s.calls.Add(1)
	return nil, fmt.Errorf("工具内部崩溃: disk full")
}
func (s *boomSkill) ToolDefinition() llm.ToolDefinition {
	return llm.NewToolDefinition("boom", "总是失败的工具", nil)
}

func newSkillRegistry(t *testing.T, skills ...skill.Skill) *skill.DefaultRegistry {
	t.Helper()
	reg := skill.NewRegistry()
	for _, s := range skills {
		if err := reg.Register(s); err != nil {
			t.Fatalf("注册 skill %q 失败: %v", s.Name(), err)
		}
	}
	return reg
}

func baseMsg(id, user, content string) *adapter.Message {
	return &adapter.Message{
		ID:       id,
		Platform: adapter.PlatformAPI,
		UserID:   user,
		Content:  content,
	}
}

// ============================================================================
// 闭环 1：最朴素的 新建会话 → 处理 → 响应落库可读
// ============================================================================

// TestE2E_BasicLoop_ResponsePersistedAndReadable 验证最基本的闭环是否真闭合：
// Process 返回的内容必须真正落到存储里，并且能通过 BuildContext 读回。
// 这是"闭环是否真的闭合（响应是否落库可读）"的核心校验。
func TestE2E_BasicLoop_ResponsePersistedAndReadable(t *testing.T) {
	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{
				Content: "你好，我是助手",
				Usage:   hexagon.Usage{PromptTokens: 5, CompletionTokens: 4, TotalTokens: 9},
			}, nil
		})
	h := newE2EHarness(t, provider, e2eOptions{})

	msg := baseMsg("m1", "user-1", "你好")
	reply, err := h.eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("Process 失败: %v", err)
	}
	if reply.Content != "你好，我是助手" {
		t.Fatalf("回复内容不匹配: %q", reply.Content)
	}
	if msg.SessionID == "" {
		t.Fatal("Process 应回填 msg.SessionID")
	}

	// 落库校验：存储里应有 user + assistant 两条消息。
	recs := h.dbMessages(t, msg.SessionID)
	if len(recs) != 2 {
		t.Fatalf("期望落库 2 条消息（user+assistant），实际 %d: %+v", len(recs), recs)
	}
	if recs[0].Role != "user" || recs[0].Content != "你好" {
		t.Fatalf("第 1 条应为 user/你好，实际 %s/%q", recs[0].Role, recs[0].Content)
	}
	if recs[1].Role != "assistant" || recs[1].Content != "你好，我是助手" {
		t.Fatalf("第 2 条应为 assistant 且内容为助手回复，实际 %s/%q", recs[1].Role, recs[1].Content)
	}

	// 跨层一致性：会话层 BuildContext 必须能读回助手回复（闭环闭合）。
	ctxMsgs, err := h.mgr.BuildContext(context.Background(), msg.SessionID)
	if err != nil {
		t.Fatalf("BuildContext 失败: %v", err)
	}
	if len(ctxMsgs) != 2 || ctxMsgs[1].Content != "你好，我是助手" {
		t.Fatalf("BuildContext 未读回助手回复: %+v", ctxMsgs)
	}
}

// ============================================================================
// 闭环 2：工具调用闭环 —— LLM 决策 tool_call → 执行 → 回灌 → 最终回复落库
// ============================================================================

// TestE2E_ToolCallLoop_ResultFlowsBackAndPersists 驱动完整工具循环：
//
//	第 1 次 LLM 调用 → 返回 get_weather tool_call
//	ToolExecutor 执行 weatherSkill → "天气(北京): 晴 25C"
//	工具结果回灌 → 第 2 次 LLM 调用 → 返回最终自然语言回复
//	最终回复落库 + reply.ToolCalls 带工具调用记录
func TestE2E_ToolCallLoop_ResultFlowsBackAndPersists(t *testing.T) {
	ws := &weatherSkill{}
	reg := newSkillRegistry(t, ws)

	var llmCalls atomic.Int32
	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			call := llmCalls.Add(1)
			if call == 1 {
				// 第一轮：模型决定调用 get_weather 工具。
				return &hexagon.CompletionResponse{
					ToolCalls: []llm.ToolCall{{
						ID:        "tc-1",
						Type:      "function",
						Name:      "get_weather",
						Arguments: `{"city":"北京"}`,
					}},
					Usage: hexagon.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13},
				}, nil
			}
			// 第二轮：工具结果应已回灌到消息列表中。
			joined := allContent(req.Messages)
			if !strings.Contains(joined, "天气(北京)") {
				t.Errorf("第二轮 LLM 请求未包含工具执行结果，messages=%s", joined)
			}
			return &hexagon.CompletionResponse{
				Content: "北京今天晴天，25 度",
				Usage:   hexagon.Usage{PromptTokens: 20, CompletionTokens: 6, TotalTokens: 26},
			}, nil
		})

	h := newE2EHarness(t, provider, e2eOptions{skills: reg})

	msg := baseMsg("m-tool", "user-1", "北京天气怎么样")
	reply, err := h.eng.Process(context.Background(), msg)
	if err != nil {
		t.Fatalf("Process（工具循环）失败: %v", err)
	}

	if ws.calls.Load() != 1 {
		t.Fatalf("weatherSkill 期望被执行 1 次，实际 %d", ws.calls.Load())
	}
	if got, _ := ws.lastArg.Load().(string); got != "北京" {
		t.Fatalf("工具收到的 city 参数应为 北京，实际 %q", got)
	}
	if reply.Content != "北京今天晴天，25 度" {
		t.Fatalf("最终回复不匹配: %q", reply.Content)
	}
	if len(reply.ToolCalls) == 0 {
		t.Fatal("reply.ToolCalls 应记录 get_weather 工具调用")
	}

	// 闭环闭合：最终自然语言回复（而非工具原始输出）必须落库且可读回。
	recs := h.dbMessages(t, msg.SessionID)
	var assistantFinal string
	for _, r := range recs {
		if r.Role == "assistant" {
			assistantFinal = r.Content
		}
	}
	if assistantFinal != "北京今天晴天，25 度" {
		t.Fatalf("落库的助手最终回复不匹配，实际 %q（全量=%+v）", assistantFinal, dumpRecs(recs))
	}
}

// ============================================================================
// 闭环 3：错误跨层传播 —— engine 内 LLM 失败必须完整冒泡到调用方，
//          且失败请求不得污染会话历史（不留半条假回复）
// ============================================================================

// TestE2E_LLMError_PropagatesAndDoesNotPersistFakeReply 验证：
//   - 显式 provider+model 时 LLM 失败 → 错误冒泡到 Process 调用方
//   - 底层错误原因（DeadlineExceeded）保留在错误链里
//   - 失败时不得落库任何 assistant 消息（闭环未闭合就不能伪造结果）
func TestE2E_LLMError_PropagatesAndDoesNotPersistFakeReply(t *testing.T) {
	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return nil, context.DeadlineExceeded
		})
	h := newE2EHarness(t, provider, e2eOptions{})

	msg := baseMsg("m-err", "user-1", "触发错误")
	// 显式指定 provider+model 关闭跨 provider fallback，使错误确定地冒泡。
	msg.Metadata = map[string]string{"provider": "mock", "model": "mock-model"}

	reply, err := h.eng.Process(context.Background(), msg)
	if err == nil {
		t.Fatalf("LLM 失败时 Process 应返回错误，却得到 reply=%+v", reply)
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("错误未透传底层原因 DeadlineExceeded: %v", err)
	}

	// 失败请求不得在会话里留下任何 assistant 回复。
	recs := h.dbMessages(t, msg.SessionID)
	for _, r := range recs {
		if r.Role == "assistant" {
			t.Fatalf("LLM 失败后不应落库 assistant 消息，却发现 %q", r.Content)
		}
	}
}

// TestE2E_ToolError_SurfacesInClosedLoop 验证工具执行失败时的闭环行为：
// 工具崩溃后，闭环要么把错误冒泡到调用方，要么由模型在后续轮恢复并给出最终回复，
// 但无论哪种，都不能"静默吞掉错误后落库一条空助手消息"。
func TestE2E_ToolError_SurfacesInClosedLoop(t *testing.T) {
	bs := &boomSkill{}
	reg := newSkillRegistry(t, bs)

	var llmCalls atomic.Int32
	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			call := llmCalls.Add(1)
			if call == 1 {
				return &hexagon.CompletionResponse{
					ToolCalls: []llm.ToolCall{{
						ID:        "tc-boom",
						Type:      "function",
						Name:      "boom",
						Arguments: `{}`,
					}},
					Usage: hexagon.Usage{TotalTokens: 10},
				}, nil
			}
			// 若闭环把工具错误回灌给模型，模型据此给出降级回复。
			joined := allContent(req.Messages)
			recovered := ""
			if strings.Contains(joined, "disk full") || strings.Contains(joined, "崩溃") {
				recovered = "已观察到工具错误"
			}
			return &hexagon.CompletionResponse{
				Content: "工具失败了，已为你记录: " + recovered,
				Usage:   hexagon.Usage{TotalTokens: 12},
			}, nil
		})

	h := newE2EHarness(t, provider, e2eOptions{skills: reg})

	msg := baseMsg("m-toolerr", "user-1", "执行那个会崩的工具")
	reply, err := h.eng.Process(context.Background(), msg)

	// 工具必须被尝试执行过（闭环确实进了工具层）。
	if bs.calls.Load() == 0 {
		t.Fatal("boom 工具应至少被执行一次（闭环应进入工具层）")
	}

	if err != nil {
		// 路径 A：错误冒泡。错误链应能体现工具失败原因。
		if !strings.Contains(err.Error(), "disk full") && !strings.Contains(err.Error(), "崩溃") {
			t.Fatalf("工具错误冒泡时应保留根因，实际: %v", err)
		}
		// 错误冒泡时不应落库 assistant 回复。
		for _, r := range h.dbMessages(t, msg.SessionID) {
			if r.Role == "assistant" {
				t.Fatalf("工具错误冒泡后不应落库 assistant 消息，却发现 %q", r.Content)
			}
		}
		return
	}

	// 路径 B：模型恢复并给出最终回复 → 回复必须落库且非空。
	if strings.TrimSpace(reply.Content) == "" {
		t.Fatal("工具失败但闭环未报错时，最终回复不应为空（不得静默吞错）")
	}
	recs := h.dbMessages(t, msg.SessionID)
	var assistantFinal string
	for _, r := range recs {
		if r.Role == "assistant" {
			assistantFinal = r.Content
		}
	}
	if assistantFinal == "" {
		t.Fatalf("工具失败恢复路径下助手最终回复应落库，实际全量=%+v", dumpRecs(recs))
	}
	// 跨层一致性：必须证明模型确实收到了工具错误，而不是凭空恢复。
	if !strings.Contains(reply.Content, "已观察到工具错误") {
		t.Fatalf("工具错误未回灌给模型（闭环错误传播断裂）: 最终回复=%q", reply.Content)
	}
}

// ============================================================================
// 闭环 4：多轮会话历史一致性 —— 第二轮请求必须携带第一轮的完整历史
// ============================================================================

// TestE2E_MultiTurn_HistoryConsistency 验证多轮闭环的历史一致性：
// 同一 session 的第二轮请求，engine 必须把第一轮的 user+assistant 注入 LLM。
func TestE2E_MultiTurn_HistoryConsistency(t *testing.T) {
	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			last := req.Messages[len(req.Messages)-1].Content
			switch last {
			case "我叫小明":
				return &hexagon.CompletionResponse{Content: "你好小明", Usage: hexagon.Usage{TotalTokens: 6}}, nil
			case "我叫什么":
				// 历史必须包含上一轮的 user/assistant。
				joined := allContent(req.Messages)
				if !strings.Contains(joined, "我叫小明") {
					t.Errorf("第二轮缺少历史 user 消息 '我叫小明'，messages=%s", joined)
				}
				if !strings.Contains(joined, "你好小明") {
					t.Errorf("第二轮缺少历史 assistant 消息 '你好小明'，messages=%s", joined)
				}
				return &hexagon.CompletionResponse{Content: "你叫小明", Usage: hexagon.Usage{TotalTokens: 8}}, nil
			default:
				t.Errorf("未预期的请求内容: %q", last)
				return &hexagon.CompletionResponse{Content: "??"}, nil
			}
		})
	h := newE2EHarness(t, provider, e2eOptions{})

	m1 := baseMsg("mt-1", "user-1", "我叫小明")
	if _, err := h.eng.Process(context.Background(), m1); err != nil {
		t.Fatalf("首轮失败: %v", err)
	}
	m2 := baseMsg("mt-2", "user-1", "我叫什么")
	m2.SessionID = m1.SessionID
	r2, err := h.eng.Process(context.Background(), m2)
	if err != nil {
		t.Fatalf("次轮失败: %v", err)
	}
	if r2.Content != "你叫小明" {
		t.Fatalf("次轮回复不匹配: %q", r2.Content)
	}

	// 落库应累计 4 条（2 user + 2 assistant）。
	recs := h.dbMessages(t, m1.SessionID)
	if len(recs) != 4 {
		t.Fatalf("两轮后应有 4 条消息，实际 %d: %s", len(recs), dumpRecs(recs))
	}
}

// ============================================================================
// 闭环 5：上下文压缩/归档触发后历史一致性
// ============================================================================

// TestE2E_Compaction_TriggersAndPreservesRecentHistory 验证压缩闭环：
// 当会话消息超过阈值，finalizeReply 异步触发压缩，旧消息被摘要替换、保留最近 N 条。
// 压缩在后台 goroutine 执行，由 eng.Stop()（bgWg.Wait）保证完成，无需 sleep 轮询。
func TestE2E_Compaction_TriggersAndPreservesRecentHistory(t *testing.T) {
	var summarizeCalls atomic.Int32
	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			// 压缩摘要请求的 prompt 含"压缩为简洁的摘要"，据此区分。
			if strings.Contains(allContent(req.Messages), "压缩为简洁的摘要") {
				summarizeCalls.Add(1)
				return &hexagon.CompletionResponse{Content: "历史摘要内容", Usage: hexagon.Usage{TotalTokens: 5}}, nil
			}
			return &hexagon.CompletionResponse{Content: "ok", Usage: hexagon.Usage{TotalTokens: 4}}, nil
		})

	// 阈值压低：MaxMessages=6, KeepRecent=2，几轮对话即可触发。
	h := newE2EHarness(t, provider, e2eOptions{
		compactionEnabled: true,
		maxMessages:       6,
		keepRecent:        2,
	})

	sessionID := ""
	// 跑 5 轮 → 10 条消息，超过 6 的阈值，触发压缩。
	for i := 0; i < 5; i++ {
		m := baseMsg(fmt.Sprintf("cmp-%d", i), "user-1", fmt.Sprintf("第%d个问题", i))
		if sessionID != "" {
			m.SessionID = sessionID
		}
		if _, err := h.eng.Process(context.Background(), m); err != nil {
			t.Fatalf("第 %d 轮失败: %v", i, err)
		}
		sessionID = m.SessionID
	}

	// Stop 会等待所有后台压缩 goroutine 完成（bgWg.Wait）。
	if err := h.eng.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}

	if summarizeCalls.Load() == 0 {
		t.Fatal("超过阈值后应至少触发一次压缩摘要 LLM 调用")
	}

	// 压缩后历史一致性：必须存在一条 system 摘要消息，且消息总数显著小于 10。
	recs := h.dbMessages(t, sessionID)
	var hasSummary bool
	for _, r := range recs {
		if r.Role == "system" && strings.Contains(r.Content, "[上下文摘要]") {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Fatalf("压缩后应存在 [上下文摘要] system 消息，实际全量=%s", dumpRecs(recs))
	}
	if len(recs) >= 10 {
		t.Fatalf("压缩后消息数应小于压缩前(10)，实际 %d", len(recs))
	}

	// 最关键：压缩后会话仍可正常 BuildContext（历史未损坏），
	// 且摘要消息位于历史前部、最近消息保留在尾部。
	ctxMsgs, err := h.mgr.BuildContext(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("压缩后 BuildContext 失败（历史损坏）: %v", err)
	}
	if len(ctxMsgs) == 0 {
		t.Fatal("压缩后 BuildContext 不应为空")
	}
}

// ============================================================================
// 闭环 6：并发会话隔离 —— 不同 session 并发处理互不串台
// ============================================================================

// TestE2E_ConcurrentSessions_Isolation 验证并发会话隔离：
// 多个用户/会话并发调用 Process，各自的回复只能落到各自的会话，
// 不得出现 A 的回复写进 B 的历史（跨会话串台）。
func TestE2E_ConcurrentSessions_Isolation(t *testing.T) {
	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			// 回复直接回显最后一条 user 内容，便于校验归属。
			last := req.Messages[len(req.Messages)-1].Content
			return &hexagon.CompletionResponse{
				Content: "回复:" + last,
				Usage:   hexagon.Usage{TotalTokens: 5},
			}, nil
		})
	// 串行化写，确保本测试只考察"会话隔离"语义，不受 SQLite 写写抖动影响（确定性）。
	// 注：跨会话并发下的 SQLITE_BUSY 丢消息问题，由 TestE2E_ConcurrentWrite_*
	// 专门以确定性方式覆盖。
	h := newE2EHarness(t, provider, e2eOptions{serializeWrites: true})

	const n = 12
	type result struct {
		sessionID string
		want      string
	}
	results := make([]result, n)

	var wg sync.WaitGroup
	var firstErr atomic.Value
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			content := fmt.Sprintf("用户%d-问题", i)
			m := baseMsg(fmt.Sprintf("conc-%d", i), fmt.Sprintf("user-%d", i), content)
			reply, err := h.eng.Process(context.Background(), m)
			if err != nil {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			results[i] = result{sessionID: m.SessionID, want: "回复:" + content}
			if reply.Content != "回复:"+content {
				firstErr.CompareAndSwap(nil, fmt.Errorf("goroutine %d 回复串台: got %q want %q", i, reply.Content, "回复:"+content))
			}
		}(i)
	}
	wg.Wait()
	if e := firstErr.Load(); e != nil {
		t.Fatalf("并发处理出错: %v", e)
	}

	// 逐会话校验隔离：每个会话恰好 1 user + 1 assistant，内容彼此独立。
	seenSessions := map[string]bool{}
	for i, r := range results {
		if r.sessionID == "" {
			t.Fatalf("goroutine %d 未拿到 sessionID", i)
		}
		if seenSessions[r.sessionID] {
			t.Fatalf("会话 ID 冲突（并发未隔离）: %s", r.sessionID)
		}
		seenSessions[r.sessionID] = true

		recs := h.dbMessages(t, r.sessionID)
		if len(recs) != 2 {
			t.Fatalf("会话 %s 应有 2 条消息，实际 %d: %s", r.sessionID, len(recs), dumpRecs(recs))
		}
		var gotAssistant string
		for _, rec := range recs {
			if rec.Role == "assistant" {
				gotAssistant = rec.Content
			}
		}
		if gotAssistant != r.want {
			t.Fatalf("会话 %s 的助手回复串台: got %q want %q", r.sessionID, gotAssistant, r.want)
		}
	}
}

// ============================================================================
// 闭环 7：同一会话并发请求的串行化（session lane）+ 历史不丢
// ============================================================================

// TestE2E_SameSessionConcurrent_NoMessageLoss 验证同一会话被并发打入多条请求时，
// session lane 串行化后，所有 user 消息和对应 assistant 回复都不丢失。
// 这是"并发会话隔离"在同会话维度的另一面：不要求顺序，但要求不丢消息、不串内容。
func TestE2E_SameSessionConcurrent_NoMessageLoss(t *testing.T) {
	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			last := req.Messages[len(req.Messages)-1].Content
			return &hexagon.CompletionResponse{Content: "ans:" + last, Usage: hexagon.Usage{TotalTokens: 5}}, nil
		})
	h := newE2EHarness(t, provider, e2eOptions{})

	// 先建一个会话，拿到 sessionID。
	seed := baseMsg("seed", "user-x", "种子消息")
	if _, err := h.eng.Process(context.Background(), seed); err != nil {
		t.Fatalf("种子请求失败: %v", err)
	}
	sessionID := seed.SessionID

	const n = 8
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := baseMsg(fmt.Sprintf("same-%d", i), "user-x", fmt.Sprintf("并发问题%d", i))
			m.SessionID = sessionID
			if _, err := h.eng.Process(context.Background(), m); err != nil {
				firstErr.CompareAndSwap(nil, err)
			}
		}(i)
	}
	wg.Wait()
	if e := firstErr.Load(); e != nil {
		t.Fatalf("同会话并发出错: %v", e)
	}

	recs := h.dbMessages(t, sessionID)
	// 种子(2) + n 轮各 2 条 = 2 + 2n。
	wantTotal := 2 + 2*n
	if len(recs) != wantTotal {
		t.Fatalf("同会话并发后消息丢失/重复: 期望 %d 条，实际 %d 条: %s", wantTotal, len(recs), dumpRecs(recs))
	}
	// 每条并发 user 消息都应有一条精确匹配其内容的 assistant 回复。
	userContents := map[string]bool{}
	assistantContents := map[string]bool{}
	for _, r := range recs {
		switch r.Role {
		case "user":
			userContents[r.Content] = true
		case "assistant":
			assistantContents[r.Content] = true
		}
	}
	for i := 0; i < n; i++ {
		q := fmt.Sprintf("并发问题%d", i)
		if !userContents[q] {
			t.Fatalf("丢失 user 消息: %q", q)
		}
		if !assistantContents["ans:"+q] {
			t.Fatalf("丢失对应 assistant 回复: %q", "ans:"+q)
		}
	}
}

// ============================================================================
// 闭环 8：token 预算/历史窗口截断后，BuildContext 仍以 user 消息开头
// ============================================================================

// TestE2E_BuildContext_BudgetCutKeepsValidShape 验证上下文预算截断的跨层一致性：
// 当历史超出 token 预算被从前部截断时，BuildContext 必须保证截断后首条是 user 消息
// （避免给 LLM 一个以孤立 assistant 开头的非法消息序列）。
func TestE2E_BuildContext_BudgetCutKeepsValidShape(t *testing.T) {
	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			// 助手回复填充较长内容，加速触达 token 预算。
			return &hexagon.CompletionResponse{
				Content: strings.Repeat("助", 400),
				Usage:   hexagon.Usage{TotalTokens: 5},
			}, nil
		})

	// 关掉压缩，单独考察 BuildContext 的预算截断逻辑。
	h := newE2EHarness(t, provider, e2eOptions{maxTurns: 200})

	sessionID := ""
	for i := 0; i < 8; i++ {
		m := baseMsg(fmt.Sprintf("bud-%d", i), "user-1", strings.Repeat("问", 400))
		if sessionID != "" {
			m.SessionID = sessionID
		}
		if _, err := h.eng.Process(context.Background(), m); err != nil {
			t.Fatalf("第 %d 轮失败: %v", i, err)
		}
		sessionID = m.SessionID
	}

	// 直接以一个很小的 token 预算重建上下文，强制触发前部截断。
	tightCfg := config.DefaultConfig().Memory
	tightCfg.Conversation.MaxTurns = 200
	tightCfg.Conversation.TokenBudget = 200 // 极小预算，必然截断
	tightMgr := session.NewManager(h.store, tightCfg)

	msgs, err := tightMgr.BuildContext(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("紧预算 BuildContext 失败: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("紧预算下 BuildContext 不应为空")
	}
	if msgs[0].Role != "user" {
		t.Fatalf("预算截断后首条消息必须是 user（避免孤立 assistant），实际首条 role=%q", msgs[0].Role)
	}
}

// ============================================================================
// 闭环 9：持久化失败的跨层传播（关键集成断裂点）
//
//	engine.finalizeReply 在 SaveAssistantReply 失败时只 trace.L().Error 记日志，
//	不把错误返回给 Process 的调用方。于是 Process 返回一条"成功"的 reply，
//	但助手回复并未落库 —— 闭环看似闭合实则断裂，调用方/前端拿到了一条
//	永远读不回来的回复。本测试用确定性的失败 store 钉死这一行为。
// ============================================================================

// failingAssistantStore 包装真实 store，让 assistant 角色的 SaveMessage 失败，
// 其余操作（含 user 消息）正常透传。用于确定性复现持久化失败场景。
type failingAssistantStore struct {
	storage.Store
	failAssistant atomic.Bool
}

func (s *failingAssistantStore) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	if msg.Role == "assistant" && s.failAssistant.Load() {
		return fmt.Errorf("注入的存储故障: assistant 写入失败")
	}
	return s.Store.SaveMessage(ctx, msg)
}

// WithTx 必须用包装后的 store 调用 fn，否则压缩等事务路径会绕过故障注入。
// 这里直接透传真实事务（assistant 故障只针对主路径 SaveAssistantReply）。
func (s *failingAssistantStore) WithTx(ctx context.Context, fn func(storage.Store) error) error {
	return s.Store.WithTx(ctx, fn)
}

// TestE2E_PersistFailure_SwallowedBreaksClosedLoop 验证持久化失败的跨层行为。
//
// 回归: W12-Bug1 —— engine 吞掉助手回复持久化错误，Process 返回成功但回复永读不回（假闭合）。
// 修复后 finalizeReply 在 SaveAssistantReply 失败时把 persist_error 写入 reply.Metadata，
// 让调用方可观测；本用例断言"未落库时调用方必须感知失败信号"，不得弱化。
//
// 期望（正确的闭环语义）：助手回复落库失败时，调用方应当能感知到 —— 要么
// Process 返回错误，要么 reply 上带出"未持久化"的明确信号。否则就是
// “响应没真正落库但调用方以为成功”的闭环断裂。
//
// 实测：当前实现吞掉了 SaveAssistantReply 错误，Process 返回成功 reply，
// 但存储中查无此 assistant 消息，且 reply 不携带任何失败信号。
func TestE2E_PersistFailure_SwallowedBreaksClosedLoop(t *testing.T) {
	dir := t.TempDir()
	real, err := sqlitestore.New(filepath.Join(dir, "persistfail.db"))
	if err != nil {
		t.Fatalf("创建底层 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("初始化 store 失败: %v", err)
	}
	fs := &failingAssistantStore{Store: real}

	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "这条回复将无法落库", Usage: hexagon.Usage{TotalTokens: 5}}, nil
		})

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "mock"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"mock": {Model: "mock-model"}}
	cfg.Knowledge.Enabled = false
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "off" // 无工具，走最直白的 Complete → finalizeReply 路径

	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"mock": provider})
	reg := skill.NewRegistry()
	eng := engine.NewReActEngine(cfg, router, fs, reg)
	eng.SetToolCollector(engine.NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(engine.NewToolExecutor(reg, nil))
	eng.SetSessionLock(session.NewSessionLock())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	fs.failAssistant.Store(true)

	msg := baseMsg("pf-1", "user-1", "你好")
	reply, procErr := eng.Process(context.Background(), msg)

	// 存储真相：assistant 回复确实没有落库。
	mgr := session.NewManager(real, cfg.Memory)
	got, err := mgr.BuildContext(context.Background(), msg.SessionID)
	if err != nil {
		t.Fatalf("BuildContext 失败: %v", err)
	}
	var persistedAssistant bool
	for _, m := range got {
		if m.Role == "assistant" {
			persistedAssistant = true
		}
	}
	if persistedAssistant {
		t.Skip("assistant 回复意外落库，故障注入未生效，跳过（非缺陷）")
	}

	// 此刻：回复未落库。正确闭环要求调用方能感知。
	callerAwareOfFailure := procErr != nil
	if reply != nil && reply.Metadata != nil {
		if _, ok := reply.Metadata["persist_error"]; ok {
			callerAwareOfFailure = true
		}
	}
	if !callerAwareOfFailure {
		t.Errorf("[闭环断裂] 助手回复未落库（BuildContext 读不到 assistant），"+
			"但 Process 返回成功且无任何失败信号: procErr=%v reply=%+v。"+
			"finalizeReply 吞掉了 SaveAssistantReply 错误，调用方以为响应已持久化。", procErr, reply)
	}
}

// busyOnceStore 在第一次 user 消息写入时返回一个 SQLITE_BUSY 风格错误，
// 之后恢复正常。用于确定性复现"并发写竞争导致一次写失败"的场景，
// 而无需依赖真实的 SQLite 并发抖动。
type busyOnceStore struct {
	storage.Store
	tripped atomic.Bool
}

func (s *busyOnceStore) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	if msg.Role == "user" && s.tripped.CompareAndSwap(false, true) {
		// 模拟 modernc.org/sqlite 在 WAL 写写冲突下抛出的 database is locked。
		return fmt.Errorf("database is locked (517)")
	}
	return s.Store.SaveMessage(ctx, msg)
}
func (s *busyOnceStore) WithTx(ctx context.Context, fn func(storage.Store) error) error {
	return s.Store.WithTx(ctx, fn)
}

// TestE2E_ConcurrentWrite_BusySwallowedLosesMessage 以确定性方式钉死并发写竞争下的
// 闭环断裂：一次 SQLITE_BUSY 级别的写失败被 engine 吞掉，Process 仍返回成功，
// 但用户消息从未落库 —— 后续 BuildContext 读不到，且该轮的助手回复因此脱离上下文。
//
// 这正是 TestE2E_ConcurrentSessions_Isolation 在真实 SQLite 下偶发观测到的
// "应有 2 条消息，实际 1 条"的根因，此处用注入故障使其稳定可复现。
//
// 回归: W12-Bug2 —— 修复后 session 写路径对 BUSY/517 做有限退避重试，一次性故障
// 会在重试后成功落库（此时该用例走 t.Skip("意外落库") 分支，证明消息未丢）；若重试
// 仍无法落库，则要求 persist_error 信号随 reply 返回，调用方可感知。两条断言都不弱化。
// 重试后必定落库的正向断言见 TestE2E_BusyWrite_RetriedAndPersisted。
func TestE2E_ConcurrentWrite_BusySwallowedLosesMessage(t *testing.T) {
	dir := t.TempDir()
	real, err := sqlitestore.New(filepath.Join(dir, "busy.db"))
	if err != nil {
		t.Fatalf("创建底层 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("初始化 store 失败: %v", err)
	}
	bs := &busyOnceStore{Store: real}

	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "助手回复", Usage: hexagon.Usage{TotalTokens: 5}}, nil
		})

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "mock"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"mock": {Model: "mock-model"}}
	cfg.Knowledge.Enabled = false
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "off"

	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"mock": provider})
	reg := skill.NewRegistry()
	eng := engine.NewReActEngine(cfg, router, bs, reg)
	eng.SetToolCollector(engine.NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(engine.NewToolExecutor(reg, nil))
	eng.SetSessionLock(session.NewSessionLock())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	msg := baseMsg("busy-1", "user-1", "这条用户消息会因 BUSY 丢失")
	reply, procErr := eng.Process(context.Background(), msg)

	// 用底层真实 store 验证真相：user 消息确实没落库。
	recs, err := real.ListMessages(context.Background(), msg.SessionID, 100, 0)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	var userPersisted bool
	for _, r := range recs {
		if r.Role == "user" {
			userPersisted = true
		}
	}
	if userPersisted {
		t.Skip("user 消息意外落库，故障注入未生效，跳过（非缺陷）")
	}

	// 正确闭环要求：用户消息丢失这一事实必须能被调用方感知。
	callerAware := procErr != nil
	if reply != nil && reply.Metadata != nil {
		if _, ok := reply.Metadata["persist_error"]; ok {
			callerAware = true
		}
	}
	if !callerAware {
		t.Errorf("[闭环断裂] 用户消息因 SQLITE_BUSY 写失败被吞掉，未落库，"+
			"但 Process 返回成功且无失败信号: procErr=%v reply.Content=%q。"+
			"engine.Process 仅 trace.L().Error 记录 SaveUserMessage 失败，"+
			"导致并发写竞争下静默丢消息、上下文残缺。", procErr, reply.Content)
	}
}

// TestE2E_BusyWrite_RetriedAndPersisted 正向钉死 W12-Bug2 修复：
//
// 回归: W12-Bug2 —— 注入一次性 SQLITE_BUSY(517) 写失败后，session 写路径的有限退避
// 重试必须让用户消息最终成功落库（而不是丢消息）。断言用户消息可读回 + 闭环 2 条消息齐全，
// 且 reply 不带 persist_error（因为重试已成功）。这是比 *_SwallowedLosesMessage 的 Skip
// 分支更强的正向证据：直接证明"重试 → 落库"链路成立。
func TestE2E_BusyWrite_RetriedAndPersisted(t *testing.T) {
	dir := t.TempDir()
	real, err := sqlitestore.New(filepath.Join(dir, "busy_retry.db"))
	if err != nil {
		t.Fatalf("创建底层 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("初始化 store 失败: %v", err)
	}
	bs := &busyOnceStore{Store: real}

	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			return &hexagon.CompletionResponse{Content: "助手回复", Usage: hexagon.Usage{TotalTokens: 5}}, nil
		})

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "mock"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"mock": {Model: "mock-model"}}
	cfg.Knowledge.Enabled = false
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "off"

	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"mock": provider})
	reg := skill.NewRegistry()
	eng := engine.NewReActEngine(cfg, router, bs, reg)
	eng.SetToolCollector(engine.NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(engine.NewToolExecutor(reg, nil))
	eng.SetSessionLock(session.NewSessionLock())
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	msg := baseMsg("busy-retry-1", "user-1", "这条用户消息应在 BUSY 重试后落库")
	reply, procErr := eng.Process(context.Background(), msg)
	if procErr != nil {
		t.Fatalf("Process 应成功（BUSY 已被重试吸收），却返回错误: %v", procErr)
	}

	// 注入的一次性 BUSY 必须已触发过（否则没测到重试路径）。
	if !bs.tripped.Load() {
		t.Fatal("busyOnceStore 未触发，故障注入未生效，无法验证重试落库")
	}

	// 用户消息必须最终落库（重试成功）。
	recs, err := real.ListMessages(context.Background(), msg.SessionID, 100, 0)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	var userPersisted, assistantPersisted bool
	for _, r := range recs {
		switch r.Role {
		case "user":
			userPersisted = true
		case "assistant":
			assistantPersisted = true
		}
	}
	if !userPersisted {
		t.Fatalf("[W12-Bug2] 一次性 BUSY 后用户消息应经重试落库，实际未落库: %s", dumpRecs(recs))
	}
	if !assistantPersisted {
		t.Fatalf("[W12-Bug2] 助手回复应落库，实际未落库: %s", dumpRecs(recs))
	}
	// 重试成功后，reply 不应携带 persist_error（因为最终没有失败）。
	if reply != nil && reply.Metadata != nil {
		if pe, ok := reply.Metadata["persist_error"]; ok {
			t.Fatalf("[W12-Bug2] 重试已成功落库，reply 不应带 persist_error，实际=%q", pe)
		}
	}
}

// laneProbeStore 探测同一会话的写入是否被串行化。
//
// 它在每次 SaveMessage 进入时按 sessionID 标记"该会话正有一个写入在途"，
// 若发现同一会话已有在途写入（即并发重入），记录一次串行化违规。短暂 sleep
// 放大并发窗口，让"无锁交错"稳定可观测，不依赖墙钟做决策（只放大窗口）。
type laneProbeStore struct {
	storage.Store
	mu         sync.Mutex
	inflight   map[string]bool
	violations atomic.Int32
}

func newLaneProbeStore(inner storage.Store) *laneProbeStore {
	return &laneProbeStore{Store: inner, inflight: map[string]bool{}}
}

func (s *laneProbeStore) SaveMessage(ctx context.Context, msg *storage.MessageRecord) error {
	s.mu.Lock()
	if s.inflight[msg.SessionID] {
		// 同一会话已有写入在途仍被并发重入 → session lane 未串行化。
		s.violations.Add(1)
	}
	s.inflight[msg.SessionID] = true
	s.mu.Unlock()

	// 放大并发窗口（仅用于让交错稳定可观测，不参与正确性判定）。
	time.Sleep(2 * time.Millisecond)
	err := s.Store.SaveMessage(ctx, msg)

	s.mu.Lock()
	s.inflight[msg.SessionID] = false
	s.mu.Unlock()
	return err
}

func (s *laneProbeStore) WithTx(ctx context.Context, fn func(storage.Store) error) error {
	return s.Store.WithTx(ctx, fn)
}

// TestE2E_NewReActEngine_DefaultSessionLockSerializes 钉死 W12-Bug3 修复：
//
// 回归: W12-Bug3 —— NewReActEngine 必须内置默认 session 锁，调用方即使漏调
// SetSessionLock，同一会话的并发请求也应被串行化。本用例显式构造一个【从不】调用
// SetSessionLock 的引擎，对同一 session 打入并发请求，用 laneProbeStore 直接探测
// "同会话写入是否被串行化"。修复前（acquireSessionLane 在 lane/lock 均 nil 时直接
// return(nil,nil) 零串行化）会观测到并发重入违规；修复后默认锁串行化，违规必须为 0。
//
// 注：本断言探测的是"串行化语义"本身，独立于 BUSY 重试（重试只能掩盖丢消息、
// 掩盖不了并发重入），因此即便存在 sqlite 重试，缺默认锁时本用例仍会失败。
func TestE2E_NewReActEngine_DefaultSessionLockSerializes(t *testing.T) {
	dir := t.TempDir()
	real, err := sqlitestore.New(filepath.Join(dir, "defaultlock.db"))
	if err != nil {
		t.Fatalf("创建底层 store 失败: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })
	if err := real.Init(context.Background()); err != nil {
		t.Fatalf("初始化 store 失败: %v", err)
	}
	probe := newLaneProbeStore(real)

	provider := mockllm.NewLLMProvider("mock").WithResponseFn(
		func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
			last := req.Messages[len(req.Messages)-1].Content
			return &hexagon.CompletionResponse{Content: "ans:" + last, Usage: hexagon.Usage{TotalTokens: 5}}, nil
		})

	cfg := config.DefaultConfig()
	cfg.LLM.Default = "mock"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{"mock": {Model: "mock-model"}}
	cfg.Knowledge.Enabled = false
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "off"

	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{"mock": provider})
	reg := skill.NewRegistry()
	// 关键：构造后【不】调用 SetSessionLock，只依赖 NewReActEngine 的内置默认锁。
	eng := engine.NewReActEngine(cfg, router, probe, reg)
	eng.SetToolCollector(engine.NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(engine.NewToolExecutor(reg, nil))
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })

	// 先建一个会话拿到 sessionID。
	seed := baseMsg("dl-seed", "user-x", "种子消息")
	if _, err := eng.Process(context.Background(), seed); err != nil {
		t.Fatalf("种子请求失败: %v", err)
	}
	sessionID := seed.SessionID

	const n = 8
	var wg sync.WaitGroup
	var firstErr atomic.Value
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := baseMsg(fmt.Sprintf("dl-%d", i), "user-x", fmt.Sprintf("默认锁并发%d", i))
			m.SessionID = sessionID
			if _, err := eng.Process(context.Background(), m); err != nil {
				firstErr.CompareAndSwap(nil, err)
			}
		}(i)
	}
	wg.Wait()
	if e := firstErr.Load(); e != nil {
		t.Fatalf("默认锁下同会话并发出错: %v", e)
	}

	// 核心断言：默认锁必须把同会话写入串行化，零并发重入违规。
	if v := probe.violations.Load(); v != 0 {
		t.Fatalf("[W12-Bug3] NewReActEngine 默认 session 锁未生效：同会话并发写入发生 %d 次重入（未串行化）", v)
	}

	// 辅助断言：串行化后消息不丢。种子(2) + n 轮各 2 条 = 2 + 2n。
	recs, err := real.ListMessages(context.Background(), sessionID, 1000, 0)
	if err != nil {
		t.Fatalf("读取消息失败: %v", err)
	}
	wantTotal := 2 + 2*n
	if len(recs) != wantTotal {
		t.Fatalf("[W12-Bug3] 默认 session 锁未生效，同会话并发丢/重复消息: 期望 %d 条，实际 %d 条: %s",
			wantTotal, len(recs), dumpRecs(recs))
	}
}

// ============================================================================
// 辅助函数
// ============================================================================

// allContent 把请求里所有消息的文本（含多模态文本片段）拼成一个字符串，便于断言。
func allContent(msgs []hexagon.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		sb.WriteString(string(m.Role))
		sb.WriteByte(':')
		sb.WriteString(m.Content)
		for _, part := range m.MultiContent {
			sb.WriteString(part.Text)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// dumpRecs 把消息记录格式化为可读字符串用于失败诊断。
func dumpRecs(recs []*storage.MessageRecord) string {
	b, _ := json.MarshalIndent(simplify(recs), "", "  ")
	return string(b)
}

func simplify(recs []*storage.MessageRecord) []map[string]string {
	out := make([]map[string]string, 0, len(recs))
	for _, r := range recs {
		c := r.Content
		if len(c) > 80 {
			c = c[:80] + "…"
		}
		out = append(out, map[string]string{
			"role":    r.Role,
			"content": c,
			"at":      r.CreatedAt.Format(time.RFC3339Nano),
		})
	}
	return out
}
