package engine

// Real-LLM decision eval (BUG-20260611, the last leg of the closed loop):
// verifies that an actual model — given the production tool definitions and
// a cron-dispatched "summarize … and add to the knowledge base" prompt —
// decides on its own to call knowledge_ingest, and that the document lands
// in a real SQLite knowledge base.
//
// Non-deterministic and token-spending by nature, so it is gated behind
// HEXCLAW_REAL_LLM_EVAL=1 and reads the local ~/.hexclaw/hexclaw.yaml
// provider config. Optional overrides:
//
//	HEXCLAW_REAL_LLM_PROVIDER  route to a specific configured provider
//	HEXCLAW_REAL_LLM_MODEL     override the model name
//
// Run:
//
//	HEXCLAW_REAL_LLM_EVAL=1 go test ./engine/ -run TestBug20260611_RealLLMDecisionEval -count=1 -v -timeout 300s

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func TestBug20260611_RealLLMDecisionEval(t *testing.T) {
	if os.Getenv("HEXCLAW_REAL_LLM_EVAL") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run the real-LLM decision eval (spends real tokens)")
	}

	cfg, err := config.Load("")
	if err != nil {
		t.Skipf("no local hexclaw config available: %v", err)
	}
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "on"

	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "eval.db"))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("failed to init store: %v", err)
	}

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

	skills := skill.NewRegistry()
	if err := skills.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("failed to register SummarySkill: %v", err)
	}
	if err := skills.Register(builtin.NewKnowledgeIngestSkill(kbMgr)); err != nil {
		t.Fatalf("failed to register KnowledgeIngestSkill: %v", err)
	}

	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Skipf("failed to build real provider router: %v", err)
	}

	eng := NewReActEngine(cfg, router, store, skills)
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("failed to start engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	eng.SetToolCollector(NewToolCollector(skills, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(skills, nil))

	msg := cronAgentMessage("msg-bug-20260611-real-eval")
	// The prompt embeds the material so the eval needs no network beyond the
	// LLM itself; the model only has to decide to call knowledge_ingest.
	msg.Content = "总结以下材料的要点，并把总结结果加入知识库：\n\n" +
		"今日科技动态：1) Nex AGI 发布 nex-n2-pro 模型，上下文窗口提升到 200 万 token；" +
		"2) 国产 3nm 芯片进入风险量产阶段，良率爬升超预期；" +
		"3) 开源社区发布新一代向量数据库，写入吞吐提升 4 倍。"
	if p := os.Getenv("HEXCLAW_REAL_LLM_PROVIDER"); p != "" {
		msg.Metadata["provider"] = p
	}
	if m := os.Getenv("HEXCLAW_REAL_LLM_MODEL"); m != "" {
		msg.Metadata["model"] = m
	}

	runCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
	defer cancel()
	start := time.Now()
	reply, err := eng.Process(runCtx, msg)
	if err != nil {
		t.Skipf("real provider unavailable (network/credit), eval inconclusive: %v", err)
	}
	elapsed := time.Since(start)

	docs, listErr := kbStore.List(ctx)
	if listErr != nil {
		t.Fatalf("failed to list knowledge documents: %v", listErr)
	}

	t.Logf("eval: provider=%s model=%s elapsed=%s tool_calls=%d docs=%d",
		reply.Metadata["provider"], reply.Metadata["model"], elapsed, len(reply.ToolCalls), len(docs))
	for _, tc := range reply.ToolCalls {
		t.Logf("  tool call: %s args=%s", tc.Name, tc.Arguments)
	}

	if len(docs) == 0 {
		t.Errorf("[BUG-20260611][EVAL] the real LLM did not persist anything via knowledge_ingest.\n"+
			"reply: %q\n"+
			"This means the configured model either ignored the tool or hallucinated success — "+
			"check tool support for the model, or strengthen the knowledge_ingest tool description.", reply.Content)
		return
	}
	t.Logf("eval PASS: document %q (source=%s) persisted by the real LLM", docs[0].Title, docs[0].Source)
}
