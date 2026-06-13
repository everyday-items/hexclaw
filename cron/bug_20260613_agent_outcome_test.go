package cron

// BUG-20260613: agent-mode runs were recorded "success" whenever the engine
// loop completed, even when the agent's reply said it could NOT accomplish
// the task ("无法使用浏览器工具访问…"). The dispatch prompt now carries an
// outcome contract (engine.NewCronDispatchMessage) and the scheduler parses
// the trailing TASK_STATUS line to map the run to success/failed and strips
// the marker from the stored stdout.

import (
	"context"
	"strings"
	"testing"
)

func newAgentOutcomeScheduler(t *testing.T, reply string) *Scheduler {
	t.Helper()
	s := newTestScheduler(t, setupTestDB(t))
	s.SetAgentRunner(func(ctx context.Context, job *Job) (AgentResult, error) {
		return AgentResult{Content: reply}, nil
	})
	return s
}

func agentJobFixture() *Job {
	return &Job{
		ID: "job-outcome", Name: "outcome probe", Schedule: "@daily", UserID: "u-1",
		Spec: &JobSpec{Runtime: RuntimeAgent, TimeoutSec: 60},
	}
}

func TestBug20260613_AgentFailedMarkerMapsToFailedRun(t *testing.T) {
	s := newAgentOutcomeScheduler(t,
		"摘要已生成，但无法访问目标页面。\nTASK_STATUS: failed - browser fetch EOF")
	res := s.runAgentJob(context.Background(), agentJobFixture())

	if res.Status != "failed" {
		t.Errorf("agent reply with failed marker must record status=failed, got %q", res.Status)
	}
	if !strings.Contains(res.Error, "browser fetch EOF") {
		t.Errorf("failure reason must surface in Error, got %q", res.Error)
	}
	if strings.Contains(res.Stdout, "TASK_STATUS") {
		t.Errorf("marker line must be stripped from stdout, got %q", res.Stdout)
	}
}

func TestBug20260613_AgentDoneMarkerStrippedAndSuccess(t *testing.T) {
	s := newAgentOutcomeScheduler(t, "已完成采集并入库。\nTASK_STATUS: done")
	res := s.runAgentJob(context.Background(), agentJobFixture())

	if res.Status != "success" {
		t.Errorf("done marker must record success, got %q", res.Status)
	}
	if strings.Contains(res.Stdout, "TASK_STATUS") {
		t.Errorf("marker line must be stripped, got %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "已完成采集并入库") {
		t.Errorf("original content must be preserved, got %q", res.Stdout)
	}
}

func TestBug20260613_AgentMissingMarkerStaysSuccess(t *testing.T) {
	// Backward compatibility: a model that ignores the contract must not
	// flip historical behavior — no marker means success, stdout untouched.
	s := newAgentOutcomeScheduler(t, "普通回复，没有结果标记。")
	res := s.runAgentJob(context.Background(), agentJobFixture())

	if res.Status != "success" || res.Stdout != "普通回复，没有结果标记。" {
		t.Errorf("missing marker must keep success + raw stdout, got %q / %q", res.Status, res.Stdout)
	}
}

func TestBug20260613_AgentMarkerWithBackticksParsed(t *testing.T) {
	// Models often wrap the literal in backticks; the parser must tolerate it.
	s := newAgentOutcomeScheduler(t, "无法完成。\n`TASK_STATUS: failed - blocked`")
	res := s.runAgentJob(context.Background(), agentJobFixture())

	if res.Status != "failed" {
		t.Errorf("backtick-wrapped failed marker must parse, got %q", res.Status)
	}
}

// The live model (glm-4-flash) replies in Chinese and localizes the marker to
// "任务状态：失败 - <reason>". The parser must catch the localized form, or the
// English-only contract is inert against the production model (audit C1).
func TestBug20260613_AgentChineseLocalizedFailedMarker(t *testing.T) {
	s := newAgentOutcomeScheduler(t,
		"采集任务失败，浏览器工具调用被阻止。\n任务状态：失败 - 工具调用被阻止")
	res := s.runAgentJob(context.Background(), agentJobFixture())

	if res.Status != "failed" {
		t.Errorf("Chinese localized failed marker must record failed, got %q", res.Status)
	}
	if !strings.Contains(res.Error, "工具调用被阻止") {
		t.Errorf("reason must surface, got %q", res.Error)
	}
	if strings.Contains(res.Stdout, "任务状态") {
		t.Errorf("localized marker line must be stripped, got %q", res.Stdout)
	}
}

func TestBug20260613_AgentChineseDoneMarker(t *testing.T) {
	s := newAgentOutcomeScheduler(t, "已完成采集并入库。\n任务状态：完成")
	res := s.runAgentJob(context.Background(), agentJobFixture())
	if res.Status != "success" {
		t.Errorf("Chinese done marker must record success, got %q", res.Status)
	}
	if strings.Contains(res.Stdout, "任务状态") {
		t.Errorf("marker stripped, got %q", res.Stdout)
	}
}

// Marker followed by a postscript line must still be detected (L1).
func TestBug20260613_AgentMarkerWithTrailingPostscript(t *testing.T) {
	s := newAgentOutcomeScheduler(t,
		"无法访问页面。\nTASK_STATUS: failed - page unreachable\n（如需帮助请重试）")
	res := s.runAgentJob(context.Background(), agentJobFixture())
	if res.Status != "failed" {
		t.Errorf("marker before a postscript must still parse, got %q", res.Status)
	}
}

// Review M1: a success verdict that merely mentions a failure-ish word
// ("failover") must NOT be misclassified as failed.
func TestBug20260613_AgentDoneWithFailoverWordIsSuccess(t *testing.T) {
	s := newAgentOutcomeScheduler(t, "采集完成，期间用了 failover 链路。\nTASK_STATUS: done")
	res := s.runAgentJob(context.Background(), agentJobFixture())
	if res.Status != "success" {
		t.Errorf("done verdict mentioning failover must be success, got %q (err=%q)", res.Status, res.Error)
	}
}

// BUG-20260613: agent timeout was hardcoded 300s; it's now configurable with a
// raised default and a sane ceiling.
func TestBug20260613_AgentTimeoutConfigurable(t *testing.T) {
	if agentTimeoutSec(0) != defaultAgentTimeoutSec {
		t.Errorf("0 must map to default %d, got %d", defaultAgentTimeoutSec, agentTimeoutSec(0))
	}
	if agentTimeoutSec(120) != 120 {
		t.Errorf("explicit override must be honored, got %d", agentTimeoutSec(120))
	}
	if agentTimeoutSec(9999) != 1800 {
		t.Errorf("override must be clamped to 1800, got %d", agentTimeoutSec(9999))
	}
}
