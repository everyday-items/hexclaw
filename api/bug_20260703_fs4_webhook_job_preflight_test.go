package api

// FS-4（BUG-20260703）：绑定 cron job 的 webhook 触发时跑 job 的 SourcePrompt，
// 而非空的 wh.Prompt。总览预检若仍用空 wh.Prompt 会把 job 的真实能力面漏成
// 「全绿」，误导用户直接启用。契约：绑 job 的 webhook 预检用 job 的 prompt。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/hexagon-codes/hexclaw/adapter"
	"github.com/hexagon-codes/hexclaw/config"
	"github.com/hexagon-codes/hexclaw/cron"
	"github.com/hexagon-codes/hexclaw/webhook"
)

func TestBug20260703_JobBoundWebhookPreflightUsesJobPrompt(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	ctx := context.Background()

	// 真实 scheduler + 一个会写文件的 job（触发 files 类别，非全绿）。
	provider := &stubProvider{content: stubScriptResp}
	scheduler := cron.NewScheduler(db, cron.NewLLMCompilerStatic(provider, ""),
		cron.NewScriptExecutor().WithWorkdir(t.TempDir()).WithVenvCache(t.TempDir()))
	if err := scheduler.Init(ctx); err != nil {
		t.Fatalf("scheduler Init: %v", err)
	}
	job := &cron.Job{
		ID:           "job-fs4",
		Name:         "归档周报",
		Schedule:     "@daily",
		Type:         cron.JobTypeCron,
		SourcePrompt: "把本周结果下载并保存归档成文件",
		Spec:         &cron.JobSpec{Runtime: "python3", Script: "print(1)"},
		UserID:       "u1",
	}
	if err := scheduler.AddJob(ctx, job); err != nil {
		t.Fatalf("AddJob: %v", err)
	}

	// 绑定该 job 的 webhook（wh.Prompt 为空）。
	whMgr := webhook.NewManager(db)
	if err := whMgr.Init(ctx); err != nil {
		t.Fatalf("webhook Init: %v", err)
	}
	if err := whMgr.Register(ctx, &webhook.Webhook{
		Name: "wh-fs4", Type: webhook.TypeGeneric, JobID: "job-fs4", UserID: "u1",
	}); err != nil {
		t.Fatalf("webhook Register: %v", err)
	}

	srv := NewServer(config.DefaultConfig(), &mockEngine{reply: &adapter.Reply{Content: "ok"}}, nil, nil)
	srv.SetCronScheduler(scheduler)
	srv.SetWebhookManager(whMgr)
	// strict profile：files 类别不自动放行 → 若预检拿到 job prompt，estimated 含 files 且 needs_decision 非空。
	srv.cfg.Security.Autonomy.Profile = "strict"

	// 创建流预检：只带 cron_job_id + 空 prompt（模拟绑 job 的 webhook 预检）。
	body := `{"source":"webhook","cron_job_id":"job-fs4"}`
	req := httptest.NewRequest("POST", "/api/v1/autonomy/preflight", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleAutonomyPreflight(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("预检应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var pf struct {
		Estimated     []string `json:"estimated"`
		NeedsDecision []string `json:"needs_decision"`
		AllClear      bool     `json:"all_clear"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !slices.Contains(pf.Estimated, "files") {
		t.Errorf("[FS-4] 绑 job 的 webhook 预检未拿到 job prompt：estimated=%v（应含 files）", pf.Estimated)
	}
	if pf.AllClear {
		t.Errorf("[FS-4] strict 下写文件任务预检误报 all_clear（漏用 job prompt）")
	}
}
