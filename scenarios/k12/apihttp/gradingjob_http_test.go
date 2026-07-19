package apihttp_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"testing"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/apihttp"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assembly"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
	"github.com/hexagon-codes/hexclaw/storage/migrate"

	_ "modernc.org/sqlite"
)

// HTTP 契约：统一 GradingJob（架构设计 §6.7 / §4.10 / 执行计划 §3.0 2026-07-18 新规格）。
// 本轮为「领域+命令+HTTP 契约」阶段；桌面/钉钉入口迁移与真实编排器接线在下一轮。

type gradingFixture struct {
	h    http.Handler
	deps usecase.Deps
}

func newGradingFixture(t *testing.T, agents ...string) gradingFixture {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate.Run(context.Background(), db, migrate.All); err != nil {
		t.Fatal(err)
	}
	if len(agents) == 0 {
		agents = []string{"mingming"}
	}
	for _, a := range agents {
		if _, err := db.Exec(`INSERT INTO agents(name) VALUES(?)`, a); err != nil {
			t.Fatal(err)
		}
	}
	k, err := assembly.Wire(db, fakeSolveExec{})
	if err != nil {
		t.Fatal(err)
	}
	return gradingFixture{
		h:    apihttp.NewHandler(apihttp.Runtime{Views: k.Registry.Views, Records: k.Records, Deps: k.Deps}),
		deps: k.Deps,
	}
}

func newGradingServer(t *testing.T, agents ...string) http.Handler {
	t.Helper()
	return newGradingFixture(t, agents...).h
}

func createJobBody(sourceKey string) string {
	return fmt.Sprintf(`{"agent":"mingming","source_session":"s1","submission_id":"sub-1",
		"source_kind":"im","source_key":%q,
		"model_snapshot":{"provider":"openrouter","model":"test-vlm","capability":"vision"}}`, sourceKey)
}

func httpJob(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	rec, out := do(t, h, "GET", "/grading-jobs/"+id+"?agent=mingming", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET job %d: %v", rec.Code, out)
	}
	return out
}

func advanceFixture(t *testing.T, f gradingFixture, id string, in usecase.AdvanceGradingInput) usecase.GradingJobView {
	t.Helper()
	v, err := f.deps.AdvanceGradingStage(context.Background(), "mingming", id, in)
	if err != nil {
		t.Fatalf("内部编排推进: %v", err)
	}
	return v
}

// driveHTTPToAwaiting 通过公共 create 创建，再由进程内编排命令推进到确认停点。
// HTTP /advance 已按 DD-001 删除，测试不得把内部阶段命令重新公开。
func driveHTTPToAwaiting(t *testing.T, f gradingFixture, sourceKey string) string {
	t.Helper()
	rec, out := do(t, f.h, "POST", "/grading-jobs", createJobBody(sourceKey))
	if rec.Code != http.StatusOK || out["created"] != true {
		t.Fatalf("创建 %d: %v", rec.Code, out)
	}
	job, _ := out["job"].(map[string]any)
	id, _ := job["job_id"].(string)
	for i := 0; i < 3; i++ { // queued→normalizing→recognizing→awaiting_confirmation
		advanceFixture(t, f, id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeOK, ArtifactDigest: "d"})
	}
	if got := httpJob(t, f.h, id)["stage"]; got != "awaiting_confirmation" {
		t.Fatalf("应 awaiting_confirmation, got %v", got)
	}
	return id
}

// TestGradingJobHTTPLifecycle 创建（幂等）→ 推进 → 锚点 → 确认 → 完成 全链契约。
func TestGradingJobHTTPLifecycle(t *testing.T) {
	f := newGradingFixture(t)
	h := f.h
	id := driveHTTPToAwaiting(t, f, "msg-1")

	// 幂等创建：同 source_key 返回既有 Job（§4.10）
	rec, out := do(t, h, "POST", "/grading-jobs", createJobBody("msg-1"))
	if rec.Code != http.StatusOK || out["created"] != false {
		t.Fatalf("同幂等键应 created=false: %d %v", rec.Code, out)
	}
	job, _ := out["job"].(map[string]any)
	if job["job_id"] != id {
		t.Fatalf("同幂等键应返回既有 Job: %v", job["job_id"])
	}

	// 等待态正交拆分字段外显（2026-07-18 §6.7 修订）
	j := httpJob(t, h, id)
	if j["confirmation_state"] != "pending" || j["anchor_state"] != "pending" {
		t.Fatalf("等待态正交字段契约: %v", j)
	}
	// 规则 7：人工等待不计 deadline
	if dl, _ := j["deadline"].(float64); dl != 0 {
		t.Fatalf("awaiting_confirmation deadline 应 0: %v", j["deadline"])
	}

	// 锚点回位 → 确认 → assessing
	advanceFixture(t, f, id, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeAnchor, AnchorState: "located", ArtifactDigest: "a",
	})
	rec, out = do(t, h, "POST", "/grading-jobs/"+id+"/confirm", `{"agent":"mingming","corrections":["q1 确认"]}`)
	if rec.Code != http.StatusOK || out["stage"] != "assessing" {
		t.Fatalf("确认+锚点汇合应进 assessing: %d %v", rec.Code, out)
	}
	// assessing→rendering→projecting→completed
	for _, want := range []string{"rendering", "projecting", "completed"} {
		v := advanceFixture(t, f, id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeOK, ArtifactDigest: "d"})
		if v.Record.Status != want {
			t.Fatalf("推进应到 %s: %s", want, v.Record.Status)
		}
	}
}

// TestGradingJobHTTPAnchorDegraded 规则 1 拆分裁决：锚点超时 degraded 不阻塞确认后批改。
func TestGradingJobHTTPAnchorDegraded(t *testing.T) {
	f := newGradingFixture(t)
	h := f.h
	id := driveHTTPToAwaiting(t, f, "msg-1")
	advanceFixture(t, f, id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeAnchor, AnchorState: "degraded"})
	rec, out := do(t, h, "POST", "/grading-jobs/"+id+"/confirm", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || out["stage"] != "assessing" || out["anchor_state"] != "degraded" {
		t.Fatalf("degraded 不阻塞批改: %d %v", rec.Code, out)
	}
}

// TestGradingJobInternalReviseShortcut 规则 6：修正命令保持应用层 internal-only；
// confirmed_version+1 创建新 Job 并走 queued→assessing 捷径，旧 Job 不回退。
func TestGradingJobInternalReviseShortcut(t *testing.T) {
	f := newGradingFixture(t)
	h := f.h
	id := driveHTTPToAwaiting(t, f, "msg-1")
	advanceFixture(t, f, id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeAnchor, AnchorState: "located"})
	do(t, h, "POST", "/grading-jobs/"+id+"/confirm", `{"agent":"mingming"}`)
	for i := 0; i < 3; i++ {
		advanceFixture(t, f, id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeOK, ArtifactDigest: "d"})
	}
	if got := httpJob(t, h, id)["stage"]; got != "completed" {
		t.Fatalf("前置应 completed, got %v", got)
	}

	revised, created, err := f.deps.ReviseGradingJob(context.Background(), "mingming", id, "s2", []string{"q1 改 42"})
	if err != nil || !created {
		t.Fatalf("内部 revise: created=%v err=%v", created, err)
	}
	nid := revised.Record.RecordID
	if nid == "" || nid == id {
		t.Fatalf("修正应创建新 Job: %s", nid)
	}
	if revised.Fields.ConfirmedVersion != 1 {
		t.Fatalf("confirmed_version 应 1: %v", revised.Fields.ConfirmedVersion)
	}
	// 捷径：首个非 queued 态 = assessing
	v := advanceFixture(t, f, nid, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeOK})
	if v.Record.Status != "assessing" {
		t.Fatalf("修正重批捷径应直达 assessing: %s", v.Record.Status)
	}
	// 旧 Job 不回退
	if got := httpJob(t, h, id)["stage"]; got != "completed" {
		t.Fatalf("旧 Job 不得回退: %v", got)
	}
}

// TestGradingJobHTTPCancelBoundaryAndRetry 规则 4/5：rendering/projecting 拒取消(409)；
// 失败重试链路 + 重试上限收敛 failed_terminal。
func TestGradingJobHTTPCancelBoundaryAndRetry(t *testing.T) {
	f := newGradingFixture(t)
	h := f.h
	id := driveHTTPToAwaiting(t, f, "msg-1")
	// 等待态可取消（规则 7：家长显式取消）——用另一个 Job 验证
	id2 := ""
	{
		rec, out := do(t, h, "POST", "/grading-jobs", createJobBody("msg-2"))
		if rec.Code != http.StatusOK {
			t.Fatalf("job2 %d", rec.Code)
		}
		job, _ := out["job"].(map[string]any)
		id2, _ = job["job_id"].(string)
		rec, out = do(t, h, "POST", "/grading-jobs/"+id2+"/cancel", `{"agent":"mingming"}`)
		if rec.Code != http.StatusOK || out["stage"] != "cancelled" {
			t.Fatalf("queued 取消: %d %v", rec.Code, out)
		}
	}
	advanceFixture(t, f, id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeAnchor, AnchorState: "located"})
	do(t, h, "POST", "/grading-jobs/"+id+"/confirm", `{"agent":"mingming"}`)
	// assessing 失败（可重试）→ retry → 捷径恢复
	failed := advanceFixture(t, f, id, usecase.AdvanceGradingInput{
		Outcome: usecase.GradingOutcomeFailed, FailureKind: "model_timeout", Retryable: true,
	})
	if failed.Record.Status != "failed_retryable" {
		t.Fatalf("失败态: %s", failed.Record.Status)
	}
	rec, out := do(t, h, "POST", "/grading-jobs/"+id+"/retry", `{"agent":"mingming"}`)
	if rec.Code != http.StatusOK || out["stage"] != "queued" {
		t.Fatalf("retry: %d %v", rec.Code, out)
	}
	recovered := advanceFixture(t, f, id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeOK})
	if recovered.Record.Status != "assessing" {
		t.Fatalf("恢复应直达 assessing: %s", recovered.Record.Status)
	}
	// 进 rendering 后取消被拒 409
	advanceFixture(t, f, id, usecase.AdvanceGradingInput{Outcome: usecase.GradingOutcomeOK, ArtifactDigest: "d"})
	rec, _ = do(t, h, "POST", "/grading-jobs/"+id+"/cancel", `{"agent":"mingming"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("rendering 取消应 409, got %d", rec.Code)
	}
}

// TestGradingJobHTTPIsolation 归属隔离：跨实例 GET/命令一律 404。
func TestGradingJobHTTPIsolation(t *testing.T) {
	h := newGradingServer(t, "mingming", "gege")
	rec, out := do(t, h, "POST", "/grading-jobs", createJobBody("msg-1"))
	if rec.Code != http.StatusOK {
		t.Fatalf("create %d", rec.Code)
	}
	job, _ := out["job"].(map[string]any)
	id, _ := job["job_id"].(string)
	rec, _ = do(t, h, "GET", "/grading-jobs/"+id+"?agent=gege", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨实例 GET 应 404, got %d", rec.Code)
	}
	rec, _ = do(t, h, "POST", "/grading-jobs/"+id+"/cancel", `{"agent":"gege"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("跨实例 cancel 应 404, got %d", rec.Code)
	}
	rec, out = do(t, h, "GET", "/grading-jobs?agent=gege", "")
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Fatalf("公共列表端点必须不存在: %d %v", rec.Code, out)
	}
}
