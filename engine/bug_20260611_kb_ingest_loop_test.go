package engine

// ====================================================================
// BUG-20260611: knowledge-base ingestion loop broken — cron agent jobs
// hijacked by the skill keyword fast path.
//
// Symptom: cron-dispatched agent jobs report status=success but the
//          knowledge base stays empty; run history stdout looks like
//          "摘要：…" (local extractive summary output).
// Cause:   messages dispatched by the cron AgentRunner (source=cron)
//          hit skills.Match before anything else — a job prompt that
//          starts with "总结" prefix-matches SummarySkill, the local
//          summarizer replies directly, and the full ReAct tool loop
//          (including knowledge_ingest) never runs.
// Contract: source=cron messages must skip the skill keyword fast path
//          and take the LLM main path (same precedent as the cron
//          bypass of intent guidance in buildCompletionRequest).
// ====================================================================

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hexagon-codes/ai-core/llm"
	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexagon/rag/splitter"
	mockllm "github.com/hexagon-codes/hexagon/testing/mock"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	agentrouter "github.com/hexagon-codes/hexclaw/router"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

// newEngineWithProviderAndSkills builds an engine with a mock LLM provider
// and a caller-supplied skill registry.
func newEngineWithProviderAndSkills(t *testing.T, provider hexagon.Provider, skills *skill.DefaultRegistry) *ReActEngine {
	t.Helper()

	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Compaction.Enabled = false
	cfg.LLM.Default = "test"
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"test": {Model: "mock-model"},
	}
	router := llmrouter.NewWithProviders(cfg.LLM, map[string]hexagon.Provider{
		"test": provider,
	})

	eng := NewReActEngine(cfg, router, store, skills)
	if err := eng.Start(context.Background()); err != nil {
		t.Fatalf("failed to start engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	return eng
}

// cronAgentMessage builds a cron-dispatched message through the same
// production constructor the cmd/hexclaw AgentRunner uses, so these tests
// also regression-lock the source=cron stamping contract.
func cronAgentMessage(id string) *adapter.Message {
	msg := NewCronDispatchMessage("user-001", "chat-001", "job-test",
		"总结今天的科技新闻要点，整理后加入知识库")
	msg.ID = id
	return msg
}

// Contract lock: NewCronDispatchMessage is the single place that stamps the
// metadata the engine guards depend on. If the stamping is ever dropped, the
// fast-path guard silently dies — this test fails first.
func TestBug20260611_CronDispatchMessageContract(t *testing.T) {
	msg := NewCronDispatchMessage("u-1", "c-1", "job-9", "summarize prompt")

	if msg.Metadata["source"] != "cron" {
		t.Errorf("cron dispatch must stamp source=cron, got %q", msg.Metadata["source"])
	}
	if msg.Metadata["cron_job_id"] != "job-9" {
		t.Errorf("cron dispatch must carry cron_job_id, got %q", msg.Metadata["cron_job_id"])
	}
	if msg.Platform != adapter.PlatformCron {
		t.Errorf("cron dispatch platform must be cron (excluded from chat listings), got %q", msg.Platform)
	}
	if msg.UserID != "u-1" || msg.ChatID != "c-1" {
		t.Errorf("field pass-through wrong: %+v", msg)
	}
	if !strings.HasPrefix(msg.Content, "summarize prompt") {
		t.Errorf("prompt must lead the content, got %q", msg.Content)
	}
	if !strings.Contains(msg.Content, "TASK_STATUS:") {
		t.Errorf("content must carry the outcome contract, got %q", msg.Content)
	}

	// Empty chatID falls back to a stable per-job scope so repeated runs
	// reuse one session instead of creating one per tick (BUG-20260613).
	noChat := NewCronDispatchMessage("u-1", "", "job-9", "p")
	if noChat.ChatID != "cron:job-9" {
		t.Errorf("empty chatID must fall back to cron:<jobID>, got %q", noChat.ChatID)
	}
}

// Before fix: SummarySkill.Match("总结…") prefix-matches → fast path returns
//
//	"摘要：…" and the LLM provider is never called.
//
// After fix:  source=cron skips the fast path → LLM main path, provider
//
//	called exactly once.
func TestBug20260611_CronDispatchSkipsSkillFastPath_Process(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{
			Content: "agent run done",
			Usage:   hexagon.Usage{TotalTokens: 8},
		}, nil
	})

	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("failed to register SummarySkill: %v", err)
	}
	eng := newEngineWithProviderAndSkills(t, provider, skills)

	reply, err := eng.Process(context.Background(), cronAgentMessage("msg-bug-20260611-sync"))
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if strings.HasPrefix(reply.Content, "摘要：") {
		t.Errorf("[BUG-20260611] cron dispatch hijacked by summary fast path, got local summary: %q", reply.Content)
	}
	if provider.CallCount() != 1 {
		t.Errorf("[BUG-20260611] cron dispatch should take the LLM main path (want 1 provider call, got %d)", provider.CallCount())
	}
	if reply.Content != "agent run done" {
		t.Errorf("want LLM reply %q, got %q", "agent run done", reply.Content)
	}
}

// Same contract for the streaming path (the skills.Match call site in
// ProcessStream is an independent code path).
func TestBug20260611_CronDispatchSkipsSkillFastPath_Stream(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{
			Content: "agent stream done",
			Usage:   hexagon.Usage{TotalTokens: 8},
		}, nil
	})

	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("failed to register SummarySkill: %v", err)
	}
	eng := newEngineWithProviderAndSkills(t, provider, skills)

	ch, err := eng.ProcessStream(context.Background(), cronAgentMessage("msg-bug-20260611-stream"))
	if err != nil {
		t.Fatalf("ProcessStream failed: %v", err)
	}

	var full strings.Builder
	for chunk := range ch {
		full.WriteString(chunk.Content)
		for _, tc := range chunk.ToolCalls {
			if tc.Name == "summary" {
				t.Errorf("[BUG-20260611] streaming cron dispatch hijacked by summary fast path")
			}
		}
	}

	if strings.HasPrefix(full.String(), "摘要：") {
		t.Errorf("[BUG-20260611] streaming cron dispatch returned local summary: %q", full.String())
	}
	if provider.CallCount() != 1 {
		t.Errorf("[BUG-20260611] streaming cron dispatch should take the LLM main path (want 1 provider call, got %d)", provider.CallCount())
	}
}

// End-to-end lock for the full ingestion loop: a cron-dispatched job whose
// prompt starts with "总结" goes through the ReAct tool loop, the LLM calls
// knowledge_ingest, and the document lands in a real SQLite knowledge base.
// This is the user's core "collect → ingest" scenario that was silently
// broken before the fix.
func TestBug20260611_CronDispatchKnowledgeIngestClosedLoop(t *testing.T) {
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

	// Real SQLite-backed knowledge base (FTS-only: nil embedder degrades to
	// text indexing, same as production without an embedding provider).
	kbStore := knowledge.NewSQLiteStore(store.DB())
	if err := kbStore.Init(ctx); err != nil {
		t.Fatalf("failed to init knowledge store: %v", err)
	}
	kbMgr := knowledge.NewManager(kbStore, kbStore, nil, knowledge.WithSplitter(
		splitter.NewRecursiveSplitter(
			splitter.WithRecursiveChunkSize(400),
			splitter.WithRecursiveChunkOverlap(80),
		),
	))

	// Turn 1: the LLM answers the cron prompt with a knowledge_ingest call.
	// Turn 2: it confirms completion.
	var turns int32
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		if atomic.AddInt32(&turns, 1) == 1 {
			hasIngestTool := false
			for _, tool := range req.Tools {
				if tool.Function.Name == "knowledge_ingest" {
					hasIngestTool = true
				}
			}
			if !hasIngestTool {
				t.Errorf("[BUG-20260611] knowledge_ingest missing from tools offered to the LLM")
			}
			return &hexagon.CompletionResponse{
				ToolCalls: []llm.ToolCall{{
					ID:        "tc-ingest-1",
					Type:      "function",
					Name:      "knowledge_ingest",
					Arguments: `{"title":"tech news digest","content":"key points: model release, chip production ramp-up","source":"cron:job-test"}`,
				}},
				Usage: hexagon.Usage{TotalTokens: 16},
			}, nil
		}
		return &hexagon.CompletionResponse{
			Content: "digest saved to knowledge base",
			Usage:   hexagon.Usage{TotalTokens: 8},
		}, nil
	})

	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("failed to register SummarySkill: %v", err)
	}
	if err := skills.Register(builtin.NewKnowledgeIngestSkill(kbMgr)); err != nil {
		t.Fatalf("failed to register KnowledgeIngestSkill: %v", err)
	}

	eng := newEngineWithProviderAndSkills(t, provider, skills)
	eng.cfg.LLM.Tools.Enabled = "on"
	eng.SetToolCollector(NewToolCollector(skills, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(skills, nil))

	reply, err := eng.Process(ctx, cronAgentMessage("msg-bug-20260611-e2e"))
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if reply.Content != "digest saved to knowledge base" {
		t.Errorf("want final LLM confirmation, got %q", reply.Content)
	}

	// The decisive assertion: the knowledge base actually holds the document
	// (before the fix this was 0 even though the job reported success).
	docs, err := kbStore.List(ctx)
	if err != nil {
		t.Fatalf("failed to list knowledge documents: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("[BUG-20260611] knowledge base should hold exactly 1 document, got %d", len(docs))
	}
	doc, err := kbStore.Get(ctx, docs[0].ID)
	if err != nil {
		t.Fatalf("failed to load document: %v", err)
	}
	if doc.Title != "tech news digest" || !strings.Contains(doc.Content, "chip production ramp-up") {
		t.Errorf("ingested document mismatch: title=%q content=%q", doc.Title, doc.Content)
	}
	if doc.Source != "cron:job-test" {
		t.Errorf("document source should be the cron job marker, got %q", doc.Source)
	}

	// Retrieval leg of the loop: the ingested document must be findable via
	// the production read path (the original field evidence was "job success
	// but KB search returns 0 results"). With a nil embedder the manager
	// degrades to FTS text search, same as production without an embedding
	// provider.
	hits, err := kbStore.TextSearch(ctx, "chip production", 5)
	if err != nil {
		t.Fatalf("TextSearch failed: %v", err)
	}
	if len(hits) == 0 {
		t.Errorf("[BUG-20260611] ingested content is not retrievable via text search")
	}
	kbResult, err := kbMgr.Query(ctx, "chip production ramp-up", 3)
	if err != nil {
		t.Fatalf("knowledge Query failed: %v", err)
	}
	if !strings.Contains(kbResult, "chip production") {
		t.Errorf("[BUG-20260611] knowledge Query should surface the ingested content, got %q", kbResult)
	}
}

// brokenKB simulates a knowledge-base outage at the ingestion boundary.
type brokenKB struct{}

func (brokenKB) AddDocument(context.Context, string, string, string) (*knowledge.Document, error) {
	return nil, errInjectedKBOutage
}

var errInjectedKBOutage = errors.New("knowledge base offline (injected fault)")

// Fault injection (test-closure method #2): when the knowledge base fails,
// the error must flow back into the tool loop as a tool result — so the LLM
// sees the failure instead of the run silently "succeeding" — and the engine
// must neither panic nor abort the conversation.
func TestBug20260611_KnowledgeIngestFaultInjection(t *testing.T) {
	var turns int32
	var sawToolError atomic.Bool
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(req hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		if atomic.AddInt32(&turns, 1) == 1 {
			return &hexagon.CompletionResponse{
				ToolCalls: []llm.ToolCall{{
					ID:        "tc-ingest-fault",
					Type:      "function",
					Name:      "knowledge_ingest",
					Arguments: `{"title":"t","content":"c","source":"cron:job-test"}`,
				}},
				Usage: hexagon.Usage{TotalTokens: 16},
			}, nil
		}
		// Turn 2: the runtime must have fed the tool failure back as a
		// RoleTool message containing the injected error.
		for _, m := range req.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "knowledge base offline") {
				sawToolError.Store(true)
			}
		}
		return &hexagon.CompletionResponse{
			Content: "could not save: knowledge base unavailable",
			Usage:   hexagon.Usage{TotalTokens: 8},
		}, nil
	})

	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewKnowledgeIngestSkill(brokenKB{})); err != nil {
		t.Fatalf("failed to register KnowledgeIngestSkill: %v", err)
	}

	eng := newEngineWithProviderAndSkills(t, provider, skills)
	eng.cfg.LLM.Tools.Enabled = "on"
	eng.SetToolCollector(NewToolCollector(skills, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(skills, nil))

	reply, err := eng.Process(context.Background(), cronAgentMessage("msg-bug-20260611-fault"))
	if err != nil {
		t.Fatalf("KB outage must not abort the conversation: %v", err)
	}
	if !sawToolError.Load() {
		t.Error("[BUG-20260611] injected KB failure never reached the LLM as a tool error message")
	}
	if reply.Content != "could not save: knowledge base unavailable" {
		t.Errorf("final reply should be the LLM's failure acknowledgement, got %q", reply.Content)
	}
}

// Install-test finding #1: cron-dispatched jobs were re-routed by the chat
// agent router (rule/semantic classification) to arbitrary chat personas —
// in production a local-model persona with tools disabled, so the model
// pantomimed knowledge_ingest in markdown and the run was a false success.
// Cron jobs must execute on the engine's default provider, untouched by chat
// routing.
func TestBug20260611_CronDispatchSkipsAgentRouting(t *testing.T) {
	defaultProvider := mockllm.NewLLMProvider("test").WithResponseFn(func(hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "default provider reply", Usage: hexagon.Usage{TotalTokens: 8}}, nil
	})
	personaProvider := mockllm.NewLLMProvider("local-persona-llm").WithResponseFn(func(hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "persona reply", Usage: hexagon.Usage{TotalTokens: 8}}, nil
	})

	eng := newEngineWithProviders(t, map[string]hexagon.Provider{
		"test":              defaultProvider,
		"local-persona-llm": personaProvider,
	}, map[string]config.LLMProviderConfig{
		"test":              {Model: "mock-model"},
		"local-persona-llm": {Model: "mock-local"},
	}, "test")

	dispatcher := agentrouter.New()
	if err := dispatcher.Register(agentrouter.AgentConfig{Name: "persona", Provider: "local-persona-llm"}); err != nil {
		t.Fatalf("failed to register agent: %v", err)
	}
	// Matches every API-platform message — exactly how a broad user rule (or
	// the semantic-classifier fallback) captured the production cron run.
	if err := dispatcher.AddRule(agentrouter.Rule{Platform: "api", AgentName: "persona", Priority: 1}); err != nil {
		t.Fatalf("failed to add rule: %v", err)
	}
	eng.SetAgentRouter(dispatcher)

	reply, err := eng.Process(context.Background(), cronAgentMessage("msg-bug-20260611-route"))
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}
	if personaProvider.CallCount() != 0 {
		t.Errorf("[BUG-20260611] cron dispatch was re-routed to a chat persona provider (%d calls)", personaProvider.CallCount())
	}
	if defaultProvider.CallCount() != 1 || reply.Content != "default provider reply" {
		t.Errorf("cron dispatch should run on the default provider (calls=%d, reply=%q)", defaultProvider.CallCount(), reply.Content)
	}

	// Control: a regular user message on the same platform still follows the
	// agent routing rule.
	userReply, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "msg-bug-20260611-route-user",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "随便聊聊今天的天气怎么样呢",
	})
	if err != nil {
		t.Fatalf("Process (user) failed: %v", err)
	}
	if personaProvider.CallCount() != 1 || userReply.Content != "persona reply" {
		t.Errorf("regular user message should still be agent-routed (persona calls=%d, reply=%q)", personaProvider.CallCount(), userReply.Content)
	}
}

// Install-test finding #2: recurring cron jobs with a static prompt hit the
// semantic cache (enabled by default, 24h TTL) — observed as 5-11ms "success"
// runs that returned the previous answer without executing anything. A
// scheduled job must re-execute every tick; the cache is for conversational
// queries.
func TestBug20260611_CronDispatchSkipsSemanticCache(t *testing.T) {
	provider := mockllm.NewLLMProvider("test").WithResponseFn(func(hexagon.CompletionRequest) (*hexagon.CompletionResponse, error) {
		return &hexagon.CompletionResponse{Content: "fresh execution", Usage: hexagon.Usage{TotalTokens: 8}}, nil
	})
	skills := skill.NewRegistry()
	eng := newEngineWithProviderAndSkills(t, provider, skills)

	for i := 1; i <= 2; i++ {
		if _, err := eng.Process(context.Background(), cronAgentMessage("msg-bug-20260611-cache")); err != nil {
			t.Fatalf("Process run %d failed: %v", i, err)
		}
	}
	if provider.CallCount() != 2 {
		t.Errorf("[BUG-20260611] recurring cron run was served from the semantic cache (want 2 provider calls, got %d)", provider.CallCount())
	}

	// Control: identical user messages still benefit from the cache.
	userMsg := func(id string) *adapter.Message {
		return &adapter.Message{
			ID: id, Platform: adapter.PlatformAPI, UserID: "user-001",
			Content: "请问 Go 的 context 取消是怎么传播的？",
		}
	}
	if _, err := eng.Process(context.Background(), userMsg("msg-cache-u1")); err != nil {
		t.Fatalf("Process (user 1) failed: %v", err)
	}
	if _, err := eng.Process(context.Background(), userMsg("msg-cache-u2")); err != nil {
		t.Fatalf("Process (user 2) failed: %v", err)
	}
	if provider.CallCount() != 3 {
		t.Errorf("identical user messages should hit the cache (want 3 total provider calls, got %d)", provider.CallCount())
	}
}

// The guard applies to cron dispatch only: regular user messages keep the
// skill keyword fast path behavior unchanged.
func TestBug20260611_UserMessageFastPathUnchanged(t *testing.T) {
	provider := mockllm.NewLLMProvider("test")

	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("failed to register SummarySkill: %v", err)
	}
	eng := newEngineWithProviderAndSkills(t, provider, skills)

	reply, err := eng.Process(context.Background(), &adapter.Message{
		ID:       "msg-bug-20260611-user",
		Platform: adapter.PlatformAPI,
		UserID:   "user-001",
		Content:  "总结一下：今天发布了新版本，修复了若干问题，性能提升明显。",
	})
	if err != nil {
		t.Fatalf("Process failed: %v", err)
	}

	if !strings.HasPrefix(reply.Content, "摘要：") {
		t.Errorf("regular user message should keep the summary fast path, got: %q", reply.Content)
	}
	if provider.CallCount() != 0 {
		t.Errorf("fast path should not call the LLM, got %d calls", provider.CallCount())
	}
}
