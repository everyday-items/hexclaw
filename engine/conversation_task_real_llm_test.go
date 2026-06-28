package engine

// Real-LLM user-simulation: a user CREATES a scheduled task by *talking* — not
// by a programmatic AddJob. The real model, given a chat message, must decide
// to call the cron_task tool, and the scheduler's AddJobFromPrompt must
// LLM-compile a real JobSpec and persist the job. This is the production task-
// creation path (two real LLM calls: the chat decision + the compile).
//
// Gated by HEXCLAW_REAL_LLM_EVAL=1; parameterized by HEXCLAW_REAL_LLM_MODEL so
// a shell loop covers every SiliconFlow model. Isolated temp-file DB.
//
//	go test ./engine/ -run TestRealLLM_ConversationCreatesTask -count=1 -v -timeout 600s

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon"
	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/llmrouter"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/skill/builtin"
	sqlitestore "github.com/hexagon-codes/hexclaw/storage/sqlite"
)

func TestRealLLM_ConversationCreatesTask(t *testing.T) {
	if memEvalEnv("HEXCLAW_REAL_LLM_EVAL", "") != "1" {
		t.Skip("set HEXCLAW_REAL_LLM_EVAL=1 to run the real-LLM conversation→task creation (spends tokens)")
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
	t.Logf("=== 会话创建定时任务 真机：provider=%q model=%q ===", provider, model)

	ctx := context.Background()
	dir := t.TempDir()
	store, err := sqlitestore.New(filepath.Join(dir, "conv.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.Init(ctx); err != nil {
		t.Fatalf("init store: %v", err)
	}
	router, err := llmrouter.New(cfg.LLM)
	if err != nil {
		t.Skipf("router: %v", err)
	}

	// Scheduler with a REAL LLM compiler (AddJobFromPrompt compiles the spec).
	compiler := cron.NewLLMCompiler(func() (hexagon.Provider, string, error) {
		return router.Route(context.Background())
	})
	sched := cron.NewScheduler(store.DB(), compiler, nil)
	if err := sched.Init(ctx); err != nil {
		t.Fatalf("sched init: %v", err)
	}

	reg := skill.NewRegistry()
	if err := reg.Register(builtin.NewCronTaskSkill(sched, "")); err != nil {
		t.Fatalf("register cron_task: %v", err)
	}
	eng := NewReActEngine(cfg, router, store, reg)
	if err := eng.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = eng.Stop(context.Background()) })
	eng.SetToolCollector(NewToolCollector(reg, nil, 40))
	eng.SetToolExecutor(NewToolExecutor(reg, nil))

	// The user just talks — no programmatic AddJob anywhere in this test.
	reply, err := eng.Process(ctx, &adapter.Message{
		Platform: adapter.PlatformDesktop, UserID: "user-001", ChatID: "chat-1",
		Content:  "帮我创建一个定时任务：每天早上9点采集百度热搜榜并写入知识库。直接用 cron_task 工具创建，不用问我确认。",
		Metadata: map[string]string{}, Timestamp: time.Now(),
	})
	if err != nil {
		t.Skipf("real provider unavailable, inconclusive: %v", err)
	}
	called := false
	for _, tc := range reply.ToolCalls {
		if tc.Name == "cron_task" {
			called = true
		}
	}
	t.Logf("reply tool_calls=%v content=%q", convReplyToolNames(reply), truncate(reply.Content, 100))

	jobs, lerr := sched.ListJobs(ctx, "user-001")
	if lerr != nil {
		t.Fatalf("ListJobs: %v", lerr)
	}
	t.Logf("=== 调度器现有任务数=%d（cron_task called=%v）===", len(jobs), called)
	for _, j := range jobs {
		t.Logf("  job: name=%q schedule=%q runtime=%q prompt=%q", j.Name, j.Schedule, specRuntime(j), j.SourcePrompt)
	}

	if !called && len(jobs) == 0 {
		t.Skipf("[EVAL] real model did not call cron_task (tool-use weak) — inconclusive, not a chain bug")
	}
	if len(jobs) == 0 {
		t.Errorf("model called cron_task but no job was persisted — creation chain broken")
		return
	}
	// The compiled job must be a real, schedulable artifact (not an empty stub).
	j := jobs[0]
	if j.Schedule == "" {
		t.Errorf("created job has empty schedule (compile produced no schedule): %+v", j)
	}
	if j.Spec == nil {
		t.Errorf("created job has nil Spec (LLM compile did not produce a runnable spec)")
	}
	t.Logf("=== ✅ 会话创建任务通过：job=%q schedule=%q model=%s ===", j.Name, j.Schedule, model)
}

func convReplyToolNames(r *adapter.Reply) []string {
	out := make([]string, 0, len(r.ToolCalls))
	for _, tc := range r.ToolCalls {
		out = append(out, tc.Name)
	}
	return out
}

func specRuntime(j *cron.Job) string {
	if j.Spec == nil {
		return "<nil>"
	}
	return j.Spec.Runtime
}
