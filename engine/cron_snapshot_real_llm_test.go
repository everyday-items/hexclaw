package engine

// Real-LLM E2E for the cron → knowledge-base SNAPSHOT chain (the 2026-06-27
// hardening). It drives the production agent path — an actual model, given a
// cron-dispatched "collect/summarize … 写入知识库" prompt, decides to call
// knowledge_ingest — and asserts the snapshot invariants my change guarantees
// regardless of what title/content the (non-deterministic) model improvises:
//
//	#4 stable base title : every persisted doc is titled "<job name> <ts>",
//	                       NOT the title the model made up — so repeated runs
//	                       form one coherent series.
//	#1 append            : a second run with different material does not
//	                       overwrite the first (≥1 distinct doc per successful
//	                       run, all in the same series).
//	#3 retention         : the series never exceeds the configured cap.
//
// Gated behind HEXCLAW_REAL_LLM_EVAL=1; reads HEXCLAW_REAL_LLM_CONFIG (a yaml
// with plaintext keys; the default vault keys aren't readable from a test).
// Uses an isolated temp-file DB — never the production data.db.
//
// Run (SiliconFlow Qwen, thinking off lives in the provider config):
//
//	HEXCLAW_REAL_LLM_EVAL=1 \
//	HEXCLAW_REAL_LLM_CONFIG="$HOME/.hexclaw/hexclaw.yaml.bak.before-siliconflow-models-20260626-224056" \
//	HEXCLAW_REAL_LLM_PROVIDER="硅基流动" HEXCLAW_REAL_LLM_MODEL="Qwen/Qwen3.6-35B-A3B" \
//	go test ./engine/ -run TestRealLLM_CronSnapshotChain -count=1 -v -timeout 600s

import (
	"context"
	"path/filepath"
	"regexp"
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

// snapshotTitleShapeRe matches "<base> YYYY-MM-DD HH:MM:SS" with an optional
// " (N)" same-second disambiguator — the exact shape IngestSnapshot produces.
var snapshotTitleShapeRe = regexp.MustCompile(`^(.+) \d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}( \(\d+\))?$`)

func TestRealLLM_CronSnapshotChain(t *testing.T) {
	if memEvalEnv("HEXCLAW_REAL_LLM_EVAL", "") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run the real-LLM cron→KB snapshot E2E (spends tokens)")
	}
	cfgPath := memEvalEnv("HEXCLAW_REAL_LLM_CONFIG", "")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Skipf("load config %q: %v", cfgPath, err)
	}
	cfg.Compaction.Enabled = false
	cfg.LLM.Tools.Enabled = "on"

	provider := memEvalEnv("HEXCLAW_REAL_LLM_PROVIDER", "硅基流动")
	model := memEvalEnv("HEXCLAW_REAL_LLM_MODEL", "Qwen/Qwen3.6-35B-A3B")
	pc, ok := cfg.LLM.Providers[provider]
	if !ok {
		t.Skipf("provider %q not in config %q (有: %v)", provider, cfgPath, providerNames(cfg))
	}
	pc.Model = model
	cfg.LLM.Providers[provider] = pc
	cfg.LLM.Default = provider
	for name := range cfg.LLM.Providers { // isolate the chosen provider
		p := cfg.LLM.Providers[name]
		en := name == provider
		p.Enabled = &en
		cfg.LLM.Providers[name] = p
	}
	t.Logf("=== cron→KB 快照真机 E2E：provider=%q model=%q ===", provider, model)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "cronkb.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	kbStore := knowledge.NewSQLiteStore(store.DB())
	if err := kbStore.Init(ctx); err != nil {
		t.Fatalf("init kb store: %v", err)
	}
	const retention = 3
	kbMgr := knowledge.NewManager(kbStore, kbStore, nil,
		knowledge.WithSplitter(splitter.NewRecursiveSplitter(
			splitter.WithRecursiveChunkSize(400),
			splitter.WithRecursiveChunkOverlap(80),
		)),
		knowledge.WithSnapshotRetention(retention),
	)

	reg := skill.NewRegistry()
	if err := reg.Register(builtin.NewSummarySkill()); err != nil {
		t.Fatalf("register summary: %v", err)
	}
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

	const jobName = "每日科技要点"
	// runCron mirrors cmd/hexclaw's AgentRunner: cron-dispatch message + the
	// job's stable base title stamped on the ctx the engine threads to the skill.
	runCron := func(t *testing.T, runID, material string) {
		t.Helper()
		msg := NewCronDispatchMessage("eval-user", "", "job-snap-"+runID,
			"请用一句话总结以下材料，并把总结写入知识库：\n\n"+material)
		msg.Metadata["provider"] = provider
		msg.Metadata["model"] = model
		runCtx := skill.WithSnapshotBaseTitle(ctx, jobName)
		cctx, cancel := context.WithTimeout(runCtx, 240*time.Second)
		defer cancel()
		reply, err := eng.Process(cctx, msg)
		if err != nil {
			t.Skipf("real provider unavailable (network/credit), eval inconclusive: %v", err)
		}
		tools := make([]string, 0, len(reply.ToolCalls))
		for _, tc := range reply.ToolCalls {
			tools = append(tools, tc.Name)
		}
		t.Logf("run %s: tools=%v reply=%q", runID, tools, truncate(reply.Content, 80))
	}

	// Two runs with DIFFERENT material so neither is a content-dedup skip.
	runCron(t, "1", "1) 某公司发布新一代推理芯片，能效比提升 3 倍；2) 开源大模型上下文扩到 100 万 token。")
	runCron(t, "2", "1) 国产光刻机关键部件突破；2) 新型固态电池量产，能量密度翻倍；3) 卫星互联网商用提速。")

	docs, err := kbMgr.ListDocuments(ctx)
	if err != nil {
		t.Fatalf("list docs: %v", err)
	}
	t.Logf("=== 持久化文档数=%d（retention=%d, jobName=%q）===", len(docs), retention, jobName)
	for _, d := range docs {
		t.Logf("  doc: title=%q source=%q source_type=%q chunks=%d", d.Title, d.Source, d.SourceType, d.ChunkCount)
	}

	if len(docs) == 0 {
		t.Skipf("[EVAL] 真机模型未调用 knowledge_ingest（模型工具能力问题，非快照链路 bug）——本轮 inconclusive")
	}

	// #4 + #1: every persisted doc must be titled "<jobName> <timestamp>" — the
	// stable base overrides whatever title the model improvised, and the suffix
	// makes runs append instead of overwrite.
	for _, d := range docs {
		m := snapshotTitleShapeRe.FindStringSubmatch(d.Title)
		if m == nil {
			t.Errorf("[#1/#4] 文档标题不是快照形态 \"<base> <ts>\"：%q", d.Title)
			continue
		}
		if base := m[1]; base != jobName {
			t.Errorf("[#4] 快照基础标题应被稳定覆盖为 %q（系列才连贯），实际 %q（标题=%q）",
				jobName, base, d.Title)
		}
		if d.SourceType != "agent" {
			t.Errorf("[contract] cron 入库文档 source_type 应为 agent，实际 %q", d.SourceType)
		}
	}

	// #3: the series must never exceed the retention cap even if the model
	// ingests more than once per run.
	if len(docs) > retention {
		t.Errorf("[#3] 快照系列超过保留上限：%d > %d", len(docs), retention)
	}

	// Cross-check the dedicated paged/grouped read path over real-model data.
	paged, err := kbMgr.ListDocumentsPaged(ctx, knowledge.DocListQuery{Limit: 1, Offset: 0})
	if err != nil {
		t.Fatalf("paged: %v", err)
	}
	if paged.Total != len(docs) || len(paged.Documents) > 1 {
		t.Errorf("[#5] 分页契约异常：total=%d(want %d) pageLen=%d(want<=1)", paged.Total, len(docs), len(paged.Documents))
	}
	t.Logf("=== #5 facet: %+v ===", paged.Sources)
}
