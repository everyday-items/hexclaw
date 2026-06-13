package cron

// BUG-20260613 (audit C1): agent-mode success was decided purely from the
// model's self-reported TASK_STATUS line. A job whose purpose is to store
// knowledge could print "已入库 / TASK_STATUS: done" without ever calling
// knowledge_ingest — the only channel that persists a document — and the run
// was recorded as success. The dispatch closure now passes the invoked tool
// names through, and runAgentJob downgrades an ingest-intent job to failed
// when knowledge_ingest was never called, so a hallucinated ingest can no
// longer masquerade as a result.

import (
	"context"
	"strings"
	"testing"
)

// newIngestProofScheduler injects a runner returning a fixed reply and the set
// of tool names the agent "invoked" this round.
func newIngestProofScheduler(t *testing.T, reply string, toolNames []string) *Scheduler {
	t.Helper()
	s := newTestScheduler(t, setupTestDB(t))
	s.SetAgentRunner(func(ctx context.Context, job *Job) (AgentResult, error) {
		return AgentResult{Content: reply, ToolNames: toolNames}, nil
	})
	return s
}

func ingestJobFixture(prompt string) *Job {
	return &Job{
		ID: "job-ingest", Name: "ingest probe", Schedule: "@daily", UserID: "u-1",
		SourcePrompt: prompt,
		Spec:         &JobSpec{Runtime: RuntimeAgent, TimeoutSec: 60},
	}
}

// The core C1 invariant: an ingest-intent job that self-reports success but
// never called knowledge_ingest stored nothing, so the run must be failed.
func TestBug20260613_IngestJobWithoutToolCallDowngradedToFailed(t *testing.T) {
	s := newIngestProofScheduler(t,
		"已采集百度热搜并入库 20 条。\nTASK_STATUS: done", nil)
	res := s.runAgentJob(context.Background(), ingestJobFixture("采集百度热搜 TOP10 并入库知识库"))

	if res.Status != "failed" {
		t.Errorf("ingest job that never called knowledge_ingest must be failed, got %q", res.Status)
	}
	if !strings.Contains(res.Error, knowledgeIngestTool) {
		t.Errorf("failure reason must name the missing tool, got %q", res.Error)
	}
	// The cleaned stdout is still preserved for the history row.
	if !strings.Contains(res.Stdout, "已采集百度热搜并入库") {
		t.Errorf("stdout must be preserved, got %q", res.Stdout)
	}
}

// The same job with a real knowledge_ingest call is a genuine success.
func TestBug20260613_IngestJobWithToolCallStaysSuccess(t *testing.T) {
	s := newIngestProofScheduler(t,
		"已采集百度热搜并入库 20 条。\nTASK_STATUS: done", []string{"browser", "knowledge_ingest"})
	res := s.runAgentJob(context.Background(), ingestJobFixture("采集百度热搜 TOP10 并入库知识库"))

	if res.Status != "success" {
		t.Errorf("ingest job that called knowledge_ingest must be success, got %q (err=%q)", res.Status, res.Error)
	}
}

// Tool-name matching is case-insensitive and whitespace-tolerant — providers
// vary the casing of skill names.
func TestBug20260613_IngestToolMatchIsCaseInsensitive(t *testing.T) {
	s := newIngestProofScheduler(t,
		"入库完成。\nTASK_STATUS: done", []string{" Knowledge_Ingest "})
	res := s.runAgentJob(context.Background(), ingestJobFixture("把今天的要闻收录到知识库"))
	if res.Status != "success" {
		t.Errorf("case/space-variant tool name must count as a real ingest, got %q", res.Status)
	}
}

// A non-ingest cognitive job (pure summarization) has no ingest requirement —
// it stays success with no tool calls, so the guard must not over-reach.
func TestBug20260613_NonIngestJobNoToolCallStaysSuccess(t *testing.T) {
	s := newIngestProofScheduler(t,
		"今日要点：A、B、C。\nTASK_STATUS: done", nil)
	res := s.runAgentJob(context.Background(), ingestJobFixture("每天总结今天的新闻要点"))
	if res.Status != "success" {
		t.Errorf("non-ingest job must stay success without any tool call, got %q (err=%q)", res.Status, res.Error)
	}
}

// An explicit failure verdict on an ingest job keeps the model's own reason —
// the failure path runs first and must not be overwritten by the ingest guard.
func TestBug20260613_IngestJobExplicitFailedKeepsModelReason(t *testing.T) {
	s := newIngestProofScheduler(t,
		"无法访问目标页面。\nTASK_STATUS: failed - browser fetch EOF", nil)
	res := s.runAgentJob(context.Background(), ingestJobFixture("采集页面并入库知识库"))
	if res.Status != "failed" {
		t.Errorf("explicit failed marker must record failed, got %q", res.Status)
	}
	if !strings.Contains(res.Error, "browser fetch EOF") {
		t.Errorf("model's own failure reason must be preserved, got %q", res.Error)
	}
}

// jobRequiresIngest is the intent detector; pin its decisions directly so the
// keyword set's behavior is documented and regressions are caught.
func TestBug20260613_JobRequiresIngestDetection(t *testing.T) {
	ingest := []string{
		"采集百度热搜并入库", "整理要闻收录到知识库", "store the summary in the knowledge base",
		"call knowledge_ingest with the digest",
	}
	for _, p := range ingest {
		if !jobRequiresIngest(&Job{SourcePrompt: p}) {
			t.Errorf("prompt should be detected as ingest intent: %q", p)
		}
	}
	notIngest := []string{
		"每天总结今天的新闻要点", "分析本周销售数据并给出建议", "draft a morning report",
	}
	for _, p := range notIngest {
		if jobRequiresIngest(&Job{SourcePrompt: p}) {
			t.Errorf("prompt should NOT be detected as ingest intent: %q", p)
		}
	}
}
