package cron

// 持续型任务「整个链路」E2E：把单点功能串成端到端链条，证明 PLUMBING 真的通——
//
//	创建(AddJobFromPrompt, Continuous=true → 强制 agent 模式 + meta 持久化)
//	  → 多 tick 真实推进(每 tick agent 据注入的检查点推出"下一章" → 真写知识库一篇)
//	  → 模拟桌面强退重启(同一 DB 起新 Scheduler：Continuous 标志 + 检查点都从 DB 复活，续作不重头)
//	  → 完成信号(agent 报 TASK_COMPLETE: yes) → 任务自动收为 done
//	  → 产物落地(知识库恰好 N 篇、无重复 = 增量推进而非每 tick 重做)
//
// 与既有 webhook→cron→KB E2E 同法：runner 替身"决策后的 agent"（模型决定调 knowledge_ingest
// 由真机门覆盖），这里钉死链路本身真的把检查点喂回 agent、产物真的逐 tick 累积、重启真的续上。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hexagon-codes/hexagon/rag/splitter"
	"github.com/hexagon-codes/hexclaw/knowledge"
)

func TestContinuous_FullChain_E2E(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// 真知识库（同库，如产线）。
	kbStore := knowledge.NewSQLiteStore(db)
	if err := kbStore.Init(ctx); err != nil {
		t.Fatalf("kb init: %v", err)
	}
	kbMgr := knowledge.NewManager(kbStore, kbStore, nil,
		knowledge.WithSplitter(splitter.NewRecursiveSplitter(
			splitter.WithRecursiveChunkSize(400), splitter.WithRecursiveChunkOverlap(80))))

	const goalChapters = 3
	var lastPrompt string
	// 忠实"决策后的 agent"：据**注入的检查点**数出已完成章数 → 推进下一章 → 真写 KB 一篇 →
	// 回报 PROGRESS / TASK_COMPLETE。下一章号完全由检查点决定（非全局计数），故能验证链路真的把
	// 进度档案喂回 agent；重启后新 Scheduler 读回同一档案 → agent 续作而非重头。
	makeRunner := func() AgentRunner {
		return func(rctx context.Context, job *Job) (AgentResult, error) {
			lastPrompt = job.SourcePrompt
			done := strings.Count(job.SourcePrompt, "完成第") // 进度档案里 "完成第N章" 的条数
			next := done + 1
			title := fmt.Sprintf("《长报告》第%d章摘要", next)
			body := fmt.Sprintf("这是第 %d 章的整理摘要，内容足够长以便切块入库，覆盖该章要点若干。", next)
			if _, err := kbMgr.AddDocument(rctx, title, body, "continuous"); err != nil {
				return AgentResult{}, err
			}
			cv := "no"
			if next >= goalChapters {
				cv = "yes"
			}
			return AgentResult{
				Content:   fmt.Sprintf("已整理第%d章并入库。\nPROGRESS: 完成第%d章\nTASK_COMPLETE: %s", next, next, cv),
				ToolNames: []string{knowledgeIngestTool}, // 满足 C1：声称入库就必须真调 ingest
			}, nil
		}
	}

	// ── 1. 创建：持续型任务（prompt 含"知识库"→ 也顺带验 C1 ingest 校验在 continuous 路径生效）──
	newSched := func() *Scheduler {
		return NewScheduler(db, &stubCompiler{}, NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	}
	s1 := newSched()
	if err := s1.Init(ctx); err != nil {
		t.Fatalf("s1 init: %v", err)
	}
	s1.SetAgentRunner(makeRunner())
	job, err := s1.AddJobFromPrompt(ctx, AddJobRequest{
		Name: "逐章整理长报告", Schedule: "@daily", UserID: "u1",
		Prompt:     "持续把《长报告》逐章整理进知识库，每次只整理下一章",
		Continuous: true,
	})
	if err != nil {
		t.Fatalf("AddJobFromPrompt: %v", err)
	}
	if job.Spec == nil || job.Spec.Runtime != RuntimeAgent || !job.Continuous {
		t.Fatalf("创建应得持续型 agent 任务，得 Continuous=%v Spec=%+v", job.Continuous, job.Spec)
	}

	// ── 2. 推进两个 tick（KB 应累积 ch1、ch2）──
	if r := s1.runContinuousAgentJob(ctx, job); r.Status != "success" {
		t.Fatalf("tick1 应成功，得 %s: %s", r.Status, r.Error)
	}
	if r := s1.runContinuousAgentJob(ctx, job); r.Status != "success" {
		t.Fatalf("tick2 应成功，得 %s: %s", r.Status, r.Error)
	}
	if n := countDocs(t, ctx, kbMgr); n != 2 {
		t.Fatalf("两 tick 后知识库应有 2 篇，实际 %d", n)
	}
	// 第二 tick 的 prompt 应注入第一 tick 的进度（链路把检查点喂回 agent）。
	if !strings.Contains(lastPrompt, "完成第1章") {
		t.Errorf("链路断裂：第2 tick prompt 未注入上次进度档案，得：%q", lastPrompt)
	}
	before := s1.loadContinuousCheckpoint(job.ID)
	if before.Tick != 2 || before.Completed {
		t.Fatalf("两 tick 后检查点应 tick=2 未完成，得 %+v", before)
	}

	// ── 3. 模拟桌面强退重启：同一 DB 起新 Scheduler，Continuous 标志 + 检查点都该从 DB 复活 ──
	s2 := newSched()
	if err := s2.Init(ctx); err != nil {
		t.Fatalf("s2 init: %v", err)
	}
	s2.SetAgentRunner(makeRunner())
	reloaded, ok := s2.GetJob(ctx, job.ID)
	if !ok || !reloaded.Continuous {
		t.Fatalf("重启后任务应仍是持续型（meta 往返），得 %+v", reloaded)
	}
	if cp := s2.loadContinuousCheckpoint(job.ID); cp.Tick != 2 {
		t.Fatalf("重启后应读回检查点 tick=2，得 %d", cp.Tick)
	}

	// ── 4. 在重启后的调度器上再推进一 tick → agent 据档案续到第 3 章 → 完成 → 收为 done ──
	if r := s2.runContinuousAgentJob(ctx, reloaded); r.Status != "success" {
		t.Fatalf("tick3 应成功，得 %s: %s", r.Status, r.Error)
	}

	// ── 5. 产物落地 + 收工核验 ──
	cp := s2.loadContinuousCheckpoint(job.ID)
	if !cp.Completed || cp.Tick != 3 {
		t.Errorf("第3 tick 后应完成、tick=3，得 %+v", cp)
	}
	got, _ := s2.GetJob(ctx, job.ID)
	if statusOf(got) != StatusDone {
		t.Errorf("完成后任务应收为 done（停止调度），实际 %v", statusOf(got))
	}
	docs := listDocs(t, ctx, kbMgr)
	if len(docs) != goalChapters {
		t.Fatalf("产物落地：知识库应恰好 %d 篇（每 tick 一篇、无重复=增量推进），实际 %d", goalChapters, len(docs))
	}
	// 每章各一篇、互不重复（证明"续作不重头"：重启没让它重新整理第 1 章）。
	for ch := 1; ch <= goalChapters; ch++ {
		want := fmt.Sprintf("第%d章", ch)
		hit := 0
		for _, d := range docs {
			if strings.Contains(d.Title, want) {
				hit++
			}
		}
		if hit != 1 {
			t.Errorf("第%d章应恰好一篇（无重复/无遗漏），实际 %d 篇", ch, hit)
		}
	}
}

func countDocs(t *testing.T, ctx context.Context, m *knowledge.Manager) int {
	t.Helper()
	return len(listDocs(t, ctx, m))
}

func listDocs(t *testing.T, ctx context.Context, m *knowledge.Manager) []*knowledge.Document {
	t.Helper()
	docs, err := m.ListDocuments(ctx)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	return docs
}
