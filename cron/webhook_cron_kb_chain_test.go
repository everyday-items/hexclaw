package cron

// End-to-end of the automation chain that strings webhook + cron + the KB
// snapshot work together — the gap both subsystem audits flagged (no test
// exercised an inbound webhook actually running its bound cron job through to a
// knowledge-base write).
//
// Flow under test (mirrors cmd/hexclaw wiring):
//
//	HTTP POST /webhooks/{name}  →  webhook.Manager.Handler (stamps event.JobID
//	from the bound webhook)  →  EventHandler: JobID set → scheduler.TriggerJob
//	→  executeJob → AgentRunner (the agent ingests a snapshot)  →  KB.
//
// Both hops are fire-and-forget goroutines, so the test polls. The runner
// stands in for the post-decision agent (the model's decision to call
// knowledge_ingest is covered by the real-model gate); here we prove the
// PLUMBING: an event really runs the bound job and a snapshot really lands,
// and a second event APPENDS (the webhook path inherits #1 append, not
// overwrite).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/knowledge"
	"github.com/hexagon-codes/hexclaw/skill"
	"github.com/hexagon-codes/hexclaw/webhook"
)

func TestWebhookCronKBSnapshotChain_E2E(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Knowledge base on the same DB (as in production).
	kbStore := knowledge.NewSQLiteStore(db)
	if err := kbStore.Init(ctx); err != nil {
		t.Fatalf("kb init: %v", err)
	}
	kbMgr := knowledge.NewManager(kbStore, kbStore, nil,
		knowledge.WithSplitter(splitter.NewRecursiveSplitter(
			splitter.WithRecursiveChunkSize(400), splitter.WithRecursiveChunkOverlap(80))),
		knowledge.WithSnapshotRetention(0))

	// Scheduler with an agent runner that ingests a snapshot, faithfully to the
	// cmd/hexclaw AgentRunner: base title = job.Name, content varies per run.
	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("sched init: %v", err)
	}
	var runN int32
	s.SetAgentRunner(func(rctx context.Context, _ *Job) (AgentResult, error) {
		n := atomic.AddInt32(&runN, 1)
		// Read the base title the SCHEDULER stamped (relocated from cmd wiring),
		// not job.Name directly — so this e2e also proves the cron→ctx→runner
		// base-title contract flows through the webhook TriggerJob path.
		base := skill.SnapshotBaseTitle(rctx)
		if base == "" {
			return AgentResult{}, fmt.Errorf("runAgentJob did not stamp the snapshot base title")
		}
		if _, _, err := kbMgr.IngestSnapshot(rctx, base,
			fmt.Sprintf("webhook 触发采集，第 %d 次，内容足够长以便切块。", n), "webhook-collected"); err != nil {
			return AgentResult{}, err
		}
		return AgentResult{Content: "已采集入库\nTASK_STATUS: done", ToolNames: []string{"knowledge_ingest"}}, nil
	})

	job := &Job{
		Name: "百度热搜采集", Schedule: "@hourly", UserID: "u1",
		SourcePrompt: "采集百度热搜并写入知识库",
		Spec:         &JobSpec{Runtime: RuntimeAgent, TimeoutSec: 60},
	}
	if err := s.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// Webhook bound to the job (JobID set → trigger job, prompt ignored).
	wmgr := webhook.NewManager(db)
	if err := wmgr.Init(ctx); err != nil {
		t.Fatalf("webhook init: %v", err)
	}
	if err := wmgr.Register(ctx, &webhook.Webhook{
		Name: "deploy-hook", Type: webhook.TypeGeneric, JobID: job.ID, UserID: "u1",
		Enabled: true, // 派发链路测试：显式启用（默认未启用）
	}); err != nil {
		t.Fatalf("register webhook: %v", err)
	}
	// Exact cmd/hexclaw EventHandler contract: JobID non-empty → TriggerJob.
	wmgr.SetHandler(func(hctx context.Context, event *webhook.Event, _ string) error {
		if event.JobID != "" {
			return s.TriggerJob(hctx, event.JobID)
		}
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/{name}", wmgr.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	fireEvent := func() {
		resp, err := http.Post(srv.URL+"/webhooks/deploy-hook", "application/json", strings.NewReader(`{"ref":"main"}`))
		if err != nil {
			t.Fatalf("POST webhook: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("webhook responded %d, want 200", resp.StatusCode)
		}
	}
	waitDocs := func(want int) []*knowledge.Document {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			docs, _ := kbMgr.ListDocuments(ctx)
			if len(docs) >= want {
				return docs
			}
			time.Sleep(40 * time.Millisecond)
		}
		docs, _ := kbMgr.ListDocuments(ctx)
		return docs
	}
	waitIdle := func() { // ensure run N fully finished before firing N+1 (overlap guard)
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if _, busy := s.running.Load(job.ID); !busy {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// Event #1: webhook → cron job → snapshot.
	fireEvent()
	if docs := waitDocs(1); len(docs) != 1 {
		t.Fatalf("inbound webhook must run the bound cron job and write 1 snapshot, got %d docs", len(docs))
	}
	waitIdle()

	// Event #2: must APPEND (the webhook path inherits the cron snapshot
	// semantics — never overwrite the previous run).
	fireEvent()
	docs := waitDocs(2)
	if len(docs) != 2 {
		t.Fatalf("a second webhook event must append a second snapshot (not overwrite), got %d docs", len(docs))
	}

	// Both docs are snapshots of the job's stable series, typed agent.
	for _, d := range docs {
		if !strings.HasPrefix(d.Title, job.Name+" ") {
			t.Errorf("doc title must be in the job's snapshot series %q, got %q", job.Name, d.Title)
		}
		if d.SourceType != "agent" {
			t.Errorf("webhook→cron collected doc source_type must be agent, got %q", d.SourceType)
		}
	}
}

// A webhook with NO bound job must take the prompt→agent path, not TriggerJob —
// the EventHandler's two branches must be mutually exclusive (a regression here
// would either drop event data or run the wrong action).
func TestWebhookNoJob_TakesPromptPath(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	s := newTestScheduler(t, db)
	if err := s.Init(ctx); err != nil {
		t.Fatalf("sched init: %v", err)
	}
	var triggered int32
	s.SetAgentRunner(func(context.Context, *Job) (AgentResult, error) {
		atomic.AddInt32(&triggered, 1)
		return AgentResult{Content: "x"}, nil
	})

	wmgr := webhook.NewManager(db)
	if err := wmgr.Init(ctx); err != nil {
		t.Fatalf("webhook init: %v", err)
	}
	if err := wmgr.Register(ctx, &webhook.Webhook{
		Name: "notify-hook", Type: webhook.TypeGeneric, Prompt: "处理事件", UserID: "u1", // no JobID
		Enabled: true, // 派发链路测试：显式启用（默认未启用）
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	var promptRuns int32
	var gotJobID string
	wmgr.SetHandler(func(hctx context.Context, event *webhook.Event, prompt string) error {
		if event.JobID != "" {
			gotJobID = event.JobID
			return s.TriggerJob(hctx, event.JobID)
		}
		atomic.AddInt32(&promptRuns, 1)
		return nil
	})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/{name}", wmgr.Handler())
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/webhooks/notify-hook", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&promptRuns) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt32(&promptRuns) != 1 {
		t.Errorf("unbound webhook must take the prompt path once, got %d", promptRuns)
	}
	if atomic.LoadInt32(&triggered) != 0 {
		t.Errorf("unbound webhook must NOT trigger any cron job, got %d", triggered)
	}
	if gotJobID != "" {
		t.Errorf("unbound webhook event must carry empty JobID, got %q", gotJobID)
	}
}
