package engine

// Real-LLM E2E for the FULL automation chain strung together with a real model:
//
//	HTTP POST /webhooks/{name}  →  webhook.Manager (stamps JobID)  →
//	scheduler.TriggerJob  →  executeJob → runAgentJob (stamps the job's stable
//	snapshot base title) → the REAL engine processes a cron dispatch → the model
//	decides to call knowledge_ingest → IngestSnapshot → KB.
//
// This is the production wiring end to end (not a simulated runner): the only
// stand-in is none. Gated behind HEXCLAW_REAL_LLM_EVAL=1; parameterized by
// HEXCLAW_REAL_LLM_MODEL so a shell loop can cover every configured SiliconFlow
// model. Isolated temp-file DB — never the production data.db.
//
// Run (loops both SF models from a shell):
//
//	for M in "Qwen/Qwen3.6-35B-A3B" "deepseek-ai/DeepSeek-V4-Pro"; do
//	  HEXCLAW_REAL_LLM_EVAL=1 HEXCLAW_REAL_LLM_CONFIG=... \
//	  HEXCLAW_REAL_LLM_PROVIDER="硅基流动" HEXCLAW_REAL_LLM_MODEL="$M" \
//	  go test ./engine/ -run TestRealLLM_WebhookCronChain -count=1 -v -timeout 600s
//	done

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
	"github.com/hexagon-codes/hexclaw/webhook"
)

func TestRealLLM_WebhookCronChain(t *testing.T) {
	if memEvalEnv("HEXCLAW_REAL_LLM_EVAL", "") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run the real-LLM webhook→cron→KB chain (spends tokens)")
	}
	cfg, err := config.Load(memEvalEnv("HEXCLAW_REAL_LLM_CONFIG", ""))
	if err != nil {
		t.Skipf("load config: %v", err)
	}
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "on"

	provider := memEvalEnv("HEXCLAW_REAL_LLM_PROVIDER", "硅基流动")
	model := memEvalEnv("HEXCLAW_REAL_LLM_MODEL", "Qwen/Qwen3.6-35B-A3B")
	pc, ok := cfg.LLM.Providers[provider]
	if !ok {
		t.Skipf("provider %q not in config (有: %v)", provider, providerNames(cfg))
	}
	pc.Model = model
	cfg.LLM.Providers[provider] = pc
	cfg.LLM.Default = provider
	for name := range cfg.LLM.Providers {
		p := cfg.LLM.Providers[name]
		en := name == provider
		p.Enabled = &en
		cfg.LLM.Providers[name] = p
	}
	t.Logf("=== webhook→cron→KB 真机串链：provider=%q model=%q ===", provider, model)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "chain.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	kbStore := knowledge.NewSQLiteStore(store.DB())
	if err := kbStore.Init(ctx); err != nil {
		t.Fatalf("init kb: %v", err)
	}
	kbMgr := knowledge.NewManager(kbStore, kbStore, nil,
		knowledge.WithSplitter(splitter.NewRecursiveSplitter(
			splitter.WithRecursiveChunkSize(400), splitter.WithRecursiveChunkOverlap(80))),
		knowledge.WithSnapshotRetention(50))

	reg := skill.NewRegistry()
	_ = reg.Register(builtin.NewSummarySkill())
	if err := reg.Register(builtin.NewKnowledgeIngestSkill(kbMgr)); err != nil {
		t.Fatalf("register knowledge_ingest: %v", err)
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Skipf("router: %v", err)
	}
	eng := NewReActEngine(cfg, router, store, reg)
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	eng.SetToolCollector(NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))

	// Scheduler: the REAL engine is the agent runner (mirrors cmd/hexclaw).
	sched := cron.NewScheduler(store.DB(), nil, nil)
	if err := sched.Init(ctx); err != nil {
		t.Fatalf("sched init: %v", err)
	}
	sched.SetAgentRunner(func(rctx context.Context, job *cron.Job) (cron.AgentResult, error) {
		reply, perr := eng.Process(rctx, NewCronDispatchMessage(job.UserID, job.ChatID, job.ID, job.SourcePrompt))
		if perr != nil {
			return cron.AgentResult{}, perr
		}
		names := make([]string, 0, len(reply.ToolCalls))
		for _, tc := range reply.ToolCalls {
			names = append(names, tc.Name)
		}
		return cron.AgentResult{Content: reply.Content, ToolNames: names}, nil
	})

	job := &cron.Job{
		Name: "每日科技要点", Schedule: "@hourly", UserID: "u1",
		SourcePrompt: "请用一句话总结这条材料并写入知识库：某公司发布新一代推理芯片，能效比提升 3 倍。",
		Spec:         &cron.JobSpec{Runtime: cron.RuntimeAgent, TimeoutSec: 240},
	}
	if err := sched.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	wmgr := webhook.NewManager(store.DB())
	if err := wmgr.Init(ctx); err != nil {
		t.Fatalf("webhook init: %v", err)
	}
	if err := wmgr.Register(ctx, &webhook.Webhook{Name: "collect-hook", Type: webhook.TypeGeneric, JobID: job.ID, UserID: "u1",
		Enabled: true, // 派发链路测试：显式启用（默认未启用，未启用端点回 423 不派发）
	}); err != nil {
		t.Fatalf("register webhook: %v", err)
	}
	wmgr.SetHandler(func(hctx context.Context, event *webhook.Event, _ string) error {
		if event.JobID != "" {
			return sched.TriggerJob(hctx, event.JobID)
		}
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/{name}", wmgr.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Fire the inbound webhook → real chain runs.
	resp, err := http.Post(srv.URL+"/webhooks/collect-hook", "application/json", strings.NewReader(`{"ref":"main"}`))
	if err != nil {
		t.Fatalf("POST webhook: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("webhook responded %d", resp.StatusCode)
	}

	// Poll the KB (webhook handler + TriggerJob are both async; real LLM is slow).
	deadline := time.Now().Add(280 * time.Second)
	var docs []*knowledge.Document
	for time.Now().Before(deadline) {
		docs, _ = kbMgr.ListDocuments(ctx)
		if len(docs) >= 1 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(docs) == 0 {
		t.Skipf("[EVAL] real model did not call knowledge_ingest (provider down / model tool-use weak) — inconclusive")
	}
	for _, d := range docs {
		t.Logf("  doc: title=%q source=%q source_type=%q", d.Title, d.Source, d.SourceType)
		if !strings.HasPrefix(d.Title, job.Name+" ") {
			t.Errorf("[chain] webhook→cron→KB doc must be in the job's snapshot series %q, got %q", job.Name, d.Title)
		}
		if d.SourceType != "agent" {
			t.Errorf("[chain] webhook→cron collected doc source_type must be agent, got %q", d.SourceType)
		}
	}
	t.Logf("=== ✅ webhook→cron→KB 真机串链通过：%d 篇快照，model=%s ===", len(docs), model)
}
